package vpx

import (
	"fmt"

	kolovideo "github.com/ciranda-dev/kolo-video"
)

// frameInfo is what the decoder checks before handing a frame to libvpx.
// width and height are 0 when the header does not state a size.
type frameInfo struct {
	key           bool
	width, height int
}

// vp8Info parses the frame tag and key frame header of RFC 6386 section
// 9.1.
func vp8Info(b []byte) (frameInfo, error) {
	if len(b) < 3 {
		return frameInfo{}, fmt.Errorf("%w: VP8 frame of %d bytes", ErrCorrupt, len(b))
	}
	tag := uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16
	info := frameInfo{key: tag&1 == 0}
	first := int(tag >> 5)
	hdr := 3
	if info.key {
		hdr = 10
		if len(b) < hdr || b[3] != 0x9d || b[4] != 0x01 || b[5] != 0x2a {
			return frameInfo{}, fmt.Errorf("%w: VP8 key frame start code", ErrCorrupt)
		}
		info.width = int(b[6]) | int(b[7]&0x3f)<<8
		info.height = int(b[8]) | int(b[9]&0x3f)<<8
		if info.width == 0 || info.height == 0 {
			return frameInfo{}, fmt.Errorf("%w: VP8 size %dx%d", ErrCorrupt, info.width, info.height)
		}
	}
	if first > len(b)-hdr {
		return frameInfo{}, fmt.Errorf("%w: VP8 first partition of %d bytes in %d", ErrCorrupt, first, len(b))
	}
	return info, nil
}

// vp9Frames splits a VP9 packet into its frames using the superframe index
// (VP9 bitstream specification, Annex B). A packet without an index is one
// frame.
func vp9Frames(b []byte) ([][]byte, bool, error) {
	if len(b) == 0 {
		return nil, false, fmt.Errorf("%w: empty VP9 packet", ErrCorrupt)
	}
	marker := b[len(b)-1]
	if marker&0xe0 != 0xc0 {
		return [][]byte{b}, false, nil
	}
	n := int(marker&7) + 1
	mag := int(marker>>3&3) + 1
	size := 2 + mag*n
	// A marker-like last byte without its twin is frame data, as in libvpx.
	if len(b) < size || b[len(b)-size] != marker {
		return [][]byte{b}, false, nil
	}
	idx := b[len(b)-size+1 : len(b)-1]
	data := b[:len(b)-size]
	frames := make([][]byte, 0, n)
	for i := range n {
		var sz uint32
		for j := range mag {
			sz |= uint32(idx[i*mag+j]) << (8 * j)
		}
		if sz == 0 || uint64(sz) > uint64(len(data)) {
			return nil, false, fmt.Errorf("%w: VP9 superframe frame %d of %d bytes", ErrCorrupt, i, sz)
		}
		frames = append(frames, data[:sz])
		data = data[sz:]
	}
	return frames, true, nil
}

// appendSuperframeIndex appends a one-frame superframe index to a VP9
// frame, so libvpx decodes exactly that frame and nothing after it.
func appendSuperframeIndex(frame []byte) []byte {
	mag := 1
	for n := uint64(len(frame)) >> 8; n > 0; n >>= 8 {
		mag++
	}
	marker := byte(0xc0 | (mag-1)<<3)
	out := append(frame, marker)
	for j := range mag {
		out = append(out, byte(len(frame)>>(8*j)))
	}
	return append(out, marker)
}

type bitReader struct {
	b   []byte
	pos int
}

func (r *bitReader) read(n int) (int, error) {
	v := 0
	for range n {
		if r.pos >= 8*len(r.b) {
			return 0, fmt.Errorf("%w: VP9 header truncated", ErrCorrupt)
		}
		v = v<<1 | int(r.b[r.pos/8]>>(7-r.pos%8)&1)
		r.pos++
	}
	return v, nil
}

// vp9Info parses the start of a VP9 uncompressed header (VP9 bitstream
// specification, section 6.2) up to the frame size. Only profile 0 (8-bit
// 4:2:0) is accepted. A frame that takes its size from a reference frame
// reports no size: that reference already passed the check.
func vp9Info(b []byte) (frameInfo, error) {
	r := &bitReader{b: b}
	var info frameInfo
	var err error
	field := func(n int) int {
		if err != nil {
			return 0
		}
		var v int
		v, err = r.read(n)
		return v
	}
	if field(2) != 2 {
		return info, fmt.Errorf("%w: VP9 frame marker", ErrCorrupt)
	}
	profile := field(1)
	profile |= field(1) << 1
	if profile == 3 {
		field(1)
	}
	if err == nil && profile != 0 {
		return info, fmt.Errorf("kolovideo/vpx: VP9 profile %d, only profile 0 (8-bit 4:2:0) is supported: %w", profile, kolovideo.ErrUnsupportedFormat)
	}
	if field(1) == 1 { // show_existing_frame
		field(3)
		return info, err
	}
	frameType := field(1)
	showFrame := field(1)
	errorResilient := field(1)
	info.key = frameType == 0
	sizeFollows := info.key
	if info.key {
		if !syncCode(field) || !colorConfig(field) {
			return info, firstErr(err, fmt.Errorf("%w: VP9 key frame header", ErrCorrupt))
		}
	} else {
		intraOnly := 0
		if showFrame == 0 {
			intraOnly = field(1)
		}
		if errorResilient == 0 {
			field(2) // reset_frame_context
		}
		if intraOnly == 1 {
			// Profile 0 intra-only frames carry no color config.
			if !syncCode(field) {
				return info, firstErr(err, fmt.Errorf("%w: VP9 intra-only header", ErrCorrupt))
			}
			field(8) // refresh_frame_flags
			sizeFollows = true
		} else {
			field(8)
			for range 3 {
				field(4) // ref_frame_idx and sign bias
			}
			found := 0
			for range 3 {
				if found = field(1); found == 1 {
					break
				}
			}
			sizeFollows = found == 0
		}
	}
	if sizeFollows {
		info.width = field(16) + 1
		info.height = field(16) + 1
	}
	if err != nil {
		return frameInfo{}, err
	}
	return info, nil
}

func syncCode(field func(int) int) bool {
	return field(8) == 0x49 && field(8) == 0x83 && field(8) == 0x42
}

// colorConfig reads a profile 0 color_config; RGB is not allowed there.
func colorConfig(field func(int) int) bool {
	if field(3) == 7 {
		return false
	}
	field(1) // color_range
	return true
}

func firstErr(a, b error) error {
	if a != nil {
		return a
	}
	return b
}
