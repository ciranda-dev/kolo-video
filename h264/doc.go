// Package h264 encodes and decodes H.264 video with the codec of the
// operating system: VideoToolbox on macOS, the Media Foundation H.264
// transforms on Windows desktop editions. It contains no H.264 encoder or
// decoder and needs no installed library, compiler or cgo. Where the system
// has no usable codec, Capabilities and the constructors report why with an
// *UnavailableError.
//
// Packets are Annex B access units with 4-byte start codes, one picture each,
// holding only slices, SPS and PPS; every keyframe starts with its SPS and
// PPS. Encoders produce Constrained Baseline, Main or High within level 5.1,
// without B-frames, one packet per frame. Decoders check every packet in Go
// before the system sees it and refuse streams outside those profiles and
// levels.
//
// Experimental: the API may change before v1.0.0.
package h264
