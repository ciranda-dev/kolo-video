#!/bin/sh
# Runs in the pinned llvm-mingw container with no network: builds libkolovpx.dll for $1 (x86_64 or aarch64).
set -eu
. /in/pins.sh
. /in/common.sh
a=$1
w=/tmp/w
rm -rf "$w"
mkdir -p "$w/obj"
unpack_libvpx "/cache/libvpx-$LIBVPX_VERSION.tar" "$w"
src=$w/libvpx-$LIBVPX_VERSION
case $a in
x86_64) T=x86_64-win64-gcc; arch=amd64; build_nasm /cache/nasm-$NASM_VERSION.tar.xz "$w/nasm"; PATH=$w/nasm/bin:$PATH ;;
aarch64) T=arm64-win64-gcc; arch=arm64 ;;
esac
cd "$w/obj"
CROSS=$a-w64-mingw32- "$src/configure" --target=$T --enable-multithread $LIBVPX_FLAGS >configure.log 2>&1
make -j"$(nproc)" >make.log 2>&1
{ echo EXPORTS; cat /in/exports.txt; } >exports.def
$a-w64-mingw32-clang -O3 -shared -I"$src" -I"$w/obj" /in/kv.c libvpx.a exports.def -static -lpthread \
	-Wl,--no-insert-timestamp -s -o /out/libkolovpx-windows-$arch.dll
chmod 755 /out/libkolovpx-windows-$arch.dll
$a-w64-mingw32-clang --version | head -1
