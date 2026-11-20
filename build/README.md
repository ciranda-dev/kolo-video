# build

Maintainer tooling. Consumers never run it: the libraries and the generated Go are committed.

## Layout

| Path | Role |
|---|---|
| `libvpx/pins.sh` | Every pinned input: libvpx commit, NASM, wasi-sdk, Docker image digests, stack and memory limits, macOS toolchain. |
| `libvpx/build.sh` | `build.sh [--out dir] wasm linux-amd64 linux-arm64 windows-amd64 windows-arm64 darwin-arm64 darwin-amd64 all`. |
| `libvpx/inner-*.sh`, `common.sh`, `wasm.Dockerfile` | What runs inside the containers (no network). |
| `libvpx/kv.c`, `exports.txt`, `setjmp.h` | The flat C shim, its exported names, the `setjmp.h` stub of the wasm build. |
| `libvpx/kolovpx.wasm` | The committed wasm. |
| `libvpx/SHA256SUMS` | SHA-256 of the wasm and the six libraries. |
| `libvpx/sums.sh` | Writes `SHA256SUMS` and `vpx/internal/native/sums.go` from the committed files. |
| `regen.sh` | wasm to Go: wasm2go, then shardgen. |
| `verify.sh` | Checks hashes, generated Go, notices, symbols and toolchain pins. `--rebuild` also rebuilds. |
| `tools/go.mod` | Pins wasm2go v0.4.22 with a `tool` directive. |
| `tools/shardgen` | Splits wasm2go's single package into packages (below). |

## Inputs

| Input | Pin |
|---|---|
| libvpx | tag v1.16.0, commit `1024874c5919305883187e2953de8fcb4c3d7fa6` (tag object `04def0a07f8bfa95785e30e6db95036cda17f9b2`). `build.sh` clones the tag, checks both hashes and archives the commit with `git archive`; the containers check the commit again in the archive header. The release tarball has sha256 `7a479a3c66b9f5d5542a4c6a1b7d3768a983b1e5c14c60a9396edc9b649e015c` (reference only). |
| NASM 3.01 | source `nasm-3.01.tar.xz` sha256 `b7324cbe86e767b65f26f467ed8b12ad80e124e3ccb89076855c98e43a9eddd4`, built inside the Linux and Windows containers; macOS binary zip sha256 `400b797db8fc7cb7046db0dda996fbae779f388869db39896f82cfac0534e94c`. |
| wasm toolchain | wasi-sdk 34 (clang 23.1.0), `wasi-sdk-34.0-arm64-linux.tar.gz` sha256 `f7e243dff54d60bcc576e94d6166b69f410f2500ae4a9ceef34315be10e77971`, in `debian:bookworm-slim@sha256:734a72b3d6142a71993b4bc580ca01d8ca687f77a2c422d96a961d3376b179bc` plus `make` from the Debian snapshot of 2026-11-04 (`wasm.Dockerfile`). |
| Linux | `quay.io/pypa/manylinux_2_28_x86_64@sha256:d086f64cddcace250ef0f0073583c51b8298159303290a730c9f59bd91edfd2c` and `quay.io/pypa/manylinux_2_28_aarch64@sha256:34a5e614a4eb26f73f8bf44cbbf2acf8011620453d03ea35a58537bb3cde44de` (glibc 2.28, gcc 14.2.1). |
| Windows | `mstorsjo/llvm-mingw@sha256:b2a697af8fc50c5cc56011143453d02168ccfcff635d12ca76a1c363f5f6b0c6` (clang 23.1.5, mingw-w64 and winpthreads linked statically). |
| macOS | Xcode 26.2 (build 17C52), SDK MacOSX26.2, Apple clang 17.0.0 (clang-1700.6.3.2), macOS 26.7.1 host, `-mmacosx-version-min=12.0`. `build.sh` refuses to build with any other toolchain and `verify.sh` compares the host and the Mach-O headers (minos 12.0, sdk 26.2) with these pins. |

## Flags

- libvpx, all targets: `--enable-vp8 --enable-vp9 --disable-vp9-highbitdepth --enable-realtime-only
  --enable-static --disable-shared --enable-pic --disable-examples --disable-tools --disable-docs
  --disable-install-docs --disable-unit-tests --disable-webm-io --disable-libyuv`.
- Native: `--enable-multithread`, run-time CPU detection on; targets `x86_64-linux-gcc`, `arm64-linux-gcc`,
  `x86_64-win64-gcc`, `arm64-win64-gcc`, `x86_64-darwin21-gcc`, `arm64-darwin21-gcc` (macOS also
  `--extra-cflags=-mmacosx-version-min=12.0`).
- wasm: `--target=generic-gnu --disable-multithread --disable-runtime-cpu-detect`, libvpx's default `-O3` (also the link of `kv.c`), `-mcpu=lime1`, the `setjmp.h` stub
  (setjmp returns 0, longjmp traps: wasm2go has no exception handling), `-mexec-model=reactor`,
  `-Wl,--strip-all`, `-Wl,-z,stack-size=1048576`, `-Wl,--max-memory=1073741824`.
- Shared libraries: Linux `-shared -static-libgcc -Wl,--version-script (kv_* only) -Wl,-z,relro,-z,now -Wl,-s`; Windows
  `-shared -static -lpthread` with a `.def` of the kv_* names, `--no-insert-timestamp`, `-s`; macOS
  `-dynamiclib -exported_symbols_list -dead_strip -install_name @rpath/libkolovpx.dylib`, then `codesign -s -` (ad hoc,
  identifier `kolovpx`).
- NASM objects on macOS carry no platform load command and make the linker warn. The build puts a `nasm` wrapper first
  on `PATH` that adds `--pragma "macho64 build_version macos, 12, 0 sdk_version 26, 2"`, so the link prints no warning.

## Shim ABI

`kv_abi_version()` returns 1; the loader refuses any other value. The exports of every library are exactly: `kv_malloc kv_free
kv_enc_new kv_enc_free kv_encode kv_next_frame kv_set_bitrate kv_dec_new kv_dec_free kv_decode kv_version kv_abi_version`
(`exports.txt`). The wasm exports the same names plus `memory` and `_initialize`.

## Regeneration

```sh
build/regen.sh                 # wasm to Go, writes vpx/internal/portable
build/libvpx/sums.sh           # SHA256SUMS and sums.go
build/verify.sh                # everything above, read only
build/verify.sh --rebuild      # also rebuilds in Docker and on this Mac
```

`regen.sh` runs `go tool wasm2go` in `build/tools` with `GOWORK=off` and `GOFLAGS=`; the tool comes from the module cache or the
network, verified by `build/tools/go.sum` (warm cache: works with `GOPROXY=off`). Each output is written next to its
destination and renamed into place, an unchanged file is not touched, and a generated file that is no longer produced is
removed. Every generated file starts with `// Code generated ... DO NOT EDIT.`.

`build.sh` needs Docker (the wasm and the Linux and Windows libraries), a Mac with the pinned Xcode (the macOS libraries), and
the network once to fill `${KOLO_VIDEO_BUILD_CACHE:-$TMPDIR/kolo-video-build-cache}`; the containers run with `--network none`.
The wasm is the only artifact that must rebuild byte for byte in a fresh container. The native libraries are not claimed to be
reproducible across hosts (the macOS ones are built with the pinned toolchain on one Mac).

## Generated Go layout

`kolovpx.wasm` is one module. wasm2go writes it as one 7.2 MB package, and Go compiles a package as a unit: the cold
`go build` of that package peaked at 2.3 GB on this Mac and was killed by the kernel in a container with
`--memory=1g` (linux/arm64 native and linux/amd64 emulated). The wasm call graph has no cycles, so `tools/shardgen`
orders the functions callees first and cuts them into six shards of equal source size; a shard only calls shards below it.

| Path | Content |
|---|---|
| `vpx/internal/portable/libvpx.go` | package `portable`: `Module` (embeds `core.State`), `New`, the exported `X...` methods, the data segment |
| `vpx/internal/portable/wasm/core` | `State` (memory, table, globals, imports), `Slice`, `Grow` |
| `vpx/internal/portable/wasm/s0` to `s5` | `code.go` with the functions `F<n>(m *core.State, ...)`, `helpers.go` with the wasm2go helpers |
| `vpx/internal/portable/wasm/staticcheck.conf` | turns SA4000 and SA4006 off for the translated code only |

Shard `n` imports shard `n-1` even when it calls nothing in it, so `go build` compiles them one after the other; without
that the compiles of independent shards would run at the same time and their peaks would add up. Output is identical
to the unsplit package (same packets for VP8 and VP9 at three sizes). Encode speed is the same within noise
(640x480 VP9 38.9 ms against 39.2 ms per frame on noisy content, VP8 22.3 against 23.7 ms).

Cold `go build ./vpx/internal/portable`, empty build cache, go1.27.3 on the Mac and go1.27.4 in `golang:1.27-bookworm`:

| Where | One package (reference) | Six shards |
|---|---|---|
| macOS arm64, `/usr/bin/time -l`, maximum resident set | 2340372480 B | 572194816 B |
| linux/arm64 Docker, `--memory=1g --memory-swap=1g`, 8 CPUs, cgroup `memory.peak` | killed (oom_kill 1) | 733601792 B, exit 0 |
| linux/amd64 emulated, same limits | killed (oom_kill 1) | 788721664 B, exit 0 |

The cgroup figure includes the page cache of the source and the Go toolchain.

## wasm interface

Imports, all from `wasi_snapshot_preview1`: `clock_time_get`, `fd_close`, `fd_fdstat_get`, `fd_fdstat_set_flags`,
`fd_prestat_dir_name`, `fd_prestat_get`, `fd_read`, `fd_seek`, `fd_write`, `path_open`, `proc_exit` (the generated
`Xwasi_snapshot_preview1` interface). Exports: the twelve `kv_*` names, `memory`, `_initialize` (a reactor: call
`X_initialize` once after `New`).

## Limits of the wasm

| Setting | Value | Derivation |
|---|---|---|
| Stack | 1048576 B, placed first in memory (an overflow is an out-of-bounds trap, never a write into data) | The stack was painted and the deepest use measured over VP8 and VP9 encoders (speeds -16 to 16 and 0 to 9, sizes 64x64 to 1920x1080 and 4096x2176, 8 frames, 4 at 1920x1080 and above, and a bitrate change) and decoders (the same streams plus 150 corrupted packets per codec up to 320x240). Highest: VP9 encoder 58416 B, VP8 encoder 3216 B, VP9 decoder 12032 B, VP8 decoder 2224 B. The stack is 17.9 times the highest value (policy: at least twice). |
| `--max-memory` | 1073741824 B (16384 pages) | Highest memory of a module in the same matrix: VP9 encoder 118 MiB at 1920x1080 and 452 MiB at 4096x2176 (8912896 pixels, the `MaxPixels` default), VP8 encoder 62 and 256 MiB, decoders 15 and 59 MiB. 1 GiB is 2.3 times the 8912896-pixel peak. |
| Initial memory | 0x120000 B | Set by the linker: stack, data and the first heap page. |

The matrix used noisy generated frames, one encoder thread (`--disable-multithread`) and speeds 7 for VP9 and -6 for VP8 at the largest sizes.

## Notices

The files named here are in `third_party/licenses/`; the copies the library embeds are in `vpx/notices/`.

`LICENSE-libvpx` holds the libvpx `LICENSE` and the `third_party/x86inc/LICENSE` (the x86 assembly of the native libraries
includes `x86inc.asm`) verbatim from the pinned commit; `PATENTS-libvpx` holds `PATENTS`. The upstream `AUTHORS` file is not
copied: the BSD conditions name "The WebM Project authors", and the file lists personal e-mail addresses. `LICENSE-wasi-libc`
is the text 0008 reproduces for the same wasi-sdk 34 commit (`2e6fb9d8ee0cdf9e431fbcabe8af3115de000a13`),
`LICENSE-wasm2go` the wasm2go v0.4.22 licence, and `LICENSE-llvm-mingw` the mingw-w64 (ZPL), mingw-w64 runtime,
winpthreads and LLVM texts of the pinned llvm-mingw image (the DLLs link the mingw-w64 runtime and winpthreads statically).
