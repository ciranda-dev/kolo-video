package h264

import (
	"bytes"
	"sync"
	"testing"
)

// useEngine swaps the engine and the cached capabilities for one test.
// Tests that call it must not run in parallel.
func useEngine(t testing.TB, e engine) {
	t.Helper()
	engines.Lock()
	oldEng, oldCaps := engines.eng, engines.caps
	engines.eng, engines.caps = e, nil
	engines.Unlock()
	t.Cleanup(func() {
		engines.Lock()
		engines.eng, engines.caps = oldEng, oldCaps
		engines.Unlock()
	})
}

// fakeEngine is an engine whose encoders and decoders run in Go.
type fakeEngine struct {
	mu        sync.Mutex
	limits    Limits
	capsCalls int
	encErr    error
	decErr    error
	setupEnc  func(*fakeEnc)
	setupDec  func(*fakeDec)
	encs      []*fakeEnc
	decs      []*fakeDec
}

func (e *fakeEngine) name() string { return "fake" }

func (e *fakeEngine) caps() Limits {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.capsCalls++
	return e.limits
}

func (e *fakeEngine) newEncoder(p encParams) (encImpl, error) {
	if e.encErr != nil {
		return nil, e.encErr
	}
	f := &fakeEnc{p: p, sliceCount: 1, inband: true, sliceFrameBits: 4}
	switch p.profile {
	case Main:
		f.sps = testSPSFor(p.width, p.height, 77, 0x40, 51)
	case High:
		f.sps = testSPSFor(p.width, p.height, 100, 0, 51)
	default:
		f.sps = testSPSFor(p.width, p.height, 66, 0xc0, 51)
	}
	f.pps = testPPS(0, 0)
	if e.setupEnc != nil {
		e.setupEnc(f)
	}
	e.mu.Lock()
	e.encs = append(e.encs, f)
	e.mu.Unlock()
	return f, nil
}

func (e *fakeEngine) newDecoder(p decParams) (decImpl, error) {
	if e.decErr != nil {
		return nil, e.decErr
	}
	d := &fakeDec{p: p}
	if e.setupDec != nil {
		e.setupDec(d)
	}
	e.mu.Lock()
	e.decs = append(e.decs, d)
	e.mu.Unlock()
	return d, nil
}

type encCall struct {
	pts, dur int64
	key      bool
}

// fakeEnc emits one access unit per frame: optional extra units, SPS and PPS
// in IDR units when inband is set, and sliceCount slices.
type fakeEnc struct {
	p          encParams
	sps, pps   []byte
	inband     bool     // SPS and PPS inside IDR access units
	preParams  bool     // params returns SPS and PPS before the first frame
	extra      [][]byte // units put first in every access unit
	sliceCount int
	// sliceFrameBits and slicePocBits match the SPS: 4 and 0 for testSPSFor.
	sliceFrameBits, slicePocBits int
	ignoreKeys                   int          // key requests to ignore
	autoIDR                      map[int]bool // calls where the system adds an IDR on its own
	drop                         map[int]bool // calls without output
	twice                        bool         // emit two access units
	bSlices                      bool
	edit                         func(nals [][]byte) [][]byte
	encodeErr                    error
	hw                           bool

	calls    []encCall
	frameNum uint32
	idrID    uint32
	bps      []int
	closed   int
}

func (f *fakeEnc) params() [][]byte {
	if f.preParams {
		return [][]byte{f.sps, f.pps}
	}
	return nil
}

func (f *fakeEnc) encode(y, u, v []byte, yStride, uvStride int, pts, dur int64, key bool, emit func(nals, params [][]byte, key bool)) error {
	n := len(f.calls)
	f.calls = append(f.calls, encCall{pts, dur, key})
	if f.encodeErr != nil {
		return f.encodeErr
	}
	if f.drop[n] {
		return nil
	}
	if key && f.ignoreKeys > 0 {
		f.ignoreKeys--
		key = false
	}
	idr := key || f.autoIDR[n]
	nals := append([][]byte(nil), f.extra...)
	if idr {
		f.frameNum = 0
		f.idrID++
		if f.inband {
			nals = append(nals, f.sps, f.pps)
		}
	} else {
		f.frameNum = (f.frameNum + 1) % (1 << f.sliceFrameBits)
	}
	mbs := uint32(((f.p.width + 15) / 16) * ((f.p.height + 15) / 16))
	for i := range f.sliceCount {
		st := uint32(7)
		if !idr {
			st = 5
			if f.bSlices {
				st = 6
			}
		}
		nals = append(nals, testSliceHeader{idr: idr, refIDC: 2, firstMb: uint32(i) * mbs / uint32(f.sliceCount), sliceType: st,
			frameNum: f.frameNum, frameBits: f.sliceFrameBits, idrPicID: f.idrID, pocLsb: uint32(2 * n), pocBits: f.slicePocBits}.nal())
	}
	if f.edit != nil {
		nals = f.edit(nals)
	}
	params := [][]byte{f.sps, f.pps}
	emit(nals, params, idr)
	if f.twice {
		emit(nals, params, idr)
	}
	return nil
}

func (f *fakeEnc) setBitrate(bps int) error { f.bps = append(f.bps, bps); return nil }
func (f *fakeEnc) hardware() bool           { return f.hw }
func (f *fakeEnc) close()                   { f.closed++ }

// fakeDec records each access unit and returns a picture with a pattern.
type fakeDec struct {
	p      decParams
	aus    []accessUnit
	layout func(au accessUnit) picture // nil: tight I420 of the cropped size
	err    error
	noPic  bool
	hw     bool
	closed int
}

func (d *fakeDec) decode(au accessUnit) (picture, bool, error) {
	c := au
	c.sps, c.pps, c.slices = cloneUnits(au.sps), cloneUnits(au.pps), cloneUnits(au.slices)
	d.aus = append(d.aus, c)
	if d.err != nil {
		return picture{}, false, d.err
	}
	if d.noPic {
		return picture{}, false, nil
	}
	if d.layout != nil {
		return d.layout(au), true, nil
	}
	return patternPicture(au.width, au.height, au.width, au.height), true, nil
}

func (d *fakeDec) hardware() bool { return d.hw }
func (d *fakeDec) close()         { d.closed++ }

func cloneUnits(u [][]byte) [][]byte {
	var out [][]byte
	for _, b := range u {
		out = append(out, bytes.Clone(b))
	}
	return out
}

// patternPicture returns an I420 buffer of frameW x frameH whose top-left
// w x h corner holds a pattern, with chroma planes after the padded height.
func patternPicture(w, h, frameW, frameH int) picture {
	y := make([]byte, frameW*frameH)
	cw, ch := frameW/2, frameH/2
	u, v := make([]byte, cw*ch), make([]byte, cw*ch)
	for r := range frameH {
		for c := range frameW {
			y[r*frameW+c] = lumaAt(c, r)
		}
	}
	for r := range ch {
		for c := range cw {
			u[r*cw+c], v[r*cw+c] = byte(2*c+r), byte(c+3*r)
		}
	}
	return picture{width: frameW, height: frameH, y: y, u: u, v: v, yStride: frameW, uvStride: cw}
}

func lumaAt(x, y int) byte { return byte(x + 7*y) }
