# Guide

How to use kolo-video: frames, the VP8 and VP9 codec and its two engines, H.264 through the operating system, and capture on each operating system. The README has the quick start; bounds are in [limits](limits.md) and what ran where is in [platforms](platforms.md).

## Frames

A `kolovideo.Frame` holds one I420 picture: 8-bit `Y`, `U` and `V` planes (U and V at half width and height, rounded up), BT.601 matrix, limited range (Y 16 to 235), plus `Timestamp` and `Duration`. `NewFrame(w, h)` allocates a black frame. Every function that takes a frame calls `Validate` and returns `ErrInvalidFrame` instead of panicking.

`FromNV12`, `FromYUY2`, `FromUYVY`, `FromBGRA` and `FromRGBA` convert a raw picture with any stride to a new frame. `FromYUY2` and `FromUYVY` need an even width. `Frame.RGBA()` converts back; `Frame.YCbCr()` shares the planes but the standard library reads its samples as full range, so colors drawn from it are slightly flat. `Renderer{Scale: Fit}` draws into an `*image.RGBA` with letterboxing; `Fill` crops. A renderer keeps a scratch image, so use one per stream.

## Encoding and decoding

```go
enc, err := vpx.NewEncoder(vpx.Config{Codec: vpx.VP9, Width: 1280, Height: 720})
dec, err := vpx.NewDecoder(vpx.DecoderConfig{Codec: vpx.VP9})

pkts, err := enc.Encode(frame, false) // zero or one packet
out, err := dec.Decode(pkts[0])       // a new frame, or nil if the packet shows none
```

- The encoder is one-pass constant bitrate with no lag, a real-time deadline, error resilience and frame dropping. `Encode` returns no packet when rate control drops the frame. The first frame and one frame per `KeyframeInterval` of frame time are keyframes; `Encode(f, true)` forces one. A frame whose timestamp does not advance counts one frame time later. `SetBitrate` changes the target from the next frame.
- Zero `Config` fields take defaults: 30 fps, 0.07 bits per pixel per frame, a keyframe every 2 s (see [limits](limits.md)). `FrameRate` otherwise runs from `kolovideo.MinFrameRate` (one frame per hour) to `kolovideo.MaxFrameRate` (1000); `CaptureConfig.FrameRate` has the same range. Anything else is `ErrInvalidConfig`.
- A `Packet` holds one VP8 frame or one VP9 frame or superframe. VP9 is profile 0 (8-bit 4:2:0). `Timestamp` and `Duration` pass from frame to packet to decoded frame unchanged.
- The decoder refuses a packet above `MaxPacketBytes` (`ErrTooLarge`) and checks every frame header against `MaxPixels` before libvpx allocates anything (`ErrTooLarge`), decodes from its own copy of the packet, and refuses packets with `ErrNeedKeyframe` from the start and after any error until a keyframe arrives. Ask the sender for a keyframe when you see it.
- Encoder and decoder are safe for concurrent use. `Close` is idempotent and later calls return `ErrClosed`.

## Choosing an engine

Two engines run the same libvpx 1.16.0 and the choice is made once per process.

| Engine | What it is | Speed (measured on an Apple M5) | Where it runs |
|---|---|---|---|
| native | A prebuilt shared library embedded in the module, loaded without cgo. SIMD and threads | Real time up to 1920x1080 at 30 fps, encode and decode | macOS and Windows by default, Linux with `-tags kolovideo_native`, on amd64 and arm64 |
| portable | The same libvpx compiled to WebAssembly and translated to Go. No SIMD, one thread | Encode real time up to 640x480 at 30 fps, decode up to 1920x1080 | Every platform; no file written, no JIT, works under the macOS hardened runtime and in static binaries |

`vpx.NewEncoder` and `vpx.NewDecoder` load the default configuration on first use: `EngineAuto` tries the native engine and falls back to the portable one, so a program always works. To see and control the outcome:

```go
rt, err := vpx.Load(vpx.LoadConfig{})  // Engine: EngineAuto
fmt.Println(rt.Engine, rt.Version)     // native or portable, "v1.16.0"
fmt.Println(rt.NativeErr)              // why native is not in use, nil if it is; log it

lim := vpx.Capabilities()              // the largest real-time modes of the engine in use
fmt.Println(lim.Encode.Width, lim.Encode.Height, lim.Encode.FrameRate)
```

- `Load(LoadConfig{Engine: vpx.EngineNative})` returns `ErrUnavailable` wrapping the cause when the native library cannot load, instead of falling back. `EnginePortable` never loads the native library.
- With `EngineAuto` a native failure is not an error from `Load`; only `Runtime.NativeErr` and `LoadError()` report it. `ErrNativeIntegrity` and `ErrNativeSignature` there mean corruption or tampering (the embedded bytes or a bundle library did not verify), unlike `ErrNativeUnsupported`; log them.
- `Load` is optional and runs once. After an engine is active, `Load` with another configuration returns `ErrInvalidConfig` and the first engine stays.
- `vpx.Engine()`, `vpx.LoadError()` and `vpx.Capabilities()` answer the same questions without a `Runtime` value, but they are not free queries: if nothing is loaded they load the default configuration (on macOS and Windows that extracts and loads the native library), and after that `Load` with another configuration returns `ErrInvalidConfig`. Call `Load` first when you want another configuration. `LoadError` names the operating system's message and, where it knows, the fix, and matches `errors.Is` against `vpx.ErrNativeUnsupported`, `ErrNativeIntegrity`, `ErrNativeABI`, `ErrNativeSignature` and `ErrNativeCache`.
- `Capabilities()` returns the table below, not a run-time probe: encode and decode limits for the engine in use, measured once with a camera-like stream at a 33.3 ms frame period. A slower CPU reaches less, so use it to cap resolution rather than to promise a speed. The portable engine still makes valid 720p and 1080p streams, only slower than real time.

| Engine | Encode | Decode |
|---|---|---|
| native | 1920x1080 at 30 fps | 1920x1080 at 30 fps |
| portable | 640x480 at 30 fps | 1920x1080 at 30 fps |

- Streams from the two engines are byte-identical only when the encoder runs one thread. The default thread count follows the frame size and `runtime.NumCPU()` (VP8 at most 4, VP9 at most 4), so two machines can make different, equally valid streams. Decoding does not depend on it.
- The portable engine runs on one thread. It ignores the thread count for VP8, and for VP9 uses it only to set tile columns, so its VP9 stream differs from the native one at two threads or more.

### Static and glibc builds on Linux

Without a tag a Linux binary is static and starts everywhere, with the portable engine. A binary that loads a native library is dynamically linked to glibc, which does not start at all on Alpine or on a distroless static image, and a runtime fallback cannot help because the failure happens before `main`. So the native engine is opt-in on Linux:

```sh
go build -tags kolovideo_native ./...   # glibc 2.28 or later, amd64 or arm64
```

The library loads from an anonymous in-memory file, so nothing is written to disk. If the kernel or a seccomp profile refuses that, the cache directory is used (it must belong to you and no directory above it may be writable by others, see [limits](limits.md#native-loader)), and if that fails too (`noexec`, no `/proc`, a glibc older than 2.28) the portable engine runs and `LoadError` says which step failed.

### Shipping on macOS and Windows

An app that is signed and notarized on macOS enforces library validation, which refuses the extracted native library, so the portable engine runs unless the app signs the library itself or carries an entitlement. On Windows, Smart App Control or an application-control policy can refuse the unsigned DLL with the same result. `vpx.WriteLibrary(dir)` writes the embedded library for signing and `LoadConfig.Library` names a library the app ships. The steps are in [platforms](platforms.md#macos).

## H.264 through the operating system

The `h264` package encodes and decodes H.264 with the codec the operating system ships: VideoToolbox on macOS, the Media Foundation H.264 transforms on Windows desktop editions. kolo-video contains no H.264 code and bundles none, so there is nothing to install and nothing is downloaded. Where the system has no codec, H.264 is reported absent with a typed reason, and VP8 and VP9 are unaffected: `h264` does not import `vpx`, so a program that uses only H.264 does not link libvpx.

### When to use it

Use VP8 or VP9 when you need a codec that runs the same everywhere: it works on every platform with the same bytes. Use H.264 when the bitstream matters, because other software decodes it, and the machine has a codec for it. H.264 here is not about speed: on macOS the system codec uses the hardware and on Windows it is the system's software codec (see [platforms](platforms.md#h264)).

The pair for a sender is VP8 always, plus H.264 when `Capabilities` says it can encode: two encodes of the same frames, published as two renditions of one source. A receiver that decodes H.264 takes it, and one that cannot (Linux, or any system without the codec) takes VP8. Nothing falls back by itself: your application checks `Capabilities` and chooses, and if the decoder reports `ErrUnsupportedStream` for a sender's stream you stop using that rendition.

### Ask the system first

```go
caps := h264.Capabilities() // probed once per process
fmt.Println(caps.Encode.Available, caps.Encode.Hardware, caps.Encode.Engine, caps.Encode.Profiles)
fmt.Println(caps.Decode.Available, caps.Decode.Err)
```

`Capabilities` returns a `Limits` with one `Support` per direction: `Available`, `Hardware` (the system reports a hardware codec for that direction), `Engine` (`VideoToolbox` or `Media Foundation`), `Profiles`, `MaxWidth` and `MaxHeight` (what the vendor documents, 0 when it documents nothing) and `Err`, the reason when it is not available. That error is an `*UnavailableError` that matches `errors.Is(err, h264.ErrUnavailable)`; it carries `GOOS`, `GOARCH`, the engine, the system's message and code, and on Linux the V4L2 stateful H.264 devices that were found and are not used. On Linux and on every platform with no engine it reads like `kolovideo/h264: no H.264 engine on linux/arm64 in this release; V4L2 stateful H.264 devices: none`. `NewEncoder` and `NewDecoder` return the same error. Every error that matches `ErrUnavailable` is an `*UnavailableError`, also one from `Encode` or `Decode` when VideoToolbox reports during a stream that no codec is found or that it is busy. The answer does not change during the process. The probe opens no camera: on macOS and Windows it creates and frees codec sessions, and on Linux it opens each `/dev/videoN` read-only for `VIDIOC_QUERYCAP` and `VIDIOC_ENUM_FMT` and never streams. On macOS the profile list comes from the API and the encoder confirms its profile at the first keyframe; on Windows the encoder's list is probed by creating an encoder for each profile.

### Encode and decode

```go
enc, err := h264.NewEncoder(h264.Config{Width: 1280, Height: 720, Profile: h264.Main})
if errors.Is(err, h264.ErrUnavailable) {
	// publish VP8 only
}
defer enc.Close()
pkts, err := enc.Encode(frame, false)        // zero or one packet; the first frame is a keyframe
sps, err := h264.ParseKeyframe(pkts[0].Data) // the codec string for your track description
fmt.Println(sps.Codec(), enc.Hardware())     // for example avc3.4d001f true on a Mac

dec, err := h264.NewDecoder(h264.DecoderConfig{})
defer dec.Close()
out, err := dec.Decode(pkts[0])              // a new frame, or nil if the packet shows no picture
if errors.Is(err, h264.ErrNeedKeyframe) {
	// ask the sender for a keyframe
}
```

[examples/h264](../examples/h264) is the complete program: it encodes and decodes generated frames, prints the engine and the quality, and falls back to VP8 where H.264 is absent.

- `Config` mirrors `vpx.Config`: size, `FrameRate` (30 by default), `Bitrate` (0.07 bits per pixel per frame by default), `KeyframeInterval` (2 s on frame timestamps, negative for keyframes on request only) and `Profile`: `ConstrainedBaseline` (the default), `Main` or `High`. Sizes must be even, 48 to 4096 on each side, and stay within level 5.1, which also bounds the frame rate (1920x1080 up to 120 fps, 1280x720 up to 273 fps, 3840x2160 up to 30 fps, 4096x2304 up to 26 fps). Anything else is `kolovideo.ErrInvalidConfig`. The exact bounds are in [limits](limits.md#h264).
- The encoder works in real time and low latency only: one packet out for every frame in, inside the same `Encode` call, no B-frames and no reordering. `Encode` returns no packet when the system drops the frame. `Timestamp` and `Duration` pass from frame to packet to decoded frame unchanged; the system codec never sees them.
- A `Packet` is one Annex B access unit with 4-byte start codes. A keyframe packet starts with the SPS and PPS, so a receiver can start at any keyframe and no description is needed out of band. The decoder ignores `Packet.Keyframe` and reads the NAL unit types. The format is in [formats](formats.md#h264-packets).
- Keyframes: the first frame, one per `KeyframeInterval` of frame time, and `Encode(f, true)` on request. The system is asked for each one and the next frame asks again if it did not honour it.
- `SetBitrate` changes the target from the next frame. On the macOS hardware encoder it followed a change from 1.9 to 0.5 Mbit/s within 1 percent; the macOS software encoder measured 988 kbit/s for the 500 kbit/s target (not converged). The Windows encoder produced 456 kbit/s after a change to 500 kbit/s in the same kind of test.
- The decoder validates every packet in Go before the system sees it: size, NAL unit count and types, the SPS, PPS and slice headers, profile, level against the picture size, one picture per packet. A refused stream is `ErrUnsupportedStream`, a damaged packet `ErrCorrupt`, a size over a bound `ErrTooLarge`, and after any error the decoder returns `ErrNeedKeyframe` until a keyframe arrives. The Windows decoder never reports corrupt slice data: a damaged packet that passes the checks is concealed and returned as a picture, and the damage stays until the next keyframe. Do not rely on `ErrCorrupt` there to detect loss; use sequence numbers or the transport.
- Pictures come out in decoding order. A foreign stream with B-frames is refused, and an SPS that signals reordering is rewritten so the system decoder emits pictures as they arrive.
- The decoder passes samples through as decoded. A foreign stream that signals BT.709 or full range is not converted; `Decoder.SPS()` returns the signalled `FullRange` and `Matrix`.
- `Encoder` and `Decoder` are safe for concurrent use. `Close` is idempotent and returns after a call in progress; a call that hangs inside the system codec also blocks `Close`.
- A keyframe with a new size makes the decoder create a new system session (on macOS 41 to 59 ms measured), so a sender that changes size at every keyframe costs the receiver CPU.

## Capture

```go
devs, err := kolovideo.Devices()                  // never starts a camera
err = kolovideo.RequestAccess(ctx)                // macOS only; may show a system prompt
c, err := kolovideo.OpenCapture(kolovideo.CaptureConfig{DeviceID: devs[0].ID, Width: 1280, Height: 720})
f, err := c.Read(ctx)                             // oldest buffered frame
```

`OpenCapture` picks the supported mode nearest to the requested size, preferring modes that reach the requested frame rate and then formats that are cheaper to convert; modes above `MaxPixels` are never used. `Capture.Mode()` reports the mode and the rate the camera was set to. When the reader is slow at most `Buffer` frames are kept and the oldest is dropped; `Stats()` counts frames received, dropped and unconvertible. A disconnect, a capture failure, or no frame for `StallTimeout` ends the capture with `ErrDeviceLost` after the buffered frames are read.

Frames are always converted to BT.601 limited range I420; a camera that tags its frames BT.709 or full range is converted. `Timestamp` is the capture time on the system monotonic clock.

| System | Backend | Notes |
|---|---|---|
| macOS | AVFoundation through purego and the Objective-C runtime; system frameworks only, nothing extracted | Needs the user's consent (TCC). An app bundle needs `NSCameraUsageDescription` in its `Info.plist`; a command-line tool inherits its terminal's permission. `OpenCapture` returns `ErrPermission` until access is granted and never asks; `RequestAccess` asks. Listing lists built-in and external cameras, with modes, without starting them. Output is NV12 (BGRA if NV12 is not offered) |
| Windows | Media Foundation through COM, `golang.org/x/sys/windows` | Ships with Windows 10 and 11 desktop editions. N editions need the Media Feature Pack, otherwise `ErrUnsupported` names the missing DLL. Access follows the Windows camera privacy setting (`ErrPermission`). `Devices` does not list modes: they need the camera to be activated, so `OpenCapture` reads them from the activated source. Output is NV12 converted by the source reader |
| Linux | V4L2 in pure Go | The user needs access to `/dev/video*` through the `video` group or logind ACLs. Reads YUYV, UYVY, NV12, YU12 and Motion-JPEG. `Devices` opens each node named `videoN` read-only, all at once, for at most `DefaultStallTimeout` (2 s) in all, only to ask for its name and modes; it never streams, and a node that does not answer in time is skipped (the error is `ErrDeviceLost` if none answered). A node that another program holds exclusively (`EBUSY`) is listed with no modes. `OpenCapture` opens the chosen node read-write and gives up with `ErrDeviceLost` after `StallTimeout` if the driver does not answer. A node without usable modes (metadata, infrared) is never the default. A stepwise size range is listed by its largest size and fitted to the request when opened |
| Other | none | `Devices`, `RequestAccess` and `OpenCapture` return `ErrUnsupported`: this platform has no capture backend. Frames, conversion, rendering and the codec still work |

`RequestAccess` returns nil on Linux and Windows. [examples/snapshot](../examples/snapshot) saves frames as PNG files into a directory given with `-dir`; run it with `-list` first.

Only the macOS arm64 backend streamed from a real camera. The Windows backend ran against a file-backed source and the Linux backend against a model of the kernel interface; see [platforms](platforms.md) for exactly what that proves.
