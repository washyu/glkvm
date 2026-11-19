package main

import (
	"fmt"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// USB IDs of KVM gadgets: GL.iNet (RM1 and friends) and the kvmd default.
var defaultUSBIDs = []string{"VID_38EB&PID_0001", "VID_1D6B&PID_0104"}

type serialPort struct {
	name string
	h    windows.Handle
}

func openSerial(name string) (*serialPort, error) {
	path, err := windows.UTF16PtrFromString(`\\.\` + name)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(path, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return nil, err
	}
	// Line settings don't matter on a USB virtual port, but keep them sane
	var dcb windows.DCB
	dcb.DCBlength = uint32(unsafe.Sizeof(dcb))
	if windows.GetCommState(h, &dcb) == nil {
		dcb.BaudRate = 115200
		dcb.ByteSize = 8
		dcb.Parity = 0
		dcb.StopBits = 0
		dcb.Flags = 0x0001 | 0x0010 | 0x1000 // fBinary, DTR on, RTS on; no flow control
		_ = windows.SetCommState(h, &dcb)
	}
	// Reads return as soon as anything arrives, or after 500 ms with nothing,
	// so the serve loop can notice a quit request.
	timeouts := windows.CommTimeouts{
		ReadIntervalTimeout:        0xFFFFFFFF,
		ReadTotalTimeoutMultiplier: 0xFFFFFFFF,
		ReadTotalTimeoutConstant:   500,
		WriteTotalTimeoutConstant:  5000,
	}
	if err := windows.SetCommTimeouts(h, &timeouts); err != nil {
		windows.CloseHandle(h)
		return nil, fmt.Errorf("SetCommTimeouts: %w", err)
	}
	_ = windows.PurgeComm(h, windows.PURGE_RXCLEAR|windows.PURGE_TXCLEAR)
	return &serialPort{name: name, h: h}, nil
}

func (p *serialPort) Read(buf []byte) (int, error) {
	var n uint32
	err := windows.ReadFile(p.h, buf, &n, nil)
	return int(n), err
}

func (p *serialPort) Write(data []byte) error {
	for len(data) > 0 {
		var n uint32
		if err := windows.WriteFile(p.h, data, &n, nil); err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("write timed out (is the KVM side reading?)")
		}
		data = data[n:]
	}
	return nil
}

func (p *serialPort) Close() {
	windows.CloseHandle(p.h)
}

// ===== Finding the KVM's port

var guidDevClassPorts = windows.GUID{Data1: 0x4d36e978, Data2: 0xe325, Data3: 0x11ce, Data4: [8]byte{0xbf, 0xc1, 0x08, 0x00, 0x2b, 0xe1, 0x03, 0x18}}

// findKVMPorts returns the COM port names of present serial ports whose
// hardware ID matches one of usbIDs (like "VID_38EB&PID_0001").
func findKVMPorts(usbIDs []string) ([]string, error) {
	devs, err := windows.SetupDiGetClassDevsEx(&guidDevClassPorts, "", 0, windows.DIGCF_PRESENT, 0, "")
	if err != nil {
		return nil, err
	}
	defer devs.Close()

	var ports []string
	for i := 0; ; i++ {
		dev, err := devs.EnumDeviceInfo(i)
		if err == windows.ERROR_NO_MORE_ITEMS {
			break
		} else if err != nil {
			continue
		}
		prop, err := devs.DeviceRegistryProperty(dev, windows.SPDRP_HARDWAREID)
		if err != nil {
			continue
		}
		ids, _ := prop.([]string)
		if !matchesUSBID(ids, usbIDs) {
			continue
		}
		kh, err := devs.OpenDevRegKey(dev, windows.DICS_FLAG_GLOBAL, 0, windows.DIREG_DEV, windows.KEY_READ)
		if err != nil {
			continue
		}
		k := registry.Key(kh)
		name, _, err := k.GetStringValue("PortName")
		k.Close()
		if err == nil && strings.HasPrefix(strings.ToUpper(name), "COM") {
			ports = append(ports, name)
		}
	}
	return ports, nil
}

func matchesUSBID(hardwareIDs []string, usbIDs []string) bool {
	for _, hw := range hardwareIDs {
		hw = strings.ToUpper(hw)
		for _, id := range usbIDs {
			if strings.Contains(hw, strings.ToUpper(id)) {
				return true
			}
		}
	}
	return false
}
