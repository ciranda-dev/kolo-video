#!/bin/sh
# Rebuilds the kvabi-<goos>-<goarch> fixtures used by the load fallback tests
# and the kvglibc-linux-<goarch> fixtures (they need glibc 2.34) used by the hostile Linux tests.
# Needs clang and codesign on macOS, and Docker with the gcc and llvm-mingw images.
set -e
cd "$(dirname "$0")"
for a in arm64:arm64 amd64:x86_64; do
	clang -arch "${a#*:}" -mmacosx-version-min=12.0 -O1 -dynamiclib -o "kvabi-darwin-${a%:*}.dylib" kvabi.c
	codesign -s - -f "kvabi-darwin-${a%:*}.dylib"
done
for a in arm64 amd64; do
	docker run --rm --platform "linux/$a" -v "$PWD:/w" -w /w gcc:14 \
		gcc -O1 -shared -fPIC -Wl,-z,noexecstack -o "kvabi-linux-$a.so" kvabi.c
	docker run --rm --platform "linux/$a" -v "$PWD:/w" -w /w gcc:14 \
		gcc -O1 -shared -fPIC -Wl,-z,noexecstack -o "kvglibc-linux-$a.so" kvglibc.c
done
for a in aarch64:arm64 x86_64:amd64; do
	docker run --rm -v "$PWD:/w" -w /w mstorsjo/llvm-mingw \
		"${a%:*}-w64-mingw32-clang" -O1 -shared -o "kvabi-windows-${a#*:}.dll" kvabi.c
done

if command -v sha256sum >/dev/null; then sum=sha256sum; else sum="shasum -a 256"; fi
ls *.so *.dll *.dylib | sort | xargs $sum >SHA256SUMS
