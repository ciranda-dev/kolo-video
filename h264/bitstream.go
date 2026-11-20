package h264

import (
	"bytes"
	"errors"
	"fmt"
)

// NAL unit types (ITU-T H.264 Table 7-1).
const (
	nalSlice = 1
	nalIDR   = 5
	nalSEI   = 6
	nalSPS   = 7
	nalPPS   = 8
	nalAUD   = 9
)

var startCode = []byte{0, 0, 0, 1}

// maxNALs bounds the NAL units of one packet: one slice per macroblock at
// most, plus 32 SPS and 256 PPS ids.
func maxNALs(maxPixels int) int { return maxPixels/256 + 288 }

// nalType returns the nal_unit_type, or -1 for an empty unit.
func nalType(nal []byte) int {
	if len(nal) == 0 {
		return -1
	}
	return int(nal[0] & 0x1f)
}

// splitAnnexB splits a byte stream (ITU-T H.264 Annex B) that starts with a
// start code into NAL units without start codes. Zero bytes before a start
// code are trailing_zero_8bits and are dropped.
func splitAnnexB(b []byte, maxNALs int) ([][]byte, error) {
	var at int
	switch {
	case len(b) >= 3 && b[0] == 0 && b[1] == 0 && b[2] == 1:
		at = 3
	case len(b) >= 4 && b[0] == 0 && b[1] == 0 && b[2] == 0 && b[3] == 1:
		at = 4
	default:
		return nil, fmt.Errorf("%w: no start code at the packet start", ErrCorrupt)
	}
	var nals [][]byte
	for {
		next := bytes.Index(b[at:], startCode[1:])
		end := len(b)
		if next >= 0 {
			end = at + next
		}
		nal := b[at:end]
		for len(nal) > 0 && nal[len(nal)-1] == 0 {
			nal = nal[:len(nal)-1]
		}
		if len(nal) == 0 {
			return nil, fmt.Errorf("%w: empty NAL unit", ErrCorrupt)
		}
		if len(nals) == maxNALs {
			return nil, fmt.Errorf("%w: more than %d NAL units", ErrTooLarge, maxNALs)
		}
		nals = append(nals, nal[:len(nal):len(nal)])
		if next < 0 {
			return nals, nil
		}
		at = end + 3
	}
}

// splitAVCC splits length-prefixed NAL units, as the system encoder on macOS
// gives them. Bad lengths are a system codec failure.
func splitAVCC(b []byte, lenSize, maxNALs int) ([][]byte, error) {
	if lenSize != 1 && lenSize != 2 && lenSize != 4 {
		return nil, fmt.Errorf("%w: NAL length size %d", ErrCodec, lenSize)
	}
	var nals [][]byte
	for len(b) > 0 {
		if len(b) < lenSize {
			return nil, fmt.Errorf("%w: truncated NAL length", ErrCodec)
		}
		var n uint64
		for _, c := range b[:lenSize] {
			n = n<<8 | uint64(c)
		}
		b = b[lenSize:]
		if n == 0 || n > uint64(len(b)) {
			return nil, fmt.Errorf("%w: NAL length %d with %d bytes left", ErrCodec, n, len(b))
		}
		if len(nals) == maxNALs {
			return nil, fmt.Errorf("%w: more than %d NAL units", ErrCodec, maxNALs)
		}
		nals = append(nals, b[:n:n])
		b = b[n:]
	}
	return nals, nil
}

// appendAnnexB appends every unit of each group with a 4-byte start code.
func appendAnnexB(dst []byte, groups ...[][]byte) []byte {
	for _, g := range groups {
		for _, nal := range g {
			dst = append(dst, startCode...)
			dst = append(dst, nal...)
		}
	}
	return dst
}

// appendAVCC appends each unit with a 4-byte big-endian length.
func appendAVCC(dst []byte, units [][]byte) []byte {
	for _, nal := range units {
		n := uint32(len(nal))
		dst = append(dst, byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
		dst = append(dst, nal...)
	}
	return dst
}

// unescapeRBSP appends the payload of src with emulation_prevention_three_byte
// removed (ITU-T H.264 7.4.1).
func unescapeRBSP(dst, src []byte) []byte {
	zeros := 0
	for _, c := range src {
		if zeros >= 2 && c == 3 {
			zeros = 0
			continue
		}
		dst = append(dst, c)
		if c == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	return dst
}

// escapeRBSP appends rbsp with emulation_prevention_three_byte inserted.
func escapeRBSP(dst, rbsp []byte) []byte {
	zeros := 0
	for _, c := range rbsp {
		if zeros >= 2 && c <= 3 {
			dst = append(dst, 3)
			zeros = 0
		}
		dst = append(dst, c)
		if c == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	if zeros >= 2 {
		dst = append(dst, 3)
	}
	return dst
}

var errTruncated = errors.New("truncated")

// bitReader reads an RBSP. The first failure sticks: later reads return 0
// and err stays set.
type bitReader struct {
	b   []byte
	pos int // in bits
	err error
}

func (r *bitReader) fail(err error) {
	if r.err == nil {
		r.err = err
	}
}

func (r *bitReader) left() int { return len(r.b)*8 - r.pos }

// u reads n bits, 0 to 32.
func (r *bitReader) u(n int) uint32 {
	if r.err != nil {
		return 0
	}
	if n > r.left() {
		r.fail(errTruncated)
		return 0
	}
	var v uint32
	for range n {
		v = v<<1 | uint32(r.b[r.pos>>3]>>(7-r.pos&7)&1)
		r.pos++
	}
	return v
}

func (r *bitReader) flag() bool { return r.u(1) == 1 }

// ue reads an Exp-Golomb code (ITU-T H.264 9.1) with at most 31 leading
// zero bits, so the value fits in 32 bits.
func (r *bitReader) ue() uint32 {
	zeros := 0
	for r.err == nil && r.u(1) == 0 {
		zeros++
		if zeros > 31 {
			r.fail(errors.New("Exp-Golomb code longer than 32 bits"))
		}
	}
	if r.err != nil {
		return 0
	}
	return uint32(1)<<zeros - 1 + r.u(zeros)
}

// se reads a signed Exp-Golomb code (ITU-T H.264 9.1.1).
func (r *bitReader) se() int32 {
	k := int64(r.ue())
	if k&1 == 1 {
		return int32((k + 1) / 2)
	}
	return int32(-k / 2)
}

// ueMax reads an ue and fails when it is above max.
func (r *bitReader) ueMax(max uint32, name string) uint32 {
	v := r.ue()
	if v > max {
		r.fail(fmt.Errorf("%s %d above %d", name, v, max))
		return 0
	}
	return v
}

// seRange reads an se and fails when it is outside lo to hi.
func (r *bitReader) seRange(lo, hi int32, name string) int32 {
	v := r.se()
	if v < lo || v > hi {
		r.fail(fmt.Errorf("%s %d outside %d to %d", name, v, lo, hi))
		return 0
	}
	return v
}

// lastOne returns the bit index of the last 1 bit, the rbsp_stop_one_bit,
// or -1.
func (r *bitReader) lastOne() int {
	for i := len(r.b) - 1; i >= 0; i-- {
		if c := r.b[i]; c != 0 {
			n := 7
			for c&1 == 0 {
				c >>= 1
				n--
			}
			return i*8 + n
		}
	}
	return -1
}

// moreData is more_rbsp_data (ITU-T H.264 7.2).
func (r *bitReader) moreData() bool { return r.err == nil && r.pos < r.lastOne() }

// trailing checks rbsp_trailing_bits and that the RBSP ends with their byte:
// only slice data may carry zero bytes after them (ITU-T H.264 7.3.2.10).
func (r *bitReader) trailing() {
	if last := r.lastOne(); r.err == nil && (r.pos != last || last/8 != len(r.b)-1) {
		r.fail(errors.New("bad rbsp_trailing_bits"))
	}
}

// bitWriter builds an RBSP.
type bitWriter struct {
	b []byte
	n int // bits used in the last byte, 0 when it is full or absent
}

func (w *bitWriter) u(v uint32, n int) {
	for i := n - 1; i >= 0; i-- {
		if w.n == 0 {
			w.b = append(w.b, 0)
		}
		w.b[len(w.b)-1] |= byte(v>>i&1) << (7 - w.n)
		w.n = (w.n + 1) & 7
	}
}

func (w *bitWriter) flag(f bool) {
	if f {
		w.u(1, 1)
	} else {
		w.u(0, 1)
	}
}

func (w *bitWriter) ue(v uint32) {
	x := uint64(v) + 1
	n := 0
	for x>>n > 1 {
		n++
	}
	w.u(0, n)
	w.u(1, 1)
	w.u(uint32(x), n)
}

func (w *bitWriter) se(v int32) {
	if v > 0 {
		w.ue(uint32(2*int64(v) - 1))
	} else {
		w.ue(uint32(-2 * int64(v)))
	}
}

// copyBits appends the first n bits of b.
func (w *bitWriter) copyBits(b []byte, n int) {
	for i := 0; i < n; i++ {
		w.u(uint32(b[i>>3]>>(7-i&7)&1), 1)
	}
}

// trailing writes rbsp_trailing_bits.
func (w *bitWriter) trailing() {
	w.u(1, 1)
	for w.n != 0 {
		w.u(0, 1)
	}
}
