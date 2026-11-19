# Guest agent (qemu-ga over USB serial)

The KVM can talk to the stock [QEMU Guest Agent](https://www.qemu.org/docs/master/interop/qemu-ga.html)
running on the target host, much like a VM host does. Over a USB serial port it gets:

- hostname, OS name and version, architecture
- network interfaces and IP addresses
- logged-in users and time zone
- clean **shutdown** and **reboot** of the host

No custom software is needed on the host. Windows 10/11 and Linux both have the USB
serial driver built in.

On Windows you can use [`glkvm-agent`](../contrib/glkvm-agent/README.md) instead of qemu-ga.
It speaks the same protocol, installs without admin rights, and adds **clipboard sync**
(text, both ways). It runs only while someone is logged in. See [section 2a](#2a-or-glkvm-agent-windows-clipboard-sync).

## How it fits together

```
 KVM (kvmd)                           USB cable                 Target host
 GuestApi ── /dev/ttyGS0 ── CDC-ACM gadget (acm.usb0) ──────────── COMx / /dev/ttyACM0 ── qemu-ga
```

- `kvmd/apps/kvmd/guest_agent.py`: the qemu-ga protocol client (sync, commands, timeouts).
- `kvmd/apps/kvmd/api/guest.py`: a background session (ping every 5 s, info refresh every 60 s) and the HTTP API.
- `web/guest/index.html`: a small status page at `https://<kvm>/guest/`.
- `contrib/glkvm-agent/`: the optional Windows agent (Go).

## Requirements

The KVM's kernel must support the USB serial gadget (`CONFIG_USB_CONFIGFS_ACM`).
**Current RM1 firmware (1.10.1) does not.** Enabling serial there makes `kvmd-otg` fail at
boot, which also leaves the keyboard and mouse uninitialised. To check a device, run
`mkdir /sys/kernel/config/usb_gadget/*/functions/acm.test && rmdir $_`. If the `mkdir`
fails, the kernel has no support.

**On the RM1, use [`contrib/rm1-acm`](../contrib/rm1-acm/README.md) instead of step 1
below.** It builds the two missing kernel modules and adds the port after boot, without
touching kvmd's config. On GL firmware the web root is `/usr/share/kvmd/glweb`, so install
the status page as `/usr/share/kvmd/glweb/guest/index.html`.

## 1. Turn on the USB serial port (KVM)

Create the function at boot without starting it (it uses 3 USB endpoints), in
`/etc/kvmd/override.yaml`:

```yaml
otg:
    devices:
        serial:
            enabled: true
            start: false
```

Reboot the KVM, then link the port when you need it. Either:

- open `https://<kvm>/guest/` and click **Turn USB port on**, or
- `curl -k -X POST "https://<kvm>/api/system/otg_functions?enable_serial=1"` (with your auth).

This briefly re-enumerates the KVM's USB devices on the host. If it fails with
`No available endpoints`, turn off something you don't use (MTP, microphone, camera, or the
second mass-storage drive) and try again.

## 2. Install qemu-ga on Windows

1. Check Device Manager: **Ports (COM & LPT)** should now list **USB Serial Device (COMn)**.
   Note the number. It stays the same for this KVM.
2. Install `qemu-ga-x86_64.msi` from the virtio-win project
   (the `guest-agent` folder of the virtio-win ISO, or the `latest-qemu-ga` direct download).
   You don't need the virtio drivers themselves.
3. Point the service at the COM port. The service installer doesn't keep the `-m` channel
   option, so set the full command line yourself. In an **admin PowerShell** (avoids
   `sc.exe` quoting problems; PowerShell mangles `\"` in native-command arguments):

   ```powershell
   $exe  = 'C:\Program Files\Qemu-ga\qemu-ga.exe'
   $port = 'COM5'
   $rpcs = 'guest-sync-delimited,guest-sync,guest-ping,guest-info,guest-get-host-name,guest-get-osinfo,guest-network-get-interfaces,guest-get-users,guest-get-timezone,guest-shutdown'
   if (-not (Get-Service qemu-ga -ErrorAction SilentlyContinue)) { & $exe -s install }
   Set-ItemProperty 'HKLM:\SYSTEM\CurrentControlSet\Services\qemu-ga' -Name ImagePath -Value "`"$exe`" -d -m isa-serial -p $port -a $rpcs"
   Set-Service qemu-ga -StartupType Automatic
   sc.exe failure qemu-ga reset= 0 actions= restart/5000/restart/5000/restart/5000
   Restart-Service qemu-ga
   ```

   Or, in an **admin command prompt** (`cmd.exe`, not PowerShell):

   ```bat
   sc.exe config QEMU-GA start= auto binPath= "\"C:\Program Files\Qemu-ga\qemu-ga.exe\" -d -m isa-serial -p COM5 -a guest-sync-delimited,guest-sync,guest-ping,guest-info,guest-get-host-name,guest-get-osinfo,guest-network-get-interfaces,guest-get-users,guest-get-timezone,guest-shutdown"
   sc.exe failure QEMU-GA reset= 0 actions= restart/5000/restart/5000/restart/5000
   sc.exe stop QEMU-GA
   sc.exe start QEMU-GA
   ```

   Replace `COM5` with your port. `-a` (`--allow-rpcs`, qemu-ga 8.1+) limits the agent to the
   commands the KVM uses. Without it, qemu-ga also allows file access and running commands as
   SYSTEM. If your qemu-ga is older and rejects `-a`, drop it and use
   `-b guest-exec,guest-exec-status,guest-file-open,guest-file-read,guest-file-write,guest-file-close,guest-file-seek,guest-file-flush,guest-set-user-password`
   instead.

   The failure actions restart the service if it starts before the COM port exists
   (for example, the KVM is plugged in after boot).

4. Open `https://<kvm>/guest/`. Within a few seconds it should show **Agent connected** and the
   host's details.

## 2a. Or glkvm-agent (Windows, clipboard sync)

Instead of step 2:

1. If qemu-ga is installed, stop it first. Only one program can hold the COM port. In an admin
   PowerShell: `Stop-Service qemu-ga; Set-Service qemu-ga -StartupType Disabled`.
2. Download `glkvm-agent-amd64.exe` (or `-arm64`) from the links on `https://<kvm>/guest/`.
   To serve them, copy `contrib/glkvm-agent/dist/*.exe` next to the page.
3. Run `glkvm-agent-amd64.exe -install`. You don't need admin. It starts now and at every
   login, and finds the KVM's COM port by USB ID.

The Guest page then shows a **Clipboard** card:

- **Host clipboard:** text copied on the host appears here within about a second while the page
  is open. **Copy to this device** puts it on the clipboard of the machine you're browsing from.
- **Send to host:** puts text on the host's clipboard. Paste or type it into the box and click
  **Send to host**. **Paste from this device & send** does both in one step, once the browser
  allows clipboard access.

### In the main KVM view

`web/guest/kvm-clipboard.js` adds the same sync to the main KVM UI. On GL firmware,
`contrib/rm1-acm/S51guest-ui` adds it to GL's UI. It works like VM guest additions:

- **Ctrl+V** (Cmd+V on a Mac) on the video first puts this device's clipboard on the host,
  then sends the keystroke, so the host pastes what you copied locally. Keys pressed in the
  meantime are held and replayed in order. In text fields of the KVM UI itself, Ctrl+V works
  normally.
- **Copying on the host** puts the text on this device's clipboard while the KVM tab is in
  focus. Firefox and Safari only allow that during a click or key press, so there it happens
  on your next one.
- A small **📋 sync on** pill at the bottom left turns this off or on (remembered per browser).

The browser's own paste event carries the clipboard text, so Chrome doesn't ask for clipboard
permission. If that event doesn't arrive, the script asks the Clipboard API, which can prompt
once. If it gets nothing within 1.5 s, the keystroke goes through and the host pastes its own
clipboard.

The KVM polls the host clipboard only while someone has the page open (or has read
`/api/guest/clipboard` in the last 30 s). At other times it just pings the agent every 5 s.

## 2b. Linux hosts

```sh
sudo apt install qemu-guest-agent     # or dnf / pacman
sudo systemctl edit qemu-guest-agent  # add:
#   [Service]
#   ExecStart=
#   ExecStart=/usr/bin/qemu-ga -m isa-serial -p /dev/ttyACM0
sudo systemctl restart qemu-guest-agent
```

If the host has other `ttyACM` devices, add a udev rule that gives the KVM's port a fixed
name and use that path instead.

## API

| Method | Path | |
|---|---|---|
| GET  | `/api/guest` | State: `port_enabled`, `device`, `connected`, `last_seen`, `agent`, `info` |
| POST | `/api/guest/refresh` | Re-read host info now (503 if not connected) |
| POST | `/api/guest/power?action=shutdown\|reboot` | Ask the host to shut down / reboot |
| GET  | `/api/guest/clipboard` | Host clipboard: `supported`, `seq`, `text` (null if not text), `truncated`, `updated`, `error`. Also keeps fast polling on for 30 s |
| POST | `/api/guest/clipboard` | Body `{"text": "..."}` (max 256 KiB): set the host clipboard. 503 without glkvm-agent |

State changes are also pushed on the kvmd websocket as `guest` events.
Info from the last session stays in `info` after the agent disconnects, so you can still see
the host's last known IP while it's rebooting.
