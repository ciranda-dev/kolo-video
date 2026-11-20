# validate

Maintainer scripts that build and run the validation jobs on each platform. Every
binary is built on the Mac with `CGO_ENABLED=0`; nothing here opens a camera or calls
`RequestAccess`, and every run uses temp directories only. Run one job at a time.

| Script | Does |
|---|---|
| `build.sh [-o dir] [windows] [linux] [darwin]` | Builds the binaries below into `dir` (default a new temp directory), writes `SHA256SUMS`, prints the directory. Fails if a Linux binary without the tag is not statically linked. |
| `macos-hardened.sh [arch ...]` | Builds the macOS test binaries, signs them ad hoc with `codesign -s - -f -o runtime`, runs them, the `h264` tests included. Prints `PASS` or `FAIL`. Each run gets a temp `HOME` but the caller's `GOMODCACHE` and `GOCACHE`, so it works offline whenever the build does; the two tests that call `go list` print its error when the module cache cannot serve them. |
| `linux.sh [arch ...]` | Docker runner (arm64 and amd64; amd64 is emulated on an Apple silicon host). Prints `ok` per case and `PASS`. |
| `windows.sh [arch ...]` | Runs the Windows binaries on the Windows 11 ARM64 VM through the runner named by the required variable `KOLO_WINRUN` (no default; the contract is in the script header, including the directory form the `h264` tests need). Prints `ok` per case and `PASS`. |
| `linux-v4l2.sh` | Needs `KOLO_VM`, a helper for a Linux VM (contract in the header). Loads the kernel's vivid virtual capture driver and runs `TestV4L2Vivid`: the V4L2 streaming code against a real kernel, no camera. Prints `PASS` or `FAIL`. |
| `common.sh` | The four expected harness lines, shared. |

## Binaries

| Name | Built from | Tags |
|---|---|---|
| `vpx-<os>-<arch>.test[.exe]` | `go test -c ./vpx` | none |
| `vpx-linux-<arch>-native.test` | same | `kolovideo_native` |
| `kolovideo-<os>-<arch>.test[.exe]` | `go test -c .` | none |
| `native-<os>-<arch>[-native].test[.exe]` | `go test -c ./vpx/internal/native` (loader) | `kolovideo_native` on Linux only |
| `snapshot-<os>-<arch>[.exe]`, `vp8-linux-<arch>` | `go build ./examples/...` | none |
| `h264-<os>-<arch>.test[.exe]`, `h264-linux-386.test` | `go test -c ./h264` | none |
| `example-h264-<os>-<arch>[.exe]` | `go build ./examples/h264` | none |
| `kolovideo-windows-386.test.exe` | `go test -c .` | none |

`<os>` is `windows`, `linux` or `darwin`; `<arch>` is `arm64` and `amd64`. The untagged Linux binaries
(`vpx`, `kolovideo`, `h264`, `snapshot`, `vp8`, `example-h264`) and `h264-linux-386.test` must report `statically linked` from `file`. The `h264` tests read the package's `testdata` directory: run them from `h264/` (the scripts do).

## Harness lines

Every platform and engine must log these four lines from `TestRoundTrip` (`<e>` is `native` or `portable`):

```
engine=<e> VP8 320x240: 882 kbit/s, min PSNR 50.12 dB
engine=<e> VP8 161x99: 180 kbit/s, min PSNR 50.98 dB
engine=<e> VP9 320x240: 288 kbit/s, min PSNR 51.50 dB
engine=<e> VP9 161x99: 89 kbit/s, min PSNR 48.26 dB
```

## macOS

```sh
build/validate/macos-hardened.sh
```

For arm64 and, when Rosetta is installed, amd64:

| Case | Must print |
|---|---|
| vpx without the entitlement | `ok`; the `KVRESULT` line has `"Engine":"portable"` and a native error with `different Team IDs`, `disable-library-validation` and the `Contents/Frameworks` hint; the portable round trip logs the four harness lines |
| vpx with `com.apple.security.cs.disable-library-validation` | `ok`; `"Engine":"native"`, `"Source":"cache"`; `TestRoundTrip` logs the harness lines for both engines |
| root without the entitlement | `ok`; `TestRealListing`, `TestAVFListing`, `TestDelegateNV12`, `TestDelegateBGRA` and `TestRealSessionObjects` pass and no test fails. `TestAVFHardwareCapture` stays skipped (`KOLO_VIDEO_HW_TEST` is unset) |

For each architecture the `h264` test binary also runs from the `h264` directory, as built and ad-hoc signed with the hardened runtime and no entitlement (VideoToolbox is a system framework, so none is needed). Frames are generated; no camera is used.

| Case | Must print |
|---|---|
| `h264` as built and hardened | `ok`; `--- PASS:` for `TestVTRoundTrip`, `TestVTSoftwarePath`, `TestVTCorrupt`, `TestVTEngineCorrupt`, `TestVTInterop` and `TestVTCapabilitiesReal`, none of them skipped, no failure; the script prints the `Capabilities()` lines the last test logs (`Available:true`, engine `VideoToolbox`, the profiles) |

The last line is `PASS`.

## Linux

```sh
build/validate/linux.sh arm64        # native Docker
build/validate/linux.sh amd64        # emulated on Apple silicon: functional only
```

Images: `debian:bookworm-slim`, `debian:bullseye-slim` (glibc 2.31), `ubuntu:24.04`, `alpine:3`,
`gcr.io/distroless/static-debian12`, `gcr.io/distroless/base-debian12`. Containers have no network and
no devices.

| Case | Must print |
|---|---|
| `vpx-linux-<arch>.test` (no tag) on Debian, Alpine and distroless/static | starts, `engine=portable` harness lines, `PASS` |
| `kolovideo-linux-<arch>.test` on Debian and Alpine | `PASS` (V4L2 fakes; no `/dev/video*`) |
| `snapshot -list` on Alpine | exit 0, no output |
| `vpx-linux-<arch>-native.test` on Debian and Ubuntu | `engine=native` and `engine=portable` harness lines, `PASS` |
| the same binary on Alpine and distroless/static | does not start (`no such file or directory`): the documented limit |
| the same binary on distroless/base and on glibc 2.31 | `TestHostileLinux` passes with `engine=native` |
| glibc 2.31 with a library that needs glibc 2.34 | `engine=portable`, the native error names `GLIBC_2.34` |
| loader scenarios (`native-linux-<arch>-native.test`, `TestScenario`) | `memfd` by default and with a read-only root and `noexec` `/tmp`; `cache` with seccomp denying `memfd_create` and with `/proc` unmounted; `fail` (typed error) with seccomp and a `noexec` `/tmp`, and with no `/proc` and a `noexec` `/tmp` |
| vpx with seccomp denying `memfd_create` and a `noexec` `/tmp` | `engine=portable`, a typed native error naming `memfd_create` |

| `h264-linux-<arch>.test` on Debian and Alpine, run in the `h264` directory | `PASS`: Linux has no H.264 engine, so the tests check the absence message, the parsers and the fuzz seeds |
| `example-h264-linux-<arch>` on Debian and Alpine | `H.264 is not available, using VP8: kolovideo/h264: no H.264 engine on linux/<arch> in this release; V4L2 stateful H.264 devices: none`, then the VP8 lines and `worst PSNR` |
| `h264-linux-386.test` on `i386/alpine:3` | `PASS` (`-test.short`, emulated on a 64-bit host) |

The `noexec` cases run only when the container architecture is the host's: the emulator does not
enforce `noexec` mappings. An emulated amd64 run uses `-test.short`. The last line is `PASS`.

## Windows

```sh
build/validate/windows.sh            # arm64 (native) and amd64 (x64 emulation), then windows/386
```

| Case | Must print |
|---|---|
| `vpx-windows-<arch>.test.exe -test.v` | the harness lines for `native` and `portable`, `PASS` |
| `kolovideo-windows-<arch>.test.exe -test.v` | `--- PASS: TestMFEnumerate` (a `MFStartup ok, ... N video capture devices` line, N probably 0) and `--- PASS: TestMFReaderFile` (not skipped), `PASS` |
| `native-windows-<arch>.test.exe -test.v` | `PASS` (extraction, verification and load of the embedded DLL, protected cache directory) |
| `snapshot-windows-<arch>.exe -list` | exit 0, the camera list (probably empty) |
| `h264-windows-<arch>.test.exe -test.v` from a directory holding it and `testdata`, with `KOLOVIDEO_H264_REQUIRE=1` | `--- PASS:` for `TestMFTCapabilitiesReal`, `TestMFTProfiles`, `TestMFTRoundTrip`, `TestMFTOnePerInput` and `TestMFTInterop`; on arm64 also `TestMFTPaddedLayout`, `TestMFTLevel6Refused`, `TestMFTCorrupt`, `TestMFTOpenClose` and `TestMFTBitrateChange`; `PASS`. A missing H.264 transform fails instead of skipping |
| `example-h264-windows-<arch>.exe` | `H.264 through Media Foundation` and a `codec string avc3.42c0...` line |
| `kolovideo-windows-386.test.exe -test.v -test.run '^TestUnsupportedPlatformMessage$'` | `--- PASS: TestUnsupportedPlatformMessage`, `PASS`. Only this test: the rest of the root suite stalls or exits with status 9 under the VM's 32-bit x86 emulation (cause unknown) |

The amd64 run is x64 emulation on an ARM64 host and uses `-test.short`: functional only, not a native
amd64 validation. The VM has no camera; an empty list proves only that the API calls succeed.
Not covered: Smart App Control or WDAC enforcement (the VM policy is unknown).
