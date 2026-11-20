# kolo-video

Camera capture, raw frames, VP8/VP9 and optional H.264 for Kolo.

[![status: experimental](https://img.shields.io/badge/status-experimental-orange)](#status)
[![license: MIT OR Apache-2.0](https://img.shields.io/badge/license-MIT%20OR%20Apache--2.0-blue)](#license)
[![go: 1.26+](https://img.shields.io/badge/go-1.26%2B-00ADD8)](go.mod)

## What it is

kolo-video captures camera frames and encodes them as [VP8 (RFC 6386)](https://www.rfc-editor.org/info/rfc6386), VP9, or [H.264 (ITU-T H.264)](https://www.itu.int/rec/T-REC-H.264) Annex B access units, in I420 with the [BT.601](https://www.itu.int/rec/R-REC-BT.601) matrix. It lists and opens cameras, delivers I420 frames, converts other pixel formats to I420, draws frames into RGBA images, and decodes the same codecs for real-time use. H.264 is optional and comes from the operating system's own codec. It does not send anything over the network and does not sync audio with video.

It is Go only: no cgo, no C compiler, no libvpx, no pkg-config, none to set (`os.UserCacheDir` reads `HOME`, `XDG_CACHE_HOME` or `LocalAppData` to place the native library cache). The codec is libvpx 1.16.0 in two forms that carry the same code: a prebuilt library embedded in the module and loaded without cgo (full speed), and the same code translated to Go (works everywhere, real time up to 640x480). The package picks one at run time and tells you which. Cameras are opened through each operating system's own interface (AVFoundation, Media Foundation, V4L2). The `h264` package calls the H.264 codec of macOS (VideoToolbox) and of Windows desktop editions (Media Foundation); it contains no codec of its own, installs nothing, and reports with a typed error where the system has none.

## Install

```sh
go get github.com/ciranda-dev/kolo-video
```

The first build of the `vpx` package compiles generated Go and peaks at about 570 MB of memory on a Mac (734 MB in a container limited to 1 GiB); later builds use the build cache.

## Quick start

List the cameras (this never starts one), then encode 30 generated frames with VP8 and decode them again. Save it outside the repository and run it with `go run . <dir>` in a module that requires kolo-video. The directory is where the native engine keeps its library; an application normally leaves `CacheDir` empty, which uses the user cache directory.

```go
package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"time"

	kolovideo "github.com/ciranda-dev/kolo-video"
	"github.com/ciranda-dev/kolo-video/vpx"
)

func main() {
	if len(os.Args) != 2 {
		log.Fatal("usage: quickstart <cache directory>")
	}
	cams, err := kolovideo.Devices()
	if err != nil && !errors.Is(err, kolovideo.ErrNoDevice) && !errors.Is(err, kolovideo.ErrUnsupported) {
		log.Fatal(err)
	}
	for _, c := range cams {
		fmt.Printf("camera %q default=%v\n", c.Name, c.Default)
	}

	rt, err := vpx.Load(vpx.LoadConfig{CacheDir: os.Args[1]})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("libvpx %s, %v engine\n", rt.Version, rt.Engine)

	enc, err := vpx.NewEncoder(vpx.Config{Codec: vpx.VP8, Width: 320, Height: 240})
	if err != nil {
		log.Fatal(err)
	}
	defer enc.Close()
	dec, err := vpx.NewDecoder(vpx.DecoderConfig{Codec: vpx.VP8})
	if err != nil {
		log.Fatal(err)
	}
	defer dec.Close()

	var bytes, frames int
	for i := range 30 {
		f, err := kolovideo.NewFrame(320, 240)
		if err != nil {
			log.Fatal(err)
		}
		for j := range f.Y {
			f.Y[j] = byte(16 + (j/320+j%320+4*i)%220)
		}
		f.Timestamp = time.Duration(i) * time.Second / 30

		pkts, err := enc.Encode(f, false)
		if err != nil {
			log.Fatal(err)
		}
		for _, p := range pkts {
			out, err := dec.Decode(p)
			if err != nil {
				log.Fatal(err)
			}
			if out != nil {
				bytes += len(p.Data)
				frames++
			}
		}
	}
	fmt.Printf("%d frames encoded and decoded, %d bytes\n", frames, bytes)
}
```

The engine line depends on the platform (`native` on macOS and Windows, `portable` on Linux by default); the last does not:

```text
$ CGO_ENABLED=0 go run . "$(mktemp -d)"
...
30 frames encoded and decoded, 13596 bytes
```

To capture, open a camera and read frames. `OpenCapture` never asks for permission: on macOS call `RequestAccess` first.

```go
c, err := kolovideo.OpenCapture(kolovideo.CaptureConfig{Width: 1280, Height: 720})
if err != nil {
	return err
}
defer c.Close()
f, err := c.Read(ctx)
```

H.264 is the same shape. Ask the system first, and keep VP8 for where it says no:

```go
enc, err := h264.NewEncoder(h264.Config{Width: 1280, Height: 720})
if errors.Is(err, h264.ErrUnavailable) {
	return encodeVP8(frame) // Linux, other systems, or no codec on this one
}
pkts, err := enc.Encode(frame, false) // Annex B, SPS and PPS before every keyframe
```

[examples/snapshot](examples/snapshot) saves camera frames as PNG files (`-list` lists the cameras), [examples/vp8](examples/vp8) encodes and decodes generated frames and prints the engine, the size and the quality, and [examples/h264](examples/h264) does the same with H.264 and falls back to VP8 where the system has no codec. Details are in the [guide](docs/guide.md).

## Packages and API

| Symbol | What it does |
|---|---|
| `Devices`, `OpenCapture`, `RequestAccess` | List cameras, open one, ask for macOS camera access |
| `Capture.Read`, `Mode`, `Stats`, `Close` | Read I420 frames, drop-oldest buffering, device loss as `ErrDeviceLost` |
| `Frame`, `NewFrame`, `FromNV12`, `FromYUY2`, `FromUYVY`, `FromBGRA`, `FromRGBA` | I420 frames and conversions |
| `Frame.RGBA`, `Renderer` | Convert to `image.RGBA`; draw with `Fit` or `Fill` |
| `vpx.NewEncoder`, `vpx.NewDecoder` | VP8 and VP9 with constant bitrate, keyframe control, `SetBitrate`, hardened decoding |
| `vpx.Load`, `Engine`, `LoadError`, `Capabilities` | Choose and inspect the engine; the largest real-time sizes it reached |
| `vpx.WriteLibrary`, `Notices` | Write the embedded library for signing; the licence texts to show |
| `h264.NewEncoder`, `h264.NewDecoder`, `h264.Capabilities` | H.264 through the operating system's codec (macOS VideoToolbox, Windows Media Foundation); absent elsewhere, with a typed reason |
| `h264.ParseKeyframe`, `SPS.Codec` | The codec string of a keyframe, such as `avc3.42c01f` |

Errors are sentinels that work with `errors.Is`: `ErrNoDevice`, `ErrPermission`, `ErrDeviceLost`, `ErrUnsupported` (this platform has no capture backend) and others. `vpx` adds `ErrCorrupt`, `ErrTooLarge`, `ErrNeedKeyframe`, `ErrCodec` and `ErrUnavailable`; `h264` has the same names and adds `ErrUnsupportedStream` and `ErrProfile`, and its `ErrUnavailable` comes as an `*UnavailableError` that says why.

## Platforms

| Platform | Codec engine by default | H.264 (OS codec) | Camera backend | What was run |
|---|---|---|---|---|
| macOS arm64 | native, portable as fallback | VideoToolbox, hardware on the Mac that ran it | AVFoundation through purego | Tests, hardened runtime (ad-hoc signed), the Mac's real camera list, and one capture of 45 frames from the built-in camera. No Developer ID signature or notarization. |
| macOS amd64 | same | VideoToolbox | same | Test binary under Rosetta. No Intel Mac, no camera. |
| Windows arm64 | native, portable as fallback | Media Foundation, software | Media Foundation through COM | Tests natively in a Windows 11 VM: both engines, camera enumeration (none present) and a file-backed reader that uses the camera code path. No camera stream. |
| Windows amd64 | same | same | same | The same tests under the VM's x64 emulation. Not native. |
| Linux amd64, arm64 | portable; native with `-tags kolovideo_native` | none: `ErrUnavailable` | V4L2 in pure Go | Docker (arm64 native, amd64 emulated): both engines and hostile environments. The V4L2 streaming code ran against a model of the kernel interface and, in an arm64 VM, against the kernel's virtual vivid driver: no real camera. |
| Other | portable | none: `ErrUnavailable` | none: `ErrUnsupported` | Builds and passes `go vet`; frames, conversion and the codec work. |

A camera stream was run on one Mac's built-in camera only; Linux and Windows capture never ran on a real camera. On Linux the default binary is static and starts on Alpine and distroless images; add `-tags kolovideo_native` on a glibc host (2.28 or later) for the native engine. A signed macOS app needs one more step for it, and Windows Smart App Control may refuse the embedded DLL; in both cases the portable engine runs and `vpx.LoadError()` says why. H.264 ran on a Mac (round trips in three profiles, hardware encoder and decoder) and in a Windows 11 ARM64 VM (software), and each decoded the other's streams; it did not run on an Intel Mac, a native Windows amd64 machine or any Linux hardware. Tests that open real cameras run only with `KOLO_VIDEO_HW_TEST=1`. The steps, how each platform was validated and how to repeat it are in [platforms](docs/platforms.md).

`CGO_ENABLED=0` never needs a compiler. With cgo on, macOS builds and Linux builds with `-tags kolovideo_native` need a working C compiler, because purego imports `runtime/cgo` there; Go turns cgo off by itself when no compiler is on `PATH`.

More: [guide](docs/guide.md), [limits](docs/limits.md), [platforms](docs/platforms.md), [formats](docs/formats.md), [security policy](SECURITY.md).

## License

Licensed under either of the [Apache License, Version 2.0](LICENSE) or the [MIT license](LICENSE-MIT), at your option. The codec carries the notices of libvpx and its patent grant ([LICENSE-libvpx](third_party/licenses/LICENSE-libvpx), [PATENTS-libvpx](third_party/licenses/PATENTS-libvpx)), wasi-libc ([LICENSE-wasi-libc](third_party/licenses/LICENSE-wasi-libc)), wasm2go ([LICENSE-wasm2go](third_party/licenses/LICENSE-wasm2go)) and the mingw-w64 runtime ([LICENSE-llvm-mingw](third_party/licenses/LICENSE-llvm-mingw)). The Linux libraries link `libgcc` statically (GPLv3 with the GCC Runtime Library Exception, which asks for no notice in object code), the Apple libraries link only system libraries, the wasm links no LLVM compiler-rt builtins (the linker map of a rebuild with the committed hash lists none, and [LICENSE-wasi-libc](third_party/licenses/LICENSE-wasi-libc) lists the wasi-libc objects it does link), and the Windows libraries are built with llvm-mingw, whose clang may link compiler-rt builtins (Apache-2.0 with the LLVM exception; they were not inspected, and the texts are in [LICENSE-llvm-mingw](third_party/licenses/LICENSE-llvm-mingw) and [LICENSE-wasi-libc](third_party/licenses/LICENSE-wasi-libc)). Applications that ship binaries are responsible for showing these notices; `vpx.Notices()` returns them as text.

kolo-video ships no H.264 encoder or decoder; H.264 encoding and decoding use the codec of the operating system under its license terms, which include an H.264/AVC notice. This is not legal advice: an application that ships or uses H.264 can have licensing obligations of its own.

Dependencies in `go.mod`:

| Module | Licence | Built into |
|---|---|---|
| `github.com/ebitengine/purego` | Apache-2.0 | macOS, and Linux with `kolovideo_native` |
| `golang.org/x/sys` | BSD-3-Clause | Linux and Windows |
| `golang.org/x/image` | BSD-3-Clause | every platform (scaling) |

## Status

Experimental. The API may change before v1.0.0.
