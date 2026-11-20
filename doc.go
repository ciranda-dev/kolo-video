// Package kolovideo captures camera video and converts, scales and renders
// raw I420 frames for Kolo. Subpackage vpx encodes and decodes VP8 and VP9;
// subpackage h264 encodes and decodes H.264 with the operating system's codec
// where there is one. Nothing needs to be installed and no package uses cgo.
//
// Experimental: the API may change before v1.0.0.
package kolovideo
