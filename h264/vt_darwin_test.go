//go:build darwin && (amd64 || arm64)

package h264

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ebitengine/purego"

	kolovideo "github.com/ciranda-dev/kolo-video"
)

func realOS(t *testing.T) vtOS {
	t.Helper()
	os, err := loadVT()
	if err != nil {
		t.Skipf("VideoToolbox is not usable: %v", err)
	}
	return os
}

type realAU struct {
	nals, params [][]byte
	key          bool
	at           time.Duration
}

// engineRoundTrip encodes n frames through the real encoder layer and
// decodes them with the real decoder layer, returning the minimum luma PSNR.
func engineRoundTrip(t *testing.T, os vtOS, w, h, n int, p Profile) (aus []realAU, minPSNR float64) {
	t.Helper()
	e, err := newVTEncoder(os, encParams{width: w, height: h, fps: 30, bps: int(0.07 * float64(w*h) * 30), profile: p, keyInterval: 2 * time.Second})
	if err != nil {
		t.Fatalf("newVTEncoder: %v", err)
	}
	defer e.close()
	srcY := make([][]byte, n)
	for i := 0; i < n; i++ {
		y, u, v := vtPattern(w, h, i)
		srcY[i] = y
		start := time.Now()
		err := e.encode(y, u, v, w, w/2, int64(i)*3000, 3000, i == 0 || i == n/2, func(nals, params [][]byte, key bool) {
			aus = append(aus, realAU{nals: vtCloneParams(nals), params: vtCloneParams(params), key: key})
		})
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if len(aus) > 0 {
			aus[len(aus)-1].at = time.Since(start)
		}
	}
	if len(aus) != n {
		t.Fatalf("%d access units for %d frames", len(aus), n)
	}
	d, err := newVTDecoder(os, decParams{maxPixels: DefaultMaxPixels})
	if err != nil {
		t.Fatal(err)
	}
	defer d.close()
	minPSNR = 99
	for i, a := range aus {
		au := accessUnit{key: a.key, width: w, height: h, codedWidth: (w + 15) &^ 15, codedHeight: (h + 15) &^ 15}
		for _, nal := range a.nals {
			switch nal[0] & 0x1f {
			case 1, 5:
				au.slices = append(au.slices, nal)
			}
		}
		if a.key {
			for _, ps := range a.params {
				if ps[0]&0x1f == 7 {
					au.sps = append(au.sps, ps)
				} else {
					au.pps = append(au.pps, ps)
				}
			}
		}
		pic, ok, err := d.decode(au)
		if err != nil || !ok {
			t.Fatalf("decode %d: ok %v err %v", i, ok, err)
		}
		if pic.width != w || pic.height != h || !pic.nv12 {
			t.Fatalf("picture %dx%d nv12 %v", pic.width, pic.height, pic.nv12)
		}
		got := make([]byte, 0, w*h)
		for r := 0; r < h; r++ {
			got = append(got, pic.y[r*pic.yStride:r*pic.yStride+w]...)
		}
		minPSNR = min(minPSNR, vtLumaPSNR(srcY[i], got))
	}
	return aus, minPSNR
}

func slicesOf(a realAU, w, h int) accessUnit {
	au := accessUnit{key: a.key, width: w, height: h, codedWidth: (w + 15) &^ 15, codedHeight: (h + 15) &^ 15}
	for _, nal := range a.nals {
		if k := nal[0] & 0x1f; k == 1 || k == 5 {
			au.slices = append(au.slices, nal)
		}
	}
	if a.key {
		for _, ps := range a.params {
			if ps[0]&0x1f == 7 {
				au.sps = append(au.sps, ps)
			} else {
				au.pps = append(au.pps, ps)
			}
		}
	}
	return au
}

// TestVTEngineCorrupt feeds damaged slices straight to the system decoder,
// past the Go checks that would otherwise wait for a keyframe: it must
// return an error or a picture, keep the process alive, and decode the clean
// stream again with the same decoder.
func TestVTEngineCorrupt(t *testing.T) {
	os := realOS(t)
	const w, h = 640, 480
	aus, _ := engineRoundTrip(t, os, w, h, 60, ConstrainedBaseline)
	d, err := newVTDecoder(os, decParams{maxPixels: DefaultMaxPixels})
	if err != nil {
		t.Fatal(err)
	}
	defer d.close()
	rnd := rand.New(rand.NewSource(1))
	counts := map[string]int{}
	pictures := 0
	for k := 0; k < 3000; k++ {
		au := slicesOf(aus[rnd.Intn(len(aus))], w, h)
		for i, s := range au.slices {
			if len(s) > 2 {
				m := append([]byte(nil), s...)
				for j := 0; j < 1+rnd.Intn(6); j++ {
					m[1+rnd.Intn(len(m)-1)] ^= byte(1 << rnd.Intn(8))
				}
				if rnd.Intn(4) == 0 {
					m = m[:1+rnd.Intn(len(m)-1)]
				}
				au.slices[i] = m
			}
		}
		_, ok, err := d.decode(au)
		switch {
		case err == nil && ok:
			pictures++
		case err == nil:
			counts["no picture"]++
		case errors.Is(err, ErrCorrupt):
			counts["corrupt"]++
		case errors.Is(err, ErrCodec):
			counts["codec"]++
		default:
			t.Fatalf("untyped error %v", err)
		}
	}
	t.Logf("3000 damaged units: %d pictures, %v", pictures, counts)
	good := 0
	for _, a := range aus {
		if _, ok, err := d.decode(slicesOf(a, w, h)); err == nil && ok {
			good++
		}
	}
	if good != len(aus) {
		t.Errorf("the clean stream decodes %d of %d pictures after the damage", good, len(aus))
	}
}

func TestVTEngineRoundTrip(t *testing.T) {
	os := realOS(t)
	for _, p := range []Profile{ConstrainedBaseline, Main, High} {
		aus, psnr := engineRoundTrip(t, os, 320, 240, 30, p)
		keys := 0
		for _, a := range aus {
			if a.key {
				keys++
			}
		}
		t.Logf("%v: %d keyframes, min PSNR %.2f dB, SPS %x", p, keys, psnr, aus[0].params[0][:4])
		if psnr < 35 {
			t.Errorf("%v: PSNR %.2f", p, psnr)
		}
	}
}

// vtPercentile returns the p-quantile (0 to 1) of xs.
func vtPercentile(xs []float64, p float64) float64 {
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	return s[int(math.Min(float64(len(s)-1), p*float64(len(s))))]
}

func processRSS(t *testing.T) int64 {
	t.Helper()
	out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(os.Getpid())).Output()
	if err != nil {
		t.Skipf("ps: %v", err)
	}
	kb, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return kb << 10
}

var (
	poolPush func() uintptr
	poolPop  func(uintptr)
)

// withPool runs fn in an autorelease pool on one locked thread; the pool
// functions are bound here, only to measure whether the engine needs it.
func withPool(fn func()) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	p := poolPush()
	defer poolPop(p)
	fn()
}

func bindPool(t *testing.T) {
	t.Helper()
	lib, err := purego.Dlopen("/usr/lib/libobjc.A.dylib", purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		t.Skip(err)
	}
	purego.RegisterLibFunc(&poolPush, lib, "objc_autoreleasePoolPush")
	purego.RegisterLibFunc(&poolPop, lib, "objc_autoreleasePoolPop")
}

// pairLoop runs n encode and decode calls of a w by h stream and returns the
// RSS before (after a warm-up) and after.
func pairLoop(t *testing.T, os vtOS, w, h, n int, wrap func(func())) (before, after int64) {
	t.Helper()
	e, err := newVTEncoder(os, encParams{width: w, height: h, fps: 30, bps: int(0.07 * float64(w*h) * 30), profile: ConstrainedBaseline, keyInterval: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer e.close()
	d, err := newVTDecoder(os, decParams{maxPixels: DefaultMaxPixels})
	if err != nil {
		t.Fatal(err)
	}
	defer d.close()
	frames := make([][3][]byte, 8)
	for i := range frames {
		frames[i][0], frames[i][1], frames[i][2] = vtPattern(w, h, i)
	}
	for i := 0; i < n; i++ {
		if i == 500 {
			runtime.GC()
			before = processRSS(t)
		}
		f := frames[i%len(frames)]
		step := func() {
			var au realAU
			err := e.encode(f[0], f[1], f[2], w, w/2, int64(i)*3000, 3000, i%60 == 0, func(nals, params [][]byte, key bool) {
				au = realAU{nals: vtCloneParams(nals), params: vtCloneParams(params), key: key}
			})
			if err != nil {
				t.Fatalf("encode %d: %v", i, err)
			}
			dec := accessUnit{key: au.key, width: w, height: h}
			for _, nal := range au.nals {
				if k := nal[0] & 0x1f; k == 1 || k == 5 {
					dec.slices = append(dec.slices, nal)
				}
			}
			if au.key {
				dec.sps, dec.pps = au.params[:1], au.params[1:]
			}
			if _, ok, err := d.decode(dec); err != nil || !ok {
				t.Fatalf("decode %d: %v %v", i, ok, err)
			}
		}
		if wrap != nil {
			wrap(step)
		} else {
			step()
		}
	}
	runtime.GC()
	return before, processRSS(t)
}

func TestVTMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("10000 encode and decode calls")
	}
	os := realOS(t)
	bindPool(t)
	for _, c := range []struct {
		name string
		wrap func(func())
	}{{"no pool", nil}, {"autorelease pool per call", withPool}} {
		before, after := pairLoop(t, os, 640, 480, 10000, c.wrap)
		growth := after - before
		t.Logf("%s: RSS %.1f MB after 500 calls, %.1f MB after 10000, growth %.1f MB", c.name, float64(before)/1e6, float64(after)/1e6, float64(growth)/1e6)
		if growth > 10<<20 {
			t.Errorf("%s: RSS grew by %.1f MB", c.name, float64(growth)/1e6)
		}
	}
}

func TestVTPoolChoice(t *testing.T) {
	if testing.Short() {
		t.Skip("timing runs")
	}
	os := realOS(t)
	defer func(v bool) { vtUsePool = v }(vtUsePool)
	const rounds, n = 5, 60
	for _, sz := range [][2]int{{640, 480}, {1280, 720}, {1920, 1080}} {
		w, h := sz[0], sz[1]
		// Made once and cycled, so the runs do not regenerate every frame.
		frames := make([][3][]byte, 30)
		for i := range frames {
			frames[i][0], frames[i][1], frames[i][2] = vtPattern(w, h, i)
		}
		p50 := map[bool][]float64{}
		for round := 0; round < rounds; round++ {
			for _, pool := range []bool{round%2 == 0, round%2 != 0} {
				vtUsePool = pool
				e, err := newVTEncoder(os, encParams{width: w, height: h, fps: 30, bps: int(0.07 * float64(w*h) * 30), profile: ConstrainedBaseline, keyInterval: 2 * time.Second})
				if err != nil {
					t.Fatal(err)
				}
				var lat []float64
				for i := 0; i < n; i++ {
					f := frames[i%len(frames)]
					s := time.Now()
					if err := e.encode(f[0], f[1], f[2], w, w/2, int64(i)*3000, 3000, i == 0, func(_, _ [][]byte, _ bool) {}); err != nil {
						t.Fatal(err)
					}
					if i > 0 {
						lat = append(lat, float64(time.Since(s).Microseconds())/1000)
					}
				}
				e.close()
				p50[pool] = append(p50[pool], vtPercentile(lat, .5))
			}
		}
		t.Logf("%dx%d encode p50 over %d interleaved runs, median: pool %.2f ms (%.2f to %.2f), CVPixelBufferCreate %.2f ms (%.2f to %.2f)",
			w, h, rounds, vtPercentile(p50[true], .5), vtPercentile(p50[true], 0), vtPercentile(p50[true], 1),
			vtPercentile(p50[false], .5), vtPercentile(p50[false], 0), vtPercentile(p50[false], 1))
	}
}

func rtFrame(t testing.TB, w, h, i int, fps float64) *kolovideo.Frame {
	t.Helper()
	f, err := kolovideo.NewFrame(w, h)
	if err != nil {
		t.Fatal(err)
	}
	y, u, v := vtPattern(w, h, i)
	copy(f.Y, y)
	copy(f.U, u)
	copy(f.V, v)
	f.Timestamp = time.Duration(float64(i) / fps * float64(time.Second))
	f.Duration = time.Duration(float64(time.Second) / fps)
	return f
}

func lumaOf(f *kolovideo.Frame) []byte {
	b := make([]byte, 0, f.Width*f.Height)
	for r := 0; r < f.Height; r++ {
		b = append(b, f.Y[r*f.YStride:r*f.YStride+f.Width]...)
	}
	return b
}

// nalTypes returns the NAL unit types of an Annex B packet and fails when a
// start code is not 4 bytes.
func nalTypes(t testing.TB, b []byte) []int {
	t.Helper()
	units, err := splitAnnexB(b, 1<<16)
	if err != nil {
		t.Fatalf("packet does not split: %v", err)
	}
	if !bytes.HasPrefix(b, []byte{0, 0, 0, 1}) {
		t.Fatal("packet does not start with a 4-byte start code")
	}
	var types []int
	for _, u := range units {
		types = append(types, nalType(u))
	}
	return types
}

type rtResult struct {
	codecs        []string
	keys          []int
	minPSNR, mean float64
	encMs, decMs  []float64
	bytes         int
	hwEnc, hwDec  bool
	packets       []Packet
}

type rtOpts struct {
	w, h, n    int
	profile    Profile
	forceAt    int
	interval   time.Duration
	bitrate    int
	setRate    func(i int) int // bitrate to set before frame i, 0 for none
	keepPacket bool
}

// roundTrip encodes generated frames and decodes each packet, checking the
// packet shape on the way.
func roundTrip(t *testing.T, o rtOpts) rtResult {
	t.Helper()
	cfg := Config{Width: o.w, Height: o.h, Profile: o.profile, KeyframeInterval: o.interval, Bitrate: o.bitrate}
	enc, err := NewEncoder(cfg)
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	defer enc.Close()
	dec, err := NewDecoder(DecoderConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	r := rtResult{minPSNR: 99}
	sum := 0.0
	for i := 0; i < o.n; i++ {
		if o.setRate != nil {
			if bps := o.setRate(i); bps > 0 {
				if err := enc.SetBitrate(bps); err != nil {
					t.Fatal(err)
				}
			}
		}
		f := rtFrame(t, o.w, o.h, i, enc.Config().FrameRate)
		s := time.Now()
		pkts, err := enc.Encode(f, i == o.forceAt)
		r.encMs = append(r.encMs, float64(time.Since(s).Microseconds())/1000)
		if err != nil || len(pkts) != 1 {
			t.Fatalf("frame %d: %d packets, err %v", i, len(pkts), err)
		}
		p := pkts[0]
		r.bytes += len(p.Data)
		types := nalTypes(t, p.Data)
		for _, ty := range types {
			if ty != 1 && ty != 5 && ty != 7 && ty != 8 {
				t.Fatalf("frame %d: NAL type %d in a Kolo packet", i, ty)
			}
		}
		if p.Keyframe {
			r.keys = append(r.keys, i)
			if len(types) < 3 || types[0] != 7 || types[1] != 8 || types[2] != 5 {
				t.Fatalf("keyframe %d starts with NAL types %v, want 7 8 5", i, types)
			}
			sps, err := ParseKeyframe(p.Data)
			if err != nil {
				t.Fatalf("keyframe %d: %v", i, err)
			}
			if sps.Matrix != 6 || sps.FullRange {
				t.Errorf("keyframe %d: SPS not normalized: matrix %d full range %v", i, sps.Matrix, sps.FullRange)
			}
			if pr, ok := sps.Profile(); !ok || pr != o.profile && !(o.profile == 0 && pr == ConstrainedBaseline) {
				t.Errorf("keyframe %d: profile %v, requested %v", i, pr, o.profile)
			}
			if len(r.codecs) == 0 || r.codecs[len(r.codecs)-1] != sps.Codec() {
				r.codecs = append(r.codecs, sps.Codec())
			}
		} else if types[0] != 1 {
			t.Fatalf("delta %d starts with NAL type %d", i, types[0])
		}
		if o.keepPacket {
			r.packets = append(r.packets, Packet{Data: append([]byte(nil), p.Data...), Keyframe: p.Keyframe, Timestamp: p.Timestamp, Duration: p.Duration})
		}
		s = time.Now()
		got, err := dec.Decode(p)
		r.decMs = append(r.decMs, float64(time.Since(s).Microseconds())/1000)
		if err != nil || got == nil {
			t.Fatalf("decode %d: %v", i, err)
		}
		if got.Width != o.w || got.Height != o.h || got.Timestamp != p.Timestamp {
			t.Fatalf("decoded %dx%d at %v", got.Width, got.Height, got.Timestamp)
		}
		ps := vtLumaPSNR(lumaOf(f), lumaOf(got))
		sum += ps
		r.minPSNR = min(r.minPSNR, ps)
	}
	r.mean = sum / float64(o.n)
	r.hwEnc, r.hwDec = enc.Hardware(), dec.Hardware()
	return r
}

func TestVTRoundTrip(t *testing.T) {
	sizes := [][2]int{{640, 480}, {1280, 720}, {1920, 1080}}
	if testing.Short() {
		sizes = sizes[:1]
	}
	for _, sz := range sizes {
		for _, p := range []Profile{ConstrainedBaseline, Main, High} {
			t.Run(fmt.Sprintf("%dx%d %v", sz[0], sz[1], p), func(t *testing.T) {
				r := roundTrip(t, rtOpts{w: sz[0], h: sz[1], n: 90, profile: p, forceAt: 45})
				t.Logf("codec %v keyframes at %v hardware enc %v dec %v bitrate %.0f bit/s PSNR min %.2f mean %.2f encode p50 %.2f p99 %.2f decode p50 %.2f p99 %.2f ms",
					r.codecs, r.keys, r.hwEnc, r.hwDec, float64(r.bytes*8)*30/90, r.minPSNR, r.mean,
					vtPercentile(r.encMs, .5), vtPercentile(r.encMs, .99), vtPercentile(r.decMs, .5), vtPercentile(r.decMs, .99))
				if r.minPSNR < 35 {
					t.Errorf("luma PSNR %.2f dB", r.minPSNR)
				}
				if len(r.keys) != 2 || r.keys[0] != 0 || r.keys[1] != 45 {
					t.Errorf("keyframes at %v, want 0 and 45", r.keys)
				}
			})
		}
	}
}

func TestVTKeyframeInterval(t *testing.T) {
	r := roundTrip(t, rtOpts{w: 320, h: 240, n: 100, profile: ConstrainedBaseline, forceAt: -1, interval: time.Second})
	t.Logf("keyframes at %v", r.keys)
	if len(r.keys) < 3 || r.keys[0] != 0 {
		t.Fatalf("keyframes at %v", r.keys)
	}
	for i := 1; i < len(r.keys); i++ {
		if gap := r.keys[i] - r.keys[i-1]; gap < 29 || gap > 32 {
			t.Errorf("gap %d between keyframes at %v", gap, r.keys)
		}
	}
	none := roundTrip(t, rtOpts{w: 320, h: 240, n: 100, profile: ConstrainedBaseline, forceAt: 50, interval: -1})
	if len(none.keys) != 2 || none.keys[1] != 50 {
		t.Errorf("negative interval: keyframes at %v, want 0 and 50 only", none.keys)
	}
}

func TestVTSoftwarePath(t *testing.T) {
	realOS(t)
	vtNoHardware.Store(true)
	defer vtNoHardware.Store(false)
	want := map[Profile]string{ConstrainedBaseline: "avc3.42e01f", Main: "avc3.4d401f"}
	for _, p := range []Profile{ConstrainedBaseline, Main} {
		r := roundTrip(t, rtOpts{w: 1280, h: 720, n: 60, profile: p, forceAt: 30})
		t.Logf("%v: codec %v hardware enc %v dec %v PSNR min %.2f encode p50 %.2f decode p50 %.2f ms", p, r.codecs, r.hwEnc, r.hwDec, r.minPSNR, vtPercentile(r.encMs, .5), vtPercentile(r.decMs, .5))
		if r.hwEnc || r.hwDec {
			t.Errorf("%v: hardware reported with hardware disabled", p)
		}
		if len(r.codecs) != 1 || r.codecs[0] != want[p] {
			t.Errorf("%v: codec %v, want %s", p, r.codecs, want[p])
		}
		if r.minPSNR < 35 {
			t.Errorf("%v: PSNR %.2f", p, r.minPSNR)
		}
	}
}

func TestVTLatency(t *testing.T) {
	for _, sz := range [][2]int{{640, 480}, {1280, 720}, {1920, 1080}} {
		r := roundTrip(t, rtOpts{w: sz[0], h: sz[1], n: 120, profile: ConstrainedBaseline, forceAt: -1})
		t.Logf("%dx%d encode p50 %.2f p99 %.2f max %.2f ms, decode p50 %.2f p99 %.2f max %.2f ms",
			sz[0], sz[1], vtPercentile(r.encMs, .5), vtPercentile(r.encMs, .99), vtPercentile(r.encMs, 1),
			vtPercentile(r.decMs, .5), vtPercentile(r.decMs, .99), vtPercentile(r.decMs, 1))
	}
}

func TestVTBitrateChange(t *testing.T) {
	const w, h, n, at = 1280, 720, 240, 120
	const before, after = 1935360, 500000
	for _, hw := range []bool{true, false} {
		vtNoHardware.Store(!hw)
		r := roundTrip(t, rtOpts{w: w, h: h, n: n, profile: ConstrainedBaseline, bitrate: before, forceAt: -1, keepPacket: true,
			setRate: func(i int) int {
				if i == at {
					return after
				}
				return 0
			}})
		vtNoHardware.Store(false)
		var pre, post, npost int
		for i, p := range r.packets {
			if i < at {
				pre += len(p.Data)
			} else if i >= at+15 {
				post += len(p.Data)
				npost++
			}
		}
		got := float64(post*8) * 30 / float64(npost)
		t.Logf("hardware=%v: %.0f bit/s before (target %d), %.0f bit/s from 0.5 s after the change (target %d)", r.hwEnc, float64(pre*8)*30/at, before, got, after)
		if r.hwEnc && (got < after*0.75 || got > after*1.25) {
			t.Errorf("hardware encoder at %.0f bit/s, target %d", got, after)
		}
	}
}

// mutate returns a damaged copy of b: random bytes, a truncation or flipped bits.
func mutate(rnd *rand.Rand, b []byte) []byte {
	b = append([]byte(nil), b...)
	switch rnd.Intn(3) {
	case 0:
		for m := 0; m < 1+rnd.Intn(8); m++ {
			b[4+rnd.Intn(len(b)-4)] = byte(rnd.Intn(256))
		}
	case 1:
		b = b[:4+rnd.Intn(len(b)-4)]
	default:
		for m := 0; m < 1+rnd.Intn(4); m++ {
			b[rnd.Intn(len(b))] ^= 1 << rnd.Intn(8)
		}
	}
	return b
}

func TestVTCorrupt(t *testing.T) {
	r := roundTrip(t, rtOpts{w: 640, h: 480, n: 90, profile: ConstrainedBaseline, forceAt: 45, keepPacket: true})
	dec, err := NewDecoder(DecoderConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	for _, p := range r.packets[:3] {
		if _, err := dec.Decode(p); err != nil {
			t.Fatal(err)
		}
	}
	rnd := rand.New(rand.NewSource(1))
	counts := map[string]int{}
	pictures := 0
	for k := 0; k < 3000; k++ {
		p := r.packets[rnd.Intn(len(r.packets))]
		p.Data = mutate(rnd, p.Data)
		f, err := dec.Decode(p)
		switch {
		case err == nil && f != nil:
			pictures++
		case err == nil:
			counts["no picture"]++
		default:
			var kind string
			for _, e := range []error{ErrCorrupt, ErrTooLarge, ErrNeedKeyframe, ErrUnsupportedStream, ErrCodec, ErrUnavailable} {
				if errors.Is(err, e) {
					kind = e.Error()
				}
			}
			if kind == "" {
				t.Fatalf("untyped error: %v", err)
			}
			counts[kind]++
		}
	}
	t.Logf("3000 mutated packets: %d pictures, %v", pictures, counts)
	ok := 0
	for _, p := range r.packets {
		if f, err := dec.Decode(p); err == nil && f != nil {
			ok++
		}
	}
	if ok != len(r.packets) {
		t.Errorf("after the mutations the clean stream decodes %d of %d pictures", ok, len(r.packets))
	}
}

func TestVTSizeChange(t *testing.T) {
	dec, err := NewDecoder(DecoderConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	for _, sz := range [][2]int{{320, 240}, {640, 480}, {160, 96}, {320, 240}} {
		enc, err := NewEncoder(Config{Width: sz[0], Height: sz[1]})
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 5; i++ {
			pkts, err := enc.Encode(rtFrame(t, sz[0], sz[1], i, 30), false)
			if err != nil || len(pkts) != 1 {
				t.Fatal(len(pkts), err)
			}
			f, err := dec.Decode(pkts[0])
			if err != nil || f.Width != sz[0] || f.Height != sz[1] {
				t.Fatalf("%v frame %d: %v", sz, i, err)
			}
		}
		enc.Close()
	}
}

// TestVTCropBelowMaxPixels is a real 4096x2304 High stream whose SPS is
// rewritten to crop it to 48x48. A decoder bounded to 4096 pixels must refuse
// it before VideoToolbox decodes and allocates the coded size.
func TestVTCropBelowMaxPixels(t *testing.T) {
	realOS(t)
	enc, err := NewEncoder(Config{Width: 4096, Height: 2304, FrameRate: 25, Profile: High})
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	var pkts []Packet
	for i := range 3 {
		p, err := enc.Encode(rtFrame(t, 4096, 2304, i, 25), false)
		if err != nil || len(p) != 1 {
			t.Fatal(len(p), err)
		}
		pkts = append(pkts, p[0])
	}
	nals, err := splitAnnexB(pkts[0].Data, maxNALs(DefaultMaxPixels))
	if err != nil {
		t.Fatal(err)
	}
	s, err := parseSPS(nals[0], DefaultMaxPixels)
	if err != nil {
		t.Fatal(err)
	}
	ts := testSPS{header: nals[0][0], f: s.f}
	ts.f.cropping, ts.f.cropR, ts.f.cropB = true, uint32((16*s.widthMbs-48)/2), uint32((16*s.heightMbs-48)/2)
	key := Packet{Data: appendAnnexB(nil, [][]byte{ts.nal()}, nals[1:])}

	small, err := NewDecoder(DecoderConfig{MaxPixels: 64 * 64})
	if err != nil {
		t.Fatal(err)
	}
	defer small.Close()
	if f, err := small.Decode(key); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("MaxPixels 4096: frame %v, err %v", f != nil, err)
	}
	for _, p := range pkts[1:] {
		if _, err := small.Decode(p); !errors.Is(err, ErrNeedKeyframe) {
			t.Fatalf("delta after the refusal: %v", err)
		}
	}
	dec, err := NewDecoder(DecoderConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	for i, p := range append([]Packet{key}, pkts[1:]...) {
		f, err := dec.Decode(p)
		if err != nil || f == nil || f.Width != 48 || f.Height != 48 {
			t.Fatalf("default bound, packet %d: %v", i, err)
		}
	}
}

func TestVTOpenClose(t *testing.T) {
	realOS(t)
	base := vtLive()
	cycles := 100
	if testing.Short() {
		cycles = 10
	}
	for i := 0; i < cycles; i++ {
		enc, err := NewEncoder(Config{Width: 320, Height: 240})
		if err != nil {
			t.Fatal(err)
		}
		dec, err := NewDecoder(DecoderConfig{})
		if err != nil {
			t.Fatal(err)
		}
		pkts, err := enc.Encode(rtFrame(t, 320, 240, i, 30), false)
		if err != nil || len(pkts) != 1 {
			t.Fatal(err)
		}
		if _, err := dec.Decode(pkts[0]); err != nil {
			t.Fatal(err)
		}
		enc.Close()
		dec.Close()
		enc.Close()
		dec.Close()
	}
	if vtLive() != base {
		t.Errorf("%d session handles left", vtLive()-base)
	}
}

func TestVTCapabilitiesReal(t *testing.T) {
	realOS(t)
	l := (&vtEngine{load: loadVT}).caps()
	t.Logf("encode %+v\ndecode %+v", l.Encode, l.Decode)
	if !l.Encode.Available || !l.Decode.Available || l.Encode.Engine != "VideoToolbox" || len(l.Encode.Profiles) != 3 {
		t.Errorf("%+v", l)
	}
	if got := Capabilities(); got.Encode.Available != l.Encode.Available || got.Decode.Hardware != l.Decode.Hardware {
		t.Errorf("Capabilities %+v", got)
	}
}

type psRow struct {
	cpu  float64
	comm string
}

// psTimes returns the CPU seconds and command of every process.
func psTimes(t *testing.T) map[int]psRow {
	t.Helper()
	out, err := exec.Command("ps", "-axo", "pid=,time=,comm=").Output()
	if err != nil {
		t.Skipf("ps: %v", err)
	}
	m := map[int]psRow{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		pid, err := strconv.Atoi(f[0])
		if err != nil {
			continue
		}
		var secs float64
		for _, part := range strings.Split(f[1], ":") {
			v, _ := strconv.ParseFloat(part, 64)
			secs = secs*60 + v
		}
		m[pid] = psRow{secs, strings.Join(f[2:], " ")}
	}
	return m
}

// vtServiceCPU sums the CPU seconds of the VideoToolbox XPC services by kind
// and lists the pids that did not exist in before.
func vtServiceCPU(now, before map[int]psRow) (enc, dec float64, fresh []int) {
	for pid, r := range now {
		switch {
		case strings.HasSuffix(r.comm, "VTEncoderXPCService"):
			enc += r.cpu - before[pid].cpu
		case strings.HasSuffix(r.comm, "VTDecoderXPCService"):
			dec += r.cpu - before[pid].cpu
		default:
			continue
		}
		if _, ok := before[pid]; !ok {
			fresh = append(fresh, pid)
		}
	}
	return
}

// TestVTProcesses records which process does the coding on each path: the
// CPU time of the application and of the VideoToolbox XPC services over a
// run of encode and decode calls, and the services that appear.
func TestVTProcesses(t *testing.T) {
	realOS(t)
	const w, h, n = 1920, 1080, 150
	for _, hw := range []bool{true, false} {
		vtNoHardware.Store(!hw)
		before := psTimes(t)
		enc, err := NewEncoder(Config{Width: w, Height: h})
		if err != nil {
			t.Fatal(err)
		}
		dec, err := NewDecoder(DecoderConfig{})
		if err != nil {
			t.Fatal(err)
		}
		start := psTimes(t)
		for i := 0; i < n; i++ {
			pkts, err := enc.Encode(rtFrame(t, w, h, i, 30), false)
			if err != nil || len(pkts) != 1 {
				t.Fatal(err)
			}
			if _, err := dec.Decode(pkts[0]); err != nil {
				t.Fatal(err)
			}
		}
		end := psTimes(t)
		pid := os.Getpid()
		se, sd, fresh := vtServiceCPU(end, start)
		t.Logf("hardware allowed=%v (encoder %v, decoder %v): over %d frames at %dx%d the application used %.2f s of CPU, the encoder service %.2f s, the decoder service %.2f s; services started since before the sessions: %v",
			hw, enc.Hardware(), dec.Hardware(), n, w, h, end[pid].cpu-start[pid].cpu, se, sd, func() []int { _, _, f := vtServiceCPU(start, before); return f }())
		_ = fresh
		enc.Close()
		dec.Close()
		time.Sleep(3 * time.Second)
		_, _, _ = vtServiceCPU(psTimes(t), before)
		alive := 0
		for _, r := range psTimes(t) {
			if strings.HasSuffix(r.comm, "VTEncoderXPCService") || strings.HasSuffix(r.comm, "VTDecoderXPCService") {
				alive++
			}
		}
		t.Logf("3 s after Close, %d VideoToolbox XPC service processes exist on the machine (all clients)", alive)
		vtNoHardware.Store(false)
	}
}

func TestVTInterop(t *testing.T) {
	realOS(t)
	files, _ := filepath.Glob(filepath.Join("testdata", "interop-*.bin"))
	if len(files) == 0 {
		t.Skip("no interop files")
	}
	for _, name := range files {
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		pkts, err := vtReadStream(b)
		if err != nil || len(pkts) != 30 {
			t.Fatalf("%s: %d packets, %v", name, len(pkts), err)
		}
		dec, err := NewDecoder(DecoderConfig{})
		if err != nil {
			t.Fatal(err)
		}
		minP := 99.0
		for i, p := range pkts {
			f, err := dec.Decode(p)
			if err != nil || f == nil {
				t.Fatalf("%s packet %d: %v", name, i, err)
			}
			minP = min(minP, vtLumaPSNR(vtPatternY(320, 240, i), lumaOf(f)))
		}
		dec.Close()
		t.Logf("%s: 30 of 30 decoded, luma PSNR min %.2f dB", filepath.Base(name), minP)
		if minP < 35 {
			t.Errorf("%s: PSNR %.2f", name, minP)
		}
	}
}

func vtPatternY(w, h, i int) []byte { y, _, _ := vtPattern(w, h, i); return y }

// TestVTExportInterop writes the interop files into the directory named by
// KOLOVIDEO_H264_INTEROP_OUT; it does nothing otherwise.
func TestVTExportInterop(t *testing.T) {
	dir := os.Getenv("KOLOVIDEO_H264_INTEROP_OUT")
	if dir == "" {
		t.Skip("KOLOVIDEO_H264_INTEROP_OUT is not set")
	}
	names := map[Profile]string{ConstrainedBaseline: "cb", Main: "main", High: "high"}
	for p, n := range names {
		r := roundTrip(t, rtOpts{w: 320, h: 240, n: 30, profile: p, forceAt: -1, keepPacket: true})
		var buf bytes.Buffer
		if err := vtWriteStream(&buf, r.packets); err != nil {
			t.Fatal(err)
		}
		name := filepath.Join(dir, "interop-vt-320x240-"+n+".bin")
		if err := os.WriteFile(name, buf.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Logf("%s: %d bytes, codec %v", name, buf.Len(), r.codecs)
	}
}

// The symbols resolve, and the two callbacks are made once for the process.
func TestVTBindings(t *testing.T) {
	realOS(t)
	for p, v := range vtProps {
		if v == 0 {
			t.Errorf("property %d is not bound", p)
		}
	}
	for l, v := range vtLevels {
		if v == 0 {
			t.Errorf("profile level %d is not bound", l)
		}
	}
	for name, v := range map[string]uintptr{"encoder callback": vtEncCallback, "decoder callback": vtDecCallback, "true": cfTrue, "false": cfFalse, "key callbacks": dictKeyCB, "value callbacks": dictValCB} {
		if v == 0 {
			t.Errorf("%s is not bound", name)
		}
	}
	enc, dec := vtEncCallback, vtDecCallback
	for i := 0; i < 3; i++ {
		if _, err := loadVT(); err != nil {
			t.Fatal(err)
		}
	}
	if vtEncCallback != enc || vtDecCallback != dec {
		t.Error("a second load made new callbacks")
	}
	for i := 0; i < 20; i++ {
		e, err := newVTEncoder(vtInst, encParams{width: 64, height: 48, fps: 30, bps: 1e5, profile: Main, keyInterval: -1})
		if err != nil {
			t.Fatal(err)
		}
		e.close()
	}
	if vtEncCallback != enc {
		t.Error("sessions made new callbacks")
	}
}
