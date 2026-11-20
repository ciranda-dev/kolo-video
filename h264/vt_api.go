package h264

import (
	"bytes"
	"fmt"
	"math"
	"runtime"
	"sync"
	"sync/atomic"

	kolovideo "github.com/ciranda-dev/kolo-video"
)

// vtOS is the VideoToolbox, CoreMedia, CoreVideo and CoreFoundation surface
// the engine uses. Handles are opaque CF object addresses and st is an
// OSStatus. vt_darwin.go implements it with purego; the tests use a fake, so
// the logic below runs on every host.
type vtOS interface {
	release(obj uintptr)

	encCreate(w, h int, hardware bool, ref uintptr) (sess uintptr, st int32)
	encSetBool(sess uintptr, p vtProp, v bool) int32
	encSetInt(sess uintptr, p vtProp, v int32) int32
	encSetFloat(sess uintptr, p vtProp, v float64) int32
	encSetProfile(sess uintptr, l vtLevel) int32
	encPrepare(sess uintptr) int32
	encHardware(sess uintptr) bool
	// encPixelBuffer returns a buffer the caller owns and releases once.
	encPixelBuffer(sess uintptr, pool bool, w, h int) (pb uintptr, st int32)
	encFrame(sess, pb uintptr, pts, dur int64, force bool, counter uintptr) (flags uint32, st int32)
	// encComplete flushes frames up to pts, or all of them when all is set.
	encComplete(sess uintptr, pts int64, all bool) int32
	encInvalidate(sess uintptr)

	pbInfo(pb uintptr) (format uint32, planes, w, h int)
	pbLock(pb uintptr, readOnly bool) int32
	pbUnlock(pb uintptr, readOnly bool) int32
	// pbPlane returns a locked plane: stride times rows bytes.
	pbPlane(pb uintptr, plane int) (data []byte, stride int)

	sbSync(sb uintptr) bool
	sbLen(sb uintptr) int
	sbCopy(sb uintptr, dst []byte) int32
	// sbParams copies the parameter sets of the sample's format description.
	sbParams(sb uintptr) (params [][]byte, lengthSize int, st int32)

	fdCreate(params [][]byte) (fd uintptr, st int32)
	decCreate(fd, ref uintptr, hardware bool) (sess uintptr, st int32)
	decCanAccept(sess, fd uintptr) bool
	decHardware(sess uintptr) bool
	blockBuffer(data []byte) (bb uintptr, st int32)
	sampleBuffer(bb, fd uintptr, size int, pts int64) (sb uintptr, st int32)
	decFrame(sess, sb uintptr, counter uintptr) (flags uint32, st int32)
	decWait(sess uintptr) int32
	decInvalidate(sess uintptr)

	encoders() (avc, hardware bool, st int32)
	hardwareDecode() bool
}

// vtNoHardware makes new sessions refuse hardware codecs; tests use it to
// reach the software path.
var vtNoHardware atomic.Bool

// vtUsePool takes the encoder's input buffers from the session's pool; only
// tests change it.
var vtUsePool = true

// The built-in 48x48 Constrained Baseline parameter sets of the decoder
// probe: a VideoToolbox stream with the SPS normalized as Encode writes it.
var (
	vtProbeSPS = []byte{0x27, 0x42, 0xc0, 0x0b, 0xab, 0x41, 0x86, 0xf0, 0x24, 0xc3, 0x33, 0x50, 0x60, 0x10, 0x60, 0x6d, 0x04, 0x42, 0x35}
	vtProbePPS = []byte{0x28, 0xce, 0x3c, 0x80}
)

func vtError(op string, st int32) error {
	kind := ErrCodec
	switch st {
	case vtErrBadData:
		kind = ErrCorrupt
	case vtErrCouldNotFindDec, vtErrCouldNotFindEnc, vtErrDecoderNotNow, vtErrEncoderNotNow:
		return &UnavailableError{GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, Engine: "VideoToolbox", Reason: op + " failed", Code: int64(st)}
	}
	return &codecError{engine: "VideoToolbox", op: op, code: int64(st), kind: kind}
}

func vtFault(op string, kind error) error {
	return &codecError{engine: "VideoToolbox", op: op, kind: kind}
}

// The two output callbacks are one function pair for the whole process and
// find their session by an integer handle, never a Go pointer.
var vtReg struct {
	mu   sync.Mutex
	next uintptr
	m    map[uintptr]*vtSlot
}

func vtRegister(s *vtSlot) uintptr {
	vtReg.mu.Lock()
	defer vtReg.mu.Unlock()
	if vtReg.m == nil {
		vtReg.m = map[uintptr]*vtSlot{}
	}
	vtReg.next++
	vtReg.m[vtReg.next] = s
	return vtReg.next
}

func vtUnregister(h uintptr) {
	vtReg.mu.Lock()
	delete(vtReg.m, h)
	vtReg.mu.Unlock()
}

func vtLookup(h uintptr) *vtSlot {
	vtReg.mu.Lock()
	defer vtReg.mu.Unlock()
	return vtReg.m[h]
}

func vtLive() int {
	vtReg.mu.Lock()
	defer vtReg.mu.Unlock()
	return len(vtReg.m)
}

// vtSlot carries one session's output from its callback to the call that
// waits for it. The callback takes only the registry lock and this one,
// never the instance lock that Encode holds while it waits.
type vtSlot struct {
	mu       sync.Mutex
	os       vtOS
	maxBytes int

	pending uintptr
	done    bool
	stale   uint64
	status  int32
	err     error
	dropped bool

	key     bool
	avcc    []byte
	params  [][]byte
	lenSize int

	wantW, wantH      int
	w, h              int
	y, uv             []byte
	yStride, uvStride int
}

func (s *vtSlot) begin(counter uintptr, w, h int) {
	s.mu.Lock()
	s.pending, s.done, s.status, s.err, s.dropped, s.key = counter, false, 0, nil, false, false
	s.wantW, s.wantH = w, h
	s.mu.Unlock()
}

// Staleness is counted, not an error: a callback for a frame that is no
// longer pending must not touch the buffers of the current one.
func (s *vtSlot) staleCount() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stale
}

func (s *vtSlot) guard() {
	if r := recover(); r != nil {
		s.done = true
		s.err = vtFault(fmt.Sprint("callback panic: ", r), ErrCodec)
	}
}

// vtEncoded is the VTCompressionOutputCallback.
func vtEncoded(ref, frameRef uintptr, status int32, flags uint32, sample uintptr) {
	s := vtLookup(ref)
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.guard()
	if frameRef != s.pending || s.done {
		s.stale++
		return
	}
	s.done = true
	switch {
	case status != 0:
		s.status = status
	case sample == 0 || flags&vtInfoFrameDropped != 0:
		s.dropped = true
	default:
		s.encoded(sample)
	}
}

func (s *vtSlot) encoded(sb uintptr) {
	n := s.os.sbLen(sb)
	if n <= 0 || n > s.maxBytes {
		s.err = vtFault("encoded sample size", ErrCodec)
		return
	}
	if cap(s.avcc) < n {
		s.avcc = make([]byte, n)
	}
	s.avcc = s.avcc[:n]
	if st := s.os.sbCopy(sb, s.avcc); st != 0 {
		s.err = vtError("copy encoded sample", st)
		return
	}
	s.key = s.os.sbSync(sb)
	if !s.key {
		return
	}
	params, ls, st := s.os.sbParams(sb)
	if st != 0 {
		s.err = vtError("read parameter sets", st)
		return
	}
	if !vtHasParamSets(params) {
		s.err = vtFault("parameter sets missing from keyframe", ErrCodec)
		return
	}
	s.params, s.lenSize = params, ls
}

func vtHasParamSets(params [][]byte) bool {
	var sps, pps bool
	for _, p := range params {
		if len(p) == 0 {
			return false
		}
		switch p[0] & 0x1f {
		case 7:
			sps = true
		case 8:
			pps = true
		}
	}
	return sps && pps
}

// vtDecoded is the VTDecompressionOutputCallback. It copies the picture out
// of the borrowed image buffer into the slot's own NV12 planes.
func vtDecoded(ref, frameRef uintptr, status int32, flags uint32, image uintptr, pts, dur cmTime) {
	s := vtLookup(ref)
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.guard()
	if frameRef != s.pending || s.done {
		s.stale++
		return
	}
	s.done = true
	switch {
	case status != 0:
		s.status = status
	case image == 0 || flags&vtInfoFrameDropped != 0:
		s.dropped = true
	default:
		s.picture(image)
	}
}

func (s *vtSlot) picture(img uintptr) {
	format, planes, w, h := s.os.pbInfo(img)
	if format != vtPixelFormat420v || planes != 2 || w != s.wantW || h != s.wantH || w <= 0 || h <= 0 {
		s.err = vtFault("decoded picture format", ErrCodec)
		return
	}
	if st := s.os.pbLock(img, true); st != 0 {
		s.err = vtError("lock decoded picture", st)
		return
	}
	defer s.os.pbUnlock(img, true)
	cw, ch := (w+1)&^1, (h+1)/2
	y, ys := s.os.pbPlane(img, 0)
	uv, uvs := s.os.pbPlane(img, 1)
	if !vtPlaneFits(len(y), ys, w, h) || !vtPlaneFits(len(uv), uvs, cw, ch) {
		s.err = vtFault("decoded plane layout", ErrCodec)
		return
	}
	s.y = vtCopyPlane(s.y, y, ys, w, h)
	s.uv = vtCopyPlane(s.uv, uv, uvs, cw, ch)
	s.w, s.h, s.yStride, s.uvStride = w, h, w, cw
}

// vtPlaneFits reports whether n bytes hold rows rows of width bytes at stride.
func vtPlaneFits(n, stride, width, rows int) bool {
	return width > 0 && rows > 0 && stride >= width && n >= stride*(rows-1)+width
}

// vtCopyPlane copies rows of width bytes into dst with a stride of width.
func vtCopyPlane(dst, src []byte, stride, width, rows int) []byte {
	if cap(dst) < width*rows {
		dst = make([]byte, width*rows)
	}
	dst = dst[:width*rows]
	for r := 0; r < rows; r++ {
		copy(dst[r*width:(r+1)*width], src[r*stride:r*stride+width])
	}
	return dst
}

// vtAVCCSplit splits a length-prefixed sample into NAL units.
func vtAVCCSplit(b []byte, lenSize int, dst [][]byte) ([][]byte, error) {
	if lenSize != 1 && lenSize != 2 && lenSize != 4 {
		return dst, vtFault("NAL length size", ErrCodec)
	}
	for i := 0; i < len(b); {
		if len(b)-i < lenSize {
			return dst, vtFault("AVCC length prefix", ErrCodec)
		}
		var n uint64
		for _, c := range b[i : i+lenSize] {
			n = n<<8 | uint64(c)
		}
		i += lenSize
		if n == 0 || n > uint64(len(b)-i) {
			return dst, vtFault("AVCC NAL length", ErrCodec)
		}
		dst = append(dst, b[i:i+int(n)])
		i += int(n)
	}
	if len(dst) == 0 {
		return dst, vtFault("empty sample", ErrCodec)
	}
	return dst, nil
}

type vtEncoder struct {
	os      vtOS
	p       encParams
	slot    *vtSlot
	ref     uintptr
	sess    uintptr
	counter uintptr
	hw      bool
	nals    [][]byte
	held    [][]byte
	nalLen  int
}

func newVTEncoder(os vtOS, p encParams) (*vtEncoder, error) {
	e := &vtEncoder{os: os, p: p, slot: &vtSlot{os: os, maxBytes: DefaultMaxPacketBytes}}
	e.ref = vtRegister(e.slot)
	sess, st := os.encCreate(p.width, p.height, !vtNoHardware.Load(), e.ref)
	if st != 0 {
		vtUnregister(e.ref)
		return nil, vtError("create compression session", st)
	}
	e.sess = sess
	if err := e.configure(); err != nil {
		e.close()
		return nil, err
	}
	return e, nil
}

func (e *vtEncoder) configure() error {
	set := func(op string, st int32) error {
		if st != 0 {
			return vtError(op, st)
		}
		return nil
	}
	if err := set("set RealTime", e.os.encSetBool(e.sess, vtPropRealTime, true)); err != nil {
		return err
	}
	if err := set("set AllowFrameReordering", e.os.encSetBool(e.sess, vtPropReorder, false)); err != nil {
		return err
	}
	if err := e.setProfile(); err != nil {
		return err
	}
	if err := set("set AverageBitRate", e.os.encSetInt(e.sess, vtPropBitrate, int32(e.p.bps))); err != nil {
		return err
	}
	if err := set("set ExpectedFrameRate", e.os.encSetFloat(e.sess, vtPropFPS, e.p.fps)); err != nil {
		return err
	}
	if e.p.keyInterval < 0 {
		if err := set("set MaxKeyFrameInterval", e.os.encSetInt(e.sess, vtPropKeyFrames, math.MaxInt32)); err != nil {
			return err
		}
	}
	if e.p.keyInterval > 0 {
		sec := e.p.keyInterval.Seconds()
		if err := set("set MaxKeyFrameIntervalDuration", e.os.encSetFloat(e.sess, vtPropKeyDuration, sec)); err != nil {
			return err
		}
	}
	if err := set("prepare to encode", e.os.encPrepare(e.sess)); err != nil {
		return err
	}
	e.hw = e.os.encHardware(e.sess)
	return nil
}

// setProfile asks for the profile. The software encoder refuses Constrained
// Baseline with kVTParameterErr and produces it for Baseline_AutoLevel; the
// hardware encoder produces plain Baseline for that value, so it is never
// asked for it.
func (e *vtEncoder) setProfile() error {
	level := vtLevelConstrainedBaseline
	switch e.p.profile {
	case Main:
		level = vtLevelMain
	case High:
		level = vtLevelHigh
	}
	st := e.os.encSetProfile(e.sess, level)
	if st == 0 {
		return nil
	}
	if level != vtLevelConstrainedBaseline || st != vtErrParameter || e.os.encHardware(e.sess) {
		return vtProfileError("set ProfileLevel", st)
	}
	if st = e.os.encSetProfile(e.sess, vtLevelBaseline); st != 0 {
		return vtProfileError("set ProfileLevel", st)
	}
	return nil
}

func vtProfileError(op string, st int32) error {
	if st == vtErrParameter {
		return &codecError{engine: "VideoToolbox", op: op, code: int64(st), kind: ErrProfile}
	}
	return vtError(op, st)
}

func (e *vtEncoder) params() [][]byte { return nil }

func (e *vtEncoder) hardware() bool { return e.hw }

func (e *vtEncoder) setBitrate(bps int) error {
	if e.sess == 0 {
		return vtFault("set AverageBitRate on a closed encoder", ErrCodec)
	}
	if st := e.os.encSetInt(e.sess, vtPropBitrate, int32(bps)); st != 0 {
		return vtError("set AverageBitRate", st)
	}
	return nil
}

func (e *vtEncoder) encode(y, u, v []byte, yStride, uvStride int, pts, dur int64, key bool,
	emit func(nals, params [][]byte, key bool)) error {
	if e.sess == 0 {
		return vtFault("encode on a closed encoder", ErrCodec)
	}
	w, h := e.p.width, e.p.height
	cw, ch := w/2, h/2
	if !vtPlaneFits(len(y), yStride, w, h) || !vtPlaneFits(len(u), uvStride, cw, ch) || !vtPlaneFits(len(v), uvStride, cw, ch) {
		return fmt.Errorf("kolovideo/h264: plane smaller than the configured size: %w", kolovideo.ErrInvalidFrame)
	}
	pb, st := e.os.encPixelBuffer(e.sess, vtUsePool, w, h)
	if st != 0 {
		return vtError("create pixel buffer", st)
	}
	if err := e.fill(pb, y, u, v, yStride, uvStride); err != nil {
		e.os.release(pb)
		return err
	}
	e.counter++
	e.slot.begin(e.counter, 0, 0)
	flags, st := e.os.encFrame(e.sess, pb, pts, dur, key, e.counter)
	e.os.release(pb)
	if st != 0 {
		return vtError("encode frame", st)
	}
	if st := e.os.encComplete(e.sess, pts, false); st != 0 {
		return vtError("complete frames", st)
	}
	return e.emit(flags, emit)
}

// emit reads what the callback left, after CompleteFrames returned.
func (e *vtEncoder) emit(flags uint32, emit func(nals, params [][]byte, key bool)) error {
	s := e.slot
	s.mu.Lock()
	done, status, err, dropped := s.done, s.status, s.err, s.dropped
	key := s.key
	var avcc []byte
	if done && err == nil && status == 0 && !dropped {
		avcc = s.avcc
		if key {
			e.held, e.nalLen = s.params, s.lenSize
		}
	}
	s.mu.Unlock()
	switch {
	case err != nil:
		return err
	case status != 0:
		return vtError("encoded frame", status)
	case !done && flags&vtInfoFrameDropped != 0, dropped:
		return nil
	case !done:
		return vtFault("no output after CompleteFrames", ErrCodec)
	}
	nals, err := vtAVCCSplit(avcc, e.nalLen, e.nals[:0])
	e.nals = nals[:0]
	if err != nil {
		return err
	}
	emit(nals, e.held, key)
	return nil
}

func (e *vtEncoder) fill(pb uintptr, y, u, v []byte, yStride, uvStride int) error {
	format, planes, w, h := e.os.pbInfo(pb)
	if format != vtPixelFormat420v || planes != 2 || w != e.p.width || h != e.p.height {
		return vtFault("pixel buffer format", ErrCodec)
	}
	if st := e.os.pbLock(pb, false); st != 0 {
		return vtError("lock pixel buffer", st)
	}
	defer e.os.pbUnlock(pb, false)
	cw, ch := w/2, h/2
	dy, dys := e.os.pbPlane(pb, 0)
	dc, dcs := e.os.pbPlane(pb, 1)
	if !vtPlaneFits(len(dy), dys, w, h) || !vtPlaneFits(len(dc), dcs, 2*cw, ch) {
		return vtFault("pixel buffer layout", ErrCodec)
	}
	for r := 0; r < h; r++ {
		copy(dy[r*dys:r*dys+w], y[r*yStride:r*yStride+w])
	}
	for r := 0; r < ch; r++ {
		joinUV(dc[r*dcs:r*dcs+2*cw], u[r*uvStride:r*uvStride+cw], v[r*uvStride:r*uvStride+cw])
	}
	return nil
}

func (e *vtEncoder) close() {
	if e.sess == 0 {
		return
	}
	e.os.encComplete(e.sess, 0, true)
	e.os.encInvalidate(e.sess)
	e.os.release(e.sess)
	e.sess = 0
	vtUnregister(e.ref)
}

var _ encImpl = (*vtEncoder)(nil)

type vtDecoder struct {
	os      vtOS
	p       decParams
	slot    *vtSlot
	ref     uintptr
	sess    uintptr
	fd      uintptr
	params  [][]byte
	counter uintptr
	avcc    []byte
	hw      bool
	hwKnown bool
}

func newVTDecoder(os vtOS, p decParams) (*vtDecoder, error) {
	d := &vtDecoder{os: os, p: p, slot: &vtSlot{os: os}}
	d.ref = vtRegister(d.slot)
	return d, nil
}

func (d *vtDecoder) hardware() bool { return d.hw }

func (d *vtDecoder) decode(au accessUnit) (picture, bool, error) {
	if d.ref == 0 {
		return picture{}, false, vtFault("decode on a closed decoder", ErrCodec)
	}
	if au.width <= 0 || au.height <= 0 || au.codedWidth*au.codedHeight > d.p.maxPixels {
		return picture{}, false, vtFault("picture size", ErrTooLarge)
	}
	if au.key {
		if err := d.keyframe(au); err != nil {
			return picture{}, false, err
		}
	}
	if d.sess == 0 {
		return picture{}, false, vtFault("no decoder session", ErrCodec)
	}
	size := 0
	for _, s := range au.slices {
		size += 4 + len(s)
	}
	if size == 0 || size > 1<<31-1 {
		return picture{}, false, vtFault("sample size", ErrTooLarge)
	}
	d.avcc = d.avcc[:0]
	for _, s := range au.slices {
		n := len(s)
		d.avcc = append(d.avcc, byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
		d.avcc = append(d.avcc, s...)
	}
	bb, st := d.os.blockBuffer(d.avcc)
	if st != 0 {
		return picture{}, false, vtError("create block buffer", st)
	}
	d.counter++
	sb, st := d.os.sampleBuffer(bb, d.fd, size, int64(d.counter)*3000)
	if st != 0 {
		d.os.release(bb)
		return picture{}, false, vtError("create sample buffer", st)
	}
	d.slot.begin(d.counter, au.width, au.height)
	_, st = d.os.decFrame(d.sess, sb, d.counter)
	d.os.release(sb)
	d.os.release(bb)
	return d.result(st)
}

func (d *vtDecoder) result(st int32) (picture, bool, error) {
	s := d.slot
	s.mu.Lock()
	done, status, err, dropped := s.done, s.status, s.err, s.dropped
	pic := picture{width: s.w, height: s.h, y: s.y, u: s.uv, yStride: s.yStride, uvStride: s.uvStride, nv12: true}
	s.mu.Unlock()
	if st == 0 {
		st = status
	}
	switch {
	case st == vtErrBadData:
		return picture{}, false, vtError("decode frame", st)
	case st != 0:
		d.dropSession()
		return picture{}, false, vtError("decode frame", st)
	case err != nil:
		d.dropSession()
		return picture{}, false, err
	case !done:
		d.dropSession()
		return picture{}, false, vtFault("no output after DecodeFrame", ErrCodec)
	case dropped:
		return picture{}, false, nil
	}
	if !d.hwKnown {
		d.hw, d.hwKnown = d.os.decHardware(d.sess), true
	}
	return pic, true, nil
}

// keyframe makes the format description and session match the parameter sets
// of au: a new description when they differ, a new session when the old one
// cannot accept it or failed.
func (d *vtDecoder) keyframe(au accessUnit) error {
	params := make([][]byte, 0, len(au.sps)+len(au.pps))
	params = append(append(params, au.sps...), au.pps...)
	if d.fd == 0 || !vtSameParams(d.params, params) {
		fd, st := d.os.fdCreate(params)
		if st != 0 {
			return vtError("create format description", st)
		}
		if d.sess != 0 && !d.os.decCanAccept(d.sess, fd) {
			d.dropSession()
		}
		if d.fd != 0 {
			d.os.release(d.fd)
		}
		d.fd, d.params = fd, vtCloneParams(params)
	}
	if d.sess == 0 {
		sess, st := d.os.decCreate(d.fd, d.ref, !vtNoHardware.Load())
		if st != 0 {
			return vtError("create decompression session", st)
		}
		d.sess, d.hwKnown, d.hw = sess, false, false
	}
	return nil
}

func (d *vtDecoder) dropSession() {
	if d.sess == 0 {
		return
	}
	d.os.decWait(d.sess)
	d.os.decInvalidate(d.sess)
	d.os.release(d.sess)
	d.sess = 0
}

func (d *vtDecoder) close() {
	if d.ref == 0 {
		return
	}
	d.dropSession()
	if d.fd != 0 {
		d.os.release(d.fd)
		d.fd = 0
	}
	vtUnregister(d.ref)
	d.ref = 0
}

var _ decImpl = (*vtDecoder)(nil)

func vtSameParams(a, b [][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !bytes.Equal(a[i], b[i]) {
			return false
		}
	}
	return true
}

func vtCloneParams(p [][]byte) [][]byte {
	out := make([][]byte, len(p))
	for i := range p {
		out[i] = bytes.Clone(p[i])
	}
	return out
}

// vtEngine is the VideoToolbox engine over an OS layer that loads on first
// use.
type vtEngine struct {
	load func() (vtOS, error)
	once sync.Once
	os   vtOS
	err  error
}

func (e *vtEngine) layer() (vtOS, error) {
	e.once.Do(func() { e.os, e.err = e.load() })
	return e.os, e.err
}

func (e *vtEngine) name() string { return "VideoToolbox" }

func (e *vtEngine) unavailable(reason string, code int64) *UnavailableError {
	return &UnavailableError{GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, Engine: "VideoToolbox", Reason: reason, Code: code}
}

func (e *vtEngine) caps() Limits {
	os, err := e.layer()
	if err != nil {
		u := e.unavailable(err.Error(), 0)
		return Limits{Encode: Support{Engine: "VideoToolbox", Err: u}, Decode: Support{Engine: "VideoToolbox", Err: u}}
	}
	profiles := []Profile{ConstrainedBaseline, Main, High}
	enc := Support{Engine: "VideoToolbox"}
	avc, hw, st := os.encoders()
	switch {
	case st != 0:
		enc.Err = e.unavailable("the encoder list is not readable", int64(st))
	case !avc:
		enc.Err = e.unavailable("no H.264 encoder is installed", 0)
	default:
		enc.Available, enc.Hardware, enc.Profiles = true, hw, profiles
	}
	dec := Support{Engine: "VideoToolbox"}
	if code := e.probeDecoder(os); code != 0 {
		dec.Err = e.unavailable("no H.264 decoder session can be created", code)
	} else {
		dec.Available, dec.Hardware, dec.Profiles = true, os.hardwareDecode(), profiles
	}
	return Limits{Encode: enc, Decode: dec}
}

// probeDecoder creates and frees a decoder session from built-in 48x48
// Constrained Baseline parameter sets and returns its OSStatus, or 0.
func (e *vtEngine) probeDecoder(os vtOS) int64 {
	fd, st := os.fdCreate([][]byte{vtProbeSPS, vtProbePPS})
	if st != 0 {
		return int64(st)
	}
	defer os.release(fd)
	slot := &vtSlot{os: os}
	ref := vtRegister(slot)
	defer vtUnregister(ref)
	sess, st := os.decCreate(fd, ref, true)
	if st != 0 {
		return int64(st)
	}
	os.decInvalidate(sess)
	os.release(sess)
	return 0
}

func (e *vtEngine) newEncoder(p encParams) (encImpl, error) {
	os, err := e.layer()
	if err != nil {
		return nil, e.unavailable(err.Error(), 0)
	}
	return newVTEncoder(os, p)
}

func (e *vtEngine) newDecoder(p decParams) (decImpl, error) {
	os, err := e.layer()
	if err != nil {
		return nil, e.unavailable(err.Error(), 0)
	}
	return newVTDecoder(os, p)
}
