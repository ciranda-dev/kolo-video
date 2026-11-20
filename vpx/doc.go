// Package vpx encodes and decodes VP8 and VP9 video with libvpx 1.16.0, set
// up for real-time use. It needs no installed library, compiler or cgo.
//
// Two engines run the same libvpx. The native engine is a prebuilt shared
// library embedded in the package and loaded without cgo; it is the default on
// macOS and Windows, and on Linux only with the build tag kolovideo_native, so
// that binaries stay static and start on any distribution. The portable engine
// is the same code translated to Go; it works everywhere, runs single-threaded
// and encodes in real time up to 640x480. When the native engine cannot load,
// the portable engine is used and LoadError says why. Streams from the two
// engines are byte-identical only when the encoder runs one thread.
//
// Experimental: the API may change before v1.0.0.
package vpx
