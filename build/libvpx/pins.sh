# Pinned inputs of the libvpx artifacts. Sourced by build.sh and the inner scripts.
LIBVPX_VERSION=1.16.0
LIBVPX_TAG=v1.16.0
LIBVPX_COMMIT=1024874c5919305883187e2953de8fcb4c3d7fa6
LIBVPX_TAG_OBJECT=04def0a07f8bfa95785e30e6db95036cda17f9b2
LIBVPX_URL=https://chromium.googlesource.com/webm/libvpx
# Reference only: the release tarball of the same commit. The build uses git archive of the commit.
LIBVPX_TARBALL_SHA256=7a479a3c66b9f5d5542a4c6a1b7d3768a983b1e5c14c60a9396edc9b649e015c

NASM_VERSION=3.01
NASM_SHA256=b7324cbe86e767b65f26f467ed8b12ad80e124e3ccb89076855c98e43a9eddd4
NASM_URL=https://www.nasm.us/pub/nasm/releasebuilds/$NASM_VERSION/nasm-$NASM_VERSION.tar.xz
NASM_MACOS_SHA256=400b797db8fc7cb7046db0dda996fbae779f388869db39896f82cfac0534e94c
NASM_MACOS_URL=https://www.nasm.us/pub/nasm/releasebuilds/$NASM_VERSION/macosx/nasm-$NASM_VERSION-macosx.zip

WASI_SDK=wasi-sdk-34.0-arm64-linux
WASI_SDK_SHA256=f7e243dff54d60bcc576e94d6166b69f410f2500ae4a9ceef34315be10e77971
WASI_SDK_URL=https://github.com/WebAssembly/wasi-sdk/releases/download/wasi-sdk-34/$WASI_SDK.tar.gz

IMAGE_DEBIAN=debian:bookworm-slim@sha256:734a72b3d6142a71993b4bc580ca01d8ca687f77a2c422d96a961d3376b179bc
IMAGE_MANYLINUX_AMD64=quay.io/pypa/manylinux_2_28_x86_64@sha256:d086f64cddcace250ef0f0073583c51b8298159303290a730c9f59bd91edfd2c
IMAGE_MANYLINUX_ARM64=quay.io/pypa/manylinux_2_28_aarch64@sha256:34a5e614a4eb26f73f8bf44cbbf2acf8011620453d03ea35a58537bb3cde44de
IMAGE_MINGW=mstorsjo/llvm-mingw@sha256:b2a697af8fc50c5cc56011143453d02168ccfcff635d12ca76a1c363f5f6b0c6

STACK=1048576
MAXMEM=1073741824
MACOS_MIN=12.0

# macOS toolchain of the committed libraries; verify.sh compares the host and the Mach-O headers to these.
XCODE_VERSION="Xcode 26.2"
XCODE_BUILD=17C52
MACOS_SDK=26.2
APPLE_CLANG="Apple clang version 17.0.0 (clang-1700.6.3.2)"
