package h264

import (
	"bytes"
	"fmt"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// mftFake is the Windows side of the Media Foundation engine with no
// operating system behind it: it logs the calls that matter for ordering,
// counts the references of every object it hands out, and records the
// goroutine of every call, which must be the instance's owner thread.
type mftFake struct {
	mu   sync.Mutex
	log  []string
	refs []*mftRef
	gs   map[int]bool

	failOps    map[string]error // operation name to error, see hook
	createErr  map[mftGUID]error
	coErr      error
	startErr   error
	provides   bool
	inTypes    []mftGUID // subtypes GetInputAvailableType offers
	outTypes   []mftGUID
	noSeq      bool
	seqFor     func(profile uint32, w, h int) []byte
	encOutputs func(n int, key bool) [][]byte // access units (Annex B) given after input n
	decLayout  func(w, h int) mftFakeLayout
	change     bool // report a stream change before the first output
	decPics    int  // pictures per input, default 1, -1 for none
	late       bool // the decoder holds the first picture until a drain
	sizeInfo   uint32

	xfs     []*mftFakeXform
	inputs  [][]byte // the data of every input sample
	inTimes [][2]int64
}

type mftFakeLayout struct {
	w, h, stride int
	aperture     *[2]int // cx, cy; nil: no aperture
	noStride     bool
}

type mftRef struct {
	what string
	n    int
}

func newMFTFake() *mftFake {
	return &mftFake{
		gs: map[int]bool{}, failOps: map[string]error{}, createErr: map[mftGUID]error{},
		inTypes:  []mftGUID{fmtYV12Test, fmtNV12Test, fmtIYUV},
		outTypes: []mftGUID{fmtNV12Test, fmtYV12Test, fmtIYUV, fmtI420},
	}
}

var (
	fmtYV12Test = mftGUID{0x32315659, 0, 0x10, [8]byte{0x80, 0, 0, 0xAA, 0, 0x38, 0x9B, 0x71}}
	fmtNV12Test = mftGUID{0x3231564E, 0, 0x10, [8]byte{0x80, 0, 0, 0xAA, 0, 0x38, 0x9B, 0x71}}
)

var mftGUIDNames = map[mftGUID]string{
	mtMajorType: "MajorType", mtSubtype: "Subtype", mtFrameSize: "FrameSize", mtFrameRate: "FrameRate",
	mtAvgBitrate: "AvgBitrate", mtInterlaceMode: "InterlaceMode", mtMPEG2Profile: "Profile",
	mtSequenceHeader: "SequenceHeader", mtDefaultStride: "DefaultStride", mtDisplayAperture: "DisplayAperture",
	sampleCleanPoint: "CleanPoint", mfLowLatency: "LowLatency",
	apiRateControlMode: "RateControlMode", apiMeanBitRate: "MeanBitRate", apiForceKeyFrame: "ForceKeyFrame",
	apiGOPSize: "GOPSize", apiBPictureCount: "BPictureCount", apiDecAcceleration: "DecAcceleration",
	apiDecMaxWidth: "DecMaxWidth", apiDecMaxHeight: "DecMaxHeight",
}

func mftGName(g mftGUID) string {
	if n, ok := mftGUIDNames[g]; ok {
		return n
	}
	return g.String()
}

func mftGoroutine() int {
	var b [64]byte
	n := runtime.Stack(b[:], false)
	id, _ := strconv.Atoi(strings.Fields(string(b[:n]))[1])
	return id
}

func (f *mftFake) add(format string, args ...any) {
	f.mu.Lock()
	f.log = append(f.log, fmt.Sprintf(format, args...))
	f.gs[mftGoroutine()] = true
	f.mu.Unlock()
}

// hook logs an operation and returns the error injected for it.
func (f *mftFake) hook(op string) error {
	f.add("%s", op)
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.failOps[op]
}

func (f *mftFake) events() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.log)
}

func (f *mftFake) has(prefix string) bool {
	return slices.ContainsFunc(f.events(), func(e string) bool { return strings.HasPrefix(e, prefix) })
}

func (f *mftFake) index(prefix string) int {
	return slices.IndexFunc(f.events(), func(e string) bool { return strings.HasPrefix(e, prefix) })
}

func (f *mftFake) count(prefix string) int {
	n := 0
	for _, e := range f.events() {
		if strings.HasPrefix(e, prefix) {
			n++
		}
	}
	return n
}

func (f *mftFake) newRef(what string) *mftRef {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := &mftRef{what: what, n: 1}
	f.refs = append(f.refs, r)
	return r
}

func (f *mftFake) drop(r *mftRef) {
	f.add("release %s", r.what)
	f.mu.Lock()
	r.n--
	f.mu.Unlock()
}

// checkReleased fails when an object was released more or fewer times than
// it was created, and when the calls did not come from exactly the given
// number of goroutines, one owner thread per instance.
func (f *mftFake) checkReleased(t testing.TB, threads int) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.refs {
		if r.n != 0 {
			t.Errorf("%s has %d references left, want 0", r.what, r.n)
		}
	}
	if len(f.gs) != threads {
		t.Errorf("calls came from %d goroutines, want %d: %v", len(f.gs), threads, f.gs)
	}
}

func (f *mftFake) coInit() (func(), error) {
	if f.coErr != nil {
		return nil, f.coErr
	}
	f.add("coInit")
	return func() { f.add("coUninit") }, nil
}

func (f *mftFake) startup() (func(), error) {
	if f.startErr != nil {
		return nil, f.startErr
	}
	f.add("startup")
	return func() { f.add("mfShutdown") }, nil
}

func (f *mftFake) create(clsid *mftGUID) (mftTransform, error) {
	f.add("create %s", map[bool]string{true: "enc", false: "dec"}[*clsid == clsidH264Encoder])
	if err := f.createErr[*clsid]; err != nil {
		return nil, err
	}
	if *clsid != clsidH264Encoder && *clsid != clsidH264Decoder {
		return nil, hrClassNotReg
	}
	x := &mftFakeXform{f: f, enc: *clsid == clsidH264Encoder, ref: f.newRef("transform")}
	if f.provides {
		x.outFlags = outProvidesSamples
	}
	f.mu.Lock()
	f.xfs = append(f.xfs, x)
	f.mu.Unlock()
	return x, nil
}

func (f *mftFake) mediaType() (mftAttrs, error) {
	return &mftFakeAttrs{f: f, ref: f.newRef("media type"), m: map[mftGUID]any{}}, nil
}

func (f *mftFake) sample(capacity int, fill func([]byte) int, pts, dur int64) (mftSample, error) {
	if err := f.hook("sample"); err != nil {
		return nil, err
	}
	s := &mftFakeSample{f: f, ref: f.newRef("sample"), buf: make([]byte, capacity), m: map[mftGUID]any{}}
	if fill != nil {
		s.bytes = s.buf[:fill(s.buf)]
	}
	s.pts, s.dur = pts, dur
	return s, nil
}

type mftFakeAttrs struct {
	f   *mftFake
	ref *mftRef
	m   map[mftGUID]any
}

func (a *mftFakeAttrs) setGUID(key, v *mftGUID) error { a.m[*key] = *v; return nil }

func (a *mftFakeAttrs) setU32(key *mftGUID, v uint32) error {
	if err := a.f.hook("attr " + mftGName(*key)); err != nil {
		return err
	}
	a.f.add("set %s=%d", mftGName(*key), v)
	a.m[*key] = v
	return nil
}

func (a *mftFakeAttrs) setU64(key *mftGUID, v uint64) error { a.m[*key] = v; return nil }

func (a *mftFakeAttrs) getGUID(key *mftGUID) (mftGUID, error) {
	if v, ok := a.m[*key].(mftGUID); ok {
		return v, nil
	}
	return mftGUID{}, hrFail
}

func (a *mftFakeAttrs) getU32(key *mftGUID) (uint32, error) {
	if v, ok := a.m[*key].(uint32); ok {
		return v, nil
	}
	return 0, hrFail
}

func (a *mftFakeAttrs) getU64(key *mftGUID) (uint64, error) {
	if v, ok := a.m[*key].(uint64); ok {
		return v, nil
	}
	return 0, hrFail
}

func (a *mftFakeAttrs) getBlob(key *mftGUID, max int) ([]byte, error) {
	v, ok := a.m[*key].([]byte)
	if !ok || len(v) > max {
		return nil, hrFail
	}
	return bytes.Clone(v), nil
}

func (a *mftFakeAttrs) release() { a.f.drop(a.ref) }

type mftFakeSample struct {
	f        *mftFake
	ref      *mftRef
	buf      []byte
	bytes    []byte
	pts, dur int64
	m        map[mftGUID]any
}

func (s *mftFakeSample) data(dst []byte, max int) ([]byte, error) {
	if len(s.bytes) > max {
		return nil, hrFail
	}
	return append(dst[:0], s.bytes...), nil
}

func (s *mftFakeSample) getU32(key *mftGUID) (uint32, error) {
	if v, ok := s.m[*key].(uint32); ok {
		return v, nil
	}
	return 0, hrFail
}

func (s *mftFakeSample) release() { s.f.drop(s.ref) }

type mftFakeCodec struct {
	f   *mftFake
	x   *mftFakeXform
	ref *mftRef
}

func (c *mftFakeCodec) setU32(key *mftGUID, v uint32) error {
	if err := c.f.hook("codec " + mftGName(*key)); err != nil {
		return err
	}
	c.f.add("codec u32 %s=%d", mftGName(*key), v)
	if *key == apiForceKeyFrame && v == 1 {
		c.x.forceKey = true
	}
	return nil
}

func (c *mftFakeCodec) setBool(key *mftGUID, v bool) error {
	if err := c.f.hook("codec " + mftGName(*key)); err != nil {
		return err
	}
	c.f.add("codec bool %s=%v", mftGName(*key), v)
	return nil
}

func (c *mftFakeCodec) release() { c.f.drop(c.ref) }

// mftFakeXform is an encoder or decoder MFT. The encoder gives the access
// units of encOutputs, one list per input; the decoder gives one picture per
// input.
type mftFakeXform struct {
	f        *mftFake
	enc      bool
	ref      *mftRef
	outFlags uint32

	inMap, outMap map[mftGUID]any
	pending       [][]byte // access units or pictures waiting for ProcessOutput
	change        bool
	inputs        int
	forceKey      bool
	held          [][]byte // pictures that appear at a drain
}

func (x *mftFakeXform) kind() string {
	if x.enc {
		return "enc"
	}
	return "dec"
}

func (x *mftFakeXform) codecAPI() (mftCodec, error) {
	if err := x.f.hook(x.kind() + " codecAPI"); err != nil {
		return nil, err
	}
	return &mftFakeCodec{f: x.f, x: x, ref: x.f.newRef("codec")}, nil
}

func (x *mftFakeXform) attributes() (mftAttrs, error) {
	if err := x.f.hook(x.kind() + " attributes"); err != nil {
		return nil, err
	}
	return &mftFakeAttrs{f: x.f, ref: x.f.newRef("attributes"), m: map[mftGUID]any{}}, nil
}

func (x *mftFakeXform) offered(types []mftGUID, i int) (mftAttrs, error) {
	if i >= len(types) {
		return nil, mftHRESULT(0xC00D36B9) // MF_E_NO_MORE_TYPES
	}
	return &mftFakeAttrs{f: x.f, ref: x.f.newRef("offered type"), m: map[mftGUID]any{mtSubtype: types[i], mtMajorType: mediaVideo}}, nil
}

func (x *mftFakeXform) inputType(i int) (mftAttrs, error) {
	x.f.add("inputType %d", i)
	if x.enc {
		return x.offered(x.f.inTypes, i)
	}
	return x.offered([]mftGUID{fmtH264}, i)
}

func (x *mftFakeXform) outputType(i int) (mftAttrs, error) {
	x.f.add("outputType %d", i)
	return x.offered(x.f.outTypes, i)
}

func mftSubtypeName(t mftAttrs) string {
	g, _ := t.getGUID(&mtSubtype)
	switch g {
	case fmtIYUV:
		return "IYUV"
	case fmtI420:
		return "I420"
	case fmtH264:
		return "H264"
	case fmtYV12Test:
		return "YV12"
	case fmtNV12Test:
		return "NV12"
	}
	return g.String()
}

func (x *mftFakeXform) setInput(t mftAttrs) error {
	if err := x.f.hook("setInput"); err != nil {
		return err
	}
	x.f.add("setInput %s", mftSubtypeName(t))
	if x.enc {
		g, _ := t.getGUID(&mtSubtype)
		switch {
		case x.outMap == nil:
			return mftHRESULT(0xC00D6D60) // the output type must come first
		case g != fmtIYUV && g != fmtI420:
			return mftHRESULT(0xC00D36B4) // MF_E_INVALIDMEDIATYPE
		}
	}
	x.inMap = maps(t.(*mftFakeAttrs).m)
	return nil
}

func maps(m map[mftGUID]any) map[mftGUID]any {
	c := map[mftGUID]any{}
	for k, v := range m {
		c[k] = v
	}
	return c
}

func (x *mftFakeXform) setOutput(t mftAttrs) error {
	if err := x.f.hook("setOutput"); err != nil {
		return err
	}
	x.f.add("setOutput %s", mftSubtypeName(t))
	if !x.enc && x.inMap == nil {
		return mftHRESULT(0xC00D6D60) // the input type must come first
	}
	x.outMap = maps(t.(*mftFakeAttrs).m)
	return nil
}

func (x *mftFakeXform) currentOutput() (mftAttrs, error) {
	if err := x.f.hook(x.kind() + " currentOutput"); err != nil {
		return nil, err
	}
	m := maps(x.outMap)
	if x.enc {
		if !x.f.noSeq {
			size := m[mtFrameSize].(uint64)
			m[mtSequenceHeader] = mftFakeSeq(x.f, m[mtMPEG2Profile].(uint32), int(size>>32), int(uint32(size)))
		}
	} else {
		l := x.layout()
		m[mtFrameSize] = pack(uint32(l.w), uint32(l.h))
		if !l.noStride {
			m[mtDefaultStride] = uint32(l.stride)
		}
		if l.aperture != nil {
			area := make([]byte, 16)
			area[8], area[12] = byte(l.aperture[0]), byte(l.aperture[1])
			area[9], area[13] = byte(l.aperture[0]>>8), byte(l.aperture[1]>>8)
			m[mtDisplayAperture] = area
		}
	}
	return &mftFakeAttrs{f: x.f, ref: x.f.newRef("current output type"), m: m}, nil
}

// mftFakeSeq is the sequence header of the encoder: the SPS of the profile
// value and a PPS, Annex B.
func mftFakeSeq(f *mftFake, profile uint32, w, h int) []byte {
	if f.seqFor != nil {
		return f.seqFor(profile, w, h)
	}
	var sps []byte
	switch profile {
	case profileMain:
		sps = testSPSFor(w, h, 77, 0x40, 31)
	case profileHigh:
		sps = testSPSFor(w, h, 100, 0, 31)
	default:
		sps = testSPSFor(w, h, 66, 0xc0, 31)
	}
	return appendAnnexB(nil, [][]byte{sps}, [][]byte{testPPS(0, 0)})
}

func (x *mftFakeXform) outputInfo() (uint32, uint32, error) {
	if err := x.f.hook("outputInfo"); err != nil {
		return 0, 0, err
	}
	return x.outFlags, x.f.sizeInfo, nil
}

func (x *mftFakeXform) message(m uint32) error {
	name := map[uint32]string{msgFlush: "flush", msgDrain: "drain", msgBeginStreaming: "begin streaming", msgEndStreaming: "end streaming", msgStartOfStream: "start of stream"}[m]
	if err := x.f.hook("message " + name); err != nil {
		return err
	}
	switch m {
	case msgFlush:
		x.pending, x.held = nil, nil
	case msgDrain:
		x.pending, x.held = append(x.pending, x.held...), nil
	}
	return nil
}

func (x *mftFakeXform) processInput(s mftSample) error {
	if err := x.f.hook("processInput"); err != nil {
		return err
	}
	fs := s.(*mftFakeSample)
	x.f.mu.Lock()
	x.f.inputs = append(x.f.inputs, bytes.Clone(fs.bytes))
	x.f.inTimes = append(x.f.inTimes, [2]int64{fs.pts, fs.dur})
	x.f.mu.Unlock()
	n := x.inputs
	x.inputs++
	x.change = x.f.change && n == 0
	if x.enc {
		key := x.forceKey || n == 0
		x.forceKey = false
		x.pending = append(x.pending, x.encOutputs(n, key)...)
		return nil
	}
	for i := 0; i < max(1, x.f.decPics); i++ {
		switch {
		case x.f.decPics < 0:
		case x.f.late && n == 0:
			x.held = append(x.held, x.picture())
		default:
			x.pending = append(x.pending, x.picture())
		}
	}
	return nil
}

func (x *mftFakeXform) encOutputs(n int, key bool) [][]byte {
	if x.f.encOutputs != nil {
		return x.f.encOutputs(n, key)
	}
	size := x.outMap[mtFrameSize].(uint64)
	nals := [][]byte{{0x09, 0xf0}} // access unit delimiter
	if key {
		nals = append(nals, testSPSFor(int(size>>32), int(uint32(size)), 66, 0xc0, 31), testPPS(0, 0), testSlice(true, 0, 7, 0, 0, 1))
	} else {
		nals = append(nals, testSlice(false, 0, 5, 0, uint32(n%16), 0))
	}
	return [][]byte{appendAnnexB(nil, nals)}
}

func (x *mftFakeXform) processOutput(s mftSample) (mftSample, error) {
	if err := x.f.hook("processOutput"); err != nil {
		return nil, err
	}
	if x.change {
		x.change = false
		return nil, hrStreamChange
	}
	if len(x.pending) == 0 {
		return nil, hrNeedMoreInput
	}
	data := x.pending[0]
	x.pending = x.pending[1:]
	var fs *mftFakeSample
	if s != nil {
		fs = s.(*mftFakeSample)
		if len(data) > len(fs.buf) {
			return nil, mftHRESULT(0x8007000E)
		}
	} else {
		fs = &mftFakeSample{f: x.f, ref: x.f.newRef("provided sample"), m: map[mftGUID]any{}}
	}
	fs.bytes = data
	if x.enc {
		clean := uint32(0)
		nals, _ := splitAnnexB(data, 1000)
		for _, n := range nals {
			if nalType(n) == 5 {
				clean = 1
			}
		}
		fs.m[sampleCleanPoint] = clean
	}
	return fs, nil
}

func (x *mftFakeXform) layout() mftFakeLayout {
	size := x.inMap[mtFrameSize].(uint64)
	w, h := int(size>>32), int(uint32(size))
	if x.f.decLayout != nil {
		return x.f.decLayout(w, h)
	}
	pw, ph := (w+15)/16*16, (h+15)/16*16
	return mftFakeLayout{w: pw, h: ph, stride: pw, aperture: &[2]int{w, h}}
}

// picture is the decoder's output for an input: an I420 frame of the layout
// with the chroma planes after the padded height.
func (x *mftFakeXform) picture() []byte {
	l := x.layout()
	out := make([]byte, l.stride*l.h+2*(l.stride/2)*(l.h/2))
	for r := 0; r < l.h; r++ {
		for c := 0; c < l.w; c++ {
			out[r*l.stride+c] = lumaAt(c, r)
		}
	}
	base := l.stride * l.h
	cs := l.stride / 2
	for r := 0; r < l.h/2; r++ {
		for c := 0; c < l.w/2; c++ {
			out[base+r*cs+c] = byte(2*c + r)
			out[base+cs*(l.h/2)+r*cs+c] = byte(c + 3*r)
		}
	}
	return out
}

func (x *mftFakeXform) release() { x.f.drop(x.ref) }
