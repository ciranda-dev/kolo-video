// Package portable runs libvpx, translated to Go from WebAssembly, with no
// native code. Each encoder or decoder owns one module instance with at most
// 1 GiB of linear memory (the link-time maximum of the module).
//
// The module is built without threads, so the encoder is single-threaded and
// its output equals the native engine's only when the native encoder also runs
// one thread. VP9 uses Threads for its tile columns, so a larger value changes
// the stream on both engines.
//
// An instance is not safe for concurrent use. A panic of the translated code
// (a libvpx error, which the build turns into a trap, or a bounds violation)
// is recovered at every call: a decoder that trapped is replaced on its next
// call and the trapping call returns ErrCorrupt; an encoder returns ErrCodec
// from then on.
package portable

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sync"
)

// Errors of the engine. Match them with errors.Is.
var (
	// ErrCorrupt means a packet is not decodable.
	ErrCorrupt = errors.New("kolovideo/vpx: corrupt packet")
	// ErrCodec means libvpx failed for a reason other than bad input.
	ErrCodec = errors.New("kolovideo/vpx: codec error")
	// ErrInvalidParam means libvpx refused a parameter.
	ErrInvalidParam = errors.New("kolovideo/vpx: invalid parameter")
	// ErrClosed means the encoder or decoder was closed.
	ErrClosed = errors.New("kolovideo/vpx: closed")
)

// vpx_codec_err_t values.
const (
	codecMem           = 2
	codecIncapable     = 4
	codecUnsupBitsream = 5
	codecCorruptFrame  = 7
	codecInvalidParam  = 8
)

var codecText = [...]string{
	"ok",
	"unspecified internal error",
	"out of memory",
	"ABI version mismatch",
	"codec does not implement the requested capability",
	"bitstream not supported by this decoder",
	"bitstream requires a feature this decoder does not support",
	"corrupt frame detected",
	"invalid parameter",
	"end of iterated list",
}

// CodecError is a vpx_codec_err_t returned by libvpx.
type CodecError struct {
	Op   string
	Code int32
}

// Error implements error.
func (e *CodecError) Error() string {
	text := "unknown error"
	if e.Code >= 0 && int(e.Code) < len(codecText) {
		text = codecText[e.Code]
	}
	return fmt.Sprintf("kolovideo/vpx: %s: %s (libvpx error %d)", e.Op, text, e.Code)
}

// Is matches ErrCorrupt for a corrupt frame or an unsupported bitstream,
// ErrInvalidParam for an invalid parameter or an unsupported capability, and
// ErrCodec for every other code.
func (e *CodecError) Is(target error) bool {
	corrupt := e.Code == codecCorruptFrame || e.Code == codecUnsupBitsream
	invalid := e.Code == codecInvalidParam || e.Code == codecIncapable
	switch target {
	case ErrCorrupt:
		return corrupt
	case ErrInvalidParam:
		return invalid
	case ErrCodec:
		return !corrupt && !invalid
	}
	return false
}

// TrapError is a panic of the translated code. It unwraps to ErrCorrupt for a
// decoder and to ErrCodec for an encoder.
type TrapError struct {
	Value any
	kind  error
}

// Error implements error.
func (e *TrapError) Error() string {
	return fmt.Sprintf("%v: portable engine trapped: %v", e.kind, e.Value)
}

// Unwrap returns ErrCorrupt or ErrCodec.
func (e *TrapError) Unwrap() error { return e.kind }

func errMemory(op string) error { return &CodecError{Op: op, Code: codecMem} }

// newModule returns an initialized module.
func newModule() (m *Module, err error) {
	defer func() {
		if r := recover(); r != nil {
			m, err = nil, &TrapError{Value: r, kind: ErrCodec}
		}
	}()
	m = New(&host{})
	m.X_initialize()
	return m, nil
}

func word(mem []byte, p int32) uint32 { return binary.LittleEndian.Uint32(mem[uint32(p):]) }

// malloc returns n bytes of module memory, or 0 when the module is at its
// memory limit.
func malloc(m *Module, n int64) int32 {
	if n < 1 || n > math.MaxInt32 {
		return 0
	}
	return m.Xkv_malloc(int32(n))
}

var version = sync.OnceValue(func() string {
	m, err := newModule()
	if err != nil {
		return ""
	}
	var s string
	func() {
		defer func() { recover() }()
		mem := *m.Slice()
		p := uint32(m.Xkv_version())
		e := p
		for mem[e] != 0 {
			e++
		}
		s = string(mem[p:e])
	}()
	return s
})

// Version returns the libvpx version string, such as "v1.16.0", or "" if the
// module cannot start.
func Version() string { return version() }

// EncParams configures an encoder. Zero Threads means 1.
type EncParams struct {
	VP9           bool
	Width, Height int
	Threads       int
	Speed         int
	Kbps          uint32
	MaxIntraPct   uint32
}

// Encoder is one libvpx encoder.
type Encoder struct {
	p        EncParams
	m        *Module
	failed   error
	closed   bool
	enc, out int32
	y, u, v  int32
	cw, ch   int
}

// NewEncoder creates an encoder.
func NewEncoder(p EncParams) (e *Encoder, err error) {
	if p.Width < 1 || p.Height < 1 || p.Width > math.MaxInt32 || p.Height > math.MaxInt32 {
		return nil, &CodecError{Op: "create encoder", Code: codecInvalidParam}
	}
	p.Threads = max(p.Threads, 1)
	e = &Encoder{p: p, cw: (p.Width + 1) / 2, ch: (p.Height + 1) / 2}
	m, err := newModule()
	if err != nil {
		return nil, err
	}
	e.m = m
	if err := e.build(); err != nil {
		return nil, err
	}
	return e, nil
}

func (e *Encoder) build() (err error) {
	defer func() {
		if r := recover(); r != nil {
			e.m, err = nil, &TrapError{Value: r, kind: ErrCodec}
		}
	}()
	p, m := e.p, e.m
	vp9 := int32(0)
	if p.VP9 {
		vp9 = 1
	}
	if e.out = malloc(m, 16); e.out == 0 {
		return errMemory("create encoder")
	}
	e.enc = m.Xkv_enc_new(vp9, int32(p.Width), int32(p.Height), int32(p.Kbps), int32(p.Threads), int32(p.Speed), int32(p.MaxIntraPct), e.out)
	if e.enc == 0 {
		return &CodecError{Op: "create encoder", Code: int32(word(*m.Slice(), e.out))}
	}
	e.y = malloc(m, int64(p.Width)*int64(p.Height))
	e.u = malloc(m, int64(e.cw)*int64(e.ch))
	e.v = malloc(m, int64(e.cw)*int64(e.ch))
	if e.y == 0 || e.u == 0 || e.v == 0 {
		return errMemory("create encoder")
	}
	return nil
}

// guard turns a panic of the translated code into a sticky ErrCodec.
func (e *Encoder) guard(err *error) {
	if r := recover(); r != nil {
		e.m = nil
		e.failed = &TrapError{Value: r, kind: ErrCodec}
		*err = e.failed
	}
}

func (e *Encoder) usable() error {
	switch {
	case e.closed:
		return ErrClosed
	case e.failed != nil:
		return e.failed
	}
	return nil
}

// Encode compresses one picture and calls emit for each packet. The planes
// are read during the call only. The data given to emit is valid until emit
// returns. A panic in emit is not caught.
func (e *Encoder) Encode(y, u, v []byte, yStride, uvStride int, pts int64, dur uint32, key bool, emit func(data []byte, key bool)) error {
	if err := e.usable(); err != nil {
		return err
	}
	w, h := e.p.Width, e.p.Height
	if !fits(y, yStride, w, h) || !fits(u, uvStride, e.cw, e.ch) || !fits(v, uvStride, e.cw, e.ch) {
		return &CodecError{Op: "encode", Code: codecInvalidParam}
	}
	if err := e.submit(y, u, v, yStride, uvStride, pts, dur, key); err != nil {
		return err
	}
	for {
		data, k, ok, err := e.next()
		if err != nil || !ok {
			return err
		}
		emit(data, k)
	}
}

// fits reports whether a plane of w by h samples fits in p with this stride.
func fits(p []byte, stride, w, h int) bool {
	return w > 0 && h > 0 && stride >= w && len(p) >= w && (len(p)-w)/stride >= h-1
}

func (e *Encoder) submit(y, u, v []byte, yStride, uvStride int, pts int64, dur uint32, key bool) (err error) {
	defer e.guard(&err)
	w, h := e.p.Width, e.p.Height
	mem := *e.m.Slice()
	copyPlane(mem[e.y:], y, yStride, w, h)
	copyPlane(mem[e.u:], u, uvStride, e.cw, e.ch)
	copyPlane(mem[e.v:], v, uvStride, e.cw, e.ch)
	k := int32(0)
	if key {
		k = 1
	}
	if r := e.m.Xkv_encode(e.enc, e.y, int32(w), e.u, e.v, int32(e.cw), pts, int32(dur), k); r != 0 {
		return &CodecError{Op: "encode", Code: r}
	}
	return nil
}

func copyPlane(dst, src []byte, stride, w, h int) {
	for r := range h {
		copy(dst[r*w:r*w+w], src[r*stride:r*stride+w])
	}
}

// next returns the next packet of the last Encode. The data points into module
// memory.
func (e *Encoder) next() (data []byte, key, ok bool, err error) {
	defer e.guard(&err)
	if e.m.Xkv_next_frame(e.enc, e.out) == 0 {
		return nil, false, false, nil
	}
	mem := *e.m.Slice()
	p, n := word(mem, e.out), word(mem, e.out+4)
	return mem[p : p+n : p+n], word(mem, e.out+8) != 0, true, nil
}

// SetBitrate changes the target bitrate in kbit/s.
func (e *Encoder) SetBitrate(kbps uint32) (err error) {
	if err := e.usable(); err != nil {
		return err
	}
	defer e.guard(&err)
	if r := e.m.Xkv_set_bitrate(e.enc, int32(kbps)); r != 0 {
		return &CodecError{Op: "set bitrate", Code: r}
	}
	return nil
}

// Close frees the encoder. It is idempotent.
func (e *Encoder) Close() {
	if e.closed {
		return
	}
	e.closed = true
	if e.m != nil {
		func() {
			defer func() { recover() }()
			e.m.Xkv_enc_free(e.enc)
		}()
		e.m = nil
	}
}

// Image is a decoded I420 picture. The planes point into the decoder's memory
// and are valid until its next call.
type Image struct {
	Width, Height     int
	Y, U, V           []byte
	YStride, UVStride int
}

// Decoder is one libvpx decoder.
type Decoder struct {
	vp9     bool
	threads int
	m       *Module
	closed  bool
	corrupt bool
	dec     int32
	out     int32
	buf     int32
	bufCap  int
}

// NewDecoder creates a decoder. Zero threads means 1.
func NewDecoder(vp9 bool, threads int) (*Decoder, error) {
	d := &Decoder{vp9: vp9, threads: max(threads, 1)}
	if err := d.build(); err != nil {
		return nil, err
	}
	return d, nil
}

// build creates a fresh module with the decoder's configuration.
func (d *Decoder) build() (err error) {
	m, err := newModule()
	if err != nil {
		return err
	}
	defer func() {
		if r := recover(); r != nil {
			d.m, err = nil, &TrapError{Value: r, kind: ErrCodec}
		}
	}()
	vp9 := int32(0)
	if d.vp9 {
		vp9 = 1
	}
	d.buf, d.bufCap = 0, 0
	if d.out = malloc(m, 64); d.out == 0 {
		return errMemory("create decoder")
	}
	if d.dec = m.Xkv_dec_new(vp9, int32(d.threads), d.out); d.dec == 0 {
		return &CodecError{Op: "create decoder", Code: int32(word(*m.Slice(), d.out))}
	}
	d.m = m
	return nil
}

// guard turns a panic of the translated code into ErrCorrupt and drops the
// module; the next call builds a new one.
func (d *Decoder) guard(err *error) {
	if r := recover(); r != nil {
		d.m, d.corrupt = nil, false
		*err = &TrapError{Value: r, kind: ErrCorrupt}
	}
}

// Decode decodes one packet. It returns ok=false when no frame is shown.
func (d *Decoder) Decode(data []byte) (img Image, ok bool, err error) {
	if d.closed {
		return Image{}, false, ErrClosed
	}
	if d.m == nil {
		if err := d.build(); err != nil {
			return Image{}, false, err
		}
	}
	defer d.guard(&err)
	return d.decode(data)
}

func (d *Decoder) decode(data []byte) (Image, bool, error) {
	d.corrupt = false
	if len(data) == 0 {
		return Image{}, false, &CodecError{Op: "decode", Code: codecInvalidParam}
	}
	if err := d.reserve(len(data)); err != nil {
		return Image{}, false, err
	}
	copy((*d.m.Slice())[d.buf:], data)
	if r := d.m.Xkv_decode(d.dec, d.buf, int32(len(data)), d.out); r != 0 {
		return Image{}, false, &CodecError{Op: "decode", Code: r}
	}
	mem := *d.m.Slice()
	var o [8]uint32
	for i := range o {
		o[i] = word(mem, d.out+int32(4*i))
	}
	d.corrupt = o[7] != 0
	if o[0] == 0 {
		return Image{}, false, nil
	}
	w, h := int(o[0]), int(o[1])
	cw, ch := (w+1)/2, (h+1)/2
	ys, cs := int(o[5]), int(o[6])
	if w < 1 || h < 1 || ys < w || cs < cw {
		return Image{}, false, &CodecError{Op: "decode", Code: 1}
	}
	yl, cl := (h-1)*ys+w, (ch-1)*cs+cw
	return Image{
		Width: w, Height: h, YStride: ys, UVStride: cs,
		Y: mem[o[2] : o[2]+uint32(yl) : o[2]+uint32(yl)],
		U: mem[o[3] : o[3]+uint32(cl) : o[3]+uint32(cl)],
		V: mem[o[4] : o[4]+uint32(cl) : o[4]+uint32(cl)],
	}, true, nil
}

// reserve makes the packet buffer hold n bytes. It grows by doubling and frees
// the old buffer first, so the module memory is reused.
func (d *Decoder) reserve(n int) error {
	if n <= d.bufCap {
		return nil
	}
	size := max(n, 2*d.bufCap, 1<<16)
	if d.buf != 0 {
		d.m.Xkv_free(d.buf)
		d.buf, d.bufCap = 0, 0
	}
	p := malloc(d.m, int64(size))
	if p == 0 && size > n {
		size = n
		p = malloc(d.m, int64(size))
	}
	if p == 0 {
		return errMemory("decode")
	}
	d.buf, d.bufCap = p, size
	return nil
}

// Corrupted reports whether the last decoded frame was flagged as corrupt.
func (d *Decoder) Corrupted() bool { return d.corrupt }

// Close frees the decoder. It is idempotent.
func (d *Decoder) Close() {
	if d.closed {
		return
	}
	d.closed = true
	if d.m != nil {
		func() {
			defer func() { recover() }()
			d.m.Xkv_dec_free(d.dec)
		}()
		d.m = nil
	}
}
