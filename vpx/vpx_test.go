package vpx

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"sync"
	"testing"
	"time"

	kolovideo "github.com/ciranda-dev/kolo-video"
)

// testFrame draws a moving gradient with a bright square, so successive
// frames differ like camera video.
func testFrame(t testing.TB, w, h, n int) *kolovideo.Frame {
	t.Helper()
	f, err := kolovideo.NewFrame(w, h)
	if err != nil {
		t.Fatal(err)
	}
	for y := range h {
		for x := range w {
			v := 16 + (x+y+2*n)%200
			if x >= n%w && x < n%w+w/8 && y >= h/4 && y < h/4+h/8 {
				v = 230
			}
			f.Y[y*f.YStride+x] = byte(v)
		}
	}
	for y := range (h + 1) / 2 {
		for x := range (w + 1) / 2 {
			f.U[y*f.UVStride+x] = byte(64 + (x+n)%128)
			f.V[y*f.UVStride+x] = byte(64 + (y+n)%128)
		}
	}
	f.Timestamp = time.Duration(n) * time.Second / 30
	f.Duration = time.Second / 30
	return f
}

func psnr(a, b *kolovideo.Frame) float64 {
	var sum float64
	for y := range a.Height {
		for x := range a.Width {
			d := float64(a.Y[y*a.YStride+x]) - float64(b.Y[y*b.YStride+x])
			sum += d * d
		}
	}
	mse := sum / float64(a.Width*a.Height)
	if mse == 0 {
		return math.Inf(1)
	}
	return 10 * math.Log10(255*255/mse)
}

func newDecoder(t testing.TB, cfg DecoderConfig) *Decoder {
	t.Helper()
	dec, err := NewDecoder(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dec.Close() })
	return dec
}

func encodeAll(t testing.TB, cfg Config, n int) []Packet {
	t.Helper()
	enc, err := NewEncoder(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	var out []Packet
	for i := range n {
		pkts, err := enc.Encode(testFrame(t, cfg.Width, cfg.Height, i), false)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, pkts...)
	}
	return out
}

// roundTripGoldens are the one-thread results of the harness stream at 2 Mbit/s
// over 60 frames, measured once and equal on both engines and every platform.
var roundTripGoldens = []struct {
	codec Codec
	w, h  int
	kbps  int
	psnr  float64
}{
	{VP8, 320, 240, 882, 50.12},
	{VP8, 161, 99, 180, 50.98},
	{VP9, 320, 240, 288, 51.50},
	{VP9, 161, 99, 89, 48.26},
}

func TestRoundTrip(t *testing.T) {
	eachEngine(t, func(t *testing.T) {
		for _, g := range roundTripGoldens {
			t.Run(fmt.Sprintf("%v/%dx%d", g.codec, g.w, g.h), func(t *testing.T) {
				codec, w, h := g.codec, g.w, g.h
				enc, err := NewEncoder(Config{Codec: codec, Width: w, Height: h, Bitrate: 2_000_000})
				if err != nil {
					t.Fatal(err)
				}
				defer enc.Close()
				dec := newDecoder(t, DecoderConfig{Codec: codec})
				minPSNR := math.Inf(1)
				decoded, bytes := 0, 0
				for i := range 60 {
					in := testFrame(t, w, h, i)
					pkts, err := enc.Encode(in, false)
					if err != nil {
						t.Fatal(err)
					}
					if i == 0 && (len(pkts) == 0 || !pkts[0].Keyframe) {
						t.Fatal("first packet is not a keyframe")
					}
					for _, p := range pkts {
						if p.Timestamp != in.Timestamp {
							t.Fatalf("packet timestamp %v, frame %v", p.Timestamp, in.Timestamp)
						}
						bytes += len(p.Data)
						out, err := dec.Decode(p)
						if err != nil {
							t.Fatalf("frame %d: %v", i, err)
						}
						if out == nil {
							continue
						}
						if err := out.Validate(); err != nil {
							t.Fatal(err)
						}
						if out.Width != w || out.Height != h || out.Timestamp != in.Timestamp {
							t.Fatalf("decoded %dx%d at %v", out.Width, out.Height, out.Timestamp)
						}
						minPSNR = min(minPSNR, psnr(in, out))
						decoded++
					}
				}
				if decoded < 55 {
					t.Fatalf("decoded %d of 60 frames", decoded)
				}
				kbps := int(math.Round(float64(bytes) * 8 / 2 / 1000))
				t.Logf("engine=%v %v %dx%d: %d kbit/s, min PSNR %.2f dB", Engine(), codec, w, h, kbps, minPSNR)
				if minPSNR < 30 {
					t.Fatalf("min PSNR %.2f dB, want >= 30", minPSNR)
				}
				if kbps != g.kbps || math.Abs(minPSNR-g.psnr) > 0.005 {
					t.Fatalf("%d kbit/s and %.2f dB, want %d kbit/s and %.2f dB", kbps, minPSNR, g.kbps, g.psnr)
				}
			})
		}
	})
}

func TestKeyframes(t *testing.T) {
	eachEngine(t, func(t *testing.T) {
		for _, codec := range []Codec{VP8, VP9} {
			t.Run(codec.String(), func(t *testing.T) {
				enc, err := NewEncoder(Config{Codec: codec, Width: 160, Height: 120, Bitrate: 1_000_000, KeyframeInterval: time.Second})
				if err != nil {
					t.Fatal(err)
				}
				defer enc.Close()
				var keys []int
				for i := range 90 {
					pkts, err := enc.Encode(testFrame(t, 160, 120, i), i == 45)
					if err != nil {
						t.Fatal(err)
					}
					for _, p := range pkts {
						if p.Keyframe {
							keys = append(keys, i)
						}
					}
				}
				// Frame 0, forced 45, then 1 s (30 frames) after 45.
				want := []int{0, 30, 45, 75}
				if len(keys) != len(want) {
					t.Fatalf("keyframes at %v, want %v", keys, want)
				}
				for i := range want {
					if keys[i] != want[i] {
						t.Fatalf("keyframes at %v, want %v", keys, want)
					}
				}
			})
		}
	})
}

func TestStillTimestamps(t *testing.T) {
	eachEngine(t, func(t *testing.T) {
		// Frames without timestamps must get the same budget as timed ones.
		size := func(timed bool) (int, float64) {
			enc, err := NewEncoder(Config{Codec: VP8, Width: 320, Height: 240, Bitrate: 300_000})
			if err != nil {
				t.Fatal(err)
			}
			defer enc.Close()
			dec := newDecoder(t, DecoderConfig{Codec: VP8})
			total, worst := 0, math.Inf(1)
			for i := range 60 {
				f := testFrame(t, 320, 240, i)
				if !timed {
					f.Timestamp, f.Duration = 0, 0
				}
				pkts, err := enc.Encode(f, false)
				if err != nil {
					t.Fatal(err)
				}
				for _, p := range pkts {
					total += len(p.Data)
					if out, err := dec.Decode(p); err != nil {
						t.Fatal(err)
					} else if out != nil {
						worst = min(worst, psnr(f, out))
					}
				}
			}
			return total, worst
		}
		a, pa := size(true)
		b, pb := size(false)
		t.Logf("timed %d bytes %.1f dB, untimed %d bytes %.1f dB", a, pa, b, pb)
		if b < a/2 || b > 2*a || pb < pa-6 {
			t.Fatalf("untimed frames got a different budget: %d vs %d bytes, %.1f vs %.1f dB", b, a, pb, pa)
		}
	})
}

func TestNoPeriodicKeyframes(t *testing.T) {
	eachEngine(t, func(t *testing.T) {
		pkts := encodeAll(t, Config{Codec: VP8, Width: 64, Height: 64, KeyframeInterval: -1}, 120)
		for i, p := range pkts {
			if p.Keyframe != (i == 0) {
				t.Fatalf("packet %d keyframe=%v", i, p.Keyframe)
			}
		}
	})
}

func TestDecoderNeedsKeyframe(t *testing.T) {
	eachEngine(t, func(t *testing.T) {
		for _, codec := range []Codec{VP8, VP9} {
			pkts := encodeAll(t, Config{Codec: codec, Width: 96, Height: 64}, 5)
			dec := newDecoder(t, DecoderConfig{Codec: codec})
			if _, err := dec.Decode(pkts[1]); !errors.Is(err, ErrNeedKeyframe) {
				t.Fatalf("%v: inter frame first: %v", codec, err)
			}
			if f, err := dec.Decode(pkts[0]); err != nil || f == nil {
				t.Fatalf("%v: keyframe: %v %v", codec, f, err)
			}
			bad := append([]byte(nil), pkts[1].Data[:len(pkts[1].Data)/3]...)
			if _, err := dec.Decode(Packet{Data: bad}); err == nil {
				t.Fatalf("%v: truncated inter frame decoded without error", codec)
			}
			if _, err := dec.Decode(pkts[2]); !errors.Is(err, ErrNeedKeyframe) {
				t.Fatalf("%v: after error: %v", codec, err)
			}
			if f, err := dec.Decode(pkts[0]); err != nil || f == nil {
				t.Fatalf("%v: keyframe after error: %v %v", codec, f, err)
			}
		}
	})
}

func TestResolutionChange(t *testing.T) {
	eachEngine(t, func(t *testing.T) {
		for _, codec := range []Codec{VP8, VP9} {
			dec := newDecoder(t, DecoderConfig{Codec: codec})
			for _, size := range [][2]int{{320, 240}, {640, 360}, {128, 96}} {
				pkts := encodeAll(t, Config{Codec: codec, Width: size[0], Height: size[1]}, 3)
				for _, p := range pkts {
					f, err := dec.Decode(p)
					if err != nil {
						t.Fatalf("%v %v: %v", codec, size, err)
					}
					if f != nil && (f.Width != size[0] || f.Height != size[1]) {
						t.Fatalf("%v: decoded %dx%d, want %v", codec, f.Width, f.Height, size)
					}
				}
			}
		}
	})
}

func TestMaxPixels(t *testing.T) {
	eachEngine(t, func(t *testing.T) {
		for _, codec := range []Codec{VP8, VP9} {
			pkts := encodeAll(t, Config{Codec: codec, Width: 320, Height: 240}, 1)
			dec := newDecoder(t, DecoderConfig{Codec: codec, MaxPixels: 320*240 - 1})
			if _, err := dec.Decode(pkts[0]); !errors.Is(err, ErrTooLarge) {
				t.Fatalf("%v: %v", codec, err)
			}
		}
	})
}

func TestMaxPacketBytes(t *testing.T) {
	for _, c := range []DecoderConfig{{Codec: VP8, MaxPacketBytes: -1}, {Codec: VP8, MaxPacketBytes: 1<<28 + 1}} {
		if _, err := NewDecoder(c); !errors.Is(err, kolovideo.ErrInvalidConfig) {
			t.Errorf("%+v: %v, want ErrInvalidConfig", c, err)
		}
	}
	if c, err := (DecoderConfig{Codec: VP8}).withDefaults(); err != nil || c.MaxPacketBytes != DefaultMaxPacketBytes {
		t.Fatalf("default: %+v, %v", c, err)
	}
	if _, err := (DecoderConfig{Codec: VP8, MaxPacketBytes: 1 << 28}).withDefaults(); err != nil {
		t.Fatal(err)
	}
	eachEngine(t, func(t *testing.T) {
		for _, codec := range []Codec{VP8, VP9} {
			pkts := encodeAll(t, Config{Codec: codec, Width: 128, Height: 96}, 1)
			n := len(pkts[0].Data)
			dec := newDecoder(t, DecoderConfig{Codec: codec, MaxPacketBytes: n})
			if _, err := dec.Decode(pkts[0]); err != nil {
				t.Fatalf("%v: packet at the bound: %v", codec, err)
			}
			big := Packet{Data: append(append([]byte(nil), pkts[0].Data...), 0)}
			if _, err := dec.Decode(big); !errors.Is(err, ErrTooLarge) {
				t.Fatalf("%v: packet one byte over: %v, want ErrTooLarge", codec, err)
			}
			if _, err := dec.Decode(pkts[0]); !errors.Is(err, ErrNeedKeyframe) && err != nil {
				t.Fatalf("%v: after the refusal: %v", codec, err)
			}
		}
	})
}

func TestDecoderBufferShrinks(t *testing.T) {
	eachEngine(t, func(t *testing.T) {
		for _, codec := range []Codec{VP8, VP9} {
			dec := newDecoder(t, DecoderConfig{Codec: codec})
			if _, err := dec.Decode(Packet{Data: make([]byte, 4*retainBytes)}); err == nil {
				t.Fatalf("%v: zero packet decoded", codec)
			}
			if c := cap(dec.buf); c > retainBytes {
				t.Fatalf("%v: kept a %d byte buffer, want at most %d", codec, c, retainBytes)
			}
			pkts := encodeAll(t, Config{Codec: codec, Width: 128, Height: 96}, 1)
			if _, err := dec.Decode(pkts[0]); err != nil {
				t.Fatalf("%v: %v", codec, err)
			}
			if cap(dec.buf) == 0 {
				t.Fatalf("%v: small buffer was dropped", codec)
			}
		}
	})
}

func TestCorruptPackets(t *testing.T) {
	eachEngine(t, func(t *testing.T) {
		for _, codec := range []Codec{VP8, VP9} {
			pkts := encodeAll(t, Config{Codec: codec, Width: 128, Height: 96}, 4)
			dec := newDecoder(t, DecoderConfig{Codec: codec})
			key := pkts[0].Data
			for n := range len(key) {
				dec.Decode(Packet{Data: key[:n]})
				dec.Decode(pkts[0])
				dec.Decode(pkts[1])
			}
			for i := range key {
				b := append([]byte(nil), key...)
				b[i] ^= 0xa5
				if f, err := dec.Decode(Packet{Data: b}); err == nil && f != nil {
					if err := f.Validate(); err != nil {
						t.Fatal(err)
					}
				}
			}
			if _, err := dec.Decode(Packet{}); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("empty packet: %v", err)
			}
		}
	})
}

func TestEncoderThreads(t *testing.T) {
	for _, c := range []struct {
		codec          Codec
		w, h, cpus     int
		threads, speed int
	}{
		{VP8, 320, 240, 16, 1, -4},
		{VP8, 640, 480, 16, 1, -6},
		{VP8, 1280, 720, 2, 1, -6},
		{VP8, 1280, 720, 3, 2, -6},
		{VP8, 1280, 720, 5, 2, -6},
		{VP8, 1280, 720, 6, 3, -6},
		{VP8, 1920, 1080, 4, 2, -6},
		{VP8, 1920, 1080, 6, 3, -6},
		{VP8, 1920, 1080, 8, 3, -6},
		{VP8, 1920, 1080, 9, 4, -6},
		{VP8, 1920, 1080, 64, 4, -6},
		{VP9, 320, 240, 16, 1, 7},
		{VP9, 640, 360, 2, 1, 7},
		{VP9, 640, 360, 3, 2, 7},
		{VP9, 1280, 720, 4, 2, 7},
		{VP9, 1280, 720, 5, 4, 7},
		{VP9, 1920, 1080, 64, 4, 7},
	} {
		threads, speed := encoderThreads(c.codec, c.w, c.h, c.cpus)
		if threads != c.threads || speed != c.speed {
			t.Errorf("%v %dx%d on %d CPUs: %d threads, speed %d; want %d, %d", c.codec, c.w, c.h, c.cpus, threads, speed, c.threads, c.speed)
		}
	}
}

func TestInvalidConfig(t *testing.T) {
	eachEngine(t, func(t *testing.T) {
		for _, c := range []Config{
			{},
			{Codec: 3, Width: 10, Height: 10},
			{Codec: VP8, Width: 0, Height: 10},
			{Codec: VP8, Width: 16384, Height: 10},
			{Codec: VP9, Width: 65537, Height: 10},
			{Codec: VP8, Width: 10, Height: 10, FrameRate: -1},
			{Codec: VP8, Width: 10, Height: 10, FrameRate: math.NaN()},
			{Codec: VP8, Width: 10, Height: 10, FrameRate: math.Inf(1)},
			{Codec: VP8, Width: 10, Height: 10, FrameRate: 1e-300},
			{Codec: VP8, Width: 10, Height: 10, FrameRate: 1e-9},
			{Codec: VP8, Width: 10, Height: 10, FrameRate: 1e9},
			{Codec: VP9, Width: 10, Height: 10, FrameRate: kolovideo.MinFrameRate / 2},
			{Codec: VP9, Width: 10, Height: 10, FrameRate: kolovideo.MaxFrameRate * 1.01},
			{Codec: VP8, Width: 10, Height: 10, Bitrate: -1},
		} {
			if _, err := NewEncoder(c); !errors.Is(err, kolovideo.ErrInvalidConfig) {
				t.Errorf("%+v: %v", c, err)
			}
		}
		for _, c := range []DecoderConfig{{}, {Codec: VP8, MaxPixels: -1}} {
			if _, err := NewDecoder(c); !errors.Is(err, kolovideo.ErrInvalidConfig) {
				t.Errorf("%+v: %v", c, err)
			}
		}
	})
}

func TestEncoderErrors(t *testing.T) {
	eachEngine(t, func(t *testing.T) {
		enc, err := NewEncoder(Config{Codec: VP8, Width: 64, Height: 48})
		if err != nil {
			t.Fatal(err)
		}
		if got := enc.Config(); got.Bitrate != 6451 || got.KeyframeInterval != 2*time.Second || got.FrameRate != 30 {
			t.Fatalf("defaults %+v", got)
		}
		if _, err := enc.Encode(testFrame(t, 32, 48, 0), false); !errors.Is(err, kolovideo.ErrInvalidFrame) {
			t.Fatalf("wrong size: %v", err)
		}
		if _, err := enc.Encode(&kolovideo.Frame{Width: 64, Height: 48}, false); !errors.Is(err, kolovideo.ErrInvalidFrame) {
			t.Fatalf("empty planes: %v", err)
		}
		if err := enc.SetBitrate(0); !errors.Is(err, kolovideo.ErrInvalidConfig) {
			t.Fatalf("zero bitrate: %v", err)
		}
		if err := enc.SetBitrate(300_000); err != nil {
			t.Fatal(err)
		}
		if enc.Config().Bitrate != 300_000 {
			t.Fatal("bitrate not updated")
		}
		if _, err := enc.Encode(testFrame(t, 64, 48, 0), false); err != nil {
			t.Fatal(err)
		}
		enc.Close()
		enc.Close()
		if _, err := enc.Encode(testFrame(t, 64, 48, 1), false); !errors.Is(err, kolovideo.ErrClosed) {
			t.Fatalf("after close: %v", err)
		}
		if err := enc.SetBitrate(1000); !errors.Is(err, kolovideo.ErrClosed) {
			t.Fatalf("after close: %v", err)
		}
		dec, _ := NewDecoder(DecoderConfig{Codec: VP8})
		dec.Close()
		dec.Close()
		if _, err := dec.Decode(Packet{Data: []byte{1}}); !errors.Is(err, kolovideo.ErrClosed) {
			t.Fatalf("decode after close: %v", err)
		}
	})
}

func TestSetBitrateChangesSize(t *testing.T) {
	eachEngine(t, func(t *testing.T) {
		enc, err := NewEncoder(Config{Codec: VP8, Width: 320, Height: 240, Bitrate: 2_000_000, KeyframeInterval: -1})
		if err != nil {
			t.Fatal(err)
		}
		defer enc.Close()
		size := func(from, n int) int {
			total := 0
			for i := from; i < from+n; i++ {
				pkts, err := enc.Encode(testFrame(t, 320, 240, i), false)
				if err != nil {
					t.Fatal(err)
				}
				for _, p := range pkts {
					total += len(p.Data)
				}
			}
			return total
		}
		high := size(0, 60)
		if err := enc.SetBitrate(100_000); err != nil {
			t.Fatal(err)
		}
		size(60, 30)
		low := size(90, 60)
		t.Logf("60 frames: %d bytes at 2 Mbit/s, %d bytes at 100 kbit/s", high, low)
		if low*3 > high {
			t.Fatalf("lower bitrate did not shrink output: %d vs %d", low, high)
		}
	})
}

func TestConcurrentUse(t *testing.T) {
	eachEngine(t, func(t *testing.T) {
		enc, err := NewEncoder(Config{Codec: VP8, Width: 64, Height: 48})
		if err != nil {
			t.Fatal(err)
		}
		frames := make([]*kolovideo.Frame, 20)
		for i := range frames {
			frames[i] = testFrame(t, 64, 48, i)
		}
		var wg sync.WaitGroup
		for g := range 4 {
			wg.Go(func() {
				for i := range frames {
					if _, err := enc.Encode(frames[i], g == 0 && i == 0); err != nil && !errors.Is(err, kolovideo.ErrClosed) {
						t.Error(err)
					}
				}
			})
		}
		wg.Go(func() { enc.SetBitrate(200_000) })
		wg.Wait()
		enc.Close()
	})
}

func BenchmarkEncode(b *testing.B) {
	for _, kind := range []EngineKind{EngineNative, EnginePortable} {
		for _, codec := range []Codec{VP8, VP9} {
			for _, size := range [][2]int{{640, 480}, {1280, 720}} {
				b.Run(fmt.Sprintf("%v/%v/%dx%d", kind, codec, size[0], size[1]), func(b *testing.B) {
					useEngine(b, kind)
					enc, err := NewEncoder(Config{Codec: codec, Width: size[0], Height: size[1]})
					if err != nil {
						b.Fatal(err)
					}
					defer enc.Close()
					frames := make([]*kolovideo.Frame, 30)
					for i := range frames {
						frames[i] = testFrame(b, size[0], size[1], i)
					}
					i := 0
					for b.Loop() {
						f := frames[i%30]
						f.Timestamp = time.Duration(i) * time.Second / 30
						if _, err := enc.Encode(f, false); err != nil {
							b.Fatal(err)
						}
						i++
					}
				})
			}
		}
	}
}

func BenchmarkDecode(b *testing.B) {
	for _, kind := range []EngineKind{EngineNative, EnginePortable} {
		for _, codec := range []Codec{VP8, VP9} {
			b.Run(fmt.Sprintf("%v/%v/1280x720", kind, codec), func(b *testing.B) {
				useEngine(b, kind)
				pkts := encodeAll(b, Config{Codec: codec, Width: 1280, Height: 720, KeyframeInterval: -1}, 30)
				dec, err := NewDecoder(DecoderConfig{Codec: codec})
				if err != nil {
					b.Fatal(err)
				}
				defer dec.Close()
				i := 0
				for b.Loop() {
					if _, err := dec.Decode(pkts[i%len(pkts)]); err != nil {
						b.Fatal(err)
					}
					i++
				}
			})
		}
	}
}

func fuzzDecoder(f *testing.F, kind EngineKind, codec Codec) {
	useEngine(f, kind)
	for _, p := range encodeAll(f, Config{Codec: codec, Width: 64, Height: 48}, 3) {
		f.Add(p.Data)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		dec := newDecoder(t, DecoderConfig{Codec: codec, MaxPixels: 1 << 20})
		defer dec.Close()
		// A keyframe header lets garbage reach libvpx.
		for range 2 {
			if fr, err := dec.Decode(Packet{Data: data}); err == nil && fr != nil {
				if err := fr.Validate(); err != nil {
					t.Fatal(err)
				}
				if fr.Width*fr.Height > 1<<20 {
					t.Fatalf("decoded %dx%d over MaxPixels", fr.Width, fr.Height)
				}
			}
		}
	})
}

func FuzzDecodeVP8Native(f *testing.F)   { fuzzDecoder(f, EngineNative, VP8) }
func FuzzDecodeVP8Portable(f *testing.F) { fuzzDecoder(f, EnginePortable, VP8) }
func FuzzDecodeVP9Native(f *testing.F)   { fuzzDecoder(f, EngineNative, VP9) }
func FuzzDecodeVP9Portable(f *testing.F) { fuzzDecoder(f, EnginePortable, VP9) }

// TestExtremeFrameRates runs an encoder at the smallest and the largest accepted
// rates with frames that carry no time: the timestamps must stay increasing and
// the encoder must keep working.
func TestExtremeFrameRates(t *testing.T) {
	eachEngine(t, func(t *testing.T) {
		for _, rate := range []float64{kolovideo.MinFrameRate, kolovideo.MaxFrameRate} {
			enc, err := NewEncoder(Config{Codec: VP8, Width: 64, Height: 48, FrameRate: rate})
			if err != nil {
				t.Fatalf("rate %v: %v", rate, err)
			}
			dec := newDecoder(t, DecoderConfig{Codec: VP8})
			for i := range 6 {
				f := testFrame(t, 64, 48, i)
				f.Timestamp, f.Duration = 0, 0
				pkts, err := enc.Encode(f, false)
				if err != nil {
					t.Fatalf("rate %v frame %d: %v", rate, i, err)
				}
				for _, p := range pkts {
					if _, err := dec.Decode(p); err != nil {
						t.Fatalf("rate %v frame %d: decode: %v", rate, i, err)
					}
				}
			}
			enc.Close()
		}
	})
}

func TestKbps(t *testing.T) {
	for bps, want := range map[int]uint32{
		-5: 1, 0: 1, 1: 1, 499: 1, 500: 1, 1499: 1, 1500: 2, 300_000: 300, 2_000_000: 2000,
	} {
		if got := kbps(bps); got != want {
			t.Errorf("kbps(%d) = %d, want %d", bps, got, want)
		}
	}
	if strconv.IntSize == 64 {
		top := uint64(math.MaxUint32) * 1000 // not a constant: it does not fit an int on 32-bit systems
		for _, bps := range []uint64{top + 499, top + 500, top + 999} {
			if got := kbps(int(bps)); got != math.MaxUint32 {
				t.Errorf("kbps(%d) = %d, want %d", bps, got, uint32(math.MaxUint32))
			}
		}
		if got := kbps(int(top - 501)); got != math.MaxUint32-1 {
			t.Errorf("just below the maximum: %d", got)
		}
	}
	if got := kbps(math.MaxInt); got == 0 {
		t.Error("the largest int became 0")
	}
}
