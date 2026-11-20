# Security Policy

## Reporting a vulnerability

Please do not report security vulnerabilities in public issues, pull
requests or discussions.

Report them privately through GitHub:
[open a private security advisory](https://github.com/ciranda-dev/kolo-video/security/advisories/new)
(Security tab, "Report a vulnerability"). Only the maintainers can see it.

Include a description of the issue, the affected version and, if possible,
steps to reproduce it. We will acknowledge your report and keep you updated
while we work on a fix.

If private reporting is not available, open a public issue that only asks
for a private contact, with no details about the vulnerability.

## Supported versions

This library is experimental and at v0, so its API may change. Only the
latest v0.x release receives security fixes.

## What the library trusts and how it fails

kolo-video decodes video that arrives from the network and loads code that ships inside
the module. This is what it does about each, so you can judge the risk.

### Decoding untrusted video

- A packet above `MaxPacketBytes` (16 MiB by default, at most 256 MiB) is refused with
  `ErrTooLarge` before it is copied. Every frame header is checked in Go against `MaxPixels`
  before libvpx allocates anything, and the decoder works on its own copy of the packet,
  which it does not keep past 1 MiB. After any error it refuses packets
  until a keyframe.
- libvpx is C code, and on the native engine it decodes without memory safety. libvpx has had
  memory-safety fixes in the past. This module pins libvpx 1.16.0, and a later fix reaches it
  only through a new release with rebuilt libraries. If you decode video from peers you do
  not trust, `EnginePortable` runs the codec as translated Go with its own bounded memory
  instead of native code; it is slower (see `docs/limits.md`).
- Portable engine traps (libvpx `longjmp`, an out-of-bounds access, a stack overflow, the
  memory limit) are recovered at the engine boundary. A decoder returns `ErrCorrupt`,
  replaces its instance and waits for a keyframe; an encoder returns `ErrCodec` from then on.
  Other encoders and decoders are unaffected. Each instance has its own memory, at most
  1 GiB, so one stream cannot exhaust the process through another.

### Decoding H.264 through the operating system

The `h264` package does not contain a decoder. It hands video from the network to the codec of the operating system, which is a different trust boundary from libvpx: VideoToolbox on macOS, the Media Foundation H.264 transforms on Windows. This is what happens before and after that hand-over.

- Every packet is split, bounded and parsed in Go before any byte reaches the system: packet size, NAL unit count and types, the SPS (with its full VUI), the PPS and the slice headers, profile, chroma format, bit depth, interlacing, cropping, the level against the picture size and the reference frames, and one picture per packet. Only NAL unit types 1, 5, 7 and 8 are passed on. A stream outside Constrained Baseline, Main and High, level 5.1, progressive 4:2:0 at 8 bits is refused with `ErrUnsupportedStream` and never decoded. The values and their sources are in [docs/limits.md](docs/limits.md#h264).
- Parameter sets are accepted only in a keyframe packet, the decoder keeps those of the last accepted keyframe, and a different set in a delta packet is refused. Timestamps never reach the system: it gets its own counter.
- The system codec still parses the slice data of the profiles it accepts. A bug in a vendor decoder, reached with a stream that passes every check, is outside what kolo-video can prevent.
- macOS: encoding and decoding ran in system XPC services (`VTEncoderXPCService` and `VTDecoderXPCService`), for the hardware path and the software path alike, so a crash in the codec should reach your process as a session error. That was not provoked and is not claimed. The SPS and PPS are parsed in your process by CoreMedia (from the normalized copy), and the output is checked (pixel format, planes and size) before it is copied.
- Windows: the decoder is a DLL that runs in your process. A crash inside it ends the application, and no helper process mode is provided: raw pictures would have to cross a process boundary at about 3 MB for a 1080p picture. The Windows decoder never reported corrupt data in any test: a damaged slice that passes the checks in Go is concealed by the system and returned as a picture, and `Decode` reports no error for it. Corrupted pictures can be shown until the next keyframe.
- A decoder that hangs inside the system blocks `Decode`, and `Close` waits for it. Nothing inside the system can cancel a call.
- A sender that changes the picture size at every keyframe makes the receiver create a system session for each one, which costs CPU. The cost is bounded by the keyframes it receives.
- Whoever can put objects in front of a receiver can send it H.264. With a catalog that anyone on the path can edit, a relay can add an H.264 track, steer receivers that list H.264 to its stream, or remove the H.264 track so that receivers take VP8. Decode H.264 only when your application asked for it, and let the keys of your transport limit who can publish. The checks above hold for any stream, but the target is the system decoder instead of libvpx.
- H.264 here protects nothing: it is a codec. kolo-video does not encrypt, sign or authenticate any video.
- Encoder output from the system is checked as well: lengths, NAL unit types, slice types and the level, with the same parser.
- Linux has no engine. The detection scan opens `/dev/video*` read-only and issues only two query ioctls under a 2 s deadline; it never streams. A driver that never returns leaves one goroutine and one descriptor until the kernel call returns.
- No new dependency and no code is bundled for H.264. The licence terms of the system codecs are the system's, not this module's (see the README).

### The embedded native libraries

- The libraries are committed in the module. What protects a consumer from a malicious copy
  is the Go module checksum database and `go.sum` (the module version and its bytes), and the
  committed bytes themselves: read them in the repository if you must. `sums.go` holds the
  SHA-256 and size of each library, a test checks them, and the loader hashes the bytes it is
  about to load (the in-memory file or the cache file) and compares them before every `dlopen`
  or `LoadLibraryEx`. That hash is embedded in the same module as the library, so it detects
  corruption and a tampered cache file, not a malicious module version: whoever could change
  the library in a release could change the hash. A mismatch is an error, and the portable
  engine runs unless you asked for the native one.
- The build is pinned in `build/` (libvpx commit, toolchains, container digests). The
  maintainer checks it with `build/verify.sh`, and `build/verify.sh --rebuild` rebuilds the
  wasm and must reproduce it byte for byte, then rebuilds the six native libraries and
  reports whether they differ. Nothing runs these checks automatically: the GitHub workflows
  in `.github/workflows` start only by hand and have never run, so no CI result and no build
  provenance attestation exists. The native libraries are not reproducible across machines,
  so for them trust rests on the committed bytes. For video from peers you do not trust,
  prefer `EnginePortable`, which runs the translated Go and no native code.
- Linux: the library is loaded from an anonymous in-memory file and nothing is written to
  disk. If that is refused, the cache directory is used.
- Cache (macOS, Windows and the Linux fallback): `<user cache dir>/kolo-video/vpx/<version>-<sha256>/`
  (`LoadConfig.CacheDir` replaces the root). Symbolic links in the path are resolved once and
  only the real path is used afterwards. Every directory from the filesystem root down to the
  cache directory is checked, because whoever can write to one of them can swap the verified
  file for another between the hash check and the load: each must be owned by the current
  user or by root and must not be writable by group or others, except a directory with the
  sticky bit such as `/tmp`, where users can remove only their own entries. On macOS no
  extended access list entry of a directory may allow anyone to write, add or delete entries,
  change its permissions or take it over, since such an entry grants what the mode denies.
  On Windows each must be owned by the user, SYSTEM, Administrators, TrustedInstaller or
  CREATOR OWNER, and no other account may hold delete, delete-child, change-permissions or
  take-ownership rights on it; every entry type that grants rights counts, conditional and
  object entries included, and an entry of an unknown type counts as all rights.
  The cache directory itself must belong to the user (on Windows one of the accounts above),
  with mode 0700 (a looser mode is repaired) or, on Windows, a protected access list for the
  user, SYSTEM and Administrators (repaired when the owner is right). A directory that fails a
  check is refused with `ErrNativeCache` and the Linux fallback or the portable engine takes
  over. A missing directory is created and checked again, in case another user created it
  first; on Windows each directory is created with the protected access list, so it never
  holds rights that its parent hands down to others. The file is
  written to a temporary name, synced, set to mode 0500 and renamed into place. A file is
  used only when its hash, size and type match and, on macOS and Linux, the current user owns
  it, group and others cannot write it, no access list entry lets anyone write it (macOS) and
  it has no other hard link; on Windows a trusted account owns it and no other account may
  write or append data, write its attributes or extended attributes, delete it or change its
  permissions or owner (the link count is not checked there). Otherwise it is replaced once.
  Concurrent first starts leave one file. The strongest route each system allows is used to make the bytes that are loaded the
  bytes that were checked. Linux: the cache file is hashed through a descriptor and loaded
  through `/proc/self/fd`, so a swap of the path cannot change what runs (without `/proc` the
  path is loaded). macOS and Windows: the loaders take a path, so the file is read through one
  handle, which is hashed and checked (macOS: owner, mode, access list, one link; Windows:
  owner and access list); its identity is compared with the
  path just before and just after the load; and the directories above it are checked, so only
  the user can swap it. On Windows the open handle also keeps the file from being replaced or
  deleted during the load. On macOS the residual is a swap by another process of the same user
  inside that window: it is noticed after the load and the library is closed again, but its
  start-up code has run. A user is not a security boundary against themselves.
- macOS: library validation under the hardened runtime refuses a library that does not
  carry the app's Team ID; the loader reports it and uses the portable engine. In an app
  bundle the loader tries `Contents/Frameworks/libkolovpx.dylib` first and checks its code
  signature with `SecStaticCodeCheckValidity` before loading, because a file whose signature
  is valid but whose code was altered is killed by the system when a page is touched.
  The file must be a plain file (not a link) owned by the user or root that no one else can
  write, and it must be the same file before and after the load. Any valid signature counts,
  also one by another signer: the bundle is as trusted as the application that holds it, and
  the hardened runtime is what ties it to the app's Team ID.
  The embedded libraries carry an ad-hoc signature only, no Developer ID.
- Windows: the DLL is loaded by absolute path with `LOAD_LIBRARY_SEARCH_DLL_LOAD_DIR |
  LOAD_LIBRARY_SEARCH_SYSTEM32`, never through the default search path. It imports only
  KERNEL32 and the Universal C Runtime. It is not Authenticode-signed, so Smart App Control
  or an application-control policy may refuse it; the portable engine runs.
- `LoadConfig.Library` loads a file you ship, without a hash check. Protect that file as you
  protect your executable. On macOS its signature is checked first, but an altered file that
  still has a valid signature can be killed by the system and that cannot be caught.
- The library makes no network connection, starts no process and uses no cgo. It reads only the
  variables that `os.UserCacheDir` reads to find the cache directory (`HOME`,
  `XDG_CACHE_HOME`, `LocalAppData`), and none when `LoadConfig.CacheDir` is set.

### Cameras

- Frames from a device are validated before conversion; sizes above `MaxPixels` and invalid
  buffers are refused, never panic. `Devices` never starts a camera (on Linux it opens each `/dev/video*` node read-only, under a
  2 s deadline, to ask for its name and modes) and `OpenCapture` never asks for permission and
  gives up on a driver that does not answer within `StallTimeout`. On macOS an Objective-C exception would end the process, so each
  documented precondition of AVFoundation is checked in Go before the call that raises.
