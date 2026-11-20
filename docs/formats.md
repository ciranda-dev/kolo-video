# Formats

The bytes kolo-video produces and accepts, so a receiver or a catalog can be written against them. This page is reference material.

## Frames

A `kolovideo.Frame` is I420: three 8-bit planes, U and V at half width and height rounded up, BT.601 matrix, limited range. `Timestamp` and `Duration` are Go durations and are never written into a packet by this module.

## VP8 and VP9 packets

`vpx.Packet.Data` is one VP8 frame, or one VP9 frame or superframe, as libvpx produces it. The encoder's timebase is 1/90000 (RFC 7741 sections 4.1 and 6.1). A VP9 stream is profile 0, 8-bit 4:2:0. `Keyframe` is set by the encoder; the decoder reads the frame header itself.

## H.264 packets

`h264.Packet.Data` is one access unit in Annex B form (ITU-T H.264 Annex B):

- Every NAL unit starts with the 4-byte start code `00 00 00 01`. The decoder also reads 3-byte start codes, but the encoder never writes them.
- Only NAL unit types 1 (non-IDR slice), 5 (IDR slice), 7 (SPS) and 8 (PPS) appear. The encoder drops SEI, access unit delimiters and filler that the system codec adds; the decoder drops them too and never passes them on.
- One packet is one picture. There are no B slices and no reordering, so packets are in decoding order and decoding order is output order.
- A keyframe packet starts with the SPS and the PPS, then the IDR slices. A delta packet holds only slices. Because every keyframe repeats its parameter sets, a receiver that joins at any keyframe can decode from there and nothing has to be sent out of band. In the vocabulary of MP4 and RFC 6381 this is "avc3".

A keyframe packet looks like this (a 1280x720 Constrained Baseline stream; the SPS is shown without its VUI):

```text
00 00 00 01  27 42 c0 1f ...    SPS, NAL type 7
00 00 00 01  28 ce 3c 80        PPS, NAL type 8
00 00 00 01  25 b8 20 02 ...    IDR slice, NAL type 5 (several slices when the system encoder splits the picture)
```

### The SPS Kolo writes

Both engines put the same VUI in every SPS: video signal type with BT.601 colour description (colour primaries 6, transfer 1, matrix 6) and limited range, and a `bitstream_restriction` group with `max_num_reorder_frames` 0 and `max_dec_frame_buffering` equal to `max_num_ref_frames`. That tells every decoder, Kolo or not, that no picture waits for reordering. Every field of the SPS before the VUI is the system encoder's, bit for bit. The decoder applies the same rewrite to an SPS that lacks `bitstream_restriction`, on its own copy: the rewritten SPS never leaves the receiver.

### Codec string

`h264.ParseKeyframe(packet)` returns the SPS of a keyframe packet and `SPS.Codec()` its codec string: `avc3.` followed by profile_idc, the constraint byte and level_idc as six lowercase hexadecimal digits (RFC 6381 section 3.3), for example `avc3.42c01f` (Constrained Baseline, level 3.1), `avc3.4d001f` (Main) or `avc3.64001f` (High). The string comes from the first keyframe and is advisory: a later keyframe can have another level, and a receiver should decide from the SPS in the stream. Encode the first frame before publishing a track, so the string is known.

| First digits | Profile (RFC 6184 Table 5) |
|---|---|
| `42` with constraint_set1 (`42c0`, `42e0`) | Constrained Baseline: profile_idc 66 with constraint_set1; the macOS software encoder sends `42e0` |
| `4d` | Main: profile_idc 77 |
| `64` | High: profile_idc 100 |

### What a decoder accepts

Profiles 66, 77 and 100 in 4:2:0, 8 bits, progressive, levels 1 to 5.1, one picture per packet; everything else is `ErrUnsupportedStream`. The full list is in [limits](limits.md#h264).

### Browsers

Decoding these packets in a browser was not tried. The WebCodecs registration for H.264 reads Annex B when no description is given and expects the parameter sets in the keyframes, which this format provides; whether a given browser decodes it was not measured.

## References

- [RFC 6386, VP8 Data Format and Decoding Guide](https://www.rfc-editor.org/info/rfc6386)
  (Informational, November 2011).
  Takes: the frame tag and the key frame header (section 9.1).
- [RFC 7741, RTP Payload Format for VP8 Video](https://www.rfc-editor.org/info/rfc7741)
  (Proposed Standard, March 2016).
  Takes: the 90 kHz timebase (sections 4.1 and 6.1).
  Not implemented: the RTP packetization.
- [ITU-T H.264, Advanced video coding for generic audiovisual services](https://www.itu.int/rec/T-REC-H.264)
  (06/2026, in force).
  Takes: the Annex B byte stream, the level limits of Table A-1 and the SPS fields.
  Not implemented: B slices, interlace, profiles other than 66, 77 and 100.
- [RFC 6381, The 'Codecs' and 'Profiles' Parameters for "Bucket" Media Types](https://www.rfc-editor.org/info/rfc6381)
  (Proposed Standard, August 2011).
  Takes: the `avc3.` codec string (section 3.3).
- [RFC 6184, RTP Payload Format for H.264 Video](https://www.rfc-editor.org/info/rfc6184)
  (Proposed Standard, May 2011).
  Takes: the profile classification (Table 5).
  Not implemented: the RTP packetization.
- ITU-T T.81, Information technology - Digital compression and coding of continuous-tone still images - Requirements and guidelines
  (09/92, in force).
  Takes: the default Huffman tables for MJPEG cameras (Annex K.3).
- [ITU-R BT.601-7, Studio encoding parameters of digital television for standard 4:3 and wide screen 16:9 aspect ratios](https://www.itu.int/rec/R-REC-BT.601)
  (03/2011, in force).
  Takes: the I420 matrix, limited range.
