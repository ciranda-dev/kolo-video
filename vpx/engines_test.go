package vpx

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/ciranda-dev/kolo-video/vpx/internal/native"
)

// testCache is a private directory for this test binary: the cache refuses a
// directory that other users can write to, and the shared temp directory is one.
var testCache = func() string {
	d, err := os.MkdirTemp("", "kolovideo-vpx-test-")
	if err != nil {
		panic(err)
	}
	return d
}()

func TestMain(m *testing.M) {
	code := m.Run()
	os.RemoveAll(testCache) // fails for a library that is still loaded on Windows
	os.Exit(code)
}

type engineResult struct {
	eng engine
	err error
}

var (
	testEngineMu sync.Mutex
	testEngines  = map[EngineKind]engineResult{}
)

// testEngine loads an engine once for the test binary; the native one from the
// embedded library through a cache directory under the temp directory.
func testEngine(kind EngineKind) (engine, error) {
	testEngineMu.Lock()
	defer testEngineMu.Unlock()
	if r, ok := testEngines[kind]; ok {
		return r.eng, r.err
	}
	var r engineResult
	if kind == EngineNative {
		r.eng, r.err = loadNative(LoadConfig{CacheDir: testCache})
	} else {
		r.eng, r.err = loadPortable()
	}
	testEngines[kind] = r
	return r.eng, r.err
}

// useEngine makes kind the active engine until the test ends. A native engine
// that this build does not have skips the test.
func useEngine(t testing.TB, kind EngineKind) {
	t.Helper()
	eng, err := testEngine(kind)
	if errors.Is(err, native.ErrUnsupported) {
		t.Skip("no native engine in this build: ", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	setLoaded(t, &loadState{LoadConfig{Engine: kind, CacheDir: testCache}, Runtime{Engine: kind, Version: eng.version()}, eng})
}

// setLoaded replaces the loaded state until the test ends.
func setLoaded(t testing.TB, st *loadState) {
	t.Helper()
	loadMu.Lock()
	prev := loaded
	loaded = st
	loadMu.Unlock()
	t.Cleanup(func() {
		loadMu.Lock()
		loaded = prev
		loadMu.Unlock()
	})
}

// eachEngine runs f once per engine. The tests do not run in parallel, because
// the active engine is process-wide.
func eachEngine(t *testing.T, f func(t *testing.T)) {
	for _, kind := range []EngineKind{EngineNative, EnginePortable} {
		t.Run(kind.String(), func(t *testing.T) {
			useEngine(t, kind)
			f(t)
		})
	}
}

// stream encodes n test frames with one engine at a fixed thread count and
// returns the packets.
func stream(t *testing.T, eng engine, p encParams, n int) []Packet {
	t.Helper()
	enc, err := eng.newEncoder(p)
	if err != nil {
		t.Fatal(err)
	}
	defer enc.close()
	var out []Packet
	for i := range n {
		f := testFrame(t, p.width, p.height, i)
		err := enc.encode(f.Y, f.U, f.V, f.YStride, f.UVStride, int64(i)*3000, 3000, i == 0, func(d []byte, key bool) {
			out = append(out, Packet{Data: append([]byte(nil), d...), Keyframe: key})
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return out
}

func harnessParams(vp9 bool, w, h, threads int) encParams {
	p := encParams{vp9: vp9, width: w, height: h, threads: threads, speed: -6, kbps: 2000, maxIntraPct: 900}
	switch {
	case vp9:
		p.speed = 7
	case w*h < 352*288:
		p.speed = -4
	}
	return p
}

// TestEnginesAgree checks that one-thread streams are byte-identical on the two
// engines and that each engine decodes the other's stream to the same pictures.
func TestEnginesAgree(t *testing.T) {
	nat, err := testEngine(EngineNative)
	if errors.Is(err, native.ErrUnsupported) {
		t.Skip("no native engine in this build")
	}
	if err != nil {
		t.Fatal(err)
	}
	port, err := testEngine(EnginePortable)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		vp9  bool
		w, h int
	}{{false, 320, 240}, {false, 161, 99}, {true, 320, 240}, {true, 161, 99}, {false, 640, 480}, {true, 640, 480}} {
		t.Run(fmt.Sprintf("vp9=%v/%dx%d", c.vp9, c.w, c.h), func(t *testing.T) {
			p := harnessParams(c.vp9, c.w, c.h, 1)
			a, b := stream(t, nat, p, 30), stream(t, port, p, 30)
			if len(a) != len(b) {
				t.Fatalf("%d native packets, %d portable", len(a), len(b))
			}
			for i := range a {
				if a[i].Keyframe != b[i].Keyframe || !bytes.Equal(a[i].Data, b[i].Data) {
					t.Fatalf("packet %d differs", i)
				}
			}
			dn, err := nat.newDecoder(c.vp9, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer dn.close()
			dp, err := port.newDecoder(c.vp9, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer dp.close()
			for i, pk := range a {
				x, okx, errx := dn.decode(pk.Data)
				y, oky, erry := dp.decode(pk.Data)
				if errx != nil || erry != nil || okx != oky {
					t.Fatalf("packet %d: %v %v %v %v", i, okx, oky, errx, erry)
				}
				if okx && !samePicture(x, y) {
					t.Fatalf("packet %d decodes differently", i)
				}
			}
		})
	}
}

func samePicture(a, b plane420) bool {
	if a.width != b.width || a.height != b.height {
		return false
	}
	cw, ch := (a.width+1)/2, (a.height+1)/2
	for y := range a.height {
		if !bytes.Equal(a.y[y*a.yStride:][:a.width], b.y[y*b.yStride:][:b.width]) {
			return false
		}
	}
	for y := range ch {
		if !bytes.Equal(a.u[y*a.uvStride:][:cw], b.u[y*b.uvStride:][:cw]) || !bytes.Equal(a.v[y*a.uvStride:][:cw], b.v[y*b.uvStride:][:cw]) {
			return false
		}
	}
	return true
}

// TestPortable720p checks that the portable engine, which is not real time at
// this size, still produces a valid stream, and that rate control drops frames
// when the bitrate is below what the picture needs.
func TestPortable720p(t *testing.T) {
	useEngine(t, EnginePortable)
	for _, codec := range []Codec{VP8, VP9} {
		t.Run(codec.String(), func(t *testing.T) {
			enc, err := NewEncoder(Config{Codec: codec, Width: 1280, Height: 720, Bitrate: 300_000})
			if err != nil {
				t.Fatal(err)
			}
			defer enc.Close()
			dec := newDecoder(t, DecoderConfig{Codec: codec})
			frames, packets, shown := 24, 0, 0
			for i := range frames {
				pkts, err := enc.Encode(testFrame(t, 1280, 720, i), false)
				if err != nil {
					t.Fatal(err)
				}
				for _, p := range pkts {
					packets++
					f, err := dec.Decode(p)
					if err != nil {
						t.Fatalf("packet %d: %v", packets, err)
					}
					if f != nil {
						if f.Width != 1280 || f.Height != 720 {
							t.Fatalf("decoded %dx%d", f.Width, f.Height)
						}
						shown++
					}
				}
			}
			t.Logf("%d frames in, %d packets, %d pictures shown", frames, packets, shown)
			if packets < 2 || shown != packets {
				t.Fatalf("%d packets, %d shown", packets, shown)
			}
			if packets >= frames {
				t.Fatalf("no frame was dropped at 300 kbit/s: %d packets for %d frames", packets, frames)
			}
		})
	}
}

// TestPortableTraps feeds corrupt VP9 packets to the portable engine until one
// traps, then checks the decoder returns ErrCorrupt and decodes the next
// keyframe.
func TestPortableTraps(t *testing.T) {
	eng, err := testEngine(EnginePortable)
	if err != nil {
		t.Fatal(err)
	}
	pkts := stream(t, eng, harnessParams(true, 160, 120, 1), 10)
	dec, err := eng.newDecoder(true, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer dec.close()
	r := rand.New(rand.NewPCG(1, 2))
	traps := 0
	for range 300 {
		for _, p := range pkts[:3] {
			if _, _, err := dec.decode(p.Data); err != nil {
				t.Fatal(err)
			}
		}
		bad := append([]byte(nil), pkts[3].Data...)
		for range 1 + r.IntN(8) {
			bad[r.IntN(len(bad))] ^= byte(1 + r.IntN(255))
		}
		if _, _, err := dec.decode(bad); err != nil {
			if strings.Contains(err.Error(), "trapped") {
				traps++
				if !errors.Is(err, ErrCorrupt) {
					t.Fatalf("trap maps to %v", err)
				}
			}
			if _, ok, err := dec.decode(pkts[0].Data); err != nil || !ok {
				t.Fatalf("keyframe after an error: %v %v", ok, err)
			}
		}
	}
	t.Logf("%d traps in 300 corrupt packets", traps)
	if traps == 0 {
		t.Fatal("no corrupt packet trapped")
	}
}

// TestThreadStreams records what the thread count does to a stream: for one
// engine, size and thread count the stream is the same every time, so a
// default derived from the CPU count makes streams differ between machines,
// not between runs.
func TestThreadStreams(t *testing.T) {
	useEngine(t, EngineNative)
	eng, err := testEngine(EngineNative)
	if err != nil {
		t.Fatal(err)
	}
	for _, vp9 := range []bool{false, true} {
		for _, threads := range []int{1, 2, 4} {
			p := harnessParams(vp9, 640, 480, threads)
			sum := func() [32]byte {
				h := sha256.New()
				for _, pk := range stream(t, eng, p, 12) {
					h.Write(pk.Data)
				}
				return [32]byte(h.Sum(nil))
			}
			first, second := sum(), sum()
			if first != second {
				t.Errorf("vp9=%v threads=%d: two runs gave different streams", vp9, threads)
			}
		}
	}
}
