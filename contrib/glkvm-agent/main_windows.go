package main

// glkvm-agent: a guest agent for the target host of a GL.iNet / PiKVM KVM.
//
// It talks the qemu-ga protocol over the KVM's USB serial port, so kvmd's
// guest API works with it unchanged, and adds clipboard sync. It runs in the
// logged-in user's session (started from HKCU\...\Run), so it needs no admin
// rights and can reach the user's clipboard.
//
//   glkvm-agent.exe                    run (normally started at login)
//   glkvm-agent.exe -install [-port COM6]
//   glkvm-agent.exe -uninstall
//   glkvm-agent.exe -stop

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const (
	version     = "0.1.0"
	appName     = "glkvm-agent"
	runKeyPath  = `Software\Microsoft\Windows\CurrentVersion\Run`
	mutexName   = `Local\glkvm-agent-instance`
	quitEvtName = `Local\glkvm-agent-quit`
)

var (
	logMu  sync.Mutex
	logOut io.Writer = io.Discard
)

func logf(format string, args ...any) {
	logMu.Lock()
	defer logMu.Unlock()
	fmt.Fprintf(logOut, "%s %s\n", time.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(format, args...))
}

func appDir() string {
	return filepath.Join(os.Getenv("LOCALAPPDATA"), "Programs", appName)
}

func openLog() {
	dir := appDir()
	_ = os.MkdirAll(dir, 0o755)
	path := filepath.Join(dir, appName+".log")
	if st, err := os.Stat(path); err == nil && st.Size() > 1<<20 {
		_ = os.Rename(path, path+".old")
	}
	if f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
		logOut = f
	}
}

func msgBox(text string, isError bool) {
	flags := uint32(windows.MB_OK | windows.MB_ICONINFORMATION)
	if isError {
		flags = windows.MB_OK | windows.MB_ICONERROR
	}
	_, _ = windows.MessageBox(0, windows.StringToUTF16Ptr(text), windows.StringToUTF16Ptr("GLKVM guest agent"), flags)
}

func main() {
	port := flag.String("port", "auto", `COM port of the KVM ("COM6"), or "auto" to find it by USB ID`)
	usbIDs := flag.String("usb-id", strings.Join(defaultUSBIDs, ","), "USB IDs to look for in auto mode")
	install := flag.Bool("install", false, "copy to %LOCALAPPDATA%\\Programs\\glkvm-agent, start at login, and start now")
	uninstall := flag.Bool("uninstall", false, "stop, remove the login entry and the installed copy")
	stop := flag.Bool("stop", false, "stop the running agent")
	flag.Parse()

	switch {
	case *install:
		if err := doInstall(*port); err != nil {
			msgBox("Install failed: "+err.Error(), true)
			os.Exit(1)
		}
		msgBox("Installed and started.\n\nThe agent now starts when you log in. Log: "+filepath.Join(appDir(), appName+".log"), false)
		return
	case *uninstall:
		if err := doUninstall(); err != nil {
			msgBox("Uninstall failed: "+err.Error(), true)
			os.Exit(1)
		}
		msgBox("Removed.", false)
		return
	case *stop:
		signalQuit()
		return
	}

	openLog()
	mutex, err := windows.CreateMutex(nil, false, windows.StringToUTF16Ptr(mutexName))
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		logf("Another instance is already running; exiting")
		return
	} else if err != nil {
		logf("CreateMutex: %v", err)
	}
	defer windows.CloseHandle(mutex)

	quit, err := windows.CreateEvent(nil, 1, 0, windows.StringToUTF16Ptr(quitEvtName))
	if err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		logf("CreateEvent: %v", err)
	}
	_ = windows.ResetEvent(quit) // A stale signal from an earlier -stop

	logf("glkvm-agent %s starting (port %s)", version, *port)
	proto := NewProtocol("glkvm-agent " + version)
	registerSysinfo(proto)
	if cb, err := startClipboard(); err != nil {
		logf("Clipboard unavailable: %v", err)
	} else {
		registerClipboard(proto, cb)
	}
	run(proto, *port, strings.Split(*usbIDs, ","), quit)
	logf("Stopped")
}

func quitRequested(quit windows.Handle) bool {
	ev, _ := windows.WaitForSingleObject(quit, 0)
	return ev == windows.WAIT_OBJECT_0
}

// run keeps (re)opening the KVM's port and serving it until asked to quit.
func run(proto *Protocol, portFlag string, usbIDs []string, quit windows.Handle) {
	lastProblem := ""
	problem := func(format string, args ...any) {
		// Log each distinct problem once, not every retry
		if msg := fmt.Sprintf(format, args...); msg != lastProblem {
			logf("%s", msg)
			lastProblem = msg
		}
	}
	for !quitRequested(quit) {
		name := portFlag
		if strings.EqualFold(portFlag, "auto") {
			ports, err := findKVMPorts(usbIDs)
			switch {
			case err != nil:
				problem("Can't list serial ports: %v", err)
				name = ""
			case len(ports) == 0:
				problem("No KVM serial port found (USB IDs %s). Is the KVM plugged in with its serial port on?", strings.Join(usbIDs, ","))
				name = ""
			default:
				if len(ports) > 1 {
					problem("Several KVM serial ports found (%s); using %s", strings.Join(ports, ", "), ports[0])
				}
				name = ports[0]
			}
		}
		if name != "" {
			port, err := openSerial(name)
			if err != nil {
				if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
					problem("%s is in use by another program (stop the qemu-ga service if it's still installed)", name)
				} else {
					problem("Can't open %s: %v", name, err)
				}
			} else {
				logf("Opened %s", name)
				lastProblem = ""
				err := serve(proto, port, quit)
				port.Close()
				if err != nil {
					logf("Closed %s: %v", name, err)
				}
			}
		}
		windows.WaitForSingleObject(quit, 3000)
	}
}

func serve(proto *Protocol, port *serialPort, quit windows.Handle) error {
	proto.Reset()
	buf := make([]byte, 64*1024)
	for !quitRequested(quit) {
		n, err := port.Read(buf)
		if err != nil {
			return err
		}
		if n == 0 {
			continue
		}
		if out := proto.Feed(buf[:n]); len(out) > 0 {
			if err := port.Write(out); err != nil {
				return err
			}
		}
	}
	return nil
}

// ===== Install / uninstall (per user, no admin)

func signalQuit() bool {
	ev, err := windows.OpenEvent(windows.EVENT_MODIFY_STATE, false, windows.StringToUTF16Ptr(quitEvtName))
	if err != nil {
		return false // Not running
	}
	defer windows.CloseHandle(ev)
	_ = windows.SetEvent(ev)
	return true
}

// waitStopped waits for a running instance to release its mutex.
func waitStopped(timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		h, err := windows.OpenMutex(windows.SYNCHRONIZE, false, windows.StringToUTF16Ptr(mutexName))
		if err != nil {
			return
		}
		windows.CloseHandle(h)
		time.Sleep(200 * time.Millisecond)
	}
}

func doInstall(port string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	dir := appDir()
	target := filepath.Join(dir, appName+".exe")
	if signalQuit() {
		waitStopped(5 * time.Second)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if !strings.EqualFold(filepath.Clean(self), filepath.Clean(target)) {
		if err := copyFile(self, target); err != nil {
			return fmt.Errorf("copy to %s: %w", target, err)
		}
	}
	cmdline := fmt.Sprintf(`"%s"`, target)
	if !strings.EqualFold(port, "auto") {
		cmdline += " -port " + port
	}
	k, _, err := registry.CreateKey(registry.CURRENT_USER, runKeyPath, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if err := k.SetStringValue(appName, cmdline); err != nil {
		return err
	}
	args := []string{}
	if !strings.EqualFold(port, "auto") {
		args = append(args, "-port", port)
	}
	cmd := exec.Command(target, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.DETACHED_PROCESS}
	return cmd.Start()
}

func doUninstall() error {
	if signalQuit() {
		waitStopped(5 * time.Second)
	}
	if k, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.SET_VALUE); err == nil {
		_ = k.DeleteValue(appName)
		k.Close()
	}
	self, _ := os.Executable()
	target := filepath.Join(appDir(), appName+".exe")
	if strings.EqualFold(filepath.Clean(self), filepath.Clean(target)) {
		// Can't delete ourselves while running; leave the folder for the user
		return nil
	}
	return os.RemoveAll(appDir())
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".new"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}
