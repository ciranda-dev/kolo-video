#!/bin/sh
# Runs in the pinned Debian container with no network: builds kolovpx.wasm into /out.
set -eu
. /in/pins.sh
. /in/common.sh
echo "$WASI_SDK_SHA256  /cache/$WASI_SDK.tar.gz" | sha256sum -c -
w=/tmp/w
rm -rf "$w"
mkdir -p "$w/obj"
tar xzf "/cache/$WASI_SDK.tar.gz" -C "$w"
unpack_libvpx "/cache/libvpx-$LIBVPX_VERSION.tar" "$w"
sdk=$w/$WASI_SDK
src=$w/libvpx-$LIBVPX_VERSION
T="--target=wasm32-wasip1 --sysroot=$sdk/share/wasi-sysroot"
F="-mcpu=lime1 -I/in"
cd "$w/obj"
CC="$sdk/bin/clang $T" CXX="$sdk/bin/clang++ $T" LD="$sdk/bin/clang $T" \
	AR=$sdk/bin/llvm-ar NM=$sdk/bin/llvm-nm STRIP=$sdk/bin/llvm-strip RANLIB=$sdk/bin/llvm-ranlib \
	CFLAGS="$F" CXXFLAGS="$F -fno-exceptions" \
	"$src/configure" --target=generic-gnu --disable-multithread --disable-runtime-cpu-detect $LIBVPX_FLAGS >configure.log 2>&1
make -j"$(nproc)" >make.log 2>&1
"$sdk/bin/clang" $T -O3 $F -mexec-model=reactor -I"$src" -I"$w/obj" /in/kv.c libvpx.a \
	-Wl,-z,stack-size=$STACK -Wl,--max-memory=$MAXMEM -Wl,--strip-all -o /out/kolovpx.wasm
chmod 644 /out/kolovpx.wasm
"$sdk/bin/clang" --version | head -1
