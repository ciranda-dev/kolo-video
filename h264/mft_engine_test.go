package h264

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	kolovideo "github.com/ciranda-dev/kolo-video"
)

func mftParams() encParams {
	return encParams{width: 64, height: 48, fps: 30, bps: 1000000, profile: ConstrainedBaseline, keyInterval: 2 * time.Second}
}

// mftNewEnc opens an encoder on the fake; the caller closes it.
func mftNewEnc(t *testing.T, f *mftFake, p encParams) encImpl {
	t.Helper()
	e, err := mftEngine{sys: f}.newEncoder(p)
	if err != nil {
		t.Fatalf("newEncoder: %v", err)
	}
	return e
}

func mftNewDec(t *testing.T, f *mftFake) decImpl {
	t.Helper()
	d, err := mftEngine{sys: f}.newDecoder(decParams{maxPixels: DefaultMaxPixels})
	if err != nil {
		t.Fatalf("newDecoder: %v", err)
	}
	return d
}

// mftPicture builds an I420 picture of w x h with tight strides and a
// pattern that tells every plane and row apart.
func mftPicture(w, h int) (y, u, v []byte) {
	y = make([]byte, w*h)
	u, v = make([]byte, w*h/4), make([]byte, w*h/4)
	for r := 0; r < h; r++ {
		for c := 0; c < w; c++ {
			y[r*w+c] = byte(c + 5*r)
		}
	}
	for i := range u {
		u[i], v[i] = byte(i+3), byte(255-i)
	}
	return y, u, v
}

type mftEmit struct {
	nals, params [][]byte
	key          bool
	n            int
}

func mftEncode(e encImpl, y, u, v []byte, ys, uvs int, pts, dur int64, key bool) (mftEmit, error) {
	var r mftEmit
	err := e.encode(y, u, v, ys, uvs, pts, dur, key, func(nals, params [][]byte, k bool) {
		r.n++
		r.nals, r.params, r.key = cloneUnits(nals), cloneUnits(params), k
	})
	return r, err
}

func TestMFTEncoderSetup(t *testing.T) {
	f := newMFTFake()
	e := mftNewEnc(t, f, mftParams())
	ev := f.events()
	order := []string{"create enc", "codec u32 RateControlMode=0", "codec u32 MeanBitRate=1000000", "codec u32 BPictureCount=0",
		"codec u32 GOPSize=60", "codec bool LowLatency=true", "setOutput H264", "enc currentOutput", "setInput IYUV",
		"message begin streaming", "message start of stream"}
	last := -1
	for _, want := range order {
		i := slices.Index(ev, want)
		if i < 0 || i < last {
			t.Fatalf("%q at %d after %d, log %v", want, i, last, ev)
		}
		last = i
	}
	// The offered YV12 and NV12 types are skipped, never set.
	for _, bad := range []string{"setInput YV12", "setInput NV12"} {
		if f.has(bad) {
			t.Errorf("the engine set %q", bad)
		}
	}
	x := f.xfs[0]
	rate := pack(30000, 1000)
	for _, c := range []struct {
		key  mftGUID
		want any
	}{{mtMPEG2Profile, uint32(256)}, {mtAvgBitrate, uint32(1000000)}, {mtFrameRate, rate}, {mtFrameSize, pack(64, 48)}, {mtInterlaceMode, uint32(2)}} {
		if got := x.outMap[c.key]; got != c.want {
			t.Errorf("output type %s is %v, want %v", mftGName(c.key), got, c.want)
		}
	}
	for _, c := range []struct {
		key  mftGUID
		want any
	}{{mtFrameRate, rate}, {mtFrameSize, pack(64, 48)}, {mtInterlaceMode, uint32(2)}} {
		if got := x.inMap[c.key]; got != c.want {
			t.Errorf("input type %s is %v, want %v", mftGName(c.key), got, c.want)
		}
	}
	params := e.params()
	if len(params) != 2 || nalType(params[0]) != 7 || nalType(params[1]) != 8 {
		t.Errorf("params %d units", len(params))
	}
	if e.hardware() {
		t.Error("Media Foundation encoder reports hardware")
	}
	e.close()
	f.checkReleased(t, 1)
}

func TestMFTProfileValues(t *testing.T) {
	for _, c := range []struct {
		p       Profile
		value   uint32
		idc, cs uint8
	}{{ConstrainedBaseline, 256, 66, 0xc0}, {Main, 77, 77, 0x40}, {High, 100, 100, 0}} {
		f := newMFTFake()
		p := mftParams()
		p.profile = c.p
		e := mftNewEnc(t, f, p)
		if got := f.xfs[0].outMap[mtMPEG2Profile]; got != c.value {
			t.Errorf("%v: MF_MT_MPEG2_PROFILE %v, want %d", c.p, got, c.value)
		}
		sps, err := ParseKeyframe(appendAnnexB(nil, e.params()))
		if err != nil || sps.ProfileIDC != c.idc || sps.Constraints != c.cs {
			t.Errorf("%v: SPS %+v, %v", c.p, sps, err)
		}
		e.close()
		f.checkReleased(t, 1)
	}
}

func TestMFTGOPAndRate(t *testing.T) {
	for _, c := range []struct {
		interval time.Duration
		fps      float64
		gop      uint32
		num, den uint32
	}{
		{-1, 30, 4294967295, 30000, 1000},
		{2 * time.Second, 30, 60, 30000, 1000},
		{500 * time.Millisecond, 24, 12, 24000, 1000},
		{time.Millisecond, 30, 1, 30000, 1000},
		{2 * time.Second, 29.97, 60, 29970, 1000},
		{2 * time.Second, 1.0 / 3600, 1, 1, 3600},
		{2 * time.Second, 0.5, 1, 1, 2},
		{2 * time.Second, 1, 2, 1000, 1000},
	} {
		f := newMFTFake()
		p := mftParams()
		p.keyInterval, p.fps = c.interval, c.fps
		e := mftNewEnc(t, f, p)
		if want := fmt.Sprintf("codec u32 GOPSize=%d", c.gop); !f.has(want) {
			t.Errorf("interval %v at %v fps: want %q, log %v", c.interval, c.fps, want, f.events())
		}
		if got := f.xfs[0].outMap[mtFrameRate]; got != pack(c.num, c.den) {
			t.Errorf("rate %v: output type %#x, want %d/%d", c.fps, got, c.num, c.den)
		}
		if got := f.xfs[0].inMap[mtFrameRate]; got != pack(c.num, c.den) {
			t.Errorf("rate %v: input type %#x, want %d/%d", c.fps, got, c.num, c.den)
		}
		e.close()
	}
}

func TestMFTTimeAndPack(t *testing.T) {
	for _, c := range []struct{ ticks, want int64 }{{0, 0}, {90000, 10000000}, {3000, 333333}, {1, 111}} {
		if got := mft100ns(c.ticks); got != c.want {
			t.Errorf("mft100ns(%d) = %d, want %d", c.ticks, got, c.want)
		}
	}
	if mft100ns(1<<62) <= 0 || mft100ns(-1<<62) >= 0 {
		t.Error("large timestamps do not saturate")
	}
	if pack(1, 2) != 1<<32|2 {
		t.Error("pack")
	}
}

func TestMFTEncode(t *testing.T) {
	for _, provides := range []bool{false, true} {
		t.Run(fmt.Sprint("provides=", provides), func(t *testing.T) {
			f := newMFTFake()
			f.provides = provides
			p := mftParams()
			e := mftNewEnc(t, f, p)
			// Strided planes with padding that must not reach the MFT.
			y, u, v := mftPicture(64, 48)
			stride := func(b []byte, w, h, s int) []byte {
				out := bytes.Repeat([]byte{0xee}, s*h)
				for r := 0; r < h; r++ {
					copy(out[r*s:], b[r*w:r*w+w])
				}
				return out
			}
			r, err := mftEncode(e, stride(y, 64, 48, 80), stride(u, 32, 24, 40), stride(v, 32, 24, 40), 80, 40, 90000, 3000, false)
			if err != nil || r.n != 1 {
				t.Fatalf("encode: %v, %d emits", err, r.n)
			}
			if !r.key || !slices.Equal(types(r.nals), []int{9, 7, 8, 5}) || len(r.params) != 2 {
				t.Errorf("first access unit: key %v, types %v, %d params", r.key, types(r.nals), len(r.params))
			}
			if want := slices.Concat(y, u, v); !bytes.Equal(f.inputs[0], want) {
				t.Error("the input sample is not the tight I420 picture")
			}
			if f.inTimes[0] != [2]int64{10000000, 333333} {
				t.Errorf("sample time and duration %v", f.inTimes[0])
			}
			r, err = mftEncode(e, y, u, v, 64, 32, 93000, 3000, false)
			if err != nil || r.n != 1 || r.key || !slices.Equal(types(r.nals), []int{9, 1}) {
				t.Errorf("delta: %v, key %v, types %v", err, r.key, types(r.nals))
			}
			if n := f.count("codec u32 ForceKeyFrame=1"); n != 0 {
				t.Errorf("%d key requests without one asked for", n)
			}
			r, err = mftEncode(e, y, u, v, 64, 32, 96000, 3000, true)
			if err != nil || !r.key {
				t.Fatalf("forced key: %v %v", err, r.key)
			}
			ev := f.events()
			k, in := slices.Index(ev, "codec u32 ForceKeyFrame=1"), -1
			for i, s := range ev {
				if s == "processInput" {
					if in++; in == 2 {
						if k < 0 || k > i {
							t.Errorf("key request at %d, third input at %d", k, i)
						}
					}
				}
			}
			e.close()
			f.checkReleased(t, 1)
		})
	}
}

func TestMFTEncodeNoOutputAndTwoOutputs(t *testing.T) {
	// An MFT that provides its samples must get the second one back too.
	f := newMFTFake()
	f.provides = true
	e := mftNewEnc(t, f, mftParams())
	y, u, v := mftPicture(64, 48)
	one := appendAnnexB(nil, [][]byte{testSlice(false, 0, 5, 0, 1, 0)})
	f.encOutputs = func(int, bool) [][]byte { return [][]byte{one, one} }
	if _, err := mftEncode(e, y, u, v, 64, 32, 3000, 3000, false); !errors.Is(err, ErrCodec) {
		t.Errorf("provides: two access units for one frame: %v", err)
	}
	e.close()
	f.checkReleased(t, 1)

	f = newMFTFake()
	e = mftNewEnc(t, f, mftParams())
	f.encOutputs = func(int, bool) [][]byte { return nil }
	if r, err := mftEncode(e, y, u, v, 64, 32, 0, 3000, false); err != nil || r.n != 0 {
		t.Errorf("dropped frame: %v, %d emits", err, r.n)
	}
	f.encOutputs = func(int, bool) [][]byte { return [][]byte{one, one} }
	if _, err := mftEncode(e, y, u, v, 64, 32, 3000, 3000, false); !errors.Is(err, ErrCodec) {
		t.Errorf("two access units for one frame: %v", err)
	}
	f.encOutputs = func(int, bool) [][]byte { return [][]byte{{0, 0, 1}} }
	if _, err := mftEncode(e, y, u, v, 64, 32, 6000, 3000, false); !errors.Is(err, ErrCodec) {
		t.Errorf("an empty access unit: %v", err)
	}
	f.encOutputs = func(int, bool) [][]byte { return [][]byte{{1, 2, 3, 4}} }
	if _, err := mftEncode(e, y, u, v, 64, 32, 9000, 3000, false); !errors.Is(err, ErrCodec) {
		t.Errorf("an access unit without a start code: %v", err)
	}
	if _, err := mftEncode(e, y[:10], u, v, 64, 32, 9000, 3000, false); !errors.Is(err, ErrCodec) {
		t.Errorf("short luma plane: %v", err)
	}
	if _, err := mftEncode(e, y, u, v, 63, 32, 9000, 3000, false); !errors.Is(err, ErrCodec) {
		t.Errorf("luma stride below the width: %v", err)
	}
	e.close()
	f.checkReleased(t, 1)
}

func TestMFTEncodeStreamChange(t *testing.T) {
	f := newMFTFake()
	f.change = true
	e := mftNewEnc(t, f, mftParams())
	y, u, v := mftPicture(64, 48)
	r, err := mftEncode(e, y, u, v, 64, 32, 0, 3000, false)
	if err != nil || r.n != 1 {
		t.Fatalf("encode across a stream change: %v, %d emits", err, r.n)
	}
	if n := f.count("enc currentOutput"); n != 2 {
		t.Errorf("the output type was read %d times, want 2 (setup and after the change)", n)
	}
	e.close()
	f.checkReleased(t, 1)
}

func TestMFTEncodeErrors(t *testing.T) {
	for _, c := range []struct {
		op   string
		err  error
		want error
		code int64
	}{
		{"processInput", hrNotAccepting, ErrCodec, 0xc00d36b5},
		{"processOutput", hrFail, ErrCodec, 0x80004005},
		{"processOutput", hrInvalidData, ErrCorrupt, 0xc00d36cb},
		{"sample", hrFail, ErrCodec, 0x80004005},
		{"codec ForceKeyFrame", hrFail, ErrCodec, 0x80004005},
	} {
		f := newMFTFake()
		e := mftNewEnc(t, f, mftParams())
		y, u, v := mftPicture(64, 48)
		f.mu.Lock()
		f.failOps[c.op] = c.err
		f.mu.Unlock()
		_, err := mftEncode(e, y, u, v, 64, 32, 0, 3000, c.op == "codec ForceKeyFrame")
		var ce *codecError
		if !errors.Is(err, c.want) || !errors.As(err, &ce) || ce.code != c.code {
			t.Errorf("%s: %v, want %v code %#x", c.op, err, c.want, c.code)
		}
		if err != nil && !strings.HasPrefix(err.Error(), "kolovideo/h264: ") {
			t.Errorf("%s: error text %q", c.op, err)
		}
		e.close()
		f.checkReleased(t, 1)
	}
}

func TestMFTSetBitrate(t *testing.T) {
	f := newMFTFake()
	e := mftNewEnc(t, f, mftParams())
	if err := e.setBitrate(500000); err != nil || !f.has("codec u32 MeanBitRate=500000") {
		t.Errorf("setBitrate: %v, log %v", err, f.events())
	}
	f.mu.Lock()
	f.failOps["codec MeanBitRate"] = hrFail
	f.mu.Unlock()
	if err := e.setBitrate(400000); !errors.Is(err, ErrCodec) {
		t.Errorf("failed setBitrate: %v", err)
	}
	e.close()
	f.checkReleased(t, 1)
}

func TestMFTEncoderSetupFailures(t *testing.T) {
	fails := []string{"enc codecAPI", "codec RateControlMode", "codec MeanBitRate", "codec BPictureCount", "codec GOPSize", "codec LowLatency",
		"attr Profile", "attr AvgBitrate", "attr InterlaceMode", "setOutput", "enc currentOutput", "setInput", "message begin streaming", "message start of stream", "outputInfo", "sample"}
	for _, op := range fails {
		f := newMFTFake()
		f.failOps[op] = hrFail
		e, err := mftEngine{sys: f}.newEncoder(mftParams())
		if err == nil {
			e.close()
			t.Errorf("%s: no error", op)
			continue
		}
		var ce *codecError
		if !errors.Is(err, ErrCodec) || !errors.As(err, &ce) {
			t.Errorf("%s: %v", op, err)
		}
		f.checkReleased(t, 1)
		if c, u := f.count("coInit"), f.count("coUninit"); c != u {
			t.Errorf("%s: %d apartments entered, %d left", op, c, u)
		}
	}
	for name, mod := range map[string]func(*mftFake){
		"no sequence header": func(f *mftFake) { f.noSeq = true },
		"sequence without PPS": func(f *mftFake) {
			f.seqFor = func(uint32, int, int) []byte { return appendAnnexB(nil, [][]byte{testSPSFor(64, 48, 66, 0xc0, 31)}) }
		},
		"no IYUV or I420": func(f *mftFake) { f.inTypes = []mftGUID{fmtYV12Test, fmtNV12Test} },
	} {
		f := newMFTFake()
		mod(f)
		if e, err := (mftEngine{sys: f}).newEncoder(mftParams()); !errors.Is(err, ErrCodec) {
			t.Errorf("%s: %v", name, err)
			if e != nil {
				e.close()
			}
		}
		f.checkReleased(t, 1)
	}
}

func TestMFTCloseOrder(t *testing.T) {
	f := newMFTFake()
	e := mftNewEnc(t, f, mftParams())
	e.close()
	idx := func(s string) int {
		i := f.index(s)
		if i < 0 {
			t.Fatalf("no %q in %v", s, f.events())
		}
		return i
	}
	// The ICodecAPI goes before the transform, both before Media Foundation
	// and the apartment are left, in that order.
	if !(idx("message end streaming") < idx("release codec") && idx("release codec") < idx("release transform") &&
		idx("release transform") < idx("mfShutdown") && idx("mfShutdown") < idx("coUninit")) {
		t.Errorf("close order %v", f.events())
	}
	f.checkReleased(t, 1)
	y, u, v := mftPicture(64, 48)
	if _, err := mftEncode(e, y, u, v, 64, 32, 0, 3000, false); !errors.Is(err, ErrCodec) {
		t.Errorf("encode after close: %v", err)
	}
	if err := e.setBitrate(1); !errors.Is(err, ErrCodec) {
		t.Errorf("setBitrate after close: %v", err)
	}

	d := mftNewDec(t, f)
	d.close()
	if _, ok, err := d.decode(mftAU(176, 144, true)); ok || !errors.Is(err, ErrCodec) {
		t.Errorf("decode after close: %v %v", ok, err)
	}
	f.checkReleased(t, 2)
}

func TestMFTUnavailable(t *testing.T) {
	missing := errors.New("mfplat.dll is missing")
	for _, c := range []struct {
		name string
		mod  func(*mftFake)
		code int64
		text string
	}{
		{"class not registered", func(f *mftFake) {
			f.createErr[clsidH264Encoder], f.createErr[clsidH264Decoder] = hrClassNotReg, hrClassNotReg
		}, 0x80040154, "create the"},
		{"missing function", func(f *mftFake) {
			f.createErr[clsidH264Encoder], f.createErr[clsidH264Decoder] = missing, missing
		}, 0, "mfplat.dll is missing"},
		{"COM", func(f *mftFake) { f.coErr = mftHRESULT(0x80010106) }, 0x80010106, "start COM"},
		{"Media Foundation", func(f *mftFake) { f.startErr = mftHRESULT(0xc00d36b2) }, 0xc00d36b2, "start Media Foundation"},
	} {
		f := newMFTFake()
		c.mod(f)
		for dir, open := range map[string]func() error{
			"encoder": func() error { _, err := mftEngine{sys: f}.newEncoder(mftParams()); return err },
			"decoder": func() error {
				_, err := mftEngine{sys: f}.newDecoder(decParams{maxPixels: DefaultMaxPixels})
				return err
			},
		} {
			err := open()
			var u *UnavailableError
			if !errors.Is(err, ErrUnavailable) || !errors.As(err, &u) || u.Code != c.code || u.Engine != "Media Foundation" || !strings.Contains(u.Reason, c.text) {
				t.Errorf("%s %s: %v", c.name, dir, err)
			}
		}
		if cu, cd := f.count("coInit"), f.count("coUninit"); cu != cd {
			t.Errorf("%s: %d apartments entered, %d left", c.name, cu, cd)
		}
		f.checkReleased(t, f.threadsSeen())
	}
	// Another failure of CoCreateInstance is a codec error, not absence.
	f := newMFTFake()
	f.createErr[clsidH264Encoder] = hrFail
	if _, err := (mftEngine{sys: f}).newEncoder(mftParams()); errors.Is(err, ErrUnavailable) || !errors.Is(err, ErrCodec) {
		t.Errorf("E_FAIL from CoCreateInstance: %v", err)
	}
}

func (f *mftFake) threadsSeen() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.gs)
}

func TestMFTCapabilities(t *testing.T) {
	f := newMFTFake()
	l := mftEngine{sys: f}.caps()
	if !l.Encode.Available || !slices.Equal(l.Encode.Profiles, []Profile{ConstrainedBaseline, Main, High}) || l.Encode.Hardware || l.Encode.Engine != "Media Foundation" {
		t.Errorf("encode support %+v", l.Encode)
	}
	if !l.Decode.Available || !slices.Equal(l.Decode.Profiles, []Profile{ConstrainedBaseline, Main, High}) || l.Decode.Hardware ||
		l.Decode.MaxWidth != 4096 || l.Decode.MaxHeight != 2304 {
		t.Errorf("decode support %+v", l.Decode)
	}
	f.checkReleased(t, 1)
	if c, u := f.count("coInit"), f.count("coUninit"); c != 1 || u != 1 {
		t.Errorf("%d apartments entered, %d left", c, u)
	}

	// An encoder that always gives Main lists Main only.
	f = newMFTFake()
	f.seqFor = func(_ uint32, w, h int) []byte { return mftFakeSeq(newMFTFake(), 77, w, h) }
	if l := (mftEngine{sys: f}).caps(); !slices.Equal(l.Encode.Profiles, []Profile{Main}) || !l.Encode.Available {
		t.Errorf("Main-only encoder: %+v", l.Encode)
	}
	f.checkReleased(t, 1)

	// No usable header: the encoder is absent, the decoder is not.
	f = newMFTFake()
	f.noSeq = true
	l = mftEngine{sys: f}.caps()
	var nu *UnavailableError
	if l.Encode.Available || !errors.As(l.Encode.Err, &nu) || !l.Decode.Available {
		t.Errorf("encoder without a sequence header: %+v", l)
	}
	f.checkReleased(t, 1)

	// Another CoCreateInstance failure is reported as absence too, with its code.
	f = newMFTFake()
	f.createErr[clsidH264Decoder], f.createErr[clsidH264Encoder] = hrFail, hrFail
	l = mftEngine{sys: f}.caps()
	var de, ee *UnavailableError
	if l.Decode.Available || !errors.As(l.Decode.Err, &de) || de.Code != int64(uint32(hrFail)) ||
		l.Encode.Available || !errors.As(l.Encode.Err, &ee) || ee.Code != int64(uint32(hrFail)) {
		t.Errorf("E_FAIL from CoCreateInstance: %+v", l)
	}
	f.checkReleased(t, 1)

	f = newMFTFake()
	f.createErr[clsidH264Decoder] = hrClassNotReg
	l = mftEngine{sys: f}.caps()
	var u *UnavailableError
	if l.Decode.Available || !errors.As(l.Decode.Err, &u) || u.Code != 0x80040154 || !l.Encode.Available {
		t.Errorf("no decoder class: %+v", l)
	}

	f = newMFTFake()
	f.coErr = hrFail
	l = mftEngine{sys: f}.caps()
	if l.Encode.Available || l.Decode.Available || !errors.Is(l.Encode.Err, ErrUnavailable) || !errors.Is(l.Decode.Err, ErrUnavailable) {
		t.Errorf("no COM: %+v", l)
	}
}

func TestMFTThreadPanic(t *testing.T) {
	f := newMFTFake()
	th, err := mftEngine{sys: f}.startThread()
	if err != nil {
		t.Fatal(err)
	}
	if err := th.run(func() error { panic("boom") }); !errors.Is(err, ErrCodec) {
		t.Errorf("panic in a call: %v", err)
	}
	ran := false
	if err := th.run(func() error { ran = true; return nil }); err != nil || !ran {
		t.Errorf("the thread did not survive: %v", err)
	}
	th.stop()
	th.stop()
	if err := th.run(func() error { return nil }); err == nil {
		t.Error("run after stop")
	}
}

// mftAU builds a keyframe or delta access unit for a stream of w x h whose
// coded size is w and h rounded up to 16.
func mftAU(w, h int, key bool) accessUnit {
	cw, ch := (w+15)/16*16, (h+15)/16*16
	au := accessUnit{key: key, width: w, height: h, codedWidth: cw, codedHeight: ch}
	if key {
		au.sps = [][]byte{testSPSFor(w, h, 66, 0xc0, 31)}
		au.pps = [][]byte{testPPS(0, 0)}
		au.slices = [][]byte{testSlice(true, 0, 7, 0, 0, 1)}
	} else {
		au.slices = [][]byte{testSlice(false, 0, 5, 0, 1, 0)}
	}
	return au
}

func TestMFTDecoderSetup(t *testing.T) {
	f := newMFTFake()
	d := mftNewDec(t, f)
	if f.has("setInput") || f.has("outputType") {
		t.Fatal("the decoder was set up before the first keyframe")
	}
	au := mftAU(176, 144, true)
	pic, ok, err := d.decode(au)
	if err != nil || !ok {
		t.Fatalf("decode: %v %v", ok, err)
	}
	ev := f.events()
	order := []string{"create dec", "dec attributes", "set LowLatency=1", "set DecAcceleration=0", "set DecMaxWidth=4096", "set DecMaxHeight=4096",
		"setInput H264", "outputType 0", "outputType 1", "outputType 2", "setOutput IYUV", "dec currentOutput", "message begin streaming",
		"message start of stream", "processInput", "processOutput"}
	last := -1
	for _, want := range order {
		i := slices.Index(ev, want)
		if i < 0 || i < last {
			t.Fatalf("%q at %d after %d, log %v", want, i, last, ev)
		}
		last = i
	}
	x := f.xfs[0]
	for _, c := range []struct {
		key  mftGUID
		want any
	}{{mtFrameSize, pack(176, 144)}, {mtInterlaceMode, uint32(7)}} {
		if got := x.inMap[c.key]; got != c.want {
			t.Errorf("input type %s is %v, want %v", mftGName(c.key), got, c.want)
		}
	}
	// Annex B with 4-byte start codes, SPS and PPS first.
	if want := appendAnnexB(nil, au.sps, au.pps, au.slices); !bytes.Equal(f.inputs[0], want) {
		t.Errorf("input %x, want %x", f.inputs[0], want)
	}
	if pic.width != 176 || pic.height != 144 || pic.yStride != 176 || pic.uvStride != 88 || pic.nv12 || len(pic.y) != 176*144 || len(pic.u) != 88*72 || len(pic.v) != 88*72 {
		t.Errorf("picture %dx%d strides %d %d lengths %d %d %d", pic.width, pic.height, pic.yStride, pic.uvStride, len(pic.y), len(pic.u), len(pic.v))
	}
	if pic.y[3+176*2] != lumaAt(3, 2) || pic.u[1+88*2] != byte(2+2) || pic.v[1+88*2] != byte(1+3*2) {
		t.Error("planes carry the wrong samples")
	}
	// A delta packet sends the slices alone.
	if _, ok, err := d.decode(mftAU(176, 144, false)); err != nil || !ok {
		t.Fatalf("delta: %v %v", ok, err)
	}
	if want := appendAnnexB(nil, mftAU(176, 144, false).slices); !bytes.Equal(f.inputs[1], want) {
		t.Errorf("delta input %x", f.inputs[1])
	}
	if f.inTimes[1][0] <= f.inTimes[0][0] {
		t.Errorf("sample times do not increase: %v", f.inTimes)
	}
	if f.count("create dec") != 1 {
		t.Errorf("%d decoders made for one stream", f.count("create dec"))
	}
	if d.hardware() {
		t.Error("Media Foundation decoder reports hardware")
	}
	d.close()
	f.checkReleased(t, 1)
}

func TestMFTFakePaddedLayout(t *testing.T) {
	f := newMFTFake()
	d := mftNewDec(t, f)
	pic, ok, err := d.decode(mftAU(1920, 1080, true))
	if err != nil || !ok {
		t.Fatalf("decode: %v %v", ok, err)
	}
	if pic.width != 1920 || pic.height != 1088 || pic.yStride != 1920 || pic.uvStride != 960 || len(pic.u) != 960*544 {
		t.Fatalf("picture %dx%d strides %d %d, %d chroma bytes", pic.width, pic.height, pic.yStride, pic.uvStride, len(pic.u))
	}
	// The chroma planes start at row 1088, not at the cropped row 1080.
	if pic.u[0] != 0 || pic.u[1] != 2 || pic.u[960] != 1 || pic.v[1] != 1 || pic.v[960] != 3 {
		t.Errorf("chroma offset: u %v v %v", pic.u[:3], pic.v[:3])
	}
	d.close()
	f.checkReleased(t, 1)
}

func TestMFTDecoderStreamChangeAndProvides(t *testing.T) {
	for _, provides := range []bool{false, true} {
		f := newMFTFake()
		f.provides, f.change = provides, true
		d := mftNewDec(t, f)
		pic, ok, err := d.decode(mftAU(176, 144, true))
		if err != nil || !ok || pic.width != 176 {
			t.Fatalf("provides %v: %v %v", provides, ok, err)
		}
		if n := f.count("dec currentOutput"); n != 2 {
			t.Errorf("provides %v: output type read %d times", provides, n)
		}
		if _, ok, err := d.decode(mftAU(176, 144, false)); err != nil || !ok {
			t.Errorf("provides %v: delta %v %v", provides, ok, err)
		}
		d.close()
		f.checkReleased(t, 1)
	}
}

func TestMFTDecoderNoPictureBeforeKeyframe(t *testing.T) {
	f := newMFTFake()
	d := mftNewDec(t, f)
	if _, ok, err := d.decode(mftAU(176, 144, false)); ok || err != nil {
		t.Errorf("delta before the first keyframe: %v %v", ok, err)
	}
	if f.has("processInput") {
		t.Error("a delta packet reached the MFT before any keyframe")
	}
	d.close()
	f.checkReleased(t, 1)
}

func TestMFTDecoderErrors(t *testing.T) {
	for _, c := range []struct {
		op   string
		err  error
		want error
		code int64
	}{
		{"processInput", hrNotAccepting, ErrCodec, 0xc00d36b5},
		{"processInput", hrFail, ErrCodec, 0x80004005},
		{"processOutput", hrFail, ErrCodec, 0x80004005},
		{"processOutput", hrInvalidData, ErrCorrupt, 0xc00d36cb},
		{"sample", hrFail, ErrCodec, 0x80004005},
	} {
		f := newMFTFake()
		d := mftNewDec(t, f)
		if _, _, err := d.decode(mftAU(176, 144, true)); err != nil {
			t.Fatal(err)
		}
		f.mu.Lock()
		f.failOps[c.op] = c.err
		f.mu.Unlock()
		before := f.count("message flush")
		_, ok, err := d.decode(mftAU(176, 144, false))
		var ce *codecError
		if ok || !errors.Is(err, c.want) || !errors.As(err, &ce) || ce.code != c.code {
			t.Errorf("%s %v: %v %v, want %v code %#x", c.op, c.err, ok, err, c.want, c.code)
		}
		if c.op != "sample" && f.count("message flush") != before+1 {
			t.Errorf("%s: no flush after the failure: %v", c.op, f.events())
		}
		// The next keyframe decodes again on the same MFT.
		f.mu.Lock()
		delete(f.failOps, c.op)
		f.mu.Unlock()
		if _, ok, err := d.decode(mftAU(176, 144, true)); err != nil || !ok {
			t.Errorf("%s: keyframe after the failure: %v %v", c.op, ok, err)
		}
		d.close()
		f.checkReleased(t, 1)
	}
}

func TestMFTDecoderRefusesOutput(t *testing.T) {
	for name, c := range map[string]struct {
		layout func(w, h int) mftFakeLayout
		pics   int
		size   uint32
	}{
		"two pictures for one packet": {pics: 2},
		"frame smaller than picture":  {layout: func(w, h int) mftFakeLayout { return mftFakeLayout{w: w - 16, h: h, stride: w - 16} }},
		"frame beyond the coded size": {layout: func(w, h int) mftFakeLayout { return mftFakeLayout{w: w + 16, h: h + 16, stride: w + 16} }},
		"stride below the width":      {layout: func(w, h int) mftFakeLayout { return mftFakeLayout{w: w, h: h, stride: w - 2} }},
		"odd stride":                  {layout: func(w, h int) mftFakeLayout { return mftFakeLayout{w: w, h: h, stride: w + 1} }},
		"negative stride":             {layout: func(w, h int) mftFakeLayout { return mftFakeLayout{w: w, h: h, stride: -w} }},
		"aperture is another size": {layout: func(w, h int) mftFakeLayout {
			return mftFakeLayout{w: w, h: h, stride: w, aperture: &[2]int{w - 2, h}}
		}},
		"huge output buffer": {size: 1 << 30},
	} {
		f := newMFTFake()
		f.decLayout, f.decPics, f.sizeInfo = c.layout, c.pics, c.size
		d := mftNewDec(t, f)
		_, ok, err := d.decode(mftAU(176, 144, true))
		if ok || !errors.Is(err, ErrCodec) {
			t.Errorf("%s: %v %v", name, ok, err)
		}
		d.close()
		f.checkReleased(t, 1)
	}
	// A type without a stride attribute takes the width; no aperture is fine.
	f := newMFTFake()
	f.decLayout = func(w, h int) mftFakeLayout { return mftFakeLayout{w: w, h: h, stride: w, noStride: true} }
	d := mftNewDec(t, f)
	if pic, ok, err := d.decode(mftAU(176, 144, true)); err != nil || !ok || pic.yStride != 176 {
		t.Errorf("no stride attribute: %v %v", ok, err)
	}
	d.close()
	f.checkReleased(t, 1)
}

func TestMFTDecoderSizeChange(t *testing.T) {
	f := newMFTFake()
	d := mftNewDec(t, f)
	for _, w := range []int{176, 176, 320} {
		pic, ok, err := d.decode(mftAU(w, w*3/4/16*16, true))
		if err != nil || !ok || pic.width != w {
			t.Fatalf("width %d: %v %v", w, ok, err)
		}
	}
	if f.count("create dec") != 2 {
		t.Errorf("%d decoders for two sizes, want 2: %v", f.count("create dec"), f.events())
	}
	if f.count("release transform") != 1 {
		t.Errorf("%d transforms released before close, want 1", f.count("release transform"))
	}
	d.close()
	f.checkReleased(t, 1)
}

func TestMFTDecoderSetupFailures(t *testing.T) {
	for _, op := range []string{"dec attributes", "attr LowLatency", "attr DecAcceleration", "attr DecMaxWidth", "attr DecMaxHeight",
		"setInput", "setOutput", "dec currentOutput", "message begin streaming", "message start of stream", "outputInfo", "sample"} {
		f := newMFTFake()
		d := mftNewDec(t, f)
		f.mu.Lock()
		f.failOps[op] = hrFail
		f.mu.Unlock()
		if _, ok, err := d.decode(mftAU(176, 144, true)); ok || !errors.Is(err, ErrCodec) {
			t.Errorf("%s: %v %v", op, ok, err)
		}
		d.close()
		f.checkReleased(t, 1)
	}
	f := newMFTFake()
	f.outTypes = []mftGUID{fmtNV12Test}
	d := mftNewDec(t, f)
	if _, ok, err := d.decode(mftAU(176, 144, true)); ok || !errors.Is(err, ErrCodec) {
		t.Errorf("no IYUV or I420 offered: %v %v", ok, err)
	}
	d.close()
	f.checkReleased(t, 1)
}

func TestMFTLayoutRead(t *testing.T) {
	area := func(offX, offY int16, cx, cy int32) []byte {
		b := make([]byte, 16)
		b[2], b[3], b[6], b[7] = byte(offX), byte(offX>>8), byte(offY), byte(offY>>8)
		for i, v := range []int32{cx, cy} {
			for k := 0; k < 4; k++ {
				b[8+4*i+k] = byte(v >> (8 * k))
			}
		}
		return b
	}
	mk := func(m map[mftGUID]any) mftAttrs { return &mftFakeAttrs{f: newMFTFake(), ref: &mftRef{n: 1}, m: m} }
	l, err := mftReadLayout(mk(map[mftGUID]any{mtFrameSize: pack(1920, 1088), mtDefaultStride: uint32(1920), mtDisplayAperture: area(0, 0, 1920, 1080)}))
	if err != nil || l.w != 1920 || l.h != 1088 || l.stride != 1920 || !l.aperture || l.cx != 1920 || l.cy != 1080 {
		t.Errorf("layout %+v, %v", l, err)
	}
	if _, err := mftReadLayout(mk(map[mftGUID]any{mtFrameSize: pack(16, 16), mtDisplayAperture: area(2, 0, 16, 16)})); err == nil {
		t.Error("an aperture away from the origin was accepted")
	}
	if _, err := mftReadLayout(mk(map[mftGUID]any{mtFrameSize: pack(16, 16), mtDisplayAperture: make([]byte, 8)})); err == nil {
		t.Error("a short aperture was accepted")
	}
	if _, err := mftReadLayout(mk(map[mftGUID]any{})); err == nil {
		t.Error("a type without a frame size was accepted")
	}
}

// With the fake engine under the public API: the core's normalization and
// validation run on what the Media Foundation engine returns.
func TestMFTThroughCore(t *testing.T) {
	f := newMFTFake()
	useEngine(t, mftEngine{sys: f})
	enc, err := NewEncoder(Config{Width: 64, Height: 48, Bitrate: 500000})
	if err != nil {
		t.Fatal(err)
	}
	fr := newFrame(t, 64, 48, 0)
	pkts, err := enc.Encode(fr, false)
	if err != nil || len(pkts) != 1 || !pkts[0].Keyframe {
		t.Fatalf("encode: %v %v", pkts, err)
	}
	nals := units(t, pkts[0].Data)
	if !slices.Equal(types(nals), []int{7, 8, 5}) || !bytes.HasPrefix(pkts[0].Data, []byte{0, 0, 0, 1}) {
		t.Errorf("keyframe NAL types %v", types(nals))
	}
	if sps, err := ParseKeyframe(pkts[0].Data); err != nil || sps.Matrix != 6 {
		t.Errorf("the SPS is not normalized: %+v %v", sps, err)
	}
	fr2 := newFrame(t, 64, 48, 33*time.Millisecond)
	pkts, err = enc.Encode(fr2, false)
	if err != nil || len(pkts) != 1 || pkts[0].Keyframe || !slices.Equal(types(units(t, pkts[0].Data)), []int{1}) {
		t.Errorf("delta: %v %v", pkts, err)
	}
	if err := enc.SetBitrate(300000); err != nil || !f.has("codec u32 MeanBitRate=300000") {
		t.Errorf("SetBitrate: %v", err)
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}

	dec, err := NewDecoder(DecoderConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if fr, err := dec.Decode(deltaPkt()); fr != nil || !errors.Is(err, ErrNeedKeyframe) {
		t.Errorf("delta before keyframe: %v %v", fr, err)
	}
	out, err := dec.Decode(keyPkt())
	if err != nil || out == nil || out.Width != 640 || out.Height != 480 {
		t.Fatalf("decode: %v %v", out, err)
	}
	if out.Y[7+640*3] != lumaAt(7, 3) || out.U[2+320*4] != byte(2*2+4) {
		t.Error("the frame does not carry the picture's samples")
	}
	if out, err := dec.Decode(deltaPkt()); err != nil || out == nil {
		t.Errorf("delta: %v %v", out, err)
	}
	if err := dec.Close(); err != nil {
		t.Fatal(err)
	}
	f.checkReleased(t, 2)
}

func TestMFTConcurrent(t *testing.T) {
	f := newMFTFake()
	useEngine(t, mftEngine{sys: f})
	enc, err := NewEncoder(Config{Width: 64, Height: 48, Bitrate: 500000})
	if err != nil {
		t.Fatal(err)
	}
	dec, err := NewDecoder(DecoderConfig{})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(3)
		go func() {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				enc.Encode(newFrame(t, 64, 48, time.Duration(g*100+i)*time.Millisecond), false)
			}
		}()
		go func() {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				enc.SetBitrate(100000 + i)
				enc.Hardware()
			}
		}()
		go func() {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				dec.Decode(keyPkt())
				dec.Decode(deltaPkt())
			}
		}()
	}
	wg.Wait()
	enc.Close()
	dec.Close()
	if _, err := enc.Encode(newFrame(t, 64, 48, 0), false); !errors.Is(err, kolovideo.ErrClosed) {
		t.Errorf("encode after close: %v", err)
	}
	f.checkReleased(t, 2)
}

// FuzzMFTLayout reads output types a hostile or broken MFT could report:
// never a panic, and a layout that passes the check holds its picture.
func FuzzMFTLayout(f *testing.F) {
	f.Add(uint64(1920)<<32|1088, uint32(1920), []byte{0, 0, 0, 0, 0, 0, 0, 0, 0x80, 0x07, 0, 0, 0x38, 0x04, 0, 0}, 1920, 1080)
	f.Add(uint64(176)<<32|112, uint32(0xfffffff0), []byte{1, 2, 3}, 162, 98)
	f.Fuzz(func(t *testing.T, size uint64, stride uint32, area []byte, w, h int) {
		m := map[mftGUID]any{mtFrameSize: size, mtDefaultStride: stride}
		if len(area) > 0 {
			m[mtDisplayAperture] = area
		}
		l, err := mftReadLayout(&mftFakeAttrs{f: newMFTFake(), ref: &mftRef{n: 1}, m: m})
		if err != nil || w <= 0 || h <= 0 || w > 1<<12 || h > 1<<12 {
			return
		}
		au := accessUnit{width: w, height: h, codedWidth: (w + 15) / 16 * 16, codedHeight: (h + 15) / 16 * 16}
		if l.check(au) != nil {
			return
		}
		if l.w < w || l.h < h || l.stride < l.w || l.stride%2 != 0 || l.bytes() < l.stride*l.h || l.bytes() <= 0 {
			t.Fatalf("accepted layout %+v for %dx%d", l, w, h)
		}
	})
}

// A decoder that finishes a picture after ProcessOutput reported nothing:
// the engine drains it and gets the picture, once.
func TestMFTDecoderDrain(t *testing.T) {
	f := newMFTFake()
	f.late = true
	d := mftNewDec(t, f)
	pic, ok, err := d.decode(mftAU(176, 144, true))
	if err != nil || !ok || pic.width != 176 {
		t.Fatalf("late picture: %v %v", ok, err)
	}
	if n := f.count("message drain"); n != 1 {
		t.Errorf("%d drains for the late picture", n)
	}
	if _, ok, err := d.decode(mftAU(176, 144, false)); err != nil || !ok || f.count("message drain") != 1 {
		t.Errorf("delta after the drain: %v %v, %d drains", ok, err, f.count("message drain"))
	}
	d.close()
	f.checkReleased(t, 1)

	// A packet with no picture is drained once and gives no picture, no error.
	f = newMFTFake()
	f.decPics = -1
	d = mftNewDec(t, f)
	if _, ok, err := d.decode(mftAU(176, 144, true)); ok || err != nil || f.count("message drain") != 1 {
		t.Errorf("no picture: %v %v, %d drains", ok, err, f.count("message drain"))
	}
	d.close()
	f.checkReleased(t, 1)

	f = newMFTFake()
	f.late = true
	d = mftNewDec(t, f)
	f.mu.Lock()
	f.failOps["message drain"] = hrFail
	f.mu.Unlock()
	if _, ok, err := d.decode(mftAU(176, 144, true)); ok || !errors.Is(err, ErrCodec) {
		t.Errorf("failed drain: %v %v", ok, err)
	}
	d.close()
	f.checkReleased(t, 1)
}
