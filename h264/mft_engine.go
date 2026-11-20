package h264

// The Media Foundation engine logic. It has no operating system dependency:
// every call into Windows goes through the interfaces below, implemented by
// mft_windows.go and by a fake in the tests, so this file builds and is
// tested on every platform. The MFT behaviour it relies on is the one the
// Microsoft pages "H.264 Video Encoder" and "H.264 Video Decoder" document
// and the Windows tests measure.

import (
	"errors"
	"fmt"
	"math"
	"runtime"
	"sync"
	"time"
)

const mftEngineName = "Media Foundation"

// mftMaxTypes bounds the media types read from one MFT.
const mftMaxTypes = 64

// mftOS is the COM and Media Foundation surface the engine uses. Every
// method and every object method runs on the owner thread of the instance,
// which entered a multithreaded apartment and started Media Foundation.
type mftOS interface {
	coInit() (func(), error)
	startup() (func(), error)
	// create makes the MFT of a CLSID (CoCreateInstance, in process).
	create(clsid *mftGUID) (mftTransform, error)
	mediaType() (mftAttrs, error)
	// sample makes a sample with a memory buffer of capacity bytes. fill, if
	// not nil, writes the data into the buffer and returns its length.
	// pts and dur are in 100 ns units.
	sample(capacity int, fill func(dst []byte) int, pts, dur int64) (mftSample, error)
}

// mftAttrs is an IMFAttributes object, such as a media type. Each object the
// engine receives is released exactly once.
type mftAttrs interface {
	setGUID(key, v *mftGUID) error
	setU32(key *mftGUID, v uint32) error
	setU64(key *mftGUID, v uint64) error
	getGUID(key *mftGUID) (mftGUID, error)
	getU32(key *mftGUID) (uint32, error)
	getU64(key *mftGUID) (uint64, error)
	// getBlob fails when the blob is longer than max.
	getBlob(key *mftGUID, max int) ([]byte, error)
	release()
}

type mftTransform interface {
	codecAPI() (mftCodec, error)
	attributes() (mftAttrs, error)
	inputType(i int) (mftAttrs, error)
	outputType(i int) (mftAttrs, error)
	setInput(t mftAttrs) error
	setOutput(t mftAttrs) error
	currentOutput() (mftAttrs, error)
	outputInfo() (flags, size uint32, err error)
	message(m uint32) error
	processInput(s mftSample) error
	// processOutput fills the sample s, or when s is nil takes the sample
	// the MFT provides. A sample returned for a nil s belongs to the caller;
	// the one returned for s is s itself.
	processOutput(s mftSample) (mftSample, error)
	release()
}

// mftCodec is the ICodecAPI of an MFT. setU32 writes a VT_UI4 variant and
// setBool a VT_BOOL one.
type mftCodec interface {
	setU32(key *mftGUID, v uint32) error
	setBool(key *mftGUID, v bool) error
	release()
}

type mftSample interface {
	// data copies the contents into dst[:0], failing when they exceed max.
	data(dst []byte, max int) ([]byte, error)
	getU32(key *mftGUID) (uint32, error)
	release()
}

// mftEngine is the Media Foundation engine over an mftOS.
type mftEngine struct{ sys mftOS }

func (mftEngine) name() string { return mftEngineName }

func (e mftEngine) unavailable(op string, err error) *UnavailableError {
	u := &UnavailableError{GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, Engine: mftEngineName, Reason: op + " failed"}
	var h mftHRESULT
	var ce *codecError
	switch {
	case errors.As(err, &h):
		u.Code = int64(uint32(h))
	case errors.As(err, &ce):
		u.Reason, u.Code = op+": "+ce.op+" failed", ce.code
	case err != nil:
		u.Reason = op + ": " + err.Error()
	}
	return u
}

// mftFail maps a failed call to a typed error.
func mftFail(op string, err error) error {
	e := &codecError{engine: mftEngineName, op: op, kind: ErrCodec}
	var h mftHRESULT
	if errors.As(err, &h) {
		e.code = int64(uint32(h))
		if h == hrInvalidData {
			e.kind = ErrCorrupt
		}
	}
	return e
}

func mftFault(op string, why string) error {
	return fmt.Errorf("%w: %s: %s", ErrCodec, mftEngineName+" "+op, why)
}

func mftCode(err error) (mftHRESULT, bool) {
	var h mftHRESULT
	return h, errors.As(err, &h)
}

// mftCreate makes an MFT; a class that is not registered, or a missing DLL,
// is absence rather than a codec failure.
func (e mftEngine) mftCreate(clsid *mftGUID, what string) (mftTransform, error) {
	xf, err := e.sys.create(clsid)
	if err == nil {
		return xf, nil
	}
	if h, ok := mftCode(err); ok && h != hrClassNotReg {
		return nil, mftFail("create "+what, err)
	}
	return nil, e.unavailable("create the "+what, err)
}

var errMFTClosed = fmt.Errorf("%w: %s instance is closed", ErrCodec, mftEngineName)

// mftThread is the owner thread of one instance: a goroutine locked to an
// OS thread that entered a multithreaded apartment and started Media
// Foundation. Every COM call of the instance runs on it, one at a time. The
// thread leaves the apartment and exits when the instance stops.
type mftThread struct {
	mu     sync.Mutex
	closed bool
	calls  chan func()
	done   chan struct{}
}

func (e mftEngine) startThread() (*mftThread, error) {
	t := &mftThread{calls: make(chan func()), done: make(chan struct{})}
	ready := make(chan error, 1)
	go func() {
		defer close(t.done)
		runtime.LockOSThread()
		uninit, err := e.sys.coInit()
		if err != nil {
			ready <- e.unavailable("start COM", err)
			return
		}
		defer uninit()
		shutdown, err := e.sys.startup()
		if err != nil {
			ready <- e.unavailable("start Media Foundation", err)
			return
		}
		defer shutdown()
		ready <- nil
		for fn := range t.calls {
			fn()
		}
	}()
	if err := <-ready; err != nil {
		<-t.done
		return nil, err
	}
	return t, nil
}

// run runs fn on the thread and waits for it. A panic in fn becomes an error.
func (t *mftThread) run(fn func() error) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return errMFTClosed
	}
	res := make(chan error, 1)
	t.calls <- func() {
		defer func() {
			if r := recover(); r != nil {
				res <- mftFault("call", fmt.Sprint("panic: ", r))
			}
		}()
		res <- fn()
	}
	return <-res
}

// stop ends the thread after the call in progress. It is idempotent.
func (t *mftThread) stop() {
	t.mu.Lock()
	if !t.closed {
		t.closed = true
		close(t.calls)
	}
	t.mu.Unlock()
	<-t.done
}

// mftProfileValue is MF_MT_MPEG2_PROFILE for a profile.
func mftProfileValue(p Profile) (uint32, bool) {
	switch p {
	case ConstrainedBaseline:
		return profileConstrainedBase, true
	case Main:
		return profileMain, true
	case High:
		return profileHigh, true
	}
	return 0, false
}

// mftRatio is the frame rate as a numerator and denominator, neither 0: a
// rate of at least 1 as round(rate x 1000) over 1000, below 1 as 1 over
// round(1 / rate).
func mftRatio(rate float64) (num, den uint32) {
	if rate >= 1 {
		return uint32(min(math.Round(rate*1000), math.MaxUint32)), 1000
	}
	return 1, uint32(max(1, min(math.Round(1/rate), math.MaxUint32)))
}

// mftGOP is AVEncMPVGOPSize: the frames in the interval, or the largest
// value for keyframes on request only (0 would let the encoder choose).
func mftGOP(interval time.Duration, fps float64) uint32 {
	if interval < 0 {
		return math.MaxUint32
	}
	return uint32(max(1, min(math.Round(interval.Seconds()*fps), math.MaxUint32)))
}

// mft100ns converts 90 kHz ticks to 100 ns units without overflow.
func mft100ns(ticks int64) int64 {
	const limit = math.MaxInt64 / 1000
	if ticks >= limit {
		return math.MaxInt64
	}
	if ticks <= -limit {
		return math.MinInt64
	}
	return ticks * 1000 / 9
}

func pack(hi, lo uint32) uint64 { return uint64(hi)<<32 | uint64(lo) }

// mftEnc is an encoder MFT with its ICodecAPI, output type and sequence
// header.
type mftEnc struct {
	xf       mftTransform
	codec    mftCodec
	seq      []byte
	params   [][]byte
	provides bool
	outSize  int
	out      mftSample // the sample the engine supplies, when the MFT does not provide one
}

// spent releases an output sample after its data was read: the one the MFT
// provided, or the one the engine supplied, which the next call replaces (a
// fresh sample for every picture, as the MFTs expect).
func (m *mftEnc) spent(out mftSample) {
	out.release()
	if !m.provides {
		m.out = nil
	}
}

// release frees the sample and the ICodecAPI before the transform.
func (m *mftEnc) release() {
	if m.out != nil {
		m.out.release()
		m.out = nil
	}
	if m.codec != nil {
		m.codec.release()
		m.codec = nil
	}
	if m.xf != nil {
		m.xf.release()
		m.xf = nil
	}
}

// openEncoder makes the encoder MFT of p with its output type set and reads
// the sequence header. With stream set it also sets the input type and starts
// streaming.
func (e mftEngine) openEncoder(p encParams, stream bool) (*mftEnc, error) {
	profile, ok := mftProfileValue(p.profile)
	if !ok {
		return nil, fmt.Errorf("%w: profile %v", ErrProfile, p.profile)
	}
	xf, err := e.mftCreate(&clsidH264Encoder, "H.264 encoder")
	if err != nil {
		return nil, err
	}
	m := &mftEnc{xf: xf}
	fail := func(err error) (*mftEnc, error) {
		m.release()
		return nil, err
	}
	if m.codec, err = xf.codecAPI(); err != nil {
		return fail(mftFail("get ICodecAPI", err))
	}
	for _, s := range []struct {
		name string
		key  *mftGUID
		v    uint32
	}{
		{"AVEncCommonRateControlMode", &apiRateControlMode, rateControlCBR},
		{"AVEncCommonMeanBitRate", &apiMeanBitRate, uint32(p.bps)},
		{"AVEncMPVDefaultBPictureCount", &apiBPictureCount, 0},
		{"AVEncMPVGOPSize", &apiGOPSize, mftGOP(p.keyInterval, p.fps)},
	} {
		if err := m.codec.setU32(s.key, s.v); err != nil {
			return fail(mftFail("set "+s.name, err))
		}
	}
	if err := m.codec.setBool(&apiLowLatencyMode, true); err != nil {
		return fail(mftFail("set AVLowLatencyMode", err))
	}
	num, den := mftRatio(p.fps)
	t, err := e.sys.mediaType()
	if err != nil {
		return fail(mftFail("create the output type", err))
	}
	err = mftSetAll(t, []mftAttr{
		{guid: &mtMajorType, g: &mediaVideo}, {guid: &mtSubtype, g: &fmtH264},
		{guid: &mtAvgBitrate, u32: uint32(p.bps), kind: 1}, {guid: &mtFrameRate, u64: pack(num, den), kind: 2},
		{guid: &mtFrameSize, u64: pack(uint32(p.width), uint32(p.height)), kind: 2},
		{guid: &mtInterlaceMode, u32: interlaceProgressive, kind: 1}, {guid: &mtMPEG2Profile, u32: profile, kind: 1},
	})
	if err == nil {
		err = xf.setOutput(t)
	}
	t.release()
	if err != nil {
		return fail(mftFail("set the output type", err))
	}
	cur, err := xf.currentOutput()
	if err != nil {
		return fail(mftFail("get the output type", err))
	}
	err = m.readSequenceHeader(cur, p.width*p.height)
	cur.release()
	if err != nil {
		return fail(err)
	}
	if !stream {
		return m, nil
	}
	if err := e.setEncoderInput(xf, p, num, den); err != nil {
		return fail(err)
	}
	for _, msg := range []uint32{msgBeginStreaming, msgStartOfStream} {
		if err := xf.message(msg); err != nil {
			return fail(mftFail("start streaming", err))
		}
	}
	flags, size, err := xf.outputInfo()
	if err != nil {
		return fail(mftFail("get the output stream info", err))
	}
	m.provides = flags&outProvidesSamples != 0
	m.outSize = max(int(size), p.width*p.height*3/2)
	if !m.provides {
		if m.out, err = e.sys.sample(m.outSize, nil, 0, 0); err != nil {
			return fail(mftFail("create the output sample", err))
		}
	}
	return m, nil
}

// readSequenceHeader takes the SPS and PPS of the output type: Annex B,
// bounded by the packet limit.
func (m *mftEnc) readSequenceHeader(t mftAttrs, pixels int) error {
	seq, err := t.getBlob(&mtSequenceHeader, DefaultMaxPacketBytes)
	if err != nil {
		return mftFail("get the sequence header", err)
	}
	nals, err := splitAnnexB(seq, maxNALs(pixels))
	if err != nil {
		return mftFault("sequence header", err.Error())
	}
	m.seq, m.params = seq, nil
	for _, n := range nals {
		if t := nalType(n); t == nalSPS || t == nalPPS {
			m.params = append(m.params, n)
		}
	}
	if len(m.params) < 2 {
		return mftFault("sequence header", "no SPS and PPS")
	}
	return nil
}

// setEncoderInput sets the first offered IYUV or I420 type with the stream's
// size and rate; a hand-built type is refused by the MFT.
func (e mftEngine) setEncoderInput(xf mftTransform, p encParams, num, den uint32) error {
	var last error = mftFault("set the input type", "no IYUV or I420 type offered")
	for i := 0; i < mftMaxTypes; i++ {
		t, err := xf.inputType(i)
		if err != nil {
			break
		}
		sub, err := t.getGUID(&mtSubtype)
		if err != nil || (sub != fmtIYUV && sub != fmtI420) {
			t.release()
			continue
		}
		err = mftSetAll(t, []mftAttr{
			{guid: &mtFrameSize, u64: pack(uint32(p.width), uint32(p.height)), kind: 2},
			{guid: &mtFrameRate, u64: pack(num, den), kind: 2},
			{guid: &mtInterlaceMode, u32: interlaceProgressive, kind: 1},
		})
		if err == nil {
			err = xf.setInput(t)
		}
		t.release()
		if err == nil {
			return nil
		}
		last = mftFail("set the input type", err)
	}
	return last
}

// mftAttr is one attribute to set: kind 0 a GUID, 1 a 32-bit value, 2 a
// 64-bit value.
type mftAttr struct {
	guid *mftGUID
	g    *mftGUID
	u32  uint32
	u64  uint64
	kind int
}

func mftSetAll(t mftAttrs, attrs []mftAttr) error {
	for _, a := range attrs {
		var err error
		switch a.kind {
		case 1:
			err = t.setU32(a.guid, a.u32)
		case 2:
			err = t.setU64(a.guid, a.u64)
		default:
			err = t.setGUID(a.guid, a.g)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (e mftEngine) newEncoder(p encParams) (encImpl, error) {
	th, err := e.startThread()
	if err != nil {
		return nil, err
	}
	enc := &mftEncoder{sys: e.sys, th: th, p: p}
	err = th.run(func() error {
		var err error
		enc.m, err = e.openEncoder(p, true)
		return err
	})
	if err != nil {
		th.stop()
		return nil, err
	}
	return enc, nil
}

type mftEncoder struct {
	sys mftOS
	th  *mftThread
	p   encParams
	m   *mftEnc
	buf []byte
}

func (e *mftEncoder) params() [][]byte { return e.m.params }

func (e *mftEncoder) hardware() bool { return false }

func (e *mftEncoder) setBitrate(bps int) error {
	return e.th.run(func() error {
		if err := e.m.codec.setU32(&apiMeanBitRate, uint32(bps)); err != nil {
			return mftFail("set AVEncCommonMeanBitRate", err)
		}
		return nil
	})
}

func (e *mftEncoder) close() {
	e.th.run(func() error {
		e.m.xf.message(msgEndStreaming)
		e.m.release()
		return nil
	})
	e.th.stop()
}

// encode submits one picture and reads the access unit the encoder gives for
// it. One input gives one output from the first frame on (measured); a second
// output for one input is an error.
func (e *mftEncoder) encode(y, u, v []byte, yStride, uvStride int, pts, dur int64, key bool,
	emit func(nals, params [][]byte, key bool)) error {
	w, h := e.p.width, e.p.height
	cw, ch := w/2, h/2
	if yStride < w || uvStride < cw || len(y) < (h-1)*yStride+w || len(u) < (ch-1)*uvStride+cw || len(v) < (ch-1)*uvStride+cw {
		return mftFault("encode", "picture planes are smaller than the frame")
	}
	var nals [][]byte
	var isKey bool
	err := e.th.run(func() error {
		if key {
			if err := e.m.codec.setU32(&apiForceKeyFrame, 1); err != nil {
				return mftFail("set AVEncVideoForceKeyFrame", err)
			}
		}
		in, err := e.sys.sample(w*h*3/2, func(dst []byte) int {
			n := 0
			for _, pl := range []struct {
				b             []byte
				stride, wd, h int
			}{{y, yStride, w, h}, {u, uvStride, cw, ch}, {v, uvStride, cw, ch}} {
				for r := 0; r < pl.h; r++ {
					n += copy(dst[n:], pl.b[r*pl.stride:r*pl.stride+pl.wd])
				}
			}
			return n
		}, mft100ns(pts), mft100ns(dur))
		if err != nil {
			return mftFail("create the input sample", err)
		}
		err = e.m.xf.processInput(in)
		in.release()
		if err != nil {
			return mftFail("ProcessInput", err)
		}
		nals, isKey, err = e.drain()
		return err
	})
	if err != nil {
		return err
	}
	if nals != nil {
		emit(nals, e.m.params, isKey)
	}
	return nil
}

// drain calls ProcessOutput until the encoder wants more input and returns
// the NAL units of the one access unit it gave, if any.
func (e *mftEncoder) drain() (nals [][]byte, key bool, err error) {
	got, changes := false, 0
	for {
		if !e.m.provides && e.m.out == nil {
			if e.m.out, err = e.sys.sample(e.m.outSize, nil, 0, 0); err != nil {
				return nil, false, mftFail("create the output sample", err)
			}
		}
		out, err := e.m.xf.processOutput(e.m.out)
		if err != nil {
			switch h, _ := mftCode(err); {
			case h == hrNeedMoreInput:
				return nals, key, nil
			case h == hrStreamChange && changes == 0:
				changes++
				if err := e.rereadOutputType(); err != nil {
					return nil, false, err
				}
				continue
			}
			return nil, false, mftFail("ProcessOutput", err)
		}
		if got {
			e.m.spent(out)
			return nil, false, mftFault("encode", "more than one access unit for one frame")
		}
		got = true
		e.buf, err = out.data(e.buf, DefaultMaxPacketBytes)
		clean, cerr := out.getU32(&sampleCleanPoint)
		e.m.spent(out)
		if err != nil {
			return nil, false, mftFail("read the access unit", err)
		}
		if nals, err = splitAnnexB(e.buf, maxNALs(e.p.width*e.p.height)); err != nil {
			return nil, false, mftFault("encode", "access unit: "+err.Error())
		}
		key = cerr == nil && clean != 0
		for _, n := range nals {
			if nalType(n) == nalIDR {
				key = true
			}
		}
	}
}

// rereadOutputType reads the output type and its sequence header again after
// the encoder reports a stream change.
func (e *mftEncoder) rereadOutputType() error {
	cur, err := e.m.xf.currentOutput()
	if err != nil {
		return mftFail("get the output type", err)
	}
	defer cur.release()
	return e.m.readSequenceHeader(cur, e.p.width*e.p.height)
}

// mftLayout is the decoder's output buffer: the padded frame size, the row
// stride and the display aperture if the type has one.
type mftLayout struct {
	w, h, stride int
	aperture     bool
	cx, cy       int
}

// mftReadLayout reads the frame size, MF_MT_DEFAULT_STRIDE and
// MF_MT_MINIMUM_DISPLAY_APERTURE of an output type. The aperture must start
// at the origin.
func mftReadLayout(t mftAttrs) (mftLayout, error) {
	size, err := t.getU64(&mtFrameSize)
	if err != nil {
		return mftLayout{}, mftFail("get the frame size", err)
	}
	l := mftLayout{w: int(size >> 32), h: int(uint32(size))}
	l.stride = l.w
	if s, err := t.getU32(&mtDefaultStride); err == nil {
		l.stride = int(int32(s))
	}
	if b, err := t.getBlob(&mtDisplayAperture, videoAreaLen); err == nil {
		if len(b) != videoAreaLen {
			return mftLayout{}, mftFault("decode", "display aperture of the wrong size")
		}
		le16 := func(i int) int { return int(int16(uint16(b[i]) | uint16(b[i+1])<<8)) }
		le32 := func(i int) int {
			return int(int32(uint32(b[i]) | uint32(b[i+1])<<8 | uint32(b[i+2])<<16 | uint32(b[i+3])<<24))
		}
		if le16(2) != 0 || le16(6) != 0 {
			return mftLayout{}, mftFault("decode", "display aperture does not start at the origin")
		}
		l.aperture, l.cx, l.cy = true, le32(8), le32(12)
	}
	return l, nil
}

// check refuses a layout that cannot hold the picture of au.
func (l mftLayout) check(au accessUnit) error {
	switch {
	case l.w < au.width || l.h < au.height || l.w > au.codedWidth || l.h > au.codedHeight:
		return mftFault("decode", fmt.Sprintf("frame size %dx%d for a %dx%d picture in a %dx%d coded frame", l.w, l.h, au.width, au.height, au.codedWidth, au.codedHeight))
	case l.stride < l.w || l.stride%2 != 0 || l.h%2 != 0:
		return mftFault("decode", fmt.Sprintf("stride %d for a frame of %dx%d", l.stride, l.w, l.h))
	case l.aperture && (l.cx != au.width || l.cy != au.height):
		return mftFault("decode", fmt.Sprintf("display aperture %dx%d for a %dx%d picture", l.cx, l.cy, au.width, au.height))
	}
	return nil
}

// bytes is the size of the I420 buffer: the chroma planes follow the padded
// frame height.
func (l mftLayout) bytes() int { return l.stride*l.h + 2*(l.stride/2)*(l.h/2) }

// mftDec is a decoder MFT set up for one coded size.
type mftDec struct {
	xf       mftTransform
	lay      mftLayout
	provides bool
	size     int
	out      mftSample // the sample the engine supplies, when the MFT does not provide one
	au       accessUnit
}

// spent releases an output sample after its data was read; see mftEnc.spent.
func (m *mftDec) spent(out mftSample) {
	out.release()
	if !m.provides {
		m.out = nil
	}
}

func (m *mftDec) release() {
	if m.out != nil {
		m.out.release()
		m.out = nil
	}
	if m.xf != nil {
		m.xf.message(msgEndStreaming)
		m.xf.release()
		m.xf = nil
	}
}

func (e mftEngine) newDecoder(p decParams) (decImpl, error) {
	th, err := e.startThread()
	if err != nil {
		return nil, err
	}
	// Creating the MFT now reports absence at NewDecoder; the stream's size
	// is known only at the first keyframe, where it is set up.
	d := &mftDecoder{e: e, sys: e.sys, th: th}
	err = th.run(func() error {
		xf, err := e.mftCreate(&clsidH264Decoder, "H.264 decoder")
		d.spare = xf
		return err
	})
	if err != nil {
		th.stop()
		return nil, err
	}
	return d, nil
}

type mftDecoder struct {
	e     mftEngine
	sys   mftOS
	th    *mftThread
	spare mftTransform // the MFT made at construction, used by the first setup
	m     *mftDec
	ts    int64
	in    []byte
	pic   []byte
}

func (d *mftDecoder) hardware() bool { return false }

func (d *mftDecoder) close() {
	d.th.run(func() error {
		if d.m != nil {
			d.m.release()
			d.m = nil
		}
		if d.spare != nil {
			d.spare.release()
			d.spare = nil
		}
		return nil
	})
	d.th.stop()
}

// open sets up the decoder MFT for the stream of au: the low-latency
// attribute, software decoding, and the input type before the output type.
func (d *mftDecoder) open(au accessUnit) (*mftDec, error) {
	xf := d.spare
	d.spare = nil
	if xf == nil {
		var err error
		if xf, err = d.e.mftCreate(&clsidH264Decoder, "H.264 decoder"); err != nil {
			return nil, err
		}
	}
	m := &mftDec{xf: xf, au: au}
	fail := func(err error) (*mftDec, error) {
		m.release()
		return nil, err
	}
	attrs, err := xf.attributes()
	if err != nil {
		return fail(mftFail("get the attributes", err))
	}
	for _, s := range []struct {
		name string
		key  *mftGUID
		v    uint32
	}{
		{"MF_LOW_LATENCY", &mfLowLatency, 1},
		{"AVDecVideoAcceleration_H264", &apiDecAcceleration, 0},
		{"AVDecVideoMaxCodedWidth", &apiDecMaxWidth, 4096},
		{"AVDecVideoMaxCodedHeight", &apiDecMaxHeight, 4096},
	} {
		if err = attrs.setU32(s.key, s.v); err != nil {
			attrs.release()
			return fail(mftFail("set "+s.name, err))
		}
	}
	attrs.release()
	t, err := d.sys.mediaType()
	if err != nil {
		return fail(mftFail("create the input type", err))
	}
	err = mftSetAll(t, []mftAttr{
		{guid: &mtMajorType, g: &mediaVideo}, {guid: &mtSubtype, g: &fmtH264},
		{guid: &mtFrameSize, u64: pack(uint32(au.width), uint32(au.height)), kind: 2},
		{guid: &mtInterlaceMode, u32: interlaceMixed, kind: 1},
	})
	if err == nil {
		err = xf.setInput(t)
	}
	t.release()
	if err != nil {
		return fail(mftFail("set the input type", err))
	}
	if err := d.negotiate(m); err != nil {
		return fail(err)
	}
	for _, msg := range []uint32{msgBeginStreaming, msgStartOfStream} {
		if err := xf.message(msg); err != nil {
			return fail(mftFail("start streaming", err))
		}
	}
	return m, nil
}

// negotiate sets the first IYUV or I420 output type the MFT offers and reads
// its layout; it runs at setup and after a stream change.
func (d *mftDecoder) negotiate(m *mftDec) error {
	var pick mftAttrs
	for i := 0; i < mftMaxTypes && pick == nil; i++ {
		t, err := m.xf.outputType(i)
		if err != nil {
			break
		}
		if sub, err := t.getGUID(&mtSubtype); err == nil && (sub == fmtIYUV || sub == fmtI420) {
			pick = t
		} else {
			t.release()
		}
	}
	if pick == nil {
		return mftFault("set the output type", "no IYUV or I420 type offered")
	}
	err := m.xf.setOutput(pick)
	pick.release()
	if err != nil {
		return mftFail("set the output type", err)
	}
	cur, err := m.xf.currentOutput()
	if err != nil {
		return mftFail("get the output type", err)
	}
	lay, err := mftReadLayout(cur)
	cur.release()
	if err != nil {
		return err
	}
	if err := lay.check(m.au); err != nil {
		return err
	}
	flags, size, err := m.xf.outputInfo()
	if err != nil {
		return mftFail("get the output stream info", err)
	}
	m.lay, m.provides = lay, flags&outProvidesSamples != 0
	m.size = max(int(size), lay.bytes())
	if m.size > 8*lay.bytes() {
		return mftFault("decode", fmt.Sprintf("output buffer of %d bytes for a %d byte picture", size, lay.bytes()))
	}
	if m.out != nil {
		m.out.release()
		m.out = nil
	}
	if !m.provides {
		if m.out, err = d.sys.sample(m.size, nil, 0, 0); err != nil {
			return mftFail("create the output sample", err)
		}
	}
	return nil
}

func (d *mftDecoder) decode(au accessUnit) (pic picture, ok bool, err error) {
	err = d.th.run(func() error {
		var err error
		pic, ok, err = d.decodeOn(au)
		return err
	})
	if err != nil {
		return picture{}, false, err
	}
	return pic, ok, nil
}

func (d *mftDecoder) decodeOn(au accessUnit) (picture, bool, error) {
	if d.m != nil && au.key && (au.width != d.m.au.width || au.height != d.m.au.height ||
		au.codedWidth != d.m.au.codedWidth || au.codedHeight != d.m.au.codedHeight) {
		d.m.release()
		d.m = nil
	}
	if d.m == nil {
		if !au.key {
			return picture{}, false, nil
		}
		m, err := d.open(au)
		if err != nil {
			return picture{}, false, err
		}
		d.m = m
	}
	m := d.m
	fail := func(err error) (picture, bool, error) {
		m.xf.message(msgFlush)
		return picture{}, false, err
	}
	d.in = appendAnnexB(d.in[:0], au.sps, au.pps, au.slices)
	data := d.in
	in, err := d.sys.sample(len(data), func(dst []byte) int { return copy(dst, data) }, d.ts, 333333)
	d.ts += 333333
	if err != nil {
		return fail(mftFail("create the input sample", err))
	}
	err = m.xf.processInput(in)
	in.release()
	if err != nil {
		return fail(mftFail("ProcessInput", err))
	}
	var pic picture
	got, drained, changes := false, false, 0
	for {
		if !m.provides && m.out == nil {
			if m.out, err = d.sys.sample(m.size, nil, 0, 0); err != nil {
				return fail(mftFail("create the output sample", err))
			}
		}
		out, err := m.xf.processOutput(m.out)
		if err != nil {
			switch h, _ := mftCode(err); {
			case h == hrNeedMoreInput:
				if got || drained {
					return pic, got, nil
				}
				// The decoder can finish a picture after ProcessOutput
				// reported nothing (seen on the first keyframe, rarely);
				// a drain makes it deliver what it holds.
				drained = true
				if err := m.xf.message(msgDrain); err != nil {
					return fail(mftFail("drain", err))
				}
				continue
			case h == hrStreamChange && changes == 0:
				changes++
				if err := d.negotiate(m); err != nil {
					return fail(err)
				}
				continue
			}
			return fail(mftFail("ProcessOutput", err))
		}
		if got {
			m.spent(out)
			return fail(mftFault("decode", "more than one picture for one packet"))
		}
		d.pic, err = out.data(d.pic, m.size)
		m.spent(out)
		if err != nil {
			return fail(mftFail("read the picture", err))
		}
		if len(d.pic) < m.lay.bytes() {
			return fail(mftFault("decode", fmt.Sprintf("picture of %d bytes, want %d", len(d.pic), m.lay.bytes())))
		}
		l := m.lay
		yEnd := l.stride * l.h
		cLen := (l.stride / 2) * (l.h / 2)
		pic = picture{
			width: l.w, height: l.h, yStride: l.stride, uvStride: l.stride / 2,
			y: d.pic[:yEnd], u: d.pic[yEnd : yEnd+cLen], v: d.pic[yEnd+cLen : yEnd+2*cLen],
		}
		got = true
	}
}

// caps probes the MFTs once: the decoder by creating it, the encoder by
// creating it for each profile and classifying the SPS of its sequence
// header. Both run on a thread of their own.
func (e mftEngine) caps() Limits {
	th, err := e.startThread()
	if err != nil {
		u, _ := err.(*UnavailableError)
		return Limits{Encode: Support{Engine: mftEngineName, Err: u}, Decode: Support{Engine: mftEngineName, Err: u}}
	}
	defer th.stop()
	l := Limits{Encode: Support{Engine: mftEngineName}, Decode: Support{Engine: mftEngineName}}
	th.run(func() error {
		l.Decode = e.decodeSupport()
		l.Encode = e.encodeSupport()
		return nil
	})
	return l
}

func (e mftEngine) decodeSupport() Support {
	s := Support{Engine: mftEngineName}
	xf, err := e.sys.create(&clsidH264Decoder)
	if err != nil {
		s.Err = e.unavailable("create the H.264 decoder", err)
		return s
	}
	xf.release()
	s.Available, s.Profiles = true, []Profile{ConstrainedBaseline, Main, High}
	s.MaxWidth, s.MaxHeight = 4096, 2304
	return s
}

func (e mftEngine) encodeSupport() Support {
	s := Support{Engine: mftEngineName}
	var last error
	for _, p := range []Profile{ConstrainedBaseline, Main, High} {
		m, err := e.openEncoder(encParams{width: 320, height: 240, fps: 30, bps: 1000000, profile: p, keyInterval: -1}, false)
		if err != nil {
			last = err
			if _, ok := err.(*UnavailableError); ok {
				break
			}
			continue
		}
		sps, err := ParseKeyframe(appendAnnexB(nil, m.params))
		m.release()
		if got, ok := sps.Profile(); err == nil && ok && got == p {
			s.Profiles = append(s.Profiles, p)
		}
	}
	s.Available = len(s.Profiles) > 0
	if !s.Available {
		var u *UnavailableError
		if errors.As(last, &u) {
			s.Err = u
		} else {
			s.Err = e.unavailable("probe the H.264 encoder profiles", last)
		}
	}
	return s
}
