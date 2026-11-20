#!/bin/sh
# Checks the committed artifacts: hashes, generated Go, notices, symbols, toolchain pins.
# Usage: verify.sh [--rebuild]
#   --rebuild also rebuilds the wasm in Docker (a difference fails) and the six libraries
#   (a difference is reported only: the native libraries are not claimed to be reproducible across hosts).
set -eu

here=$(cd "$(dirname "$0")" && pwd)
root=$(dirname "$here")
v=$here/libvpx
. "$v/pins.sh"
n=$root/vpx/internal/native
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
fail() { echo "verify.sh: $*" >&2; exit 1; }

(cd "$v" && if command -v sha256sum >/dev/null; then sha256sum -c SHA256SUMS; else shasum -a 256 -c SHA256SUMS; fi)

# Test fixtures hold native code that `go test` loads: they are checked like the libraries.
for d in vpx/testdata vpx/internal/native/testdata; do
	(cd "$root/$d" && if command -v sha256sum >/dev/null; then sha256sum -c SHA256SUMS; else shasum -a 256 -c SHA256SUMS; fi) || fail "$d: a fixture differs from SHA256SUMS"
	[ "$(cd "$root/$d" && ls *.so *.dll *.dylib | sort)" = "$(awk '{print $2}' "$root/$d/SHA256SUMS" | sort)" ] || fail "$d: SHA256SUMS does not list exactly the fixtures"
done

"$v/sums.sh" "$tmp/sums" >/dev/null
for f in vpx/internal/native/sums.go build/libvpx/SHA256SUMS; do
	cmp "$tmp/sums/$f" "$root/$f" || fail "$f differs from sums.sh output"
	echo "$f: matches sums.sh"
done

"$here/regen.sh" "$tmp/regen" >/dev/null
(cd "$tmp/regen" && find . -type f | sort) >"$tmp/want"
(cd "$root" && find vpx/internal/portable/libvpx.go vpx/internal/portable/wasm -type f -name "*.go" | sed 's|^|./|' | sort) >"$tmp/have"
cmp "$tmp/want" "$tmp/have" || fail "the generated files of the repository are not the files regen.sh writes"
while read -r f; do
	cmp "$tmp/regen/$f" "$root/$f" || fail "$f differs from regen.sh output"
	head -n 1 "$root/$f" | grep -Eq '^// Code generated .* DO NOT EDIT\.$' || fail "$f has no generated-code header"
done <"$tmp/want"
echo "$(wc -l <"$tmp/want" | tr -d ' ') generated files match regen.sh"

wasm_exports=$(sed -n 's/^func (m \*Module) X\([A-Za-z_0-9]*\)(.*/\1/p' "$root/vpx/internal/portable/libvpx.go" | sort)
[ "$wasm_exports" = "$(printf '%s\n_initialize\nmemory\n' "$(cat "$v/exports.txt")" | sort)" ] || fail "the wasm exports differ from exports.txt plus memory and _initialize"
echo "wasm exports match exports.txt"

for f in LICENSE-libvpx PATENTS-libvpx LICENSE-wasi-libc LICENSE-wasm2go LICENSE-llvm-mingw; do
	[ -s "$root/third_party/licenses/$f" ] || fail "$f is missing"
done

exports=$(sort "$v/exports.txt")
want_glibc=2.28

check_linux() { # check_linux <image> <platform> <lib>
	out=$(docker run --rm --platform "$2" --network none -v "$n:/n:ro" "$1" sh -c "objdump -T /n/$3 | grep -v UND | awk '/\\.text/ {print \$NF}' | sort; echo ---; objdump -T /n/$3 | grep -o 'GLIBC_[0-9.]*' | sort -uV | tail -1; echo ---; objdump -p /n/$3 | awk '/NEEDED/ {print \$2}' | sort")
	syms=$(echo "$out" | sed -n '1,/^---$/p' | grep -v '^---$')
	[ "$syms" = "$exports" ] || fail "$3 exports differ from exports.txt"
	g=$(echo "$out" | sed -n '/^---$/,$p' | sed -n 2p)
	[ "$(printf '%s\n%s\n' "${g#GLIBC_}" $want_glibc | sort -V | tail -1)" = "$want_glibc" ] || fail "$3 needs $g, want at most GLIBC_$want_glibc"
	echo "$3: exports match, max $g, needs $(echo "$out" | sed -n '/^---$/,$p' | sed -n '3,$p' | grep -v '^---$' | tr '\n' ' ')"
}
check_windows() { # check_windows <lib> <prefix>
	out=$(docker run --rm --platform linux/arm64 --network none -v "$n:/n:ro" "$IMAGE_MINGW" sh -c "$2-w64-mingw32-objdump -p /n/$1")
	syms=$(echo "$out" | awk '$1 ~ /^[0-9]+$/ && $2 ~ /^0x/ {print $3}' | sort)
	[ "$syms" = "$exports" ] || fail "$1 exports differ from exports.txt"
	dlls=$(echo "$out" | awk '/DLL Name:/ {print $3}' | sort -f)
	echo "$dlls" | grep -v -i -E '^(KERNEL32\.dll|api-ms-win-crt-.*\.dll)$' | grep -q . && fail "$1 imports $dlls"
	echo "$1: exports match, imports $(echo "$dlls" | tr '\n' ' ')"
}
check_darwin() { # check_darwin <lib> <arch>
	f=$n/$1
	syms=$(nm -gU "$f" | awk '{print $3}' | sed 's/^_//' | sort)
	[ "$syms" = "$exports" ] || fail "$1 exports differ from exports.txt"
	lc=$(otool -l "$f")
	echo "$lc" | grep -q "minos $MACOS_MIN\$" || fail "$1 does not have minos $MACOS_MIN"
	echo "$lc" | grep -q "sdk $MACOS_SDK\$" || fail "$1 was not built with SDK $MACOS_SDK"
	[ "$(echo "$lc" | grep -c 'cmd LC_BUILD_VERSION')" = 1 ] || fail "$1 has other than one LC_BUILD_VERSION"
	[ "$(lipo -archs "$f")" = "$2" ] || fail "$1 is not $2 only"
	deps=$(otool -L "$f" | awk 'NR>1 {print $1}' | grep -v '^/usr/lib/libSystem.B.dylib$' | grep -v '^@rpath/libkolovpx.dylib$' || true)
	[ -z "$deps" ] || fail "$1 links $deps"
	codesign -v "$f" || fail "$1 has an invalid signature"
	echo "$1: exports match, minos $MACOS_MIN, sdk $MACOS_SDK, only libSystem, signature valid"
}

if command -v docker >/dev/null && docker info >/dev/null 2>&1; then
	check_linux "$IMAGE_MANYLINUX_AMD64" linux/amd64 libkolovpx-linux-amd64.so
	check_linux "$IMAGE_MANYLINUX_ARM64" linux/arm64 libkolovpx-linux-arm64.so
	check_windows libkolovpx-windows-amd64.dll x86_64
	check_windows libkolovpx-windows-arm64.dll aarch64
else
	echo "verify.sh: docker not available, Linux and Windows symbol checks skipped"
fi
if [ "$(uname -s)" = Darwin ]; then
	check_darwin libkolovpx-darwin-arm64.dylib arm64
	check_darwin libkolovpx-darwin-amd64.dylib x86_64
	have="$(xcodebuild -version | tr '\n' ' ' | sed 's/ $//') sdk $(xcrun --sdk macosx --show-sdk-version) $(xcrun clang --version | head -1)"
	want="$XCODE_VERSION Build version $XCODE_BUILD sdk $MACOS_SDK $APPLE_CLANG"
	if [ "$have" = "$want" ]; then echo "host toolchain matches pins.sh: $want"; else echo "host toolchain differs from pins.sh: have '$have', want '$want'"; [ "${1:-}" = --rebuild ] && fail "cannot rebuild the macOS libraries"; fi
else
	echo "verify.sh: not macOS, otool checks of the dylibs skipped"
fi

if [ "${1:-}" = --rebuild ]; then
	"$v/build.sh" --out "$tmp/build" wasm
	cmp "$tmp/build/kolovpx.wasm" "$v/kolovpx.wasm" || fail "kolovpx.wasm rebuild differs"
	echo "kolovpx.wasm: rebuild matches"
	"$v/build.sh" --out "$tmp/build" linux-amd64 linux-arm64 windows-amd64 windows-arm64 darwin-arm64 darwin-amd64
	for f in "$tmp"/build/libkolovpx-*; do
		if cmp -s "$f" "$n/$(basename "$f")"; then echo "$(basename "$f"): rebuild identical"; else echo "$(basename "$f"): rebuild differs (reported, not blocking)"; fi
	done
fi
echo "verify.sh: ok"
