package main

// Info and power commands, answering like the Windows build of qemu-ga does
// (same command names and result fields), so the KVM can't tell the difference.

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

func registerSysinfo(p *Protocol) {
	p.Register("guest-get-host-name", func(json.RawMessage) (any, error) {
		name, err := os.Hostname()
		if err != nil {
			return nil, genericError("%v", err)
		}
		return map[string]string{"host-name": name}, nil
	})
	p.Register("guest-get-osinfo", func(json.RawMessage) (any, error) { return osInfo(), nil })
	p.Register("guest-network-get-interfaces", func(json.RawMessage) (any, error) { return interfaces() })
	p.Register("guest-get-users", func(json.RawMessage) (any, error) { return users() })
	p.Register("guest-get-timezone", func(json.RawMessage) (any, error) { return timezone() })
	p.Register("guest-shutdown", shutdown)
}

// ===== OS

func osInfo() map[string]string {
	info := map[string]string{"id": "mswindows", "name": "Microsoft Windows", "machine": nativeMachine()}
	ver := windows.RtlGetVersion()
	build := ver.BuildNumber

	product, displayVersion, ubr := "", "", ""
	if k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows NT\CurrentVersion`, registry.QUERY_VALUE); err == nil {
		product, _, _ = k.GetStringValue("ProductName")
		if displayVersion, _, _ = k.GetStringValue("DisplayVersion"); displayVersion == "" {
			displayVersion, _, _ = k.GetStringValue("ReleaseId")
		}
		if v, _, err := k.GetIntegerValue("UBR"); err == nil {
			ubr = strconv.FormatUint(v, 10)
		}
		k.Close()
	}
	major := strconv.FormatUint(uint64(ver.MajorVersion), 10)
	if ver.MajorVersion == 10 && build >= 22000 {
		// Windows 11 still says "Windows 10" in ProductName
		major = "11"
		product = strings.Replace(product, "Windows 10", "Windows 11", 1)
	}
	if product == "" {
		product = "Windows " + major
	}
	info["pretty-name"] = strings.TrimSpace(product + " " + displayVersion)
	info["version"] = "Microsoft Windows " + major
	info["version-id"] = major
	info["kernel-version"] = displayVersion
	info["kernel-release"] = strconv.FormatUint(uint64(build), 10)
	if ubr != "" {
		info["kernel-release"] += "." + ubr
	}
	if ver.ProductType == 1 { // VER_NT_WORKSTATION
		info["variant"], info["variant-id"] = "client", "client"
	} else {
		info["variant"], info["variant-id"] = "server", "server"
	}
	return info
}

func nativeMachine() string {
	var process, native uint16
	if err := windows.IsWow64Process2(windows.CurrentProcess(), &process, &native); err == nil {
		switch native {
		case 0x8664:
			return "x86_64"
		case 0xAA64:
			return "aarch64"
		case 0x014C:
			return "x86"
		}
	}
	return "unknown"
}

// ===== Network

func interfaces() (any, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, genericError("%v", err)
	}
	type ipAddr struct {
		Type   string `json:"ip-address-type"`
		Addr   string `json:"ip-address"`
		Prefix int    `json:"prefix"`
	}
	type iface struct {
		Name   string   `json:"name"`
		HWAddr string   `json:"hardware-address,omitempty"`
		Addrs  []ipAddr `json:"ip-addresses"`
	}
	result := []iface{}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 {
			continue
		}
		item := iface{Name: ifc.Name, HWAddr: ifc.HardwareAddr.String(), Addrs: []ipAddr{}}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ones, _ := ipn.Mask.Size()
			typ := "ipv6"
			if ipn.IP.To4() != nil {
				typ = "ipv4"
			}
			item.Addrs = append(item.Addrs, ipAddr{Type: typ, Addr: ipn.IP.String(), Prefix: ones})
		}
		result = append(result, item)
	}
	return result, nil
}

// ===== Users

var (
	modWtsapi32                     = windows.NewLazySystemDLL("wtsapi32.dll")
	procWTSQuerySessionInformationW = modWtsapi32.NewProc("WTSQuerySessionInformationW")
)

// WTSINFOW
type wtsInfo struct {
	State                   int32
	SessionID               uint32
	IncomingBytes           uint32
	OutgoingBytes           uint32
	IncomingFrames          uint32
	OutgoingFrames          uint32
	IncomingCompressedBytes uint32
	OutgoingCompressedBytes uint32
	WinStationName          [32]uint16
	Domain                  [17]uint16
	UserName                [21]uint16
	ConnectTime             int64
	DisconnectTime          int64
	LastInputTime           int64
	LogonTime               int64
	CurrentTime             int64
}

func sessionInfo(id uint32) (*wtsInfo, error) {
	const wtsSessionInfo = 24
	var buf *byte
	var size uint32
	r, _, err := procWTSQuerySessionInformationW.Call(0, uintptr(id), wtsSessionInfo, uintptr(unsafe.Pointer(&buf)), uintptr(unsafe.Pointer(&size)))
	if r == 0 {
		return nil, err
	}
	defer windows.WTSFreeMemory(uintptr(unsafe.Pointer(buf)))
	if uintptr(size) < unsafe.Sizeof(wtsInfo{}) {
		return nil, fmt.Errorf("short WTSINFO (%d bytes)", size)
	}
	info := *(*wtsInfo)(unsafe.Pointer(buf))
	return &info, nil
}

func fileTimeToUnix(ft int64) float64 {
	const epochDiff = 116444736000000000 // 1601-01-01 to 1970-01-01 in 100 ns units
	return float64(ft-epochDiff) / 1e7
}

func users() (any, error) {
	var sessions *windows.WTS_SESSION_INFO
	var count uint32
	if err := windows.WTSEnumerateSessions(0, 0, 1, &sessions, &count); err != nil {
		return nil, genericError("WTSEnumerateSessions: %v", err)
	}
	defer windows.WTSFreeMemory(uintptr(unsafe.Pointer(sessions)))

	type user struct {
		User      string  `json:"user"`
		Domain    string  `json:"domain,omitempty"`
		LoginTime float64 `json:"login-time"`
	}
	result := []user{}
	for _, s := range unsafe.Slice(sessions, count) {
		if s.State != windows.WTSActive && s.State != windows.WTSDisconnected {
			continue
		}
		info, err := sessionInfo(s.SessionID)
		if err != nil {
			continue
		}
		name := windows.UTF16ToString(info.UserName[:])
		if name == "" {
			continue
		}
		u := user{User: name, Domain: windows.UTF16ToString(info.Domain[:])}
		if info.LogonTime > 0 {
			u.LoginTime = fileTimeToUnix(info.LogonTime)
		}
		result = append(result, u)
	}
	return result, nil
}

// ===== Time zone

func timezone() (any, error) {
	var tzi windows.Timezoneinformation
	rc, err := windows.GetTimeZoneInformation(&tzi)
	if err != nil {
		return nil, genericError("GetTimeZoneInformation: %v", err)
	}
	name := windows.UTF16ToString(tzi.StandardName[:])
	if rc == 2 { // TIME_ZONE_ID_DAYLIGHT
		name = windows.UTF16ToString(tzi.DaylightName[:])
	}
	_, offset := time.Now().Zone()
	return map[string]any{"zone": name, "offset": offset}, nil
}

// ===== Power

func shutdown(args json.RawMessage) (any, error) {
	var a struct {
		Mode string `json:"mode"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return nil, err
	}
	// EWX_FORCEIFHUNG: apps that hang are closed, but an app with unsaved work
	// can still hold the shutdown up and ask the user (visible through the KVM).
	var flags uint32
	switch a.Mode {
	case "", "powerdown":
		flags = windows.EWX_SHUTDOWN | windows.EWX_POWEROFF | windows.EWX_FORCEIFHUNG
	case "halt":
		flags = windows.EWX_SHUTDOWN | windows.EWX_FORCEIFHUNG
	case "reboot":
		flags = windows.EWX_REBOOT | windows.EWX_FORCEIFHUNG
	default:
		return nil, &CommandError{Class: "InvalidParameter", Desc: "Invalid parameter 'mode'"}
	}
	if err := enablePrivilege("SeShutdownPrivilege"); err != nil {
		return nil, genericError("Can't get shutdown privilege: %v", err)
	}
	const reason = windows.SHTDN_REASON_MAJOR_OTHER | windows.SHTDN_REASON_FLAG_PLANNED
	if err := windows.ExitWindowsEx(flags, reason); err != nil {
		return nil, genericError("ExitWindowsEx: %v", err)
	}
	logf("Shutdown requested by the KVM (mode %q)", a.Mode)
	return noReply{}, nil
}

func enablePrivilege(name string) error {
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_ADJUST_PRIVILEGES|windows.TOKEN_QUERY, &token); err != nil {
		return err
	}
	defer token.Close()
	var luid windows.LUID
	if err := windows.LookupPrivilegeValue(nil, windows.StringToUTF16Ptr(name), &luid); err != nil {
		return err
	}
	tp := windows.Tokenprivileges{PrivilegeCount: 1}
	tp.Privileges[0] = windows.LUIDAndAttributes{Luid: luid, Attributes: windows.SE_PRIVILEGE_ENABLED}
	if err := windows.AdjustTokenPrivileges(token, false, &tp, 0, nil, nil); err != nil {
		return err
	}
	// AdjustTokenPrivileges "succeeds" without the privilege; it reports that via GetLastError
	if e := windows.GetLastError(); e == windows.ERROR_NOT_ALL_ASSIGNED {
		return e
	}
	return nil
}
