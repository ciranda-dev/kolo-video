#!/bin/sh
# Runs in the pinned manylinux_2_28 container with no network: builds libkolovpx.so into /out.
set -eu
. /in/pins.sh
. /in/common.sh
w=/tmp/w
rm -rf "$w"
mkdir -p "$w/obj"
unpack_libvpx "/cache/libvpx-$LIBVPX_VERSION.tar" "$w"
src=$w/libvpx-$LIBVPX_VERSION
case $(uname -m) in
x86_64) T=x86_64-linux-gcc; arch=amd64; build_nasm /cache/nasm-$NASM_VERSION.tar.xz "$w/nasm"; PATH=$w/nasm/bin:$PATH ;;
aarch64) T=arm64-linux-gcc; arch=arm64 ;;
esac
cd "$w/obj"
"$src/configure" --target=$T --enable-multithread $LIBVPX_FLAGS >configure.log 2>&1
make -j"$(nproc)" >make.log 2>&1
{ echo '{ global:'; sed 's/$/;/' /in/exports.txt; echo 'local: *; };'; } >exports.map
gcc -O3 -shared -fPIC -I"$src" -I"$w/obj" /in/kv.c libvpx.a -static-libgcc \
	-Wl,--version-script=exports.map -Wl,-z,noexecstack -Wl,-z,relro -Wl,-z,now -Wl,-s \
	-lpthread -lm -o /out/libkolovpx-linux-$arch.so
chmod 755 /out/libkolovpx-linux-$arch.so
gcc --version | head -1
nasm -v 2>/dev/null || true
