#!/bin/sh
# Rebuilds the kvstub-<goos>-<goarch> fixtures used by the loader tests.
# Needs clang and codesign on macOS, and Docker with the gcc and llvm-mingw images.
set -e
cd "$(dirname "$0")"
for a in arm64:arm64 amd64:x86_64; do
	clang -arch "${a#*:}" -mmacosx-version-min=12.0 -O1 -dynamiclib -o "kvstub-darwin-${a%:*}.dylib" kvstub.c
	codesign -s - -f "kvstub-darwin-${a%:*}.dylib"
done
for a in arm64 amd64; do
	docker run --rm --platform "linux/$a" -v "$PWD:/w" -w /w gcc:14 \
		gcc -O1 -shared -fPIC -Wl,-z,noexecstack -o "kvstub-linux-$a.so" kvstub.c
done
for a in aarch64:arm64 x86_64:amd64; do
	docker run --rm -v "$PWD:/w" -w /w mstorsjo/llvm-mingw \
		"${a%:*}-w64-mingw32-clang" -O1 -shared -o "kvstub-windows-${a#*:}.dll" kvstub.c
done

if command -v sha256sum >/dev/null; then sum=sha256sum; else sum="shasum -a 256"; fi
ls *.so *.dll *.dylib | sort | xargs $sum >SHA256SUMS
