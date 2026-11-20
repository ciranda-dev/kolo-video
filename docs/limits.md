# Limits

Every bound in kolo-video, with its value and where it comes from. The H.264 bounds are in their own section below. A value outside a range returns `ErrInvalidConfig` or `ErrInvalidFrame`, as noted. This page is reference material: the README does not repeat it. Timings were measured on an Apple M5 with libvpx 1.16.0; other machines differ.

## Frames and capture

| Field | Default | Range | Why |
|---|---|---|---|
| Frame width and height | none | 1 to 65536 | The VP9 limit (16-bit sizes). A frame whose planes or strides do not match returns `ErrInvalidFrame` |
| `CaptureConfig.Width`, `Height` | 640 x 480 | both zero or both positive, at most 65536 | The usual browser capture default |
| `CaptureConfig.FrameRate` | 30 fps | `MinFrameRate` (1/3600, one frame per hour) to `MaxFrameRate` (1000); capped at the chosen mode's highest rate | The same default. An hour keeps a frame duration in 90 kHz ticks inside libvpx's 32-bit duration (about 13 hours); 1000 fps leaves 90 ticks per frame. A rate a device reports is clamped to a frame time of one hour |
| `CaptureConfig.Buffer` | 2 frames | 1 or more | Holds one late read without adding latency; when full the oldest frame is dropped |
| `CaptureConfig.StallTimeout` | 2 s | negative disables | The select timeout of the V4L2 capture example in the kernel documentation |
| `CaptureConfig.MaxPixels` | 8,912,896 | positive | The VP9 level 5 picture size (4096x2176), which holds 3840x2160. A faulty camera cannot make every frame a huge allocation; modes above it are never chosen and MJPEG checks the decoded size |
| V4L2 driver buffers | 4 | at least 2 requested, fewer is an error | The kernel documentation's capture example |
| V4L2 and Media Foundation enumeration | 1024 entries | | A safety bound for a driver or source that never ends its list; real devices list fewer than 100 |
| NTSC frame rate tolerance | 1001/1000 | | NTSC rates are N x 1000/1001 |

Nothing grows beyond `Buffer` frames. The converters check the source buffer before they allocate the frame.

## Codec

| Value | Setting | Why |
|---|---|---|
| Frame size | VP8 1 to 16383, VP9 1 to 65536 | libvpx's own range checks (14 and 16 bits) |
| `Config.FrameRate` | 30 | `kolovideo.MinFrameRate` to `kolovideo.MaxFrameRate` (above) | Nominal rate for frames without a `Duration` |
| `Config.Bitrate` | 0.07 bits per pixel per frame, about 4.4 Mbit/s at 1080p30 | A usual live-video setting |
| `Config.KeyframeInterval` | 2 s, measured on frame timestamps; negative for keyframes only on request | A late joiner waits at most 2 s |
| Timebase | 1/90000 | RFC 7741 sections 4.1 and 6.1 |
| Rate control | one pass, constant bitrate, no lag, real-time deadline, error resilient, frame dropping at threshold 30, resizing off | The settings of a widely deployed real-time encoder and libvpx's own examples |
| Quantizer | minimum 2; maximum 56 for VP8, 52 for VP9 | Same |
| Rate control buffer | 500 ms initial, 600 ms optimal, 1000 ms maximum | Same |
| Undershoot and overshoot | VP8 100 and 15, VP9 50 and 50 | Same |
| Largest intra frame | max(300, 600 x 0.5 x frame rate / 10) percent of the per-frame bitrate | Same |
| Speed | VP8 -6 (-4 below 352x288), VP9 7 | Same |
| Threads, VP9 | 4 from 1280x720 with more than 4 CPUs, 2 from 640x360 with more than 2 CPUs, else 1; tile columns are half the threads | The rule of a widely deployed real-time encoder |
| Threads, VP8 | 3 above 1280x960 with 6 or more CPUs, 2 above 640x480 with 3 or more (3 from 6 CPUs), 4 at 1080p and above with more than 8 CPUs, else 1 | The same rule, except that 1080p with more than 8 CPUs uses 4, not 8: at 1080p 8 threads encoded no faster than 4 and had spikes of 10 to 36 ms against 5 ms (`BenchmarkEncodeThreads`, M5, 285 pictures per run, three runs) |
| `DecoderConfig.MaxPixels` | 8,912,896 | The VP9 level 5 picture size. Checked in every frame header before libvpx sees the packet; a VP9 packet without a superframe index is wrapped in a one-frame index so the frame that was checked is the frame that is decoded |
| Decoder threads | 1 | libvpx's default |
| `DecoderConfig.MaxPacketBytes` | 16 MiB; at most 256 MiB (`ErrTooLarge` and `ErrInvalidConfig`) | The default `MaxBytes` of the kolo-media playout; a VP8 or VP9 frame at `MaxPixels` is far smaller. The check runs before the packet is copied. The 256 MiB cap is the one of `h264` and stays below libvpx's 32-bit size with room for the index the decoder may append |
| Decoder packet buffer | at most 1 MiB kept between calls | The decoder copies each packet into its own buffer; a buffer that grew past 1 MiB (well above a 1080p frame at the default bitrate) is dropped after the call |
| Encoder stride | at most 2^31 - 1 | Larger strides are refused |
| `Config.Bitrate`, `SetBitrate` | at most 4,294,967,295,999 bit/s | libvpx takes kilobits in a `uint32`; a value that rounds to more is the maximum, never 0 |
| Presentation time | saturates at 2^63 - 1 ticks | A timestamp that does not advance gets one frame time more, without wrapping |

The encoder returns `ErrCodec` and the decoder `ErrCorrupt`, `ErrTooLarge` or `ErrNeedKeyframe`; none of them panics on a packet from the network.

## Engines

| Bound | Value | Where it comes from |
|---|---|---|
| Native, real time | 1920x1080 at 30 fps, encode and decode | Mean time per frame below the 33.3 ms period: VP8 1080p one thread 8.95 ms, VP9 1080p with 4 threads 6.30 ms, decode 2.97 and 3.08 ms |
| Portable, real time | encode up to 640x480 at 30 fps (VP8 12.8 ms, VP9 19.0 ms mean); decode up to 1920x1080 (16.8 and 14.0 ms) | The same measurement. At 720p the encoder needs 39 to 58 ms and at 1080p 89 to 125 ms |
| `Capabilities()` | the two rows above | A table, not a run-time probe |
| Portable memory per instance | at most 1 GiB (`--max-memory` at link time) | The largest measured module is a VP9 encoder at 8,912,896 pixels with 452 MiB (VP8 256 MiB, decoders 15 and 57 MiB); 1 GiB is 2.3 times that. An allocation past the cap fails inside the module and returns `ErrCodec` ("out of memory") |
| Portable stack | 1 MiB, placed first in memory | The measured deepest use is 57 KiB; an overflow is an out-of-bounds trap, never a write into data |
| Portable memory at common sizes | encoder VP8 10 / 28 / 62 MiB, VP9 28 / 63 / 123 MiB at 480p / 720p / 1080p; decoder 3 to 18 MiB | Measured module memory. Process memory is about four times the module memory, because translated memory grows by reallocation (523 MB for one VP9 1080p encoder and decoder) |
| Portable allocations | 0 per picture in steady state | `testing.AllocsPerRun`, VP8 and VP9 at 160x120 |
| Portable trap | recovered at the engine boundary | A decoder replaces its instance and waits for a keyframe (`ErrCorrupt`); an encoder returns `ErrCodec` from then on. Other instances are unaffected. 297 of 300 corrupt VP9 packets trapped in a test and each was recovered |
| Native library ABI | version 1 | The loader refuses any other value |
| Native library size | 1.27 to 2.0 MB per target, 9.8 MB for the six | The module holds all six; a binary embeds one |
| Build memory | about 570 MB on a Mac, 734 MB in a container limited to 1 GiB | `go build` with an empty cache, the generated portable package split into six chained shards. A single 7 MB package needed 2.3 GB and was killed in a 1 GiB container |

## Native loader

| Bound | Value | Why |
|---|---|---|
| Integrity | SHA-256 and size of the embedded bytes, checked before every load | `sums.go` is generated from the committed libraries; a test checks them |
| Cache directory | `<os.UserCacheDir()>/kolo-video/vpx/v1.16.0-<sha256>/`, mode 0700, owned by the user (Windows: the user, SYSTEM, Administrators, TrustedInstaller or CREATOR OWNER) | `LoadConfig.CacheDir` changes the root. Symbolic links in the path are resolved once and the real path is used for everything after. Every directory from the filesystem root to the cache directory is checked: it must be owned by the user or root (Windows: the accounts above) and must not be writable by group or others unless it has the sticky bit; on macOS no extended access list entry may allow anyone to write, add or delete entries, change permissions or take ownership (deny entries, such as the one on a home folder, are fine). Windows: no untrusted account may hold delete, delete-child, write-DAC or write-owner rights, and every entry type that grants rights counts, conditional and object entries included (an unknown type counts as all rights); adding entries is allowed. The leaf with a looser mode is repaired; any other failure is `ErrNativeCache` and the Linux fallback or the portable engine takes over. A group-writable ancestor counts as writable even when the group has one member: a home directory with mode 0775 and a private group (some Linux distributions) is refused, so use mode 0755 or set `LoadConfig.CacheDir`. After a successful load, temporary files older than a minute in the directory and sibling release directories not modified for 30 days are removed (only the library file, its temporary files and an empty directory) |
| Cache file | written to a temporary file, synced, mode 0500, renamed into place; a file with a wrong hash, size or type, one that another user owns, one that group or others can write (macOS: or an access list entry lets anyone write), or one with another hard link is replaced once | A concurrent first start by 16 goroutines and 4 processes leaves one file. Windows checks the owner and the access list, not the link count: no untrusted account may hold a right to write or append data, write attributes or extended attributes, delete the file or change its permissions or owner |
| Windows cache directory | protected ACL: the user, SYSTEM and Administrators, set when each missing directory is created | A new directory never holds what its parent hands down to others, not even briefly |
| Windows concurrent writers | the loser polls up to 1 s (50 x 20 ms) for the winner's file | Windows refuses to rename over a file that is open |
| Windows load | `LoadLibraryEx` with `LOAD_LIBRARY_SEARCH_DLL_LOAD_DIR` and `LOAD_LIBRARY_SEARCH_SYSTEM32` on an absolute path | A dependency cannot be planted next to the executable |
| macOS bundle | `Contents/Frameworks/libkolovpx.dylib`, signature checked with `SecStaticCodeCheckValidity` (any valid signature); the file must be a plain file owned by the user or root that no one else can write, and unchanged across the load | Only when the executable is `X.app/Contents/MacOS/*` |
| Linux | an anonymous in-memory file first, then the cache, which is loaded through the descriptor that was hashed (`/proc/self/fd`; the path when `/proc` is missing) | Nothing is written to disk when the first works |
| Minimum system | macOS 12, glibc 2.28, Windows 10; the Windows DLLs import only KERNEL32 and the Universal C Runtime | Deployment target of the build; `objdump` shows nothing newer than glibc 2.27 |
| macOS and Windows load | the file is hashed through one handle and checked as above (macOS: owner, mode, access list, one link; Windows: owner and access list), and its identity is compared with the path before and after the load | Windows keeps the handle open during the load, so the file cannot be replaced meanwhile; macOS refuses a swapped file after the load |
| `LoadConfig.Library` | no hash check | By design: a signed application ships and signs its own copy; see [SECURITY](../SECURITY.md) |

## H.264

The `h264` package keeps everything it produces or accepts within level 5.1 (ITU-T H.264 Table A-1), the highest level Microsoft documents for its decoder. A value outside a range returns `ErrInvalidConfig`; a stream outside the decoder's bounds returns `ErrTooLarge`, `ErrUnsupportedStream` or `ErrCorrupt` as noted. Timings were measured on an Apple M5 and in a Windows 11 ARM64 VM; other machines differ.

### Encoder

| Value | Setting | Why |
|---|---|---|
| `Config.Width`, `Height` | even, 48 to 4096 each | 48 is the smallest size Microsoft documents for its decoder, and the Windows encoder refused 32x32 and odd sizes; 4096 is its documented width. Cropping units are 2 for 4:2:0 (H.264 7.4.2.1.1), so odd sizes cannot be represented |
| Macroblocks per frame | at most 36864 (ceil(W/16) x ceil(H/16)) | Level 5.1 MaxFS, Table A-1 |
| Macroblocks per second | at most 983040 (macroblocks times `FrameRate`) | Level 5.1 MaxMBPS, Table A-1 |
| Largest frame rate at level 5.1 | 640x480 up to 819 fps; 1280x720 up to 273; 1920x1080 up to 120; 3840x2160 up to 30.3; 4096x2160 up to 28.4; 4096x2304 and 2304x4096 up to 26.6 | The two bounds above |
| `Config.FrameRate` | 30; `kolovideo.MinFrameRate` to `kolovideo.MaxFrameRate` | As `vpx`. No system documents or reports a frame-rate limit, so `Capabilities` reports none |
| `Config.Bitrate` | 0.07 bits per pixel per frame, about 4.4 Mbit/s at 1080p30; 1 to 240,000,000 bit/s | The default is the one `vpx` uses. The maximum is level 5.1 MaxBR (240000 in units of 1000 bit/s for Baseline and Main; High allows 1.25 times more, the smaller bound is used for all) |
| `Config.KeyframeInterval` | 2 s on frame timestamps; negative for keyframes on request only | As `vpx` |
| `Config.Profile` | Constrained Baseline; Main and High on request | The Windows encoder produced Main when no profile was set, so the engine always sets one and checks the SPS of the stream |
| Picture format | progressive 4:2:0, 8 bits | Everything else is refused |
| Mode | real time, low latency: one access unit out for every frame in, no B-frames, no reordering, no lookahead | The encoder checks every slice header and returns `ErrCodec` on a B slice or on a level above 5.1 |
| Level of the output | at most 5.1 | The system chooses it within the bounds above. Both engines signalled level 5.2 at 4096x2304 and 30 fps, which `Config` refuses |
| SPS and PPS | before every keyframe | A group of pictures is self-contained |
| Timestamps | passed through; the system sees its own counter in 90 kHz ticks | Wire timestamps never reach the system |

### Decoder

| Value | Setting | Why |
|---|---|---|
| `DecoderConfig.MaxPixels` | 9,437,184 (36864 macroblocks); at most that value | Level 5.1 MaxFS. Checked on the coded size (macroblocks times 256, before cropping) before any system call, because that is what the system decoder allocates whatever the cropping. 1920x1080 is coded as 1920x1088, and the macOS encoder codes 48x48 as 192x96, so 18,432 is the smallest bound that takes its 48x48 stream |
| `DecoderConfig.MaxPacketBytes` | 16 MiB; at most 256 MiB | The default `MaxBytes` of the kolo-media playout. An all-I_PCM picture at `MaxPixels` is about 14.2 MB. The cap keeps bit offsets within 32 bits |
| NAL units per packet | at most `MaxPixels / 256 + 288` (`ErrTooLarge`) | One slice per macroblock at most, 32 SPS ids and 256 PPS ids. Units of dropped types count |
| Start codes | the packet starts with `00 00 01` or `00 00 00 01`; an empty NAL unit is `ErrCorrupt` | Annex B |
| NAL unit types passed on | 1, 5, 7 and 8 | Types 2, 3 and 4 (data partitioning) are `ErrUnsupportedStream`; every other type (SEI, delimiters, filler, SVC and MVC units, reserved) is dropped and never reaches the system |
| Profiles | profile_idc 66, 77 and 100 | Constrained Baseline, Baseline, Main and High. 4:2:0 and 8 bits only; no transform bypass |
| Levels | `level_idc` 9 (High only), 10, 11, 12, 13, 20, 21, 22, 30, 31, 32, 40, 41, 42, 50, 51 | Levels 1 to 5.1; level 1b is `level_idc` 9 in High and 11 with constraint_set3 in Baseline and Main (H.264 7.4.2.1.1). Level 5.2 was decoded by both engines in tests and is still refused, because Microsoft documents 5.1; level 6 gave no picture and no error on Windows, so the check is in Go |
| Picture size | cropped width and height each 48 to 4096; macroblocks within MaxFS of the declared level, and at most Sqrt(MaxFS x 8) of that level per side (543 at level 5.1, 169 at level 3.1) | The Windows decoder's documented range, so a stream behaves the same on every engine; H.264 A.3.1 and Table A-1. Both system encoders chose levels within these bounds for every size tried, 4096x48 and 48x4096 included |
| Cropping | right and bottom only (`ErrUnsupportedStream` for left and top); offset checked before use | Measured crops from both encoders are right and bottom only |
| Reference frames | `max_num_ref_frames` at most MaxDpbFrames of the declared level at that size | H.264 A.3.1. At 4096x2304 and level 5.1 that is 5, so a hostile SPS cannot ask for 17 pictures of 14 MB |
| SPS ids, PPS ids | 0 to 31, 0 to 255 | H.264 7.4.2 |
| Exp-Golomb values | at most 31 leading zero bits; every field range-checked before use | H.264 9.1 |
| Slices | P and I only; an IDR slice is I; B, SP and SI are `ErrUnsupportedStream` | The encoders make no B-frames; the others are refused before the system sees them |
| PPS | no slice groups (FMO), no redundant pictures; CABAC only with Main and High | Refused before the system sees them |
| One picture per packet | the slices share nal_unit_type, pic_parameter_set_id, frame_num, idr_pic_id and picture order fields, the first starts at macroblock 0 and each next one strictly later | H.264 7.4.1.2.4. A second picture in one packet is refused; arbitrary slice order too |
| End of an SPS or PPS | nothing after `rbsp_trailing_bits`: a zero byte after them is `ErrCorrupt` | H.264 7.3.2.11; only slice data may carry zero words after its trailing bits (7.3.2.10). Neither system encoder sends them, and an SPS or PPS reaches the system parser in your process |
| Parameter sets | accepted only in a keyframe packet (one with a type-5 unit), which must hold an SPS, a PPS and only IDR slices | A parameter set in a delta packet that equals the active one is dropped; a different one is `ErrUnsupportedStream` |
| Keyframe wait | until a valid IDR with its SPS and PPS arrives, at start and after any error | `ErrNeedKeyframe`. The Windows decoder would otherwise drop delta packets without an error |
| Display order | pictures are returned in decoding order | An SPS with `pic_order_cnt_type` 0 or 1 from a foreign encoder whose display order differs from decoding order is shown in decoding order |
| Colour | not converted | Streams that signal BT.709 or full range are decoded as is; `Decoder.SPS()` exposes the values |

Every arithmetic step runs on bounded values in 64-bit integers, so it is the same on 32-bit targets. None of these checks panics on a packet from the network.

### System codec

| Value | Setting | Why |
|---|---|---|
| Windows decoder | 48x48 to 4096x2304 reported by `Capabilities`; software only (`Hardware` false) | Microsoft's documentation for the decoder. A 2304x4096 stream decoded in a test, so the size check in Go is the bound |
| Frame-rate limit | none reported | No system documents one |
| macOS hardware | used when VideoToolbox picks it, reported by `Hardware()`; never required | Hardware is the VideoToolbox default; the software path was tested with it disabled |
| Windows speed | 1280x720 encode 16.1 ms p50 and 37.5 ms p99, 1920x1080 27.0 ms p50 and 81.7 ms p99 (software, arm64 VM, one run) | At 30 fps a frame has 33.3 ms, so 1080p is at the edge of real time there; decode p50 was 6.1 ms at 720p and 13.4 ms at 1080p |
| macOS speed (hardware) | encode p50 1.8 ms at 640x480, 3.2 ms at 720p, 8.2 ms at 1080p; decode 0.5, 1.1 and 2.0 ms | Measured while other jobs ran on the machine; an earlier quiet run gave 4.4 ms and 2.1 ms at 1080p |
| Linux device scan | 2 s deadline for all nodes, at most 1024 format entries per node, read-only | A safety bound; only `VIDIOC_QUERYCAP` and `VIDIOC_ENUM_FMT` are issued, nothing streams |
| Hang in the system codec | `Decode` and `Encode` block, and so does `Close` | Nothing inside the system can be cancelled |
