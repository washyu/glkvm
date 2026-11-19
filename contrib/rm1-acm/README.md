# USB serial (CDC-ACM) gadget for GL RM1

RM1 firmware (1.10.x, kernel `4.19.111`) is built without the USB serial gadget,
so the guest agent's port (`/dev/ttyGS0`) can't exist out of the box. The kernel
does accept modules, so this adds the two missing ones without touching the
kernel or the firmware.

| File | What it is |
|---|---|
| `build.sh` | Builds `u_serial.ko` and `usb_f_acm.ko` from GL's published kernel and toolchain |
| `Kbuild` | Out-of-tree module build file used by `build.sh` |
| `S50guest-serial` | Post-boot script: loads the modules and adds `acm.usb0` to the gadget |
| `S51guest-ui` | Post-boot script: serves the Guest page and agent downloads, and adds clipboard sync to GL's main UI |

## Why it's safe to load

`build.sh` reconstructs the shipped kernel config: GL's `rv1126_gl_rm1_defconfig`
plus the gadget HID, mass storage and MTP functions and POSIX ACLs that the shipped
kernel has. Every kernel function the modules call (64), plus the whole composite,
configfs, UDC core and TTY layer (400 functions), was compared against the live
kernel through `/dev/mem`. All are identical apart from addresses and runtime-patched
jump labels. The one exception is GL's logic patch in `configfs_composite_bind`,
which leaves the structure layout unchanged.

## Why not `otg.devices.serial.enabled`

If `kvmd-otg` is told to create `acm.usb0` and the kernel can't, it aborts before
binding the gadget, and the KVM comes up with **no keyboard or mouse**. That would
happen whenever the modules are missing or built for a different kernel, for example
after a firmware update. So the serial function is never put in kvmd's config.
`S50guest-serial` adds it afterwards, and any failure just leaves serial off.

New `/etc/init.d` scripts don't work either: `rcS` lists that directory before the
overlay is mounted, so scripts added at runtime never run. GL's
`/etc/kvmd/user/scripts/S??*` hook (run by `S99custom`) is the supported place.

## Install

```sh
./build.sh                                   # -> rm1-build/acm-mod/*.ko
ssh root@<kvm> 'mkdir -p /lib/modules/4.19.111/extra /etc/kvmd/user/scripts'
scp rm1-build/acm-mod/{u_serial,usb_f_acm}.ko root@<kvm>:/lib/modules/4.19.111/extra/
scp S50guest-serial root@<kvm>:/etc/kvmd/user/scripts/
ssh root@<kvm> 'chmod +x /etc/kvmd/user/scripts/S50guest-serial && /etc/kvmd/user/scripts/S50guest-serial start'
```

The function then appears in `kvmd-otgconf -l` as `acm.usb0 [3] Serial Port`, unlinked.
Link it with `kvmd-otgconf -e acm.usb0`, or with the OTG toggle
(`/api/system/otg_functions?enable_serial=1`). The toggle saves the setting, so the
script re-links the port on every boot.

**Endpoints:** the port uses 3 of the RM1's 9. With the default functions (keyboard,
absolute mouse, relative mouse, CD-ROM, flash drive = 7) you must turn one off first,
for example the relative mouse (1) or the flash drive (2).

## Web UI (Guest page, clipboard in GL's UI)

GL's main UI is a prebuilt bundle, so clipboard sync is added with one `<script>` tag in
`/usr/share/kvmd/glweb/index.html` that loads `web/guest/kvm-clipboard.js`. `S51guest-ui`
keeps the files in `/etc/kvmd/user/guest-ui/` and on every boot copies them to
`glweb/guest/` and re-adds the tag if it's missing (for example after a firmware update).
The original page is saved as `index.html.pre-guest` the first time.

```sh
ssh root@<kvm> 'mkdir -p /etc/kvmd/user/guest-ui'
scp ../../web/guest/index.html ../../web/guest/kvm-clipboard.js \
    ../glkvm-agent/dist/glkvm-agent-*.exe root@<kvm>:/etc/kvmd/user/guest-ui/
scp S51guest-ui root@<kvm>:/etc/kvmd/user/scripts/
ssh root@<kvm> 'chmod +x /etc/kvmd/user/scripts/S51guest-ui && /etc/kvmd/user/scripts/S51guest-ui start'
```

To take it out: `/etc/kvmd/user/scripts/S51guest-ui remove`, then delete the script.

## Undo

```sh
ssh root@<kvm> 'kvmd-otgconf -d acm.usb0; rm -f /etc/kvmd/user/scripts/S50guest-serial \
  /run/kvmd/otg/acm.usb0@meta.json; rm -rf /lib/modules/4.19.111/extra'   # then reboot
```
