package h264

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

var errorsIs = errors.Is

func timeAfter(sec int) <-chan time.Time { return time.After(time.Duration(sec) * time.Second) }

// fakeVT is a vtOS that follows the VideoToolbox contract: it hands out
// objects that must be released exactly once, runs the output callbacks from
// another goroutine while the call that waits for them blocks (as the
// system's threads do), and records the order of the calls that matter.
type fakeVT struct {
	t  *testing.T
	mu sync.Mutex

	next uintptr
	objs map[uintptr]*fakeObj
	ops  []string

	// Knobs.
	hardware      bool
	hwDecode      bool
	hasAVC        bool
	listStatus    int32
	createStatus  int32
	profileStatus map[vtLevel]int32
	setStatus     map[vtProp]int32
	prepareStatus int32
	pbStatus      int32
	lockStatus    int32
	frameStatus   int32
	completeSt    int32
	fdStatus      int32
	decCreateSt   int32
	canAccept     bool
	blockStatus   int32
	sampleStatus  int32
	decStatus     int32
	noCallback    bool
	inline        bool
	pbFormat      uint32
	pbW, pbH      int
	lenSize       int
	lumaTail      int  // when set, luma planes hold stride*(rows-1)+lumaTail bytes
	decTwice      bool // a second decode callback, with an error, for the same frame

	// What the encoder callback delivers for the next frame.
	encode func(f *fakeVT, fr fakeFrame) fakeEncoded
	// What the decoder callback delivers for the next frame.
	decode func(f *fakeVT, fr fakeFrame) fakeDecoded

	profile       []vtLevel
	props         []string
	ref           uintptr
	pending       []fakeFrame
	forced        []bool
	usedPool      []bool
	frames        []fakePTS
	fdParams      [][][]byte
	decRefs       []uintptr
	decSamples    [][]byte
	samples       map[uintptr]*fakeSample
	sampleSizes   []int
	imgW, imgH    int
	imgFmt        uint32
	imgPlanes     int
	imgStrideDiff int
	imgReads      int
	panicInfo     bool
	negInterval   bool
	lastPB        []byte
	lastCbCr      []byte
	lastStride    [2]int
	decHW         bool
}

type fakeObj struct {
	kind    string
	w, h    int
	format  uint32
	planes  [2][]byte
	strides [2]int
	locked  int
}

type fakeFrame struct {
	counter uintptr
	pts     int64
	force   bool
	ref     uintptr
}

type fakePTS struct{ pts, dur int64 }

type fakeSample struct {
	avcc    []byte
	notSync bool
	absent  bool // no attachments array: a sync sample
	params  [][]byte
	lenSize int
	copySt  int32
	paramSt int32
	size    int // overrides the reported length when not 0
	panics  bool
}

type fakeEncoded struct {
	status  int32
	flags   uint32
	sample  *fakeSample
	nilSamp bool
}

type fakeDecoded struct {
	status int32
	flags  uint32
	none   bool
}

var (
	fakeSPS = []byte{0x67, 0x42, 0xc0, 0x1f, 0xab, 0x40, 0x28, 0x02, 0xdc, 0x80}
	fakePPS = []byte{0x68, 0xce, 0x3c, 0x80}
	fakeIDR = []byte{0x65, 0xb8, 0x20, 0x02, 0xbf, 0x01, 0x02}
	fakeP   = []byte{0x41, 0x9a, 0x24, 0x6c, 0x03}
)

func avccOf(lenSize int, nals ...[]byte) []byte {
	var b []byte
	for _, n := range nals {
		for i := lenSize - 1; i >= 0; i-- {
			b = append(b, byte(len(n)>>(8*i)))
		}
		b = append(b, n...)
	}
	return b
}

func newFakeVT(t *testing.T) *fakeVT {
	f := &fakeVT{
		t: t, objs: map[uintptr]*fakeObj{}, samples: map[uintptr]*fakeSample{},
		hardware: true, hwDecode: true, hasAVC: true, canAccept: true, decHW: true, lenSize: 4,
		profileStatus: map[vtLevel]int32{}, setStatus: map[vtProp]int32{},
	}
	base := vtLive()
	t.Cleanup(func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		for h, o := range f.objs {
			t.Errorf("object %d (%s) was never released", h, o.kind)
		}
		if n := vtLive(); n != base {
			t.Errorf("%d session handles left registered", n-base)
		}
	})
	return f
}

func (f *fakeVT) log(s string) {
	f.mu.Lock()
	f.ops = append(f.ops, s)
	f.mu.Unlock()
}

func (f *fakeVT) opList() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.ops, ",")
}

func (f *fakeVT) newObj(kind string) (uintptr, *fakeObj) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	o := &fakeObj{kind: kind}
	f.objs[f.next] = o
	return f.next, o
}

func (f *fakeVT) obj(h uintptr) *fakeObj {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.objs[h]
}

func (f *fakeVT) live(kind string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, o := range f.objs {
		if o.kind == kind {
			n++
		}
	}
	return n
}

func (f *fakeVT) release(h uintptr) {
	f.mu.Lock()
	o, ok := f.objs[h]
	delete(f.objs, h)
	if ok {
		f.ops = append(f.ops, "release("+o.kind+")")
		if o.locked != 0 {
			f.t.Errorf("%s released while locked", o.kind)
		}
	}
	f.mu.Unlock()
	if !ok {
		f.t.Errorf("release of %d, which is not live", h)
	}
}

func (f *fakeVT) encCreate(w, h int, hardware bool, ref uintptr) (uintptr, int32) {
	if f.createStatus != 0 {
		return 0, f.createStatus
	}
	s, o := f.newObj("encSession")
	o.w, o.h = w, h
	f.ref = ref
	f.log("encCreate")
	return s, 0
}

func (f *fakeVT) status(p vtProp) int32 { return f.setStatus[p] }

func (f *fakeVT) encSetBool(_ uintptr, p vtProp, v bool) int32 {
	f.props = append(f.props, fmt.Sprintf("%d=%v", p, v))
	return f.status(p)
}

func (f *fakeVT) encSetInt(_ uintptr, p vtProp, v int32) int32 {
	f.props = append(f.props, fmt.Sprintf("%d=%d", p, v))
	return f.status(p)
}

func (f *fakeVT) encSetFloat(_ uintptr, p vtProp, v float64) int32 {
	f.props = append(f.props, fmt.Sprintf("%d=%g", p, v))
	return f.status(p)
}

func (f *fakeVT) encSetProfile(_ uintptr, l vtLevel) int32 {
	f.profile = append(f.profile, l)
	return f.profileStatus[l]
}

func (f *fakeVT) encPrepare(uintptr) int32 { f.log("prepare"); return f.prepareStatus }

func (f *fakeVT) encHardware(uintptr) bool { return f.hardware }

func (f *fakeVT) encPixelBuffer(_ uintptr, pool bool, w, h int) (uintptr, int32) {
	f.usedPool = append(f.usedPool, pool)
	if f.pbStatus != 0 {
		return 0, f.pbStatus
	}
	pb, o := f.newObj("pixelBuffer")
	o.w, o.h, o.format = w, h, vtPixelFormat420v
	if f.pbFormat != 0 {
		o.format = f.pbFormat
	}
	if f.pbW != 0 {
		o.w, o.h = f.pbW, f.pbH
	}
	o.strides = [2]int{o.w + 48, o.w + 32}
	o.planes[0] = make([]byte, o.strides[0]*o.h)
	if f.lumaTail != 0 {
		o.planes[0] = o.planes[0][:o.strides[0]*(o.h-1)+f.lumaTail]
	}
	o.planes[1] = make([]byte, o.strides[1]*(o.h/2))
	return pb, 0
}

func (f *fakeVT) encFrame(_, pb uintptr, pts, dur int64, force bool, counter uintptr) (uint32, int32) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.forced = append(f.forced, force)
	f.frames = append(f.frames, fakePTS{pts, dur})
	if o := f.objs[pb]; o != nil {
		f.lastPB = append([]byte(nil), o.planes[0]...)
		f.lastCbCr = append([]byte(nil), o.planes[1]...)
		f.lastStride = o.strides
	}
	if f.frameStatus != 0 {
		return 0, f.frameStatus
	}
	f.pending = append(f.pending, fakeFrame{counter: counter, pts: pts, force: force, ref: f.ref})
	return 0, 0
}

func (f *fakeVT) encComplete(_ uintptr, _ int64, all bool) int32 {
	if all {
		f.log("encComplete(all)")
	} else {
		f.log("encComplete")
	}
	if f.completeSt != 0 {
		return f.completeSt
	}
	f.mu.Lock()
	pend := f.pending
	f.pending = nil
	f.mu.Unlock()
	for _, fr := range pend {
		f.deliverEncoded(fr)
	}
	return 0
}

func (f *fakeVT) deliverEncoded(fr fakeFrame) {
	if f.noCallback {
		return
	}
	e := fakeEncoded{}
	if f.encode != nil {
		e = f.encode(f, fr)
	} else {
		e.sample = f.defaultSample(fr.force)
	}
	var h uintptr
	if e.sample != nil && !e.nilSamp {
		f.mu.Lock()
		f.next++
		h = f.next
		f.samples[h] = e.sample
		f.mu.Unlock()
	}
	f.run(func() { vtEncoded(fr.ref, fr.counter, e.status, e.flags, h) })
}

func (f *fakeVT) defaultSample(key bool) *fakeSample {
	if key {
		return &fakeSample{avcc: avccOf(f.lenSize, fakeIDR), params: [][]byte{fakeSPS, fakePPS}, lenSize: f.lenSize}
	}
	return &fakeSample{avcc: avccOf(f.lenSize, fakeP), notSync: true, lenSize: f.lenSize}
}

// run calls fn on another goroutine, as a system thread would, and waits.
func (f *fakeVT) run(fn func()) {
	if f.inline {
		fn()
		return
	}
	done := make(chan struct{})
	go func() { defer close(done); fn() }()
	<-done
}

func (f *fakeVT) encInvalidate(uintptr) { f.log("encInvalidate") }

func (f *fakeVT) pbInfo(h uintptr) (uint32, int, int, int) {
	if f.panicInfo {
		panic("pbInfo")
	}
	if h == fakeImage {
		return f.imgFmt, f.imgPlanes, f.imgW, f.imgH
	}
	o := f.obj(h)
	return o.format, 2, o.w, o.h
}

func (f *fakeVT) pbLock(h uintptr, ro bool) int32 {
	if f.lockStatus != 0 {
		return f.lockStatus
	}
	if h == fakeImage {
		f.log("imageLock")
		return 0
	}
	f.mu.Lock()
	f.objs[h].locked++
	f.mu.Unlock()
	return 0
}

func (f *fakeVT) pbUnlock(h uintptr, ro bool) int32 {
	if h == fakeImage {
		f.log("imageUnlock")
		return 0
	}
	f.mu.Lock()
	f.objs[h].locked--
	f.mu.Unlock()
	return 0
}

func (f *fakeVT) pbPlane(h uintptr, plane int) ([]byte, int) {
	if h == fakeImage {
		f.imgReads++
		return f.imagePlane(plane)
	}
	o := f.obj(h)
	return o.planes[plane], o.strides[plane]
}

func (f *fakeVT) sample(sb uintptr) *fakeSample {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.samples[sb]
}

func (f *fakeVT) sbSync(sb uintptr) bool { s := f.sample(sb); return s.absent || !s.notSync }

func (f *fakeVT) sbLen(sb uintptr) int {
	s := f.sample(sb)
	if s.panics {
		panic("sbLen")
	}
	if s.size != 0 {
		return s.size
	}
	return len(s.avcc)
}

func (f *fakeVT) sbCopy(sb uintptr, dst []byte) int32 {
	s := f.sample(sb)
	f.log("sbCopy")
	if s.copySt != 0 {
		return s.copySt
	}
	copy(dst, s.avcc)
	return 0
}

func (f *fakeVT) sbParams(sb uintptr) ([][]byte, int, int32) {
	s := f.sample(sb)
	return s.params, s.lenSize, s.paramSt
}

func (f *fakeVT) fdCreate(params [][]byte) (uintptr, int32) {
	if f.fdStatus != 0 {
		return 0, f.fdStatus
	}
	h, _ := f.newObj("formatDesc")
	f.fdParams = append(f.fdParams, vtCloneParams(params))
	f.log("fdCreate")
	return h, 0
}

func (f *fakeVT) decCreate(fd, ref uintptr, hardware bool) (uintptr, int32) {
	if f.decCreateSt != 0 {
		return 0, f.decCreateSt
	}
	h, _ := f.newObj("decSession")
	f.decRefs = append(f.decRefs, ref)
	f.ref = ref
	f.log("decCreate")
	return h, 0
}

func (f *fakeVT) decCanAccept(sess, fd uintptr) bool { f.log("canAccept"); return f.canAccept }

func (f *fakeVT) decHardware(uintptr) bool { return f.decHW }

func (f *fakeVT) blockBuffer(data []byte) (uintptr, int32) {
	if f.blockStatus != 0 {
		return 0, f.blockStatus
	}
	h, _ := f.newObj("blockBuffer")
	f.decSamples = append(f.decSamples, append([]byte(nil), data...))
	return h, 0
}

func (f *fakeVT) sampleBuffer(bb, fd uintptr, size int, pts int64) (uintptr, int32) {
	if f.sampleStatus != 0 {
		return 0, f.sampleStatus
	}
	h, _ := f.newObj("sampleBuffer")
	f.sampleSizes = append(f.sampleSizes, size)
	f.frames = append(f.frames, fakePTS{pts, 0})
	return h, 0
}

func (f *fakeVT) decFrame(sess, sb, counter uintptr) (uint32, int32) {
	if f.decStatus != 0 {
		return 0, f.decStatus
	}
	if f.noCallback {
		return 0, 0
	}
	d := fakeDecoded{}
	if f.decode != nil {
		d = f.decode(f, fakeFrame{counter: counter, ref: f.ref})
	}
	img := uintptr(fakeImage)
	if d.none {
		img = 0
	}
	f.run(func() { vtDecoded(f.ref, counter, d.status, d.flags, img, cmTime{}, cmTime{}) })
	if f.decTwice {
		f.run(func() { vtDecoded(f.ref, counter, vtErrBadData, 0, 0, cmTime{}, cmTime{}) })
	}
	return 0, 0
}

func (f *fakeVT) decWait(uintptr) int32 { f.log("decWait"); return 0 }

func (f *fakeVT) decInvalidate(uintptr) { f.log("decInvalidate") }

func (f *fakeVT) encoders() (bool, bool, int32) { return f.hasAVC, f.hardware, f.listStatus }

func (f *fakeVT) hardwareDecode() bool { return f.hwDecode }

// fakeImage is the handle of the decoder callback's borrowed image buffer;
// it is not in objs because the callback does not own it.
const fakeImage = 1<<31 - 1

// imagePlane returns the plane of the image: a luma ramp and a chroma
// pattern, padded to a stride wider than the picture.
func (f *fakeVT) imagePlane(plane int) ([]byte, int) {
	w, h := f.imgW, f.imgH
	if plane == 0 {
		stride := w + 16 + f.imgStrideDiff
		b := make([]byte, stride*h)
		for r := 0; r < h; r++ {
			for c := 0; c < w; c++ {
				b[r*stride+c] = byte(r + c)
			}
		}
		if f.lumaTail != 0 {
			b = b[:stride*(h-1)+f.lumaTail]
		}
		return b, stride
	}
	stride := w + 16 + f.imgStrideDiff
	b := make([]byte, stride*((h+1)/2))
	for r := 0; r < (h+1)/2; r++ {
		for c := 0; c < w; c++ {
			b[r*stride+c] = byte(200 - r - c)
		}
	}
	return b, stride
}

func fakeEncoder(t *testing.T, f *fakeVT, mut func(*encParams)) *vtEncoder {
	t.Helper()
	p := encParams{width: 64, height: 48, fps: 30, bps: 400000, profile: ConstrainedBaseline, keyInterval: 2e9}
	if mut != nil {
		mut(&p)
	}
	e, err := newVTEncoder(f, p)
	if err != nil {
		t.Fatalf("newVTEncoder: %v", err)
	}
	t.Cleanup(e.close)
	return e
}

type fakeOut struct {
	nals, params [][]byte
	key          bool
	n            int
}

// encodeOne encodes one frame of the encoder's size and returns what emit
// saw, copied.
func encodeOne(e *vtEncoder, pts int64, key bool) (fakeOut, error) {
	w, h := e.p.width, e.p.height
	y, u, v := vtPattern(w, h, int(pts))
	var out fakeOut
	err := e.encode(y, u, v, w, w/2, pts, 3000, key, func(nals, params [][]byte, k bool) {
		out.n++
		out.nals, out.params, out.key = vtCloneParams(nals), vtCloneParams(params), k
	})
	return out, err
}

func TestVTEncodeKeyAndDelta(t *testing.T) {
	f := newFakeVT(t)
	e := fakeEncoder(t, f, nil)
	k, err := encodeOne(e, 0, true)
	if err != nil || k.n != 1 || !k.key || len(k.nals) != 1 || k.nals[0][0] != 0x65 || len(k.params) != 2 {
		t.Fatalf("keyframe: %+v %v", k, err)
	}
	d, err := encodeOne(e, 3000, false)
	if err != nil || d.n != 1 || d.key || len(d.nals) != 1 || d.nals[0][0] != 0x41 {
		t.Fatalf("delta: %+v %v", d, err)
	}
	if len(d.params) != 2 {
		t.Errorf("the encoder holds the stream's parameter sets for every packet, got %d", len(d.params))
	}
	if len(f.usedPool) != 2 || f.usedPool[0] != vtUsePool {
		t.Errorf("pool use %v", f.usedPool)
	}
	if f.forced[0] != true || f.forced[1] != false {
		t.Errorf("force flags %v", f.forced)
	}
	if f.frames[1] != (fakePTS{3000, 3000}) {
		t.Errorf("pts and dur reach the session unchanged: %v", f.frames[1])
	}
	if n := f.live("pixelBuffer"); n != 0 {
		t.Errorf("%d pixel buffers live after encode", n)
	}
}

func TestVTEncodeFillsPaddedPlanes(t *testing.T) {
	f := newFakeVT(t)
	e := fakeEncoder(t, f, nil)
	w, h := 64, 48
	y, u, v := vtPattern(w, h, 5)
	if err := e.encode(y, u, v, w, w/2, 0, 3000, true, func(_, _ [][]byte, _ bool) {}); err != nil {
		t.Fatal(err)
	}
	ys, cs := f.lastStride[0], f.lastStride[1]
	for r := 0; r < h; r++ {
		if string(f.lastPB[r*ys:r*ys+w]) != string(y[r*w:(r+1)*w]) {
			t.Fatalf("luma row %d differs", r)
		}
		if f.lastPB[r*ys+w] != 0 {
			t.Fatalf("luma row %d: padding written", r)
		}
	}
	for r := 0; r < h/2; r++ {
		for c := 0; c < w/2; c++ {
			if f.lastCbCr[r*cs+2*c] != u[r*(w/2)+c] || f.lastCbCr[r*cs+2*c+1] != v[r*(w/2)+c] {
				t.Fatalf("chroma %d,%d not interleaved", r, c)
			}
		}
	}
}

func TestVTEncodeEvenStridedInput(t *testing.T) {
	f := newFakeVT(t)
	e := fakeEncoder(t, f, nil)
	w, h := 64, 48
	y := make([]byte, 80*h)
	u, v := make([]byte, 40*(h/2)), make([]byte, 40*(h/2))
	for i := range y {
		y[i] = byte(i)
	}
	if err := e.encode(y, u, v, 80, 40, 0, 3000, true, func(_, _ [][]byte, _ bool) {}); err != nil {
		t.Fatal(err)
	}
	if f.lastPB[f.lastStride[0]+3] != y[80+3] {
		t.Error("a strided source row is not copied by stride")
	}
	short := make([]byte, 10)
	if err := e.encode(short, u, v, w, w/2, 1, 3000, false, func(_, _ [][]byte, _ bool) {}); err == nil {
		t.Error("a luma plane that is too short was accepted")
	}
	if len(f.frames) != 1 {
		t.Error("the system saw a frame the engine refused")
	}
}

func TestVTSessionSetup(t *testing.T) {
	f := newFakeVT(t)
	fakeEncoder(t, f, nil)
	want := fmt.Sprintf("%d=true,%d=false,%d=400000,%d=30,%d=2", vtPropRealTime, vtPropReorder, vtPropBitrate, vtPropFPS, vtPropKeyDuration)
	if got := strings.Join(f.props, ","); got != want {
		t.Errorf("properties %s, want %s", got, want)
	}
	if len(f.profile) != 1 || f.profile[0] != vtLevelConstrainedBaseline {
		t.Errorf("profile requests %v", f.profile)
	}
	if !strings.HasSuffix(f.opList(), "prepare") {
		t.Errorf("prepare is last: %s", f.opList())
	}
	f2 := newFakeVT(t)
	e := fakeEncoder(t, f2, func(p *encParams) { p.keyInterval = -1; p.profile = High })
	for _, p := range f2.props {
		if strings.HasPrefix(p, fmt.Sprintf("%d=", vtPropKeyDuration)) {
			t.Errorf("a negative key interval sets a duration backstop: %s", p)
		}
	}
	if !strings.Contains(strings.Join(f2.props, ","), fmt.Sprintf("%d=2147483647", vtPropKeyFrames)) {
		t.Errorf("a negative key interval leaves placement to the encoder: %v", f2.props)
	}
	for _, p := range f.props {
		if strings.HasPrefix(p, fmt.Sprintf("%d=", vtPropKeyFrames)) {
			t.Errorf("a positive key interval sets a frame count: %s", p)
		}
	}
	if f2.profile[0] != vtLevelHigh || !e.hardware() {
		t.Errorf("profile %v hardware %v", f2.profile, e.hardware())
	}
	f3 := newFakeVT(t)
	fakeEncoder(t, f3, func(p *encParams) { p.profile = Main; p.keyInterval = 500e6 })
	if f3.profile[0] != vtLevelMain || !strings.Contains(strings.Join(f3.props, ","), fmt.Sprintf("%d=0.5", vtPropKeyDuration)) {
		t.Errorf("Main, 0.5 s: %v %v", f3.profile, f3.props)
	}
}

func TestVTConstrainedBaselineRefusal(t *testing.T) {
	t.Run("software retries Baseline", func(t *testing.T) {
		f := newFakeVT(t)
		f.hardware = false
		f.profileStatus[vtLevelConstrainedBaseline] = vtErrParameter
		e := fakeEncoder(t, f, nil)
		if len(f.profile) != 2 || f.profile[1] != vtLevelBaseline || e.hardware() {
			t.Errorf("requests %v hardware %v", f.profile, e.hardware())
		}
	})
	t.Run("hardware is never asked for Baseline", func(t *testing.T) {
		f := newFakeVT(t)
		f.profileStatus[vtLevelConstrainedBaseline] = vtErrParameter
		_, err := newVTEncoder(f, encParams{width: 64, height: 48, fps: 30, bps: 1e5, profile: ConstrainedBaseline, keyInterval: -1})
		if !errorsIs(err, ErrProfile) || len(f.profile) != 1 {
			t.Errorf("err %v requests %v", err, f.profile)
		}
	})
	t.Run("both refused", func(t *testing.T) {
		f := newFakeVT(t)
		f.hardware = false
		f.profileStatus[vtLevelConstrainedBaseline] = vtErrParameter
		f.profileStatus[vtLevelBaseline] = vtErrParameter
		if _, err := newVTEncoder(f, encParams{width: 64, height: 48, fps: 30, bps: 1e5, profile: ConstrainedBaseline, keyInterval: -1}); !errorsIs(err, ErrProfile) {
			t.Errorf("err %v", err)
		}
	})
	t.Run("Main refused is not retried", func(t *testing.T) {
		f := newFakeVT(t)
		f.hardware = false
		f.profileStatus[vtLevelMain] = vtErrParameter
		_, err := newVTEncoder(f, encParams{width: 64, height: 48, fps: 30, bps: 1e5, profile: Main, keyInterval: -1})
		if !errorsIs(err, ErrProfile) || len(f.profile) != 1 {
			t.Errorf("err %v requests %v", err, f.profile)
		}
	})
	t.Run("another status is a codec error", func(t *testing.T) {
		f := newFakeVT(t)
		f.hardware = false
		f.profileStatus[vtLevelConstrainedBaseline] = -12900
		_, err := newVTEncoder(f, encParams{width: 64, height: 48, fps: 30, bps: 1e5, profile: ConstrainedBaseline, keyInterval: -1})
		if !errorsIs(err, ErrCodec) || len(f.profile) != 1 {
			t.Errorf("err %v requests %v", err, f.profile)
		}
	})
}

func TestVTSetupFailuresRelease(t *testing.T) {
	steps := []struct {
		name string
		set  func(*fakeVT)
	}{
		{"RealTime", func(f *fakeVT) { f.setStatus[vtPropRealTime] = -12900 }},
		{"AllowFrameReordering", func(f *fakeVT) { f.setStatus[vtPropReorder] = -12900 }},
		{"AverageBitRate", func(f *fakeVT) { f.setStatus[vtPropBitrate] = -12900 }},
		{"ExpectedFrameRate", func(f *fakeVT) { f.setStatus[vtPropFPS] = -12900 }},
		{"MaxKeyFrameIntervalDuration", func(f *fakeVT) { f.setStatus[vtPropKeyDuration] = -12900 }},
		{"MaxKeyFrameInterval", func(f *fakeVT) { f.setStatus[vtPropKeyFrames] = -12900; f.negInterval = true }},
		{"prepare", func(f *fakeVT) { f.prepareStatus = -12900 }},
	}
	for _, s := range steps {
		t.Run(s.name, func(t *testing.T) {
			f := newFakeVT(t)
			s.set(f)
			interval := time.Duration(2e9)
			if f.negInterval {
				interval = -1
			}
			_, err := newVTEncoder(f, encParams{width: 64, height: 48, fps: 30, bps: 1e5, profile: Main, keyInterval: interval})
			if !errorsIs(err, ErrCodec) {
				t.Fatalf("err %v", err)
			}
			if f.live("encSession") != 0 {
				t.Error("the session of a failed setup is not released")
			}
			if got := f.opList(); !strings.Contains(got, "encInvalidate") || !strings.HasSuffix(got, "release(encSession)") {
				t.Errorf("a failed setup tears the session down in order: %s", got)
			}
		})
	}
	f := newFakeVT(t)
	f.createStatus = vtErrCouldNotFindEnc
	_, err := newVTEncoder(f, encParams{width: 64, height: 48, fps: 30, bps: 1e5, profile: Main})
	var u *UnavailableError
	if !errorsIs(err, ErrUnavailable) || !errors.As(err, &u) || u.Code != vtErrCouldNotFindEnc {
		t.Errorf("no encoder: %v", err)
	}
}

func TestVTEncoderClose(t *testing.T) {
	f := newFakeVT(t)
	e := fakeEncoder(t, f, nil)
	f.mu.Lock()
	f.ops = nil
	f.mu.Unlock()
	e.close()
	e.close()
	if got, want := f.opList(), "encComplete(all),encInvalidate,release(encSession)"; got != want {
		t.Errorf("close order %q, want %q", got, want)
	}
	if err := e.setBitrate(1000); err == nil {
		t.Error("setBitrate on a closed encoder")
	}
	if _, err := encodeOne(e, 0, true); err == nil {
		t.Error("encode on a closed encoder")
	}
}

func TestVTSetBitrate(t *testing.T) {
	f := newFakeVT(t)
	e := fakeEncoder(t, f, nil)
	if err := e.setBitrate(250000); err != nil {
		t.Fatal(err)
	}
	if last := f.props[len(f.props)-1]; last != fmt.Sprintf("%d=250000", vtPropBitrate) {
		t.Errorf("last property %s", last)
	}
	f.setStatus[vtPropBitrate] = vtErrParameter
	if err := e.setBitrate(1); !errorsIs(err, ErrCodec) {
		t.Errorf("err %v", err)
	}
}

func TestVTEncoderErrorsReleaseBuffers(t *testing.T) {
	cases := []struct {
		name string
		set  func(*fakeVT)
		kind error
	}{
		{"pixel buffer", func(f *fakeVT) { f.pbStatus = -6660 }, ErrCodec},
		{"lock", func(f *fakeVT) { f.lockStatus = -6661 }, ErrCodec},
		{"format", func(f *fakeVT) { f.pbFormat = 0x42474241 }, ErrCodec},
		{"size", func(f *fakeVT) { f.pbW, f.pbH = 32, 32 }, ErrCodec},
		{"encode", func(f *fakeVT) { f.frameStatus = vtErrInvalidSession }, ErrCodec},
		{"complete", func(f *fakeVT) { f.completeSt = -12915 }, ErrUnavailable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeVT(t)
			e := fakeEncoder(t, f, nil)
			c.set(f)
			if _, err := encodeOne(e, 0, true); !errorsIs(err, c.kind) {
				t.Errorf("err %v, want %v", err, c.kind)
			}
			if f.live("pixelBuffer") != 0 {
				t.Error("pixel buffer leaked")
			}
		})
	}
}

func TestVTEncoderOutputRules(t *testing.T) {
	k := func(s *fakeSample) func(*fakeVT, fakeFrame) fakeEncoded {
		return func(*fakeVT, fakeFrame) fakeEncoded { return fakeEncoded{sample: s} }
	}
	good := func(lenSize int) *fakeSample {
		return &fakeSample{avcc: avccOf(lenSize, fakeIDR), params: [][]byte{fakeSPS, fakePPS}, lenSize: lenSize}
	}
	t.Run("length sizes 1 2 and 4", func(t *testing.T) {
		for _, ls := range []int{1, 2, 4} {
			f := newFakeVT(t)
			f.encode = k(good(ls))
			e := fakeEncoder(t, f, nil)
			out, err := encodeOne(e, 0, true)
			if err != nil || out.n != 1 || len(out.nals) != 1 {
				t.Errorf("length size %d: %+v %v", ls, out, err)
			}
		}
	})
	t.Run("length size 3", func(t *testing.T) {
		f := newFakeVT(t)
		f.encode = k(good(3))
		e := fakeEncoder(t, f, nil)
		if _, err := encodeOne(e, 0, true); !errorsIs(err, ErrCodec) {
			t.Errorf("err %v", err)
		}
	})
	bad := map[string][]byte{
		"zero length":      {0, 0, 0, 0, 0x65},
		"zero length last": append(avccOf(4, fakeIDR), 0, 0, 0, 0),
		"past the end":     {0, 0, 0, 9, 0x65, 1},
		"huge length":      {0xff, 0xff, 0xff, 0xff, 0x65},
		"truncated prefix": {0, 0, 0, 1, 0x65, 0, 0},
	}
	for name, b := range bad {
		t.Run(name, func(t *testing.T) {
			f := newFakeVT(t)
			s := good(4)
			s.avcc = b
			f.encode = k(s)
			e := fakeEncoder(t, f, nil)
			out, err := encodeOne(e, 0, true)
			if !errorsIs(err, ErrCodec) || out.n != 0 {
				t.Errorf("err %v emitted %d", err, out.n)
			}
		})
	}
	t.Run("sync attachment absent means key", func(t *testing.T) {
		f := newFakeVT(t)
		s := good(4)
		s.absent = true
		f.encode = k(s)
		e := fakeEncoder(t, f, nil)
		if out, err := encodeOne(e, 0, false); err != nil || !out.key {
			t.Errorf("%+v %v", out, err)
		}
	})
	t.Run("NotSync means delta", func(t *testing.T) {
		f := newFakeVT(t)
		e := fakeEncoder(t, f, nil)
		if _, err := encodeOne(e, 0, true); err != nil {
			t.Fatal(err)
		}
		s := good(4)
		s.notSync = true
		f.encode = k(s)
		if out, err := encodeOne(e, 3000, false); err != nil || out.key {
			t.Errorf("%+v %v", out, err)
		}
	})
	t.Run("keyframe without parameter sets", func(t *testing.T) {
		for name, ps := range map[string][][]byte{"none": nil, "no PPS": {fakeSPS}, "empty unit": {fakeSPS, {}}} {
			f := newFakeVT(t)
			s := good(4)
			s.params = ps
			f.encode = k(s)
			e := fakeEncoder(t, f, nil)
			if _, err := encodeOne(e, 0, true); !errorsIs(err, ErrCodec) {
				t.Errorf("%s: err %v", name, err)
			}
		}
	})
	t.Run("parameter set read fails", func(t *testing.T) {
		f := newFakeVT(t)
		s := good(4)
		s.paramSt = -50
		f.encode = k(s)
		e := fakeEncoder(t, f, nil)
		if _, err := encodeOne(e, 0, true); !errorsIs(err, ErrCodec) {
			t.Errorf("err %v", err)
		}
	})
	t.Run("size bound before allocation", func(t *testing.T) {
		f := newFakeVT(t)
		s := good(4)
		s.size = DefaultMaxPacketBytes + 1
		f.encode = k(s)
		e := fakeEncoder(t, f, nil)
		if _, err := encodeOne(e, 0, true); !errorsIs(err, ErrCodec) {
			t.Errorf("err %v", err)
		}
		if strings.Contains(f.opList(), "sbCopy") {
			t.Error("the oversized sample was copied")
		}
		if cap(e.slot.avcc) != 0 {
			t.Error("a buffer was allocated for the oversized sample")
		}
	})
	t.Run("empty sample", func(t *testing.T) {
		f := newFakeVT(t)
		s := good(4)
		s.avcc = nil
		f.encode = k(s)
		e := fakeEncoder(t, f, nil)
		if _, err := encodeOne(e, 0, true); !errorsIs(err, ErrCodec) {
			t.Errorf("err %v", err)
		}
	})
	t.Run("copy fails", func(t *testing.T) {
		f := newFakeVT(t)
		s := good(4)
		s.copySt = -1
		f.encode = k(s)
		e := fakeEncoder(t, f, nil)
		if _, err := encodeOne(e, 0, true); !errorsIs(err, ErrCodec) {
			t.Errorf("err %v", err)
		}
	})
	t.Run("callback status", func(t *testing.T) {
		f := newFakeVT(t)
		f.encode = func(*fakeVT, fakeFrame) fakeEncoded { return fakeEncoded{status: vtErrBadData} }
		e := fakeEncoder(t, f, nil)
		if _, err := encodeOne(e, 0, true); !errorsIs(err, ErrCorrupt) {
			t.Errorf("err %v", err)
		}
	})
	t.Run("dropped by flag", func(t *testing.T) {
		f := newFakeVT(t)
		f.encode = func(*fakeVT, fakeFrame) fakeEncoded {
			return fakeEncoded{flags: vtInfoFrameDropped, sample: good(4)}
		}
		e := fakeEncoder(t, f, nil)
		if out, err := encodeOne(e, 0, true); err != nil || out.n != 0 {
			t.Errorf("%+v %v", out, err)
		}
	})
	t.Run("dropped by nil sample", func(t *testing.T) {
		f := newFakeVT(t)
		f.encode = func(*fakeVT, fakeFrame) fakeEncoded { return fakeEncoded{nilSamp: true} }
		e := fakeEncoder(t, f, nil)
		if out, err := encodeOne(e, 0, true); err != nil || out.n != 0 {
			t.Errorf("%+v %v", out, err)
		}
	})
	t.Run("no callback", func(t *testing.T) {
		f := newFakeVT(t)
		f.noCallback = true
		e := fakeEncoder(t, f, nil)
		if _, err := encodeOne(e, 0, true); !errorsIs(err, ErrCodec) {
			t.Errorf("err %v", err)
		}
	})
	t.Run("a panic in the callback is recovered", func(t *testing.T) {
		f := newFakeVT(t)
		s := good(4)
		s.panics = true
		f.encode = k(s)
		e := fakeEncoder(t, f, nil)
		if _, err := encodeOne(e, 0, true); !errorsIs(err, ErrCodec) {
			t.Errorf("err %v", err)
		}
		f.encode = nil
		if out, err := encodeOne(e, 3000, true); err != nil || out.n != 1 {
			t.Errorf("the encoder does not recover: %+v %v", out, err)
		}
	})
}

func TestVTCallbackHandles(t *testing.T) {
	f := newFakeVT(t)
	e := fakeEncoder(t, f, nil)
	before := e.slot.staleCount()
	vtEncoded(1<<30, 1, 0, 0, 0)
	vtDecoded(1<<30, 1, 0, 0, 0, cmTime{}, cmTime{})
	if e.slot.staleCount() != before {
		t.Error("a callback with an unknown handle reached a session")
	}
	// A late callback for an old frame is dropped and counted.
	if _, err := encodeOne(e, 0, true); err != nil {
		t.Fatal(err)
	}
	vtEncoded(e.ref, e.counter, 0, 0, 0)
	vtEncoded(e.ref, e.counter-5, vtErrBadData, 0, 0)
	if got := e.slot.staleCount(); got != before+2 {
		t.Errorf("stale count %d, want %d", got, before+2)
	}
	if out, err := encodeOne(e, 3000, false); err != nil || out.n != 1 {
		t.Errorf("a stale callback disturbed the next frame: %+v %v", out, err)
	}
	e.close()
	vtEncoded(e.ref, e.counter, 0, 0, 0)
}

// The callback must not wait for anything the waiting call holds. The
// encoder waits in CompleteFrames with its slot unlocked; holding it there
// would deadlock the callback, which runs on another goroutine.
func TestVTCallbackNeverWaitsForTheCaller(t *testing.T) {
	f := newFakeVT(t)
	e := fakeEncoder(t, f, nil)
	done := make(chan error, 1)
	go func() { _, err := encodeOne(e, 0, true); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-timeAfter(5):
		t.Fatal("Encode did not return: the callback is blocked behind the caller")
	}
}

func TestVTRegistry(t *testing.T) {
	f := newFakeVT(t)
	base := vtLive()
	e := fakeEncoder(t, f, nil)
	d, err := newVTDecoder(f, decParams{maxPixels: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if vtLive() != base+2 || e.ref == d.ref || e.ref == 0 {
		t.Errorf("handles: live %d, refs %d and %d", vtLive()-base, e.ref, d.ref)
	}
	if vtLookup(e.ref) != e.slot || vtLookup(d.ref) != d.slot {
		t.Error("lookup")
	}
	d.close()
	e.close()
	if vtLive() != base {
		t.Error("handles left after close")
	}
}

func fakeDecoder(t *testing.T, f *fakeVT, w, h int) *vtDecoder {
	t.Helper()
	f.imgW, f.imgH, f.imgFmt, f.imgPlanes = w, h, vtPixelFormat420v, 2
	d, err := newVTDecoder(f, decParams{maxPixels: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.close)
	return d
}

func fakeAU(key bool, w, h int) accessUnit {
	au := accessUnit{key: key, slices: [][]byte{fakeP}, width: w, height: h, codedWidth: (w + 15) &^ 15, codedHeight: (h + 15) &^ 15}
	if key {
		au.sps, au.pps, au.slices = [][]byte{fakeSPS}, [][]byte{fakePPS}, [][]byte{fakeIDR}
	}
	return au
}

func TestVTDecodeKeyAndDelta(t *testing.T) {
	f := newFakeVT(t)
	d := fakeDecoder(t, f, 64, 48)
	pic, ok, err := d.decode(fakeAU(true, 64, 48))
	if err != nil || !ok {
		t.Fatalf("keyframe: ok %v err %v", ok, err)
	}
	if !pic.nv12 || pic.width != 64 || pic.height != 48 || pic.yStride != 64 || pic.uvStride != 64 || pic.v != nil {
		t.Fatalf("picture %+v", pic)
	}
	if pic.y[2*64+5] != byte(2+5) || pic.u[3*64+9] != byte(200-3-9) {
		t.Errorf("planes are not the image's, packed to the width: %d %d", pic.y[2*64+5], pic.u[3*64+9])
	}
	if _, ok, err := d.decode(fakeAU(false, 64, 48)); err != nil || !ok {
		t.Fatalf("delta: ok %v err %v", ok, err)
	}
	if !d.hardware() {
		t.Error("hardware flag not read")
	}
	if got := f.fdParams; len(got) != 1 || len(got[0]) != 2 || !bytes.Equal(got[0][0], fakeSPS) || !bytes.Equal(got[0][1], fakePPS) {
		t.Errorf("format description parameter sets %v", got)
	}
	want := avccOf(4, fakeIDR)
	if !bytes.Equal(f.decSamples[0], want) || !bytes.Equal(f.decSamples[1], avccOf(4, fakeP)) {
		t.Errorf("AVCC samples %x", f.decSamples)
	}
	if f.sampleSizes[0] != len(want) {
		t.Errorf("sample size %v", f.sampleSizes)
	}
	if f.frames[0].pts >= f.frames[1].pts {
		t.Errorf("sample times do not increase: %v", f.frames)
	}
	for _, k := range []string{"blockBuffer", "sampleBuffer"} {
		if f.live(k) != 0 {
			t.Errorf("%s leaked", k)
		}
	}
	if ops := f.opList(); strings.Count(ops, "imageLock") != 2 || strings.Count(ops, "imageUnlock") != 2 {
		t.Errorf("image lock balance: %s", ops)
	}
}

func TestVTDecodeMultipleSlices(t *testing.T) {
	f := newFakeVT(t)
	d := fakeDecoder(t, f, 64, 48)
	au := fakeAU(true, 64, 48)
	au.slices = [][]byte{fakeIDR, fakeIDR[:4], fakeIDR[:5]}
	if _, ok, err := d.decode(au); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if !bytes.Equal(f.decSamples[0], avccOf(4, au.slices...)) {
		t.Errorf("%x", f.decSamples[0])
	}
}

func TestVTDecoderFormatDescriptionLifecycle(t *testing.T) {
	f := newFakeVT(t)
	d := fakeDecoder(t, f, 64, 48)
	d.decode(fakeAU(true, 64, 48))
	d.decode(fakeAU(true, 64, 48))
	if strings.Count(f.opList(), "fdCreate") != 1 || strings.Count(f.opList(), "decCreate") != 1 {
		t.Errorf("a keyframe with the same parameter sets rebuilds: %s", f.opList())
	}
	// New parameter sets the session accepts: new description, same session.
	au := fakeAU(true, 64, 48)
	au.pps = [][]byte{{0x68, 0xce, 0x3c, 0x81}}
	if _, ok, err := d.decode(au); err != nil || !ok {
		t.Fatal(ok, err)
	}
	ops := f.opList()
	if strings.Count(ops, "fdCreate") != 2 || strings.Count(ops, "decCreate") != 1 || strings.Contains(ops, "release(decSession)") {
		t.Errorf("accepted change: %s", ops)
	}
	if f.live("formatDesc") != 1 {
		t.Errorf("%d format descriptions live", f.live("formatDesc"))
	}
	// New parameter sets the session cannot accept: a new session, the old one
	// drained, invalidated and released first.
	f.canAccept = false
	f.mu.Lock()
	f.ops = nil
	f.mu.Unlock()
	au.pps = [][]byte{{0x68, 0xce, 0x3c, 0x82}}
	if _, ok, err := d.decode(au); err != nil || !ok {
		t.Fatal(ok, err)
	}
	want := "fdCreate,canAccept,decWait,decInvalidate,release(decSession),release(formatDesc),decCreate"
	if got := f.opList(); !strings.HasPrefix(got, want) {
		t.Errorf("ops %s\nwant prefix %s", got, want)
	}
	if f.live("decSession") != 1 || f.live("formatDesc") != 1 {
		t.Error("live objects")
	}
}

func TestVTDecoderErrors(t *testing.T) {
	t.Run("corrupt keeps the session", func(t *testing.T) {
		f := newFakeVT(t)
		d := fakeDecoder(t, f, 64, 48)
		d.decode(fakeAU(true, 64, 48))
		f.decStatus = vtErrBadData
		if _, _, err := d.decode(fakeAU(false, 64, 48)); !errorsIs(err, ErrCorrupt) {
			t.Fatalf("err %v", err)
		}
		f.decStatus = 0
		if _, ok, err := d.decode(fakeAU(false, 64, 48)); err != nil || !ok {
			t.Errorf("the session does not recover: %v %v", ok, err)
		}
		if strings.Count(f.opList(), "decCreate") != 1 {
			t.Error("a session was recreated after bad data")
		}
	})
	t.Run("callback status", func(t *testing.T) {
		f := newFakeVT(t)
		d := fakeDecoder(t, f, 64, 48)
		d.decode(fakeAU(true, 64, 48))
		f.decode = func(*fakeVT, fakeFrame) fakeDecoded { return fakeDecoded{status: vtErrBadData} }
		if _, _, err := d.decode(fakeAU(false, 64, 48)); !errorsIs(err, ErrCorrupt) {
			t.Fatalf("err %v", err)
		}
		if d.sess == 0 {
			t.Error("bad data dropped the session")
		}
	})
	for _, st := range []int32{vtErrInvalidSession, vtErrDecoderMalfunc, -12000} {
		t.Run(fmt.Sprint("session lost ", st), func(t *testing.T) {
			f := newFakeVT(t)
			d := fakeDecoder(t, f, 64, 48)
			d.decode(fakeAU(true, 64, 48))
			f.decStatus = st
			if _, _, err := d.decode(fakeAU(false, 64, 48)); !errorsIs(err, ErrCodec) {
				t.Fatalf("err %v", err)
			}
			if f.live("decSession") != 0 {
				t.Error("the failed session is kept")
			}
			f.decStatus = 0
			if _, _, err := d.decode(fakeAU(false, 64, 48)); !errorsIs(err, ErrCodec) {
				t.Errorf("a delta without a session: %v", err)
			}
			if _, ok, err := d.decode(fakeAU(true, 64, 48)); err != nil || !ok {
				t.Errorf("recreation at the next keyframe: %v %v", ok, err)
			}
			if strings.Count(f.opList(), "fdCreate") != 1 {
				t.Error("the description of the same parameter sets was rebuilt")
			}
		})
	}
	t.Run("callback panic", func(t *testing.T) {
		f := newFakeVT(t)
		d := fakeDecoder(t, f, 64, 48)
		d.decode(fakeAU(true, 64, 48))
		f.panicInfo = true
		if _, _, err := d.decode(fakeAU(false, 64, 48)); !errorsIs(err, ErrCodec) {
			t.Errorf("err %v", err)
		}
		f.panicInfo = false
		if _, ok, err := d.decode(fakeAU(true, 64, 48)); err != nil || !ok {
			t.Errorf("the decoder does not recover: %v %v", ok, err)
		}
	})
	t.Run("dropped", func(t *testing.T) {
		f := newFakeVT(t)
		d := fakeDecoder(t, f, 64, 48)
		f.decode = func(*fakeVT, fakeFrame) fakeDecoded { return fakeDecoded{flags: vtInfoFrameDropped} }
		if _, ok, err := d.decode(fakeAU(true, 64, 48)); ok || err != nil {
			t.Errorf("ok %v err %v", ok, err)
		}
		f.decode = func(*fakeVT, fakeFrame) fakeDecoded { return fakeDecoded{none: true} }
		if _, ok, err := d.decode(fakeAU(false, 64, 48)); ok || err != nil {
			t.Errorf("nil image: ok %v err %v", ok, err)
		}
	})
	t.Run("no callback", func(t *testing.T) {
		f := newFakeVT(t)
		d := fakeDecoder(t, f, 64, 48)
		f.noCallback = true
		if _, _, err := d.decode(fakeAU(true, 64, 48)); !errorsIs(err, ErrCodec) {
			t.Errorf("err %v", err)
		}
	})
	t.Run("setup statuses", func(t *testing.T) {
		for name, set := range map[string]func(*fakeVT){
			"format description": func(f *fakeVT) { f.fdStatus = -50 },
			"session":            func(f *fakeVT) { f.decCreateSt = vtErrCouldNotFindDec },
			"block buffer":       func(f *fakeVT) { f.blockStatus = -50 },
			"sample buffer":      func(f *fakeVT) { f.sampleStatus = -50 },
		} {
			f := newFakeVT(t)
			d := fakeDecoder(t, f, 64, 48)
			set(f)
			_, _, err := d.decode(fakeAU(true, 64, 48))
			if err == nil {
				t.Errorf("%s: no error", name)
			}
			var u *UnavailableError
			if name == "session" && (!errorsIs(err, ErrUnavailable) || !errors.As(err, &u)) {
				t.Errorf("session: %v", err)
			}
			for _, k := range []string{"blockBuffer", "sampleBuffer"} {
				if f.live(k) != 0 {
					t.Errorf("%s: %s leaked", name, k)
				}
			}
		}
	})
	t.Run("sizes", func(t *testing.T) {
		f := newFakeVT(t)
		d := fakeDecoder(t, f, 64, 48)
		// 1024x1024 coded is 1<<20 pixels, the bound; a 48x48 crop of 1040x1024 is above it.
		cropped := fakeAU(true, 48, 48)
		cropped.codedWidth, cropped.codedHeight = 1040, 1024
		for _, au := range []accessUnit{fakeAU(true, 0, 48), fakeAU(true, 2000, 2000), cropped} {
			if _, _, err := d.decode(au); !errorsIs(err, ErrTooLarge) {
				t.Errorf("%dx%d coded %dx%d: %v", au.width, au.height, au.codedWidth, au.codedHeight, err)
			}
		}
		empty := fakeAU(true, 64, 48)
		empty.slices = nil
		if _, _, err := d.decode(empty); !errorsIs(err, ErrTooLarge) {
			t.Errorf("no slices: %v", err)
		}
		if f.imgReads != 0 || len(f.decSamples) != 0 {
			t.Error("the system saw a refused unit")
		}
	})
}

// Format, plane count and size of the decoded image are checked before any
// plane is read or any buffer allocated.
func TestVTDecoderImageChecks(t *testing.T) {
	cases := map[string]func(*fakeVT){
		"pixel format": func(f *fakeVT) { f.imgFmt = 0x34323066 },
		"plane count":  func(f *fakeVT) { f.imgPlanes = 3 },
		"width":        func(f *fakeVT) { f.imgW = 80 },
		"height":       func(f *fakeVT) { f.imgH = 32 },
	}
	for name, set := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFakeVT(t)
			d := fakeDecoder(t, f, 64, 48)
			set(f)
			if _, _, err := d.decode(fakeAU(true, 64, 48)); !errorsIs(err, ErrCodec) {
				t.Fatalf("err %v", err)
			}
			if f.imgReads != 0 || d.slot.y != nil || d.slot.uv != nil {
				t.Error("planes were read or allocated before the checks")
			}
			if f.live("decSession") != 0 {
				t.Error("the session survived a malformed picture")
			}
		})
	}
	t.Run("lock fails", func(t *testing.T) {
		f := newFakeVT(t)
		d := fakeDecoder(t, f, 64, 48)
		f.lockStatus = -1
		if _, _, err := d.decode(fakeAU(true, 64, 48)); !errorsIs(err, ErrCodec) {
			t.Errorf("err %v", err)
		}
	})
	t.Run("plane too small", func(t *testing.T) {
		f := newFakeVT(t)
		d := fakeDecoder(t, f, 64, 48)
		f.imgStrideDiff = -64 // stride 16 for width 64
		if _, _, err := d.decode(fakeAU(true, 64, 48)); !errorsIs(err, ErrCodec) {
			t.Errorf("err %v", err)
		}
		if strings.Count(f.opList(), "imageUnlock") != strings.Count(f.opList(), "imageLock") {
			t.Errorf("unbalanced: %s", f.opList())
		}
	})
	// A plane that ends exactly with the last row fits; one byte less is a layout
	// error found before any copy (not a recovered panic).
	t.Run("plane ends at the last row", func(t *testing.T) {
		for _, tail := range []int{64, 63} {
			f := newFakeVT(t)
			d := fakeDecoder(t, f, 64, 48)
			f.lumaTail = tail
			_, ok, err := d.decode(fakeAU(true, 64, 48))
			if tail == 64 && (err != nil || !ok) {
				t.Errorf("tight plane: %v", err)
			}
			if tail == 63 && (!errorsIs(err, ErrCodec) || !strings.Contains(err.Error(), "decoded plane layout")) {
				t.Errorf("plane one byte short: %v", err)
			}
		}
	})
}

// TestVTEncodeTightPlanes is the encoder side: fill has no recover, so a
// plane one byte short of the last row must be refused before the copy.
func TestVTEncodeTightPlanes(t *testing.T) {
	for _, tail := range []int{64, 63} {
		f := newFakeVT(t)
		f.lumaTail = tail
		e := fakeEncoder(t, f, nil)
		_, err := encodeOne(e, 0, true)
		if tail == 64 && err != nil {
			t.Errorf("tight plane: %v", err)
		}
		if tail == 63 && (!errorsIs(err, ErrCodec) || !strings.Contains(err.Error(), "pixel buffer layout")) {
			t.Errorf("plane one byte short: %v", err)
		}
		if f.live("pixelBuffer") != 0 {
			t.Error("pixel buffer leaked")
		}
	}
}

// TestVTDuplicateDecodeCallback sends a second callback for the frame that
// was already delivered: it is counted stale and changes nothing.
func TestVTDuplicateDecodeCallback(t *testing.T) {
	f := newFakeVT(t)
	d := fakeDecoder(t, f, 64, 48)
	f.decTwice = true
	pic, ok, err := d.decode(fakeAU(true, 64, 48))
	if err != nil || !ok || pic.y[2*64+5] != byte(2+5) {
		t.Fatalf("ok %v err %v", ok, err)
	}
	if n := d.slot.staleCount(); n != 1 {
		t.Fatalf("%d stale callbacks, want 1", n)
	}
}

func TestVTDecoderClose(t *testing.T) {
	f := newFakeVT(t)
	d := fakeDecoder(t, f, 64, 48)
	d.decode(fakeAU(true, 64, 48))
	f.mu.Lock()
	f.ops = nil
	f.mu.Unlock()
	d.close()
	d.close()
	if got, want := f.opList(), "decWait,decInvalidate,release(decSession),release(formatDesc)"; got != want {
		t.Errorf("close order %q, want %q", got, want)
	}
	if _, _, err := d.decode(fakeAU(true, 64, 48)); !errorsIs(err, ErrCodec) {
		t.Errorf("decode after close: %v", err)
	}
	// Closing a decoder that never saw a keyframe releases nothing.
	g := newFakeVT(t)
	e, _ := newVTDecoder(g, decParams{maxPixels: 1 << 20})
	e.close()
	if g.opList() != "" {
		t.Errorf("ops %s", g.opList())
	}
}

func TestVTDecoderHardwareFlag(t *testing.T) {
	f := newFakeVT(t)
	f.decHW = false
	d := fakeDecoder(t, f, 64, 48)
	if d.hardware() {
		t.Error("hardware before the first picture")
	}
	d.decode(fakeAU(true, 64, 48))
	if d.hardware() {
		t.Error("software session reported as hardware")
	}
}

func TestVTCapabilities(t *testing.T) {
	probe := func(f *fakeVT) Limits {
		e := &vtEngine{load: func() (vtOS, error) { return f, nil }}
		return e.caps()
	}
	t.Run("hardware", func(t *testing.T) {
		f := newFakeVT(t)
		l := probe(f)
		if !l.Encode.Available || !l.Encode.Hardware || !l.Decode.Available || !l.Decode.Hardware ||
			l.Encode.Engine != "VideoToolbox" || len(l.Encode.Profiles) != 3 || len(l.Decode.Profiles) != 3 ||
			l.Encode.MaxWidth != 0 || l.Decode.MaxHeight != 0 || l.Encode.Err != nil {
			t.Errorf("%+v", l)
		}
	})
	t.Run("software", func(t *testing.T) {
		f := newFakeVT(t)
		f.hardware, f.hwDecode = false, false
		l := probe(f)
		if !l.Encode.Available || l.Encode.Hardware || l.Decode.Hardware {
			t.Errorf("%+v", l)
		}
	})
	t.Run("no encoder", func(t *testing.T) {
		f := newFakeVT(t)
		f.hasAVC = false
		l := probe(f)
		var u *UnavailableError
		if l.Encode.Available || !errors.As(l.Encode.Err, &u) || !errorsIs(l.Encode.Err, ErrUnavailable) || u.Engine != "VideoToolbox" || !l.Decode.Available {
			t.Errorf("%+v", l)
		}
	})
	t.Run("list fails", func(t *testing.T) {
		f := newFakeVT(t)
		f.listStatus = -5
		l := probe(f)
		var u *UnavailableError
		if l.Encode.Available || !errors.As(l.Encode.Err, &u) || u.Code != -5 {
			t.Errorf("%+v", l)
		}
	})
	t.Run("no decoder", func(t *testing.T) {
		f := newFakeVT(t)
		f.decCreateSt = vtErrCouldNotFindDec
		l := probe(f)
		var u *UnavailableError
		if l.Decode.Available || !errors.As(l.Decode.Err, &u) || u.Code != vtErrCouldNotFindDec || !l.Encode.Available {
			t.Errorf("%+v", l)
		}
		if f.live("formatDesc") != 0 {
			t.Error("probe leaked")
		}
	})
	t.Run("format description fails", func(t *testing.T) {
		f := newFakeVT(t)
		f.fdStatus = -50
		if l := probe(f); l.Decode.Available {
			t.Errorf("%+v", l)
		}
	})
	t.Run("frameworks missing", func(t *testing.T) {
		e := &vtEngine{load: func() (vtOS, error) { return nil, errors.New("dlopen failed") }}
		l := e.caps()
		if l.Encode.Available || l.Decode.Available || !errorsIs(l.Encode.Err, ErrUnavailable) || !errorsIs(l.Decode.Err, ErrUnavailable) {
			t.Errorf("%+v", l)
		}
		if _, err := e.newEncoder(encParams{}); !errorsIs(err, ErrUnavailable) {
			t.Errorf("newEncoder: %v", err)
		}
		if _, err := e.newDecoder(decParams{}); !errorsIs(err, ErrUnavailable) {
			t.Errorf("newDecoder: %v", err)
		}
	})
	t.Run("probe session is freed in order", func(t *testing.T) {
		f := newFakeVT(t)
		probe(f)
		if got := f.opList(); !strings.Contains(got, "decCreate,decInvalidate,release(decSession)") {
			t.Errorf("%s", got)
		}
	})
}

func TestVTStatusMapping(t *testing.T) {
	cases := []struct {
		st   int32
		kind error
	}{
		{vtErrBadData, ErrCorrupt},
		{vtErrParameter, ErrCodec},
		{vtErrInvalidSession, ErrCodec},
		{vtErrDecoderMalfunc, ErrCodec},
		{vtErrCouldNotFindDec, ErrUnavailable},
		{vtErrCouldNotFindEnc, ErrUnavailable},
		{vtErrDecoderNotNow, ErrUnavailable},
		{vtErrEncoderNotNow, ErrUnavailable},
		{-1, ErrCodec},
	}
	for _, c := range cases {
		err := vtError("op", c.st)
		var ce *codecError
		var u *UnavailableError
		switch {
		case c.kind == ErrUnavailable:
			// The docs promise an *UnavailableError for every ErrUnavailable.
			if !errorsIs(err, c.kind) || !errors.As(err, &u) || u.Code != int64(c.st) || u.Engine != "VideoToolbox" {
				t.Errorf("%d: %v", c.st, err)
			}
		case !errorsIs(err, c.kind) || !errors.As(err, &ce) || ce.code != int64(c.st) || ce.engine != "VideoToolbox":
			t.Errorf("%d: %v", c.st, err)
		}
		if msg := err.Error(); !strings.HasPrefix(msg, "kolovideo/h264: ") || !strings.Contains(msg, fmt.Sprint(c.st)) {
			t.Errorf("message %q", msg)
		}
	}
}

func TestVTEngineFacts(t *testing.T) {
	e := &vtEngine{load: func() (vtOS, error) { return newFakeVT(t), nil }}
	if e.name() != "VideoToolbox" {
		t.Error(e.name())
	}
	enc, err := e.newEncoder(encParams{width: 64, height: 48, fps: 30, bps: 1e5, profile: High, keyInterval: -1})
	if err != nil {
		t.Fatal(err)
	}
	if enc.params() != nil || !enc.hardware() {
		t.Error("params or hardware")
	}
	enc.close()
	dec, err := e.newDecoder(decParams{maxPixels: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	dec.close()
}

func TestVTConcurrentInstances(t *testing.T) {
	const n = 8
	fakes := make([]*fakeVT, n)
	for i := range fakes {
		fakes[i] = newFakeVT(t)
	}
	var wg sync.WaitGroup
	for _, f := range fakes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e, err := newVTEncoder(f, encParams{width: 64, height: 48, fps: 30, bps: 1e5, profile: Main, keyInterval: -1})
			if err != nil {
				t.Error(err)
				return
			}
			defer e.close()
			for i := 0; i < 20; i++ {
				if out, err := encodeOne(e, int64(i)*3000, i == 0); err != nil || out.n != 1 || out.key != (i == 0) {
					t.Errorf("frame %d: %+v %v", i, out, err)
					return
				}
			}
		}()
	}
	wg.Wait()
}
