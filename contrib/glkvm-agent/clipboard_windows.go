package main

// Windows clipboard access (CF_UNICODETEXT).
//
// Setting the clipboard needs a window to own it, and the owner gets sent
// messages (WM_DESTROYCLIPBOARD and friends) when other apps change the
// clipboard later. SendMessage to a thread that never pumps messages blocks
// the sender, which would freeze other apps' copy. So all clipboard work runs
// on one locked OS thread that owns a message-only window and pumps messages.

import (
	"errors"
	"fmt"
	"runtime"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	modUser32                      = windows.NewLazySystemDLL("user32.dll")
	procCreateWindowExW            = modUser32.NewProc("CreateWindowExW")
	procGetMessageW                = modUser32.NewProc("GetMessageW")
	procDispatchMessageW           = modUser32.NewProc("DispatchMessageW")
	procPostMessageW               = modUser32.NewProc("PostMessageW")
	procOpenClipboard              = modUser32.NewProc("OpenClipboard")
	procCloseClipboard             = modUser32.NewProc("CloseClipboard")
	procEmptyClipboard             = modUser32.NewProc("EmptyClipboard")
	procGetClipboardData           = modUser32.NewProc("GetClipboardData")
	procSetClipboardData           = modUser32.NewProc("SetClipboardData")
	procIsClipboardFormatAvailable = modUser32.NewProc("IsClipboardFormatAvailable")
	procGetClipboardSequenceNumber = modUser32.NewProc("GetClipboardSequenceNumber")

	modKernel32      = windows.NewLazySystemDLL("kernel32.dll")
	procGlobalAlloc  = modKernel32.NewProc("GlobalAlloc")
	procGlobalFree   = modKernel32.NewProc("GlobalFree")
	procGlobalLock   = modKernel32.NewProc("GlobalLock")
	procGlobalUnlock = modKernel32.NewProc("GlobalUnlock")
	procGlobalSize   = modKernel32.NewProc("GlobalSize")
)

const (
	cfUnicodeText = 13
	gmemMoveable  = 0x0002
	wmApp         = 0x8000
	hwndMessage   = ^uintptr(2) // (HWND)-3
)

type winClipboard struct {
	hwnd uintptr
	jobs chan func()
}

type msg struct {
	hwnd    uintptr
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      struct{ x, y int32 }
	private uint32
}

func startClipboard() (*winClipboard, error) {
	cb := &winClipboard{jobs: make(chan func(), 16)}
	ready := make(chan error, 1)
	go func() {
		runtime.LockOSThread() // Never unlocked: this thread owns the window
		cls, _ := windows.UTF16PtrFromString("STATIC")
		hwnd, _, err := procCreateWindowExW.Call(0, uintptr(unsafe.Pointer(cls)), 0, 0, 0, 0, 0, 0, hwndMessage, 0, 0, 0)
		if hwnd == 0 {
			ready <- fmt.Errorf("CreateWindowEx: %v", err)
			return
		}
		cb.hwnd = hwnd
		ready <- nil
		var m msg
		for {
			r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
			if int32(r) <= 0 {
				return
			}
			if m.message == wmApp {
				for drained := false; !drained; {
					select {
					case job := <-cb.jobs:
						job()
					default:
						drained = true
					}
				}
				continue
			}
			procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
		}
	}()
	if err := <-ready; err != nil {
		return nil, err
	}
	return cb, nil
}

// run executes fn on the clipboard thread and waits for it.
func (cb *winClipboard) run(fn func() error) error {
	done := make(chan error, 1)
	cb.jobs <- func() { done <- fn() }
	if r, _, err := procPostMessageW.Call(cb.hwnd, wmApp, 0, 0); r == 0 {
		return fmt.Errorf("PostMessage: %v", err)
	}
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		return errors.New("clipboard thread is stuck")
	}
}

func (cb *winClipboard) Seq() uint32 {
	r, _, _ := procGetClipboardSequenceNumber.Call()
	return uint32(r)
}

// open retries for a moment: another app may hold the clipboard briefly.
func (cb *winClipboard) open() error {
	var err error
	for i := 0; i < 20; i++ {
		var r uintptr
		if r, _, err = procOpenClipboard.Call(cb.hwnd); r != 0 {
			return nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	return fmt.Errorf("OpenClipboard: %v", err)
}

func (cb *winClipboard) GetText() (text string, ok bool, err error) {
	err = cb.run(func() error {
		if r, _, _ := procIsClipboardFormatAvailable.Call(cfUnicodeText); r == 0 {
			return nil
		}
		if err := cb.open(); err != nil {
			return err
		}
		defer procCloseClipboard.Call()
		h, _, e := procGetClipboardData.Call(cfUnicodeText)
		if h == 0 {
			return fmt.Errorf("GetClipboardData: %v", e)
		}
		ptr, _, e := procGlobalLock.Call(h)
		if ptr == 0 {
			return fmt.Errorf("GlobalLock: %v", e)
		}
		defer procGlobalUnlock.Call(h)
		size, _, _ := procGlobalSize.Call(h)
		chars := unsafe.Slice((*uint16)(globalPtr(ptr)), size/2)
		text, ok = windows.UTF16ToString(chars), true // Stops at the first NUL
		return nil
	})
	return
}

func (cb *winClipboard) SetText(text string) error {
	data, err := windows.UTF16FromString(text) // NUL-terminated
	if err != nil {
		return err // Text contains a NUL
	}
	return cb.run(func() error {
		size := uintptr(len(data) * 2)
		h, _, e := procGlobalAlloc.Call(gmemMoveable, size)
		if h == 0 {
			return fmt.Errorf("GlobalAlloc: %v", e)
		}
		owned := false
		defer func() {
			if !owned {
				procGlobalFree.Call(h)
			}
		}()
		ptr, _, e := procGlobalLock.Call(h)
		if ptr == 0 {
			return fmt.Errorf("GlobalLock: %v", e)
		}
		copy(unsafe.Slice((*uint16)(globalPtr(ptr)), len(data)), data)
		procGlobalUnlock.Call(h)

		if err := cb.open(); err != nil {
			return err
		}
		defer procCloseClipboard.Call()
		if r, _, e := procEmptyClipboard.Call(); r == 0 {
			return fmt.Errorf("EmptyClipboard: %v", e)
		}
		if r, _, e := procSetClipboardData.Call(cfUnicodeText, h); r == 0 {
			return fmt.Errorf("SetClipboardData: %v", e)
		}
		owned = true // The system owns the memory now
		return nil
	})
}

// globalPtr turns a GlobalLock result into a pointer. The memory belongs to
// Windows, not the Go heap, so the GC rules for uintptr conversions don't apply.
func globalPtr(p uintptr) unsafe.Pointer {
	return *(*unsafe.Pointer)(unsafe.Pointer(&p))
}
