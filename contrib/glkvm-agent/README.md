# glkvm-agent (Windows)

A small guest agent for the KVM's target host. It speaks the
[qemu-ga](https://www.qemu.org/docs/master/interop/qemu-ga.html) protocol over the
KVM's USB serial port, so everything in [docs/guest-agent.md](../../docs/guest-agent.md)
works with it unchanged, and it adds **clipboard sync** (text) on top.

It replaces qemu-ga rather than running beside it: Windows lets only one program open
a COM port at a time.

| | qemu-ga | glkvm-agent |
|---|---|---|
| Hostname, OS, IPs, users, time zone | ✓ | ✓ |
| Shutdown / reboot | ✓ (forced) | ✓ (hung apps closed; unsaved work can still prompt) |
| Clipboard sync | | ✓ |
| Works at the login screen | ✓ (service) | only while someone is logged in |
| Install needs admin | yes | no |
| Runs other commands, file access | unless blocked | no; only the commands listed below |

It runs in the logged-in user's session because the clipboard belongs to that session:
a Windows service can't reach it.

## Install

1. Stop and disable qemu-ga if it's installed (admin PowerShell), or uninstall it:

   ```powershell
   Stop-Service qemu-ga; Set-Service qemu-ga -StartupType Disabled
   ```

2. Download `glkvm-agent-amd64.exe` (or `-arm64`) from `https://<kvm>/guest/`, or build it
   with `./build.sh`.
3. Run `glkvm-agent-amd64.exe -install` as yourself (no admin). It copies itself to
   `%LOCALAPPDATA%\Programs\glkvm-agent`, adds a login entry under
   `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`, and starts.

It finds the KVM's port by USB ID (`VID_38EB&PID_0001` for GL.iNet, `VID_1D6B&PID_0104`
for stock kvmd), so the COM number doesn't matter. To pin a port, use
`-install -port COM6`. It reopens the port by itself when the KVM is unplugged or reboots.

- **Log:** `%LOCALAPPDATA%\Programs\glkvm-agent\glkvm-agent.log`
- **Stop:** `glkvm-agent.exe -stop`
- **Remove:** `glkvm-agent.exe -uninstall` (run the downloaded copy, not the installed one,
  so it can delete the folder)

The exe isn't code-signed, so SmartScreen may warn the first time.

## Commands

Standard qemu-ga commands: `guest-sync`, `guest-sync-delimited`, `guest-ping`, `guest-info`,
`guest-get-host-name`, `guest-get-osinfo`, `guest-network-get-interfaces`, `guest-get-users`,
`guest-get-timezone`, `guest-shutdown`.

Extensions:

| Command | Arguments | Returns |
|---|---|---|
| `glkvm-clipboard-get` | `{"since": seq}` (optional) | `{"seq", "changed": false}` if the clipboard is unchanged since `seq`, else `{"seq", "changed": true, "text": str or null, "truncated": bool}` |
| `glkvm-clipboard-set` | `{"text": str}` | `{"seq"}` after the change |

`seq` is Windows' clipboard sequence number, so polling costs one cheap call when nothing
changed. Text is capped at 256 KiB (UTF-8). Line ends are `\n` on the wire and `\r\n` on
the Windows clipboard. `text` is null when the clipboard holds something else, such as an
image.

## Code

| File | |
|---|---|
| `protocol.go` | qemu-ga framing and dispatch (portable, unit-tested) |
| `clipcmds.go` | clipboard commands (portable, unit-tested) |
| `clipboard_windows.go` | Win32 clipboard on a dedicated message-pumping thread |
| `sysinfo_windows.go` | info and power commands |
| `serial_windows.go` | COM port I/O and finding the KVM's port |
| `main_windows.go` | run loop, logging, install/uninstall |
