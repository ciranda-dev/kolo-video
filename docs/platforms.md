# Platforms

What runs where, and how each platform was validated. "Run" means a test binary executed on that platform; "cross-compiled" means it builds and passes `go vet` and was not executed. The CI workflow in `.github/workflows` has not run on GitHub yet.

kolo-video has no cgo in any package, so the same `go get` and `go build` work for every target, from any host.

`CGO_ENABLED=0` never needs a compiler. Go turns cgo off by itself when no C compiler is on `PATH`, so a clean machine needs nothing. With cgo on, purego imports `runtime/cgo` on macOS and in Linux builds with `-tags kolovideo_native`, so those builds need a working C compiler (and the tagged Linux binary then links libc dynamically): a machine with a broken `cc` fails the build in `runtime/cgo` unless `CGO_ENABLED=0` is set. Measured on a Mac with a failing `cc` first on `PATH` (`CGO_ENABLED=0` builds; unset or `1` fails) and, for the tagged Linux build, by `go list -deps` of the independent review (`runtime/cgo` is imported); the CI job `linux-no-compiler` checks the Linux case and has not run yet. Windows and untagged Linux builds never import it.

## Matrix

| Platform | Camera backend | Codec engine | Validated by running | Only cross-compiled | Not validated |
|---|---|---|---|---|---|
| macOS arm64 | AVFoundation through purego | native, portable fallback | `go test`; `-race -count=100` on the capture tests; the Mac's 3 real cameras listed, identical to the cgo backend this one replaced; **one real capture** from the built-in camera (1280x720 NV12, 45 frames, 0 dropped, 0 invalid, timestamps strictly increasing); both codec engines; an ad-hoc signed hardened-runtime binary with and without the entitlement, and in a fake `.app` bundle; 1000 listings with stable memory | | A Developer ID signature, notarization, the App Sandbox; a real unplug, revoked access, a camera that offers only BGRA, a frame rate other than 30, a capture longer than 45 frames; `RequestAccess` (never called) |
| macOS amd64 | same | same | The test binaries, plain and hardened, under Rosetta: listing, unit tests, both engines | | A native Intel Mac; any camera |
| Windows arm64 | Media Foundation through COM | native, portable fallback | Native, in a Windows 11 ARM64 VM: the whole root and `vpx` suites on both engines; camera enumeration (0 cameras); a file-backed reader that runs the camera code path (activation excluded) and 300 open and close cycles; the loader tests with the cache ACL | | A camera: activating one, its native types, MJPG or YUY2 conversion, a blocking live `ReadSample`, unplug, the privacy setting; Smart App Control and WDAC; Windows N editions |
| Windows amd64 | same | same | The same tests under the VM's x64 emulation: they show the binary works, not native behavior or speed | | A native amd64 machine; a camera |
| windows/386 | none: `ErrUnsupported` | portable | The `ErrUnsupported` message test, under emulation | `go vet` | Anything else: the rest of the root suite stalls under emulation for a cause that was not investigated, and `vpx` was not run |
| Linux arm64 | V4L2 in pure Go | portable; native with the tag | Docker, native arm64: the static default binary on Debian, Alpine and distroless static (portable engine); the tagged binary on Debian, Ubuntu, distroless base and glibc 2.31 (native engine); loader cases (in-memory file, cache, seccomp without `memfd_create`, `noexec`, no `/proc`); the V4L2 streaming code (format and rate negotiation, buffer mapping and queueing, frame delivery, bad buffers, unplug, close) against a model of the kernel interface that enforces the ioctl rules; and, in a Debian 13 arm64 VM (kernel 6.12.116), against the kernel's virtual vivid capture driver: the 24 modes it lists, a 640x480 I420 capture of 40 frames with 0 dropped and 0 invalid, increasing timestamps, `Close` (`build/validate/linux-v4l2.sh`) | | A real camera behind `/dev/video*`; the YUYV, UYVY, NV12 and MJPEG conversions on a real kernel (vivid ran I420 only; they are unit tested); an unplug on a real kernel; amd64 on a real kernel; `vm.memfd_noexec`; a glibc 2.28 distribution (the library was checked for symbols only) |
| Linux amd64 | same | same | The same under emulation on an Apple silicon host, with `-test.short`; the `noexec` cases are skipped because the emulator does not enforce `noexec` mappings | | Native amd64 speed; a real camera |
| linux/386, riscv64, arm, ppc64le, s390x, mips64le | V4L2 in pure Go | portable | | `go vet`; `go build`; the 386 and riscv64 builds of the portable engine | Running anything on them |
| Other GOOS and GOARCH | none: `ErrUnsupported` | portable | | `go vet` passes for every target of `go tool dist list` except android and ios, which need cgo to link | Running anything on them |

Where "both engines" ran, the same round trip printed identical results on native and portable: VP8 320x240 882 kbit/s with a minimum PSNR of 50.12 dB, VP8 161x99 180 kbit/s 50.98 dB, VP9 320x240 288 kbit/s 51.50 dB, VP9 161x99 89 kbit/s 48.26 dB. One-thread streams from the two engines are byte-identical on macOS, Linux and Windows arm64.

## H.264

H.264 comes from the operating system, never from this module: VideoToolbox on macOS and the Media Foundation H.264 transforms on Windows desktop editions. Nothing is installed or downloaded. Where there is no usable codec, `h264.Capabilities()` and the constructors return an `*UnavailableError` (matching `h264.ErrUnavailable`) that names the platform and the reason, and VP8 and VP9 keep working. How to use it is in the [guide](guide.md#h264-through-the-operating-system).

| Platform | Available | Hardware | Validated by running | Not validated |
|---|---|---|---|---|
| macOS arm64 | yes, VideoToolbox | encode and decode on Apple silicon (measured on one Mac) | Round trips at 640x480, 1280x720 and 1920x1080 in Constrained Baseline, Main and High; the software path with hardware disabled; corrupt input; bitrate change; 10,000 encode and decode calls without memory growth; the ad-hoc signed hardened-runtime binary; streams made on Windows | Other Mac models; a Developer ID signed and notarized app |
| macOS amd64 | yes, VideoToolbox | as the system reports | The same test binary under Rosetta on the Apple silicon Mac, plain and hardened | An Intel Mac |
| Windows arm64 | yes, when the Media Foundation H.264 transforms are installed (desktop editions) | no: the transforms are software | A Windows 11 ARM64 VM: round trips in Constrained Baseline, Main and High, one picture for every packet from the first, padded 1920x1080 and cropped layouts, corrupt input, 300 open and close cycles, streams made on macOS | Windows N editions without the Media Feature Pack, Windows Server, hardware transforms |
| Windows amd64 | same | no | The VM's x64 emulation, `-test.short`: functional only | Native amd64; 1080p |
| Linux amd64, arm64 | no: `ErrUnavailable`; the reason lists V4L2 stateful H.264 devices that were found | not applicable | Docker (arm64 native, amd64 emulated, Debian and Alpine, no network): the absence message, a static binary, the parsers | Any device: no V4L2 stateful codec exists on any test machine |
| windows/386 and other targets | no: `ErrUnavailable` | not applicable | `go vet` and builds | Running |

### What ran

- macOS arm64, an Apple M5 on macOS 26.7.1, `CGO_ENABLED=0`: every round trip decoded all 90 frames with luma PSNR of at least 42.69 dB, keyframes at frame 0 and at the forced frame 45, every keyframe packet starting with the SPS and PPS and holding only NAL types 1, 5, 7 and 8, and `Hardware()` true on encoder and decoder. With hardware disabled the Constrained Baseline request was served as `avc3.42e01f` and Main as `avc3.4d401f`. 3,000 damaged slice units sent straight to VideoToolbox gave 2,976 `-12909` (`ErrCorrupt`), a few other errors and no crash, and the same decoder then decoded the clean stream. The tests ran on arm64 and, under Rosetta, amd64, each plain and ad-hoc signed with the hardened runtime and no entitlement; VideoToolbox is a system framework, so library validation does not apply to it.
- Windows 11 ARM64 VM, arm64 native: every round trip decoded all 90 frames with luma PSNR of at least 42.86 dB; every packet was one access unit and gave one picture from the first input; the 1920x1080 layout (a 1920x1088 frame with the chroma planes at row 1088), 162x98 and 48x48 decoded correctly; a level 6 stream was refused in Go before the transform; 3,000 mutated packets gave no panic and the decoder recovered; 300 encoder and decoder open and close cycles did not grow. The VM's x64 emulation passed the same tests in short mode.
- Each engine decoded the other's streams: the three Windows streams (320x240, Constrained Baseline, Main and High) decoded 30 of 30 on the Mac with luma PSNR of at least 41.18 dB, and the three macOS streams decoded 30 of 30 on Windows at 41.94 dB or more. Those streams are committed under `h264/testdata`.
- Linux, in `alpine:3` and `debian:bookworm-slim` with no network: a static `CGO_ENABLED=0` program prints `kolovideo/h264: no H.264 engine on linux/arm64 in this release; V4L2 stateful H.264 devices: none` (`linux/amd64` for the emulated run), and the package's tests pass.

### macOS

- VideoToolbox is called through purego from the system frameworks, with no Objective-C and no cgo. macOS picks hardware by default and `Hardware()` reports it; the software path runs when the hardware is absent or busy, and measured slower at 1080p and slow to follow `SetBitrate`.
- Encoding and decoding run in system XPC services (`VTEncoderXPCService` and `VTDecoderXPCService`), for the hardware path and the software path alike, not in your process: with 150 frames of 1080p the application used 1.7 s of CPU in both cases while the services used 0.03 s and 0.04 s (hardware) or 4.0 s and 0.3 s (software). A crash inside a service was not provoked, so what your process sees then was not measured. The services keep running after `Close`.
- Constrained Baseline is requested directly. When the software encoder refuses it, the engine asks for the Baseline profile, which that encoder gave as Constrained Baseline (`avc3.42e01f`); a hardware refusal is not retried, because the hardware encoder gives plain Baseline for that request, and any stream with another profile than the one asked for is `ErrProfile`.
- The first call creates a system session (40 to 110 ms measured). A decoder whose system session cannot accept the parameter sets of a new keyframe creates another one.

### Windows

- The Microsoft H.264 encoder and decoder transforms (`CLSID_CMSH264EncoderMFT` and `CLSID_CMSH264DecoderMFT`) are used through COM and `golang.org/x/sys/windows`, with no cgo. They run in your process: a crash inside the decoder ends the application, and kolo-video provides no helper process mode. A hang inside the system codec blocks `Decode` and `Close`.
- The Windows decoder never reported corrupt data in any test. A damaged slice that passes the checks in Go is concealed and returned as a picture, and `Decode` reports no error for it. Every error `Decode` returns on Windows comes from the checks in Go.
- Only the software transforms are used and `Hardware()` is false; hardware transforms are asynchronous, need an event model and could not be tested on any machine here, so they are not used.
- Microsoft lists no supported server edition for either transform and N editions lack Media Foundation components without the Media Feature Pack (Microsoft documentation, not verified here). Where the transforms are not registered, `ErrUnavailable` carries the HRESULT.
- On the VM 1920x1080 at 30 fps is at the edge of real time (27 ms median and 82 ms at the 99th percentile for one encode, one run).

### Linux and other systems

- Linux has no H.264 engine. The package scans `/dev/video*` read-only for V4L2 stateful H.264 devices and lists what it finds in the error text; it never opens a stream and never uses them. VA-API is not used because it needs a system library and would make every Linux binary that imports `h264` dynamic. Receivers on Linux take VP8, and a sender there publishes VP8 only.
- `go list -deps ./h264` shows no purego and no `runtime/cgo` on Linux, so the default Linux binary stays static.
- Every other target reports absence with the same error; `ParseKeyframe` and `SPS.Codec` work everywhere.

### Licensing

kolo-video ships no H.264 encoder or decoder; H.264 encoding and decoding use the codec of the operating system under its license terms, which include an H.264/AVC notice. This is not legal advice: an application that ships or uses H.264 can have licensing obligations of its own.

### Not validated

Linux encode and decode on any device (not provided), VA-API, V4L2 stateful devices on real hardware (detection ran only against a fake directory and fake ioctls), an Intel Mac (Rosetta only), native Windows amd64, Windows hardware transforms and vendor encoders, Windows N editions and Windows Server, a Developer ID signed and notarized app, a real camera feeding the H.264 encoder, browsers and phones (the interoperation proof is VideoToolbox against Media Foundation only), and a crash inside a VideoToolbox service.

## macOS

- Camera access needs the user's consent (TCC). An app bundle needs `NSCameraUsageDescription` in its `Info.plist`; a command-line tool inherits its terminal's permission. `OpenCapture` never asks and returns `ErrPermission`; `RequestAccess` asks.
- The camera backend calls AVFoundation, CoreMedia, CoreVideo and the Objective-C runtime through purego. They are platform binaries, so the hardened runtime and library validation never block capture. Every Objective-C call sequence on a Go thread runs inside an autorelease pool. An Objective-C exception would end the process, so each documented throwing precondition of AVFoundation is checked in Go before the call.
- The portable engine is plain Go and needs no executable-memory entitlement.
- The native engine is an embedded library extracted to a per-user cache file. The first `dlopen` of a new file takes 0.4 to 0.6 s (macOS checks it once); later loads take under a millisecond.

### Signing guide

A hardened-runtime app (every notarized app) enforces library validation, which refuses a library whose Team ID differs from the app's. Without a fix the loader reports it and the portable engine runs, which is not real time above 640x480. `vpx.LoadError()` then reads like `code signature ... not valid for use in process: mapping process and mapped file (non-platform) have different Team IDs (if the application uses the hardened runtime, add the entitlement com.apple.security.cs.disable-library-validation, or sign the library with the application's identity and ship it as Contents/Frameworks/libkolovpx.dylib)`.

Route 1, sign the library with the app's identity and put it in the bundle:

```go
path, err := vpx.WriteLibrary(dir) // at build time, in a small tool of yours
```

```sh
cp "$path" MyApp.app/Contents/Frameworks/libkolovpx.dylib
codesign --force --options runtime --timestamp -s "Developer ID Application: Your Name (TEAMID)" \
  MyApp.app/Contents/Frameworks/libkolovpx.dylib
codesign --force --options runtime --timestamp -s "Developer ID Application: Your Name (TEAMID)" MyApp.app
```

When the executable is inside `X.app/Contents/MacOS`, the loader tries `Contents/Frameworks/libkolovpx.dylib` first. It checks the file's code signature with `SecStaticCodeCheckValidity` before `dlopen`, so an altered or unsigned file is refused with a catchable error and the cache is tried next. Outside a bundle, name the file in `LoadConfig.Library`: it is tried alone, with the same signature check, and no fallback.

Route 2, add the entitlement `com.apple.security.cs.disable-library-validation` to the app's entitlements file. Notarization accepts it. It weakens the app, because any library signed by anyone can then be loaded, so route 1 is the better choice.

What was run: a hardened test binary signed ad hoc, with no entitlement (portable engine, typed error naming the entitlement and `Contents/Frameworks`), with the entitlement (native, from the cache, or from the bundle in a fake `X.app`), and with an unsigned bundle library (refused before `dlopen`, cache used). An ad-hoc signature carries no Team ID, so route 1 with an ad-hoc signed library was still refused. That route 1 loads with the app's own Team ID follows Apple's documentation and was not run, because no Developer ID certificate was available. Notarization and the Mac App Store sandbox, which also forbids code outside the bundle, were not tried.

`LoadConfig.Library` skips the hash check: the application signs and ships that file. A copy that was altered but still carries a valid signature is killed by the operating system when its code page is touched (measured: SIGKILL), which cannot be caught. `SecStaticCodeCheckValidity` hashes every page before the load and refuses such a file, which is why the check exists on the bundle and `Library` paths.

## Windows

- Media Foundation (`mfplat.dll`, `mf.dll`, `mfreadwrite.dll`) ships with Windows 10 and 11 desktop editions. N editions lack parts of it unless the Media Feature Pack is installed (from Microsoft's documentation; not verified here); capture then returns `ErrUnsupported` naming the missing DLL.
- Camera access follows the Windows camera privacy setting; a refusal returns `ErrPermission`.
- The native engine is a DLL extracted to `%LocalAppData%\kolo-video\vpx\...` and loaded with `LoadLibraryEx` and restricted search flags. It imports only KERNEL32 and the Universal C Runtime, so no Visual C++ redistributable is needed. It is not Authenticode-signed.
- **Smart App Control and application control.** Windows 11 Smart App Control, WDAC and AppLocker DLL rules can refuse an unsigned DLL from a user-writable folder. The load then fails with an ordinary error, the portable engine runs, and `LoadError()` says a code-integrity policy can block unsigned DLLs (shown for errors 5, 577, 786 and 1260; the mapping is by error number). To get the native engine there, write the DLL with `vpx.WriteLibrary`, sign it with your certificate and name it in `LoadConfig.Library`. No machine with such a policy was available: this was not tried, and the VM's own policy state is unknown.
- A closed camera is released from another COM thread by cancelling the pending read with `Flush`; shutting the source down while a read is pending blocked forever in the VM, so the backend never does that first. If a real device blocks the reader past `DefaultStallTimeout`, `Close` returns an error and releases the objects when the read finally returns; that fallback is unit-tested with a fake only.

## Linux

- A binary built without a tag is static: `file` says `statically linked`, and it starts on Debian, Alpine and distroless static images. The codec is the portable engine, real time up to 640x480.
- A binary built with `-tags kolovideo_native` on amd64 or arm64 is dynamically linked to glibc. It starts on Debian, Ubuntu, distroless base and glibc 2.31 (Debian 11), and does not start on Alpine or distroless static (the shell reports `not found` or `no such file or directory`): the failure happens before `main`, so no fallback can help. The older tag name `kolovideo_static` no longer exists.
- Inside a glibc process every failure measured gave a catchable error: seccomp without `memfd_create` (the cache is used), a `noexec` cache directory with no memfd, no `/proc`, a read-only root, a library that needs a newer glibc (the error names `GLIBC_2.34`). The portable engine then runs. `vm.memfd_noexec` was not tested.
- V4L2 is pure Go and needs no library. Its ioctl numbers are checked in a test, and the streaming path runs against a model of the kernel interface (`v4l2_fake_linux_test.go`): it checks request numbers, call order and buffer rules and covers an index or byte count out of range, a driver that changes the format, failures at each step of start-up, unplug and close. On a real kernel it ran only against vivid, which has no camera behind it, and never against a real camera. The user needs access to `/dev/video*`.
- Zero install was checked with a failing `cc`, `gcc` and `clang` first on `PATH` and a fresh build cache: `CGO_ENABLED=0 go build ./...` and the codec tests pass on macOS. A static Linux program that uses the codec ran on Alpine 3 (arm64 native, amd64 emulated) with the portable engine. The commands of the CI job `zero-install` (a clean `golang:1.27-alpine` container, no compiler, no network after `go mod download`) were run by the independent review of the repository: exit 0, eight targets built, `go test ./...` passed; the job itself has not run on GitHub.

## Run the validation yourself

The scripts are in [build/validate](../build/validate/README.md). They build every binary with `CGO_ENABLED=0` on one machine and run it on the target. Nothing opens a camera or asks for a permission, and the H.264 tests use generated frames only.

```sh
build/validate/build.sh -o /tmp/kolo-validate     # all binaries, SHA256SUMS
build/validate/macos-hardened.sh                  # macOS: sign, run with and without the entitlement, the h264 tests; prints PASS
build/validate/linux.sh arm64                     # Docker: static, native, hostile environments
build/validate/linux.sh amd64                     # emulated on Apple silicon: functional only
KOLO_VM=<helper> build/validate/linux-v4l2.sh     # a Linux VM with the vivid driver: V4L2 streaming on a real kernel
KOLO_WINRUN=<helper> build/validate/windows.sh    # maintainers: a Windows VM and a helper that runs a program in it
```

Check the committed libraries and the generated Go, which consumers never rebuild:

```sh
build/verify.sh             # hashes, regenerated Go, notices, symbols, toolchain pins; read only
build/verify.sh --rebuild   # also rebuilds the wasm in a container (must match) and the libraries (reported)
```

The wasm must rebuild byte for byte. The native libraries are rebuilt with the pinned toolchains and compared, but identical bytes across machines are not claimed. Tests that open a real camera run only with `KOLO_VIDEO_HW_TEST=1`, and only a person at the machine should set it.
