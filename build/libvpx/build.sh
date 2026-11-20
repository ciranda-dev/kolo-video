#!/bin/sh
# Rebuilds the libvpx artifacts from the pinned inputs.
# Usage: build.sh [--out dir] target...
#   targets: wasm linux-amd64 linux-arm64 windows-amd64 windows-arm64 darwin-arm64 darwin-amd64 all
# Without --out the files go to their committed places and SHA256SUMS and sums.go are rewritten.
# wasm and the Linux and Windows libraries build in Docker containers with no network; the macOS
# libraries build on this Mac with the Xcode, SDK and clang pinned in pins.sh.
set -eu

here=$(cd "$(dirname "$0")" && pwd)
root=$(dirname "$(dirname "$here")")
. "$here/pins.sh"
. "$here/common.sh"

out=
if [ "${1:-}" = --out ]; then
	mkdir -p "$2"
	out=$(cd "$2" && pwd)
	shift 2
fi
[ $# -gt 0 ] || { echo "usage: build.sh [--out dir] target..." >&2; exit 2; }
targets=$*
[ "$targets" = all ] && targets="wasm linux-amd64 linux-arm64 windows-amd64 windows-arm64 darwin-arm64 darwin-amd64"

cache=${KOLO_VIDEO_BUILD_CACHE:-${TMPDIR:-/tmp}/kolo-video-build-cache}
mkdir -p "$cache"

hostsha() { if command -v sha256sum >/dev/null; then sha256sum "$1"; else shasum -a 256 "$1"; fi | cut -d' ' -f1; }
fetch() {
	[ "$(hostsha "$cache/$3" 2>/dev/null || true)" = "$2" ] && return
	curl -fsSL -o "$cache/$3.part" "$1"
	[ "$(hostsha "$cache/$3.part")" = "$2" ] || { rm -f "$cache/$3.part"; echo "build.sh: checksum mismatch for $1" >&2; exit 1; }
	mv "$cache/$3.part" "$cache/$3"
}
fetch_libvpx() {
	f=$cache/libvpx-$LIBVPX_VERSION.tar
	if [ -f "$f" ] && head -c 1024 "$f" | grep -aq "comment=$LIBVPX_COMMIT"; then return; fi
	rm -rf "$cache/libvpx.git"
	git -c advice.detachedHead=false clone -q --depth 1 --branch "$LIBVPX_TAG" "$LIBVPX_URL" "$cache/libvpx.git"
	[ "$(git -C "$cache/libvpx.git" rev-parse HEAD)" = "$LIBVPX_COMMIT" ] || { echo "build.sh: $LIBVPX_TAG is not commit $LIBVPX_COMMIT" >&2; exit 1; }
	[ "$(git -C "$cache/libvpx.git" rev-parse "refs/tags/$LIBVPX_TAG")" = "$LIBVPX_TAG_OBJECT" ] || { echo "build.sh: tag object differs" >&2; exit 1; }
	git -C "$cache/libvpx.git" archive --format=tar --prefix="libvpx-$LIBVPX_VERSION/" HEAD >"$f.part"
	mv "$f.part" "$f"
}
dest() { # dest <file>: where a finished artifact goes
	if [ -n "$out" ]; then echo "$out"; return; fi
	case $1 in kolovpx.wasm) echo "$here" ;; *) echo "$root/vpx/internal/native" ;; esac
}
docker_run() { # docker_run <platform> <image> <script> [args]
	p=$1; img=$2; s=$3; shift 3
	tmp=$(mktemp -d)
	docker run --rm --platform "$p" --network none --user "$(id -u):$(id -g)" -e HOME=/tmp \
		-v "$here:/in:ro" -v "$cache:/cache:ro" -v "$tmp:/out" "$img" sh "/in/$s" "$@"
	for f in "$tmp"/*; do
		d=$(dest "$(basename "$f")"); mkdir -p "$d"
		mv -f "$f" "$d/.$(basename "$f").new"; mv -f "$d/.$(basename "$f").new" "$d/$(basename "$f")"
		echo "$d/$(basename "$f"): $(hostsha "$d/$(basename "$f")")"
	done
	rmdir "$tmp"
}

docker_targets() { case " $targets " in *" wasm "* | *" linux-"* | *" windows-"*) return 0 ;; esac; return 1; }

build_darwin() { # build_darwin <arm64|x86_64>
	a=$1
	[ "$(uname -s)" = Darwin ] || { echo "build.sh: darwin targets build on macOS" >&2; exit 1; }
	have="$(xcodebuild -version | tr '\n' ' ' | sed 's/ $//') sdk $(xcrun --sdk macosx --show-sdk-version) $(xcrun clang --version | head -1)"
	want="$XCODE_VERSION Build version $XCODE_BUILD sdk $MACOS_SDK $APPLE_CLANG"
	[ "$have" = "$want" ] || { echo "build.sh: toolchain is '$have', pins.sh wants '$want'" >&2; exit 1; }
	w=$(mktemp -d)
	unpack_libvpx "$cache/libvpx-$LIBVPX_VERSION.tar" "$w"
	src=$w/libvpx-$LIBVPX_VERSION
	sdk=$(xcrun --sdk macosx --show-sdk-path)
	if [ "$a" = x86_64 ]; then
		fetch "$NASM_MACOS_URL" "$NASM_MACOS_SHA256" "nasm-$NASM_VERSION-macosx.zip"
		unzip -q "$cache/nasm-$NASM_VERSION-macosx.zip" -d "$w/nasm"
		mkdir "$w/bin"
		printf '#!/bin/sh\nexec %s "$@" --pragma "macho64 build_version macos, %s sdk_version %s"\n' \
			"$w/nasm/nasm-$NASM_VERSION/nasm" "$(echo "$MACOS_MIN" | sed 's/\./, /')" "$(echo "$MACOS_SDK" | sed 's/\./, /')" >"$w/bin/nasm"
		chmod +x "$w/bin/nasm"
		PATH=$w/bin:$PATH
		extra=--as=nasm
	else
		extra=
	fi
	mkdir "$w/obj"
	(cd "$w/obj" && "$src/configure" --target=$a-darwin21-gcc --enable-multithread $LIBVPX_FLAGS $extra \
		--extra-cflags=-mmacosx-version-min=$MACOS_MIN >configure.log 2>&1 &&
		make -j"$(sysctl -n hw.ncpu)" >make.log 2>&1)
	sed 's/^/_/' "$here/exports.txt" >"$w/exports.list"
	name=libkolovpx-darwin-$([ $a = x86_64 ] && echo amd64 || echo arm64).dylib
	xcrun clang -O3 -dynamiclib -arch $a -mmacosx-version-min=$MACOS_MIN -isysroot "$sdk" \
		-I"$src" -I"$w/obj" "$here/kv.c" "$w/obj/libvpx.a" \
		-Wl,-exported_symbols_list,"$w/exports.list" -Wl,-dead_strip -Wl,-x -install_name @rpath/libkolovpx.dylib \
		-o "$w/$name" 2>"$w/link.log" || { cat "$w/link.log" >&2; exit 1; }
	cat "$w/link.log"
	codesign -s - -f --identifier kolovpx "$w/$name"
	d=$(dest "$name"); mkdir -p "$d"
	cp "$w/$name" "$d/.$name.new"; mv -f "$d/.$name.new" "$d/$name"
	echo "$d/$name: $(hostsha "$d/$name")"
	rm -rf "$w"
}

fetch_libvpx
if docker_targets; then fetch "$NASM_URL" "$NASM_SHA256" "nasm-$NASM_VERSION.tar.xz"; fi
case " $targets " in *" wasm "*)
	fetch "$WASI_SDK_URL" "$WASI_SDK_SHA256" "$WASI_SDK.tar.gz"
	docker image inspect kolo-video-wasm-build >/dev/null 2>&1 || docker build -q -t kolo-video-wasm-build -f "$here/wasm.Dockerfile" "$here" >/dev/null
	;;
esac

for t in $targets; do
	case $t in
	wasm) docker_run linux/arm64 kolo-video-wasm-build inner-wasm.sh ;;
	linux-amd64) docker_run linux/amd64 "$IMAGE_MANYLINUX_AMD64" inner-linux.sh ;;
	linux-arm64) docker_run linux/arm64 "$IMAGE_MANYLINUX_ARM64" inner-linux.sh ;;
	windows-amd64) docker_run linux/arm64 "$IMAGE_MINGW" inner-windows.sh x86_64 ;;
	windows-arm64) docker_run linux/arm64 "$IMAGE_MINGW" inner-windows.sh aarch64 ;;
	darwin-arm64) build_darwin arm64 ;;
	darwin-amd64) build_darwin x86_64 ;;
	*) echo "build.sh: unknown target $t" >&2; exit 2 ;;
	esac
done

[ -n "$out" ] || "$here/sums.sh"
