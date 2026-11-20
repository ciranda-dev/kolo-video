# Shared helpers: configure flags, source verification. Sourced by the inner scripts.
LIBVPX_FLAGS="--enable-vp8 --enable-vp9 --disable-vp9-highbitdepth --enable-realtime-only --enable-static --disable-shared --enable-pic --disable-examples --disable-tools --disable-docs --disable-install-docs --disable-unit-tests --disable-webm-io --disable-libyuv"

# unpack_libvpx <tar> <dir>: checks that the archive was made from the pinned commit.
unpack_libvpx() {
	head -c 1024 "$1" | grep -aq "comment=$LIBVPX_COMMIT" || { echo "libvpx archive is not commit $LIBVPX_COMMIT" >&2; exit 1; }
	mkdir -p "$2"
	tar xf "$1" -C "$2"
}

# build_nasm <tarball> <prefix-dir>: builds the pinned NASM from source.
build_nasm() {
	echo "$NASM_SHA256  $1" | sha256sum -c - >&2
	mkdir -p "$2/src"
	tar xJf "$1" -C "$2/src"
	(cd "$2/src/nasm-$NASM_VERSION" && ./configure >/dev/null && make -j"$(nproc)" nasm >/dev/null)
	mkdir -p "$2/bin"
	cp "$2/src/nasm-$NASM_VERSION/nasm" "$2/bin/nasm"
}
