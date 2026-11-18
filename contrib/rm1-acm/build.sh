#!/bin/bash
#
# Build u_serial.ko and usb_f_acm.ko (USB serial gadget) for GL RM1 firmware
# with kernel "4.19.111 SMP preempt mod_unload ARMv7 p2v8".
#
# Usage: ./build.sh [workdir]        (default workdir: ./rm1-build)
#
# Host needs: git, make, gcc, bc, perl, python3, bison, flex, and OpenSSL
# headers (Debian/Ubuntu: apt install bison flex libssl-dev bc).
#
# Kernel source and toolchain come from GL's published repos. Their
# rv1126_gl_rm1_defconfig is missing a few options the shipped kernel has;
# the ones that matter for module ABI are added below. This was checked by
# comparing the compiled code of every function the modules call (and the
# composite/configfs/UDC/TTY cores) against the live kernel via /dev/mem:
# all identical apart from runtime-patched jump labels and absolute addresses.

set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
WORK="$(realpath -m "${1:-$PWD/rm1-build}")"
KERNEL="$WORK/kernel"
SDK="$WORK/sdk"
OUT="$WORK/build"
MOD="$WORK/acm-mod"
TOOLCHAIN="$SDK/toolchain/gcc-linaro-6.3.1-2017.05-x86_64_arm-linux-gnueabihf/bin/arm-linux-gnueabihf-"

mkdir -p "$WORK" "$WORK/bin"
[ -d "$KERNEL" ] || git clone --depth 1 https://github.com/gl-inet/kernel-4.19.git "$KERNEL"
[ -d "$SDK" ] || GIT_LFS_SKIP_SMUDGE=1 git clone --depth 1 https://github.com/gl-inet/glkvm-opensdk-rv1126.git "$SDK"

# The kernel's gcc-wrapper.py is invoked as "python"; it runs fine under python3
command -v python >/dev/null || ln -sf "$(command -v python3)" "$WORK/bin/python"
export PATH="$WORK/bin:$PATH"

kmake() {
	# LOCALVERSION= stops setlocalversion appending "+" (git tree), which
	# would make the vermagic "4.19.111+" and the device refuse the modules.
	# -fcommon/-std=gnu11 let the 4.19 host tools build with modern gcc.
	make -C "$KERNEL" O="$OUT" ARCH=arm CROSS_COMPILE="$TOOLCHAIN" LOCALVERSION= \
		HOSTCFLAGS="-fcommon -std=gnu11 ${HOSTCFLAGS_EXTRA:-}" HOSTLDFLAGS="${HOSTLDFLAGS_EXTRA:-}" "$@"
}

kmake rv1126_gl_rm1_defconfig
"$KERNEL/scripts/config" --file "$OUT/.config" \
	-e USB_CONFIGFS_MASS_STORAGE -e USB_CONFIGFS_F_MTP -e USB_CONFIGFS_F_HID \
	-e TMPFS_POSIX_ACL -e EXT4_FS_POSIX_ACL
kmake olddefconfig
kmake -j"$(nproc)" vmlinux   # generates headers, kernel.release and Module.symvers

mkdir -p "$MOD"
cp "$HERE/Kbuild" "$MOD/"
cp "$KERNEL"/drivers/usb/gadget/function/{u_serial.c,u_serial.h,f_acm.c} "$MOD/"
kmake M="$MOD" modules
"${TOOLCHAIN}strip" --strip-debug "$MOD/u_serial.ko" "$MOD/usb_f_acm.ko"

for ko in "$MOD/u_serial.ko" "$MOD/usb_f_acm.ko"; do
	echo "$(basename "$ko"): $("${TOOLCHAIN}objcopy" -O binary --only-section=.modinfo "$ko" /dev/stdout | tr '\0' '\n' | grep vermagic)"
done
echo "Modules are in $MOD"
