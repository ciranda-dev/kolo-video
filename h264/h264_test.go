package h264

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	kolovideo "github.com/ciranda-dev/kolo-video"
)

func newFrame(t testing.TB, w, h int, ts time.Duration) *kolovideo.Frame {
	t.Helper()
	f, err := kolovideo.NewFrame(w, h)
	if err != nil {
		t.Fatal(err)
	}
	f.Timestamp = ts
	return f
}

func coreEncoder(t testing.TB, cfg Config, setup func(*fakeEnc)) (*Encoder, *fakeEnc) {
	t.Helper()
	fe := &fakeEngine{setupEnc: setup}
	useEngine(t, fe)
	e, err := NewEncoder(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	return e, fe.encs[0]
}

func coreDecoder(t testing.TB, cfg DecoderConfig, setup func(*fakeDec)) (*Decoder, *fakeDec) {
	t.Helper()
	fe := &fakeEngine{setupDec: setup}
	useEngine(t, fe)
	d, err := NewDecoder(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d, fe.decs[0]
}

// units splits a packet and checks that every start code has 4 bytes.
func units(t testing.TB, data []byte) [][]byte {
	t.Helper()
	nals, err := splitAnnexB(data, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if n := bytes.Count(data, []byte{0, 0, 0, 1}); n != len(nals) || !bytes.HasPrefix(data, startCode) {
		t.Fatalf("%d units but %d 4-byte start codes", len(nals), n)
	}
	return nals
}

func types(nals [][]byte) []int {
	var out []int
	for _, n := range nals {
		out = append(out, nalType(n))
	}
	return out
}

func TestConfigBounds(t *testing.T) {
	for _, c := range []struct {
		cfg Config
		ok  bool
	}{
		{Config{Width: 4096, Height: 2304, FrameRate: 30}, false},
		{Config{Width: 4096, Height: 2304, FrameRate: 26}, true},
		{Config{Width: 2304, Height: 4096, FrameRate: 26}, true},
		{Config{Width: 1920, Height: 1080, FrameRate: 120}, true},
		{Config{Width: 1920, Height: 1080, FrameRate: 121}, false},
		{Config{Width: 640, Height: 480, FrameRate: 819}, true},
		{Config{Width: 640, Height: 480, FrameRate: 820}, false},
		{Config{Width: 48, Height: 48}, true},
		{Config{Width: 46, Height: 48}, false},
		{Config{Width: 48, Height: 46}, false},
		{Config{Width: 641, Height: 480}, false},
		{Config{Width: 640, Height: 481}, false},
		{Config{Width: 4098, Height: 48}, false},
		{Config{Width: 48, Height: 4098}, false},
		{Config{Width: 4096, Height: 2320, FrameRate: 1}, false}, // 37120 macroblocks
		{Config{Width: 640, Height: 480, Bitrate: MaxBitrate}, true},
		{Config{Width: 640, Height: 480, Bitrate: MaxBitrate + 1}, false},
		{Config{Width: 640, Height: 480, Bitrate: -1}, false},
		{Config{Width: 640, Height: 480, FrameRate: kolovideo.MinFrameRate}, true},
		{Config{Width: 640, Height: 480, FrameRate: kolovideo.MinFrameRate / 1.001}, false},
		{Config{Width: 640, Height: 480, FrameRate: kolovideo.MaxFrameRate * 1.001}, false},
		{Config{Width: 48, Height: 48, FrameRate: kolovideo.MaxFrameRate}, true},
		{Config{Width: 640, Height: 480, FrameRate: -1}, false},
		{Config{Width: 640, Height: 480, Profile: High}, true},
		{Config{Width: 640, Height: 480, Profile: High + 1}, false},
		{Config{Width: 640, Height: 480, Profile: -1}, false},
	} {
		got, err := c.cfg.withDefaults()
		if c.ok != (err == nil) {
			t.Errorf("%+v: %v", c.cfg, err)
		}
		if err != nil && !errors.Is(err, kolovideo.ErrInvalidConfig) {
			t.Errorf("%+v: %v is not ErrInvalidConfig", c.cfg, err)
		}
		if err != nil && !strings.HasPrefix(err.Error(), "kolovideo/h264: ") {
			t.Errorf("error prefix: %v", err)
		}
		if err == nil && (got.FrameRate <= 0 || got.Bitrate <= 0 || got.KeyframeInterval == 0 || got.Profile == 0) {
			t.Errorf("defaults not filled: %+v", got)
		}
	}
	got, _ := Config{Width: 1920, Height: 1080}.withDefaults()
	if got.FrameRate != 30 || got.Bitrate != 4354560 || got.KeyframeInterval != 2*time.Second || got.Profile != ConstrainedBaseline {
		t.Errorf("defaults %+v", got)
	}
	got, _ = Config{Width: 4096, Height: 2304, FrameRate: 26, KeyframeInterval: -1}.withDefaults()
	if got.Bitrate != 17175674 || got.KeyframeInterval != -1 {
		t.Errorf("defaults %+v", got)
	}
	got, _ = Config{Width: 48, Height: 48, FrameRate: kolovideo.MinFrameRate}.withDefaults()
	if got.Bitrate != 1 {
		t.Errorf("smallest default bitrate %d", got.Bitrate)
	}
	for _, c := range []DecoderConfig{{MaxPixels: -1}, {MaxPixels: DefaultMaxPixels + 1}, {MaxPacketBytes: -1}, {MaxPacketBytes: 1<<28 + 1}} {
		if _, err := c.withDefaults(); !errors.Is(err, kolovideo.ErrInvalidConfig) {
			t.Errorf("%+v: %v", c, err)
		}
	}
	if _, err := (DecoderConfig{MaxPacketBytes: 1 << 28}).withDefaults(); err != nil {
		t.Error(err)
	}
	if c, err := (DecoderConfig{}).withDefaults(); err != nil || c.MaxPixels != DefaultMaxPixels || c.MaxPacketBytes != DefaultMaxPacketBytes {
		t.Errorf("decoder defaults %+v %v", c, err)
	}
}

func TestCapabilities(t *testing.T) {
	fe := &fakeEngine{limits: Limits{
		Encode: Support{Available: true, Engine: "fake", Profiles: []Profile{ConstrainedBaseline, Main}},
		Decode: Support{Available: true, Engine: "fake", Profiles: []Profile{High}, MaxWidth: 4096, MaxHeight: 2304},
	}}
	useEngine(t, fe)
	c := Capabilities()
	c.Encode.Profiles[0] = High
	c2 := Capabilities()
	if fe.capsCalls != 1 || c2.Encode.Profiles[0] != ConstrainedBaseline || c2.Decode.MaxWidth != 4096 || !c2.Encode.Available {
		t.Fatalf("calls %d, %+v", fe.capsCalls, c2)
	}
}

func TestUnavailable(t *testing.T) {
	ue := &UnavailableError{GOOS: "plan9", GOARCH: "386", Reason: "no H.264 engine on plan9/386"}
	useEngine(t, noEngine{ue})
	c := Capabilities()
	if c.Encode.Available || c.Decode.Available || !errors.Is(c.Encode.Err, ErrUnavailable) || !errors.Is(c.Decode.Err, ErrUnavailable) {
		t.Fatalf("%+v", c)
	}
	if _, err := NewEncoder(Config{Width: 640, Height: 480}); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	_, err := NewDecoder(DecoderConfig{})
	var u *UnavailableError
	if !errors.As(err, &u) || u != ue {
		t.Fatal(err)
	}
	if _, err := NewEncoder(Config{}); !errors.Is(err, kolovideo.ErrInvalidConfig) {
		t.Fatalf("config is checked first: %v", err)
	}
	for _, c := range []struct {
		e    UnavailableError
		want string
	}{
		{*ue, "kolovideo/h264: no H.264 engine on plan9/386"},
		{UnavailableError{GOOS: "linux", GOARCH: "arm64", Reason: "no H.264 engine on linux/arm64 in this release"},
			"kolovideo/h264: no H.264 engine on linux/arm64 in this release; V4L2 stateful H.264 devices: none"},
		{UnavailableError{GOOS: "linux", GOARCH: "amd64", Reason: "r", Devices: []string{"/dev/video11: encoder (d)", "/dev/video12: decoder (d)"}},
			"kolovideo/h264: r; V4L2 stateful H.264 devices: /dev/video11: encoder (d), /dev/video12: decoder (d)"},
		{UnavailableError{GOOS: "windows", GOARCH: "arm64", Engine: "Media Foundation", Reason: "class not registered", Code: 0x80040154},
			"kolovideo/h264: Media Foundation: class not registered (windows/arm64, code 0x80040154)"},
		{UnavailableError{GOOS: "darwin", GOARCH: "arm64", Engine: "VideoToolbox", Reason: "no encoder", Code: -12902},
			"kolovideo/h264: VideoToolbox: no encoder (darwin/arm64, code -12902)"},
	} {
		if got := c.e.Error(); got != c.want {
			t.Errorf("got  %q\nwant %q", got, c.want)
		}
	}
	// With no engine file for the target, current() reports absence.
	engines.Lock()
	oldEng, oldPlat := engines.eng, platform
	engines.eng, platform = nil, nil
	engines.Unlock()
	e := current()
	engines.Lock()
	engines.eng, platform = oldEng, oldPlat
	engines.Unlock()
	if _, err := e.newDecoder(decParams{}); !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "no H.264 engine on ") {
		t.Fatal(err)
	}
	ce := &codecError{engine: "VideoToolbox", op: "VTDecompressionSessionDecodeFrame", code: -12909, kind: ErrCorrupt}
	if !errors.Is(ce, ErrCorrupt) || errors.Is(ce, ErrCodec) || ce.Error() != "kolovideo/h264: VideoToolbox: VTDecompressionSessionDecodeFrame failed with code -12909" {
		t.Fatal(ce)
	}
	// A failed check of the system's output has no code to print.
	if err := vtFault("callback panic: boom", ErrCodec); err.Error() != "kolovideo/h264: VideoToolbox: callback panic: boom" {
		t.Fatal(err)
	}
}

func TestEncoderLevelCheck(t *testing.T) {
	for _, level := range []uint8{52, 60} {
		e, _ := coreEncoder(t, Config{Width: 640, Height: 480}, func(f *fakeEnc) {
			f.sps = testSPSFor(640, 480, 66, 0xc0, level)
		})
		if _, err := e.Encode(newFrame(t, 640, 480, 0), false); !errors.Is(err, ErrCodec) || errors.Is(err, ErrUnsupportedStream) {
			t.Fatalf("level %d: %v", level, err)
		}
	}
	// The same check on the parameter sets known before the first frame.
	fe := &fakeEngine{setupEnc: func(f *fakeEnc) {
		f.sps, f.preParams = testSPSFor(640, 480, 66, 0xc0, 52), true
	}}
	useEngine(t, fe)
	if _, err := NewEncoder(Config{Width: 640, Height: 480}); !errors.Is(err, ErrCodec) || fe.encs[0].closed != 1 {
		t.Fatalf("%v, closed %d", err, fe.encs[0].closed)
	}
}

func TestEncoderProfileCheck(t *testing.T) {
	cases := []struct {
		want                     Profile
		idc, cons                uint8
		ok                       bool
		name                     string
		preParams, withoutInband bool
	}{
		{ConstrainedBaseline, 66, 0xc0, true, "CB", false, false},
		{ConstrainedBaseline, 66, 0xe0, true, "CB from Baseline_AutoLevel", false, false},
		{ConstrainedBaseline, 66, 0x00, false, "plain Baseline", false, false},
		{ConstrainedBaseline, 100, 0, false, "High for CB", false, false},
		{Main, 77, 0x40, true, "Main", true, false},
		{Main, 66, 0xc0, false, "CB for Main", true, false},
		{High, 100, 0, true, "High", false, true},
		{High, 77, 0, false, "Main for High", false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fe := &fakeEngine{setupEnc: func(f *fakeEnc) {
				f.sps, f.preParams, f.inband = testSPSFor(320, 240, c.idc, c.cons, 30), c.preParams, !c.withoutInband
			}}
			useEngine(t, fe)
			e, err := NewEncoder(Config{Width: 320, Height: 240, Profile: c.want})
			if c.preParams && !c.ok {
				if !errors.Is(err, ErrProfile) || fe.encs[0].closed != 1 {
					t.Fatalf("NewEncoder: %v, closed %d", err, fe.encs[0].closed)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer e.Close()
			pkts, err := e.Encode(newFrame(t, 320, 240, 0), false)
			if c.ok {
				if err != nil || len(pkts) != 1 || !pkts[0].Keyframe {
					t.Fatal(err, len(pkts))
				}
				s, err := ParseKeyframe(pkts[0].Data)
				if p, _ := s.Profile(); err != nil || p != c.want {
					t.Fatalf("%v %v", err, p)
				}
				return
			}
			if !errors.Is(err, ErrProfile) || fe.encs[0].closed != 1 {
				t.Fatalf("Encode: %v, closed %d", err, fe.encs[0].closed)
			}
			if _, err := e.Encode(newFrame(t, 320, 240, 0), false); !errors.Is(err, kolovideo.ErrClosed) {
				t.Fatalf("after ErrProfile: %v", err)
			}
		})
	}
}

func TestEncoderBFrames(t *testing.T) {
	e, _ := coreEncoder(t, Config{Width: 640, Height: 480}, func(f *fakeEnc) { f.bSlices = true })
	if _, err := e.Encode(newFrame(t, 640, 480, 0), false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Encode(newFrame(t, 640, 480, time.Second/30), false); !errors.Is(err, ErrCodec) {
		t.Fatal(err)
	}
}

// TestEncoderMixedSlices checks that IDR and non-IDR slices in one access unit
// are a codec error before the parameter sets are read, so a wrong profile in
// the same unit does not close the encoder.
func TestEncoderMixedSlices(t *testing.T) {
	e, f := coreEncoder(t, Config{Width: 320, Height: 240}, func(f *fakeEnc) {
		f.sps = testSPSFor(320, 240, 100, 0, 30)
		f.edit = func(nals [][]byte) [][]byte { return append(nals, testSlice(false, 150, 5, 0, 0, 0)) }
	})
	_, err := e.Encode(newFrame(t, 320, 240, 0), false)
	if !errors.Is(err, ErrCodec) || errors.Is(err, ErrProfile) || f.closed != 0 {
		t.Fatalf("%v, closed %d", err, f.closed)
	}
}

func TestPacketNormalization(t *testing.T) {
	sei := append([]byte{nalSEI, 5, 54}, bytes.Repeat([]byte{0x42}, 55)...)
	aud := []byte{nalAUD, 0x10}
	t.Run("macOS shape", func(t *testing.T) {
		e, f := coreEncoder(t, Config{Width: 1280, Height: 720}, func(f *fakeEnc) {
			f.inband, f.extra = false, [][]byte{sei}
			f.sps = unhex(t, "2742c01fab402802dc80")
			f.pps = unhex(t, "28ce3c80")
			f.sliceFrameBits, f.slicePocBits = 5, 6
		})
		pkts, err := e.Encode(newFrame(t, 1280, 720, 0), false)
		if err != nil || len(pkts) != 1 || !pkts[0].Keyframe {
			t.Fatal(err, pkts)
		}
		nals := units(t, pkts[0].Data)
		if fmt.Sprint(types(nals)) != "[7 8 5]" {
			t.Fatalf("types %v", types(nals))
		}
		if got := fmt.Sprintf("%x", nals[0]); got != "2742c01fab402802dd3506010606d0442350" || !bytes.Equal(nals[1], f.pps) {
			t.Fatalf("SPS %s", got)
		}
		pkts, err = e.Encode(newFrame(t, 1280, 720, time.Second/30), false)
		if err != nil || pkts[0].Keyframe || fmt.Sprint(types(units(t, pkts[0].Data))) != "[1]" {
			t.Fatal(err, pkts)
		}
	})
	t.Run("Windows shape", func(t *testing.T) {
		e, _ := coreEncoder(t, Config{Width: 640, Height: 480}, func(f *fakeEnc) {
			f.extra, f.sliceCount, f.preParams = [][]byte{aud, sei, sei, {12, 0xff, 0xff, 0x80}, {10}}, 5, true
		})
		for i, want := range []string{"[7 8 5 5 5 5 5]", "[1 1 1 1 1]"} {
			pkts, err := e.Encode(newFrame(t, 640, 480, time.Duration(i)*time.Second/30), false)
			if err != nil || len(pkts) != 1 {
				t.Fatal(err)
			}
			if got := fmt.Sprint(types(units(t, pkts[0].Data))); got != want {
				t.Fatalf("frame %d: %s", i, got)
			}
		}
	})
	t.Run("3-byte start codes", func(t *testing.T) {
		e, _ := coreEncoder(t, Config{Width: 640, Height: 480}, func(f *fakeEnc) {
			f.edit = func(nals [][]byte) [][]byte {
				var b []byte
				for _, n := range nals {
					b = append(append(b, 0, 0, 1), n...)
				}
				out, err := splitAnnexB(b, 100)
				if err != nil {
					panic(err)
				}
				return out
			}
		})
		pkts, err := e.Encode(newFrame(t, 640, 480, 0), false)
		if err != nil || fmt.Sprint(types(units(t, pkts[0].Data))) != "[7 8 5]" {
			t.Fatal(err)
		}
	})
	t.Run("parameter set insertion", func(t *testing.T) {
		for _, drop := range []int{nalSPS, nalPPS} {
			e, f := coreEncoder(t, Config{Width: 640, Height: 480}, func(f *fakeEnc) {
				f.edit = func(nals [][]byte) [][]byte {
					var out [][]byte
					for _, n := range nals {
						if nalType(n) != drop {
							out = append(out, n)
						}
					}
					return out
				}
			})
			pkts, err := e.Encode(newFrame(t, 640, 480, 0), false)
			if err != nil {
				t.Fatal(err)
			}
			nals := units(t, pkts[0].Data)
			if fmt.Sprint(types(nals)) != "[7 8 5]" || !bytes.Equal(nals[1], f.pps) {
				t.Fatalf("drop %d: %v", drop, types(nals))
			}
			if s, err := parseSPS(nals[0], DefaultMaxPixels); err != nil || s.needsNormalize() || s.f.vui.matrix != 6 {
				t.Fatalf("inserted SPS not normalized: %v", err)
			}
		}
	})
	t.Run("parameter sets dropped from delta frames", func(t *testing.T) {
		e, f := coreEncoder(t, Config{Width: 640, Height: 480}, nil)
		e.Encode(newFrame(t, 640, 480, 0), false)
		f.edit = func(nals [][]byte) [][]byte { return append([][]byte{f.sps, f.pps}, nals...) }
		pkts, err := e.Encode(newFrame(t, 640, 480, time.Second/30), false)
		if err != nil || fmt.Sprint(types(units(t, pkts[0].Data))) != "[1]" {
			t.Fatal(err)
		}
	})
	t.Run("two access units for one frame", func(t *testing.T) {
		e, _ := coreEncoder(t, Config{Width: 640, Height: 480}, func(f *fakeEnc) { f.twice = true })
		if _, err := e.Encode(newFrame(t, 640, 480, 0), false); !errors.Is(err, ErrCodec) {
			t.Fatal(err)
		}
	})
	for name, edit := range map[string]func([][]byte) [][]byte{
		"empty unit":         func(n [][]byte) [][]byte { return append(n, nil) },
		"forbidden bit":      func(n [][]byte) [][]byte { return append(n, []byte{0x81}) },
		"no slice":           func(n [][]byte) [][]byte { return n[:2] },
		"IDR and non-IDR":    func(n [][]byte) [][]byte { return append(n, testSlice(false, 600, 5, 0, 1, 0)) },
		"second picture":     func(n [][]byte) [][]byte { return append(n, testSlice(true, 0, 7, 0, 0, 9)) },
		"slice without PPS":  func(n [][]byte) [][]byte { return append(n[:2], testSlice(true, 0, 7, 1, 0, 1)) },
		"bad SPS":            func(n [][]byte) [][]byte { return append([][]byte{{0x67, 66}}, n[1:]...) },
		"no parameter sets ": func(n [][]byte) [][]byte { return n[2:] },
	} {
		t.Run(name, func(t *testing.T) {
			e, f := coreEncoder(t, Config{Width: 640, Height: 480}, func(f *fakeEnc) { f.edit = edit })
			if name == "no parameter sets " {
				f.sps, f.pps = nil, nil
			}
			if _, err := e.Encode(newFrame(t, 640, 480, 0), false); !errors.Is(err, ErrCodec) {
				t.Fatal(err)
			}
		})
	}
	t.Run("delta before any keyframe", func(t *testing.T) {
		e, _ := coreEncoder(t, Config{Width: 640, Height: 480}, func(f *fakeEnc) { f.ignoreKeys = 1 })
		if _, err := e.Encode(newFrame(t, 640, 480, 0), false); !errors.Is(err, ErrCodec) {
			t.Fatal(err)
		}
	})
	t.Run("dropped frame", func(t *testing.T) {
		e, f := coreEncoder(t, Config{Width: 640, Height: 480}, func(f *fakeEnc) { f.drop = map[int]bool{0: true} })
		pkts, err := e.Encode(newFrame(t, 640, 480, 0), false)
		if err != nil || len(pkts) != 0 {
			t.Fatal(err, pkts)
		}
		pkts, err = e.Encode(newFrame(t, 640, 480, time.Second/30), false)
		if err != nil || len(pkts) != 1 || !pkts[0].Keyframe || !f.calls[1].key {
			t.Fatal("the keyframe is asked again after a drop", err)
		}
	})
	t.Run("engine error", func(t *testing.T) {
		e, f := coreEncoder(t, Config{Width: 640, Height: 480}, nil)
		f.encodeErr = &codecError{engine: "fake", op: "encode", code: 1, kind: ErrCodec}
		if _, err := e.Encode(newFrame(t, 640, 480, 0), false); !errors.Is(err, ErrCodec) {
			t.Fatal(err)
		}
	})
}

func TestEncodeFrameChecks(t *testing.T) {
	e, _ := coreEncoder(t, Config{Width: 640, Height: 480}, nil)
	if _, err := e.Encode(newFrame(t, 320, 240, 0), false); !errors.Is(err, kolovideo.ErrInvalidFrame) {
		t.Fatal(err)
	}
	if _, err := e.Encode(nil, false); !errors.Is(err, kolovideo.ErrInvalidFrame) {
		t.Fatal(err)
	}
	if e.Config().Width != 640 {
		t.Fatal(e.Config())
	}
}

func keyRequests(f *fakeEnc) []int {
	var out []int
	for i, c := range f.calls {
		if c.key {
			out = append(out, i)
		}
	}
	return out
}

func TestKeyframeSchedule(t *testing.T) {
	at := func(i int) time.Duration { return time.Duration(i) * time.Second / 30 }
	e, f := coreEncoder(t, Config{Width: 640, Height: 480, KeyframeInterval: time.Second}, func(f *fakeEnc) {
		f.autoIDR = map[int]bool{50: true}
	})
	if f.p.keyInterval != time.Second {
		t.Fatal(f.p.keyInterval)
	}
	var keys []int
	for i := range 120 {
		if i == 80 {
			f.ignoreKeys = 1
		}
		pkts, err := e.Encode(newFrame(t, 640, 480, at(i)), i == 10)
		if err != nil || len(pkts) != 1 {
			t.Fatal(i, err)
		}
		if pkts[0].Keyframe {
			keys = append(keys, i)
		}
	}
	// 0 first, 10 forced, 40 by interval, 50 by the system, 80 refused and
	// asked again at 81, 111 by interval.
	if got := fmt.Sprint(keyRequests(f)); got != "[0 10 40 80 81 111]" {
		t.Errorf("requests %s", got)
	}
	if got := fmt.Sprint(keys); got != "[0 10 40 50 81 111]" {
		t.Errorf("keyframes %s", got)
	}
	e, f = coreEncoder(t, Config{Width: 640, Height: 480, KeyframeInterval: -1}, nil)
	for i := range 200 {
		e.Encode(newFrame(t, 640, 480, at(i)), i == 150)
	}
	if got := fmt.Sprint(keyRequests(f)); got != "[0 150]" || f.p.keyInterval >= 0 {
		t.Errorf("on request only: %s, interval %v", got, f.p.keyInterval)
	}
}

func TestTimestamps(t *testing.T) {
	e, f := coreEncoder(t, Config{Width: 640, Height: 480, FrameRate: 25}, nil)
	frames := []struct{ ts, dur time.Duration }{
		{5 * time.Second, 0},
		{5*time.Second + 40*time.Millisecond, 0},
		{5*time.Second + 40*time.Millisecond, 0}, // repeated
		{5*time.Second + 100*time.Millisecond, 20 * time.Millisecond},
		{4 * time.Second, 0}, // backwards
	}
	d, _ := coreDecoder(t, DecoderConfig{}, nil)
	for _, fr := range frames {
		fm := newFrame(t, 640, 480, fr.ts)
		fm.Duration = fr.dur
		pkts, err := e.Encode(fm, false)
		if err != nil || pkts[0].Timestamp != fr.ts || pkts[0].Duration != fr.dur {
			t.Fatal(err, pkts[0].Timestamp, pkts[0].Duration)
		}
		pkts[0].Timestamp += time.Hour
		out, err := d.Decode(pkts[0])
		if err != nil || out.Timestamp != fr.ts+time.Hour || out.Duration != fr.dur {
			t.Fatal(err)
		}
	}
	var got []string
	for _, c := range f.calls {
		got = append(got, fmt.Sprintf("%d/%d", c.pts, c.dur))
	}
	if s := strings.Join(got, " "); s != "0/3600 3600/3600 7200/3600 9000/1800 12600/3600" {
		t.Errorf("pts/dur %s", s)
	}
	if ticks90k(-time.Second) != -90000 || ticks90k(1<<62) != (1<<62)/100000*9+(1<<62)%100000*9/100000 {
		t.Error("ticks90k")
	}
}

func TestSetBitrate(t *testing.T) {
	e, f := coreEncoder(t, Config{Width: 640, Height: 480}, nil)
	for _, bad := range []int{0, -1, MaxBitrate + 1} {
		if err := e.SetBitrate(bad); !errors.Is(err, kolovideo.ErrInvalidConfig) {
			t.Fatal(bad, err)
		}
	}
	if err := e.SetBitrate(MaxBitrate); err != nil || e.Config().Bitrate != MaxBitrate || fmt.Sprint(f.bps) != "[240000000]" {
		t.Fatal(err, f.bps)
	}
	e.Close()
	if err := e.SetBitrate(1000); !errors.Is(err, kolovideo.ErrClosed) {
		t.Fatal(err)
	}
}

func TestClose(t *testing.T) {
	e, f := coreEncoder(t, Config{Width: 640, Height: 480}, func(f *fakeEnc) { f.hw = true })
	if !e.Hardware() {
		t.Fatal("Hardware")
	}
	e.Close()
	e.Close()
	if f.closed != 1 || e.Hardware() {
		t.Fatal(f.closed)
	}
	if _, err := e.Encode(newFrame(t, 640, 480, 0), false); !errors.Is(err, kolovideo.ErrClosed) {
		t.Fatal(err)
	}
	d, fd := coreDecoder(t, DecoderConfig{}, func(d *fakeDec) { d.hw = true })
	if !d.Hardware() {
		t.Fatal("Hardware")
	}
	d.Close()
	d.Close()
	if fd.closed != 1 || d.Hardware() {
		t.Fatal(fd.closed)
	}
	if _, err := d.Decode(Packet{Data: []byte{0, 0, 1, 0x65}}); !errors.Is(err, kolovideo.ErrClosed) {
		t.Fatal(err)
	}
	fe := &fakeEngine{encErr: errors.New("x"), decErr: errors.New("y")}
	useEngine(t, fe)
	if _, err := NewEncoder(Config{Width: 640, Height: 480}); err == nil {
		t.Fatal("engine error ignored")
	}
	if _, err := NewDecoder(DecoderConfig{}); err == nil {
		t.Fatal("engine error ignored")
	}
}

func TestConcurrent(t *testing.T) {
	e, _ := coreEncoder(t, Config{Width: 320, Height: 240}, nil)
	d, _ := coreDecoder(t, DecoderConfig{}, nil)
	var wg sync.WaitGroup
	pkts := make(chan Packet, 64)
	wg.Add(4)
	go func() {
		defer wg.Done()
		defer close(pkts)
		for i := range 30 {
			p, err := e.Encode(newFrame(t, 320, 240, time.Duration(i)*time.Millisecond), i%7 == 0)
			if err != nil && !errors.Is(err, kolovideo.ErrClosed) {
				t.Error(err)
			}
			for _, x := range p {
				pkts <- x
			}
		}
	}()
	go func() {
		defer wg.Done()
		for range 30 {
			e.SetBitrate(100000)
			e.Hardware()
			e.Config()
		}
	}()
	go func() {
		defer wg.Done()
		for p := range pkts {
			if _, err := d.Decode(p); err != nil && !errors.Is(err, kolovideo.ErrClosed) && !errors.Is(err, ErrNeedKeyframe) {
				t.Error(err)
			}
		}
	}()
	go func() {
		defer wg.Done()
		for range 30 {
			d.SPS()
			d.Hardware()
		}
		e.Close()
		d.Close()
	}()
	wg.Wait()
}

// Decoder test streams: 640x480 at level 3.0, pic_order_cnt_type 2.
var (
	dSPS   = testSPSFor(640, 480, 66, 0xc0, 30)
	dPPS   = testPPS(0, 0)
	dIDR   = testSlice(true, 0, 7, 0, 0, 1)
	dDelta = testSlice(false, 0, 5, 0, 1, 0)
)

func pkt(nals ...[]byte) Packet { return Packet{Data: appendAnnexB(nil, nals)} }

func keyPkt() Packet   { return pkt(dSPS, dPPS, dIDR) }
func deltaPkt() Packet { return pkt(dDelta) }

func TestDecoderRules(t *testing.T) {
	expect := func(t *testing.T, d *Decoder, p Packet, want error) *kolovideo.Frame {
		t.Helper()
		f, err := d.Decode(p)
		if want == nil && err != nil || want != nil && !errors.Is(err, want) {
			t.Fatalf("got %v, want %v", err, want)
		}
		return f
	}
	// rule runs bad after a good keyframe and checks that the decoder then
	// waits for a keyframe.
	rule := func(name string, cfg DecoderConfig, bad Packet, want error) {
		t.Run(name, func(t *testing.T) {
			d, _ := coreDecoder(t, cfg, nil)
			expect(t, d, keyPkt(), nil)
			expect(t, d, bad, want)
			expect(t, d, deltaPkt(), ErrNeedKeyframe)
			expect(t, d, keyPkt(), nil)
			expect(t, d, deltaPkt(), nil)
		})
	}
	big := keyPkt()
	rule("1 packet too large", DecoderConfig{MaxPacketBytes: len(big.Data)}, pkt(dSPS, dPPS, dIDR, []byte{nalAUD}), ErrTooLarge)
	t.Run("1 packet at the bound", func(t *testing.T) {
		d, _ := coreDecoder(t, DecoderConfig{MaxPacketBytes: len(big.Data)}, nil)
		expect(t, d, big, nil)
	})
	t.Run("1 packet one byte over the bound", func(t *testing.T) {
		d, fd := coreDecoder(t, DecoderConfig{MaxPacketBytes: len(big.Data) - 1}, nil)
		expect(t, d, big, ErrTooLarge)
		if len(fd.aus) != 0 {
			t.Fatal("the system saw the packet")
		}
	})
	rule("2 no start code", DecoderConfig{}, Packet{Data: dIDR}, ErrCorrupt)
	rule("2 empty packet", DecoderConfig{}, Packet{}, ErrCorrupt)
	rule("2 empty unit", DecoderConfig{}, Packet{Data: append([]byte{0, 0, 1}, keyPkt().Data...)}, ErrCorrupt)
	aud := []byte{nalAUD, 0x10}
	many := func(n int) Packet {
		nals := [][]byte{dSPS, dPPS, dIDR}
		for range n {
			nals = append(nals, aud)
		}
		return pkt(nals...)
	}
	// 640x480 / 256 + 288 = 1488 units.
	rule("2 NAL count cap", DecoderConfig{MaxPixels: 640 * 480}, many(1486), ErrTooLarge)
	t.Run("2 NAL count at the cap", func(t *testing.T) {
		d, _ := coreDecoder(t, DecoderConfig{MaxPixels: 640 * 480}, nil)
		expect(t, d, many(1485), nil)
	})
	rule("3 forbidden bit", DecoderConfig{}, pkt(dSPS, dPPS, append([]byte{0x80 | dIDR[0]}, dIDR[1:]...)), ErrCorrupt)
	for _, typ := range []byte{2, 3, 4} {
		rule(fmt.Sprintf("3 data partitioning %d", typ), DecoderConfig{}, pkt([]byte{0x40 | typ, 0x80}), ErrUnsupportedStream)
	}
	t.Run("3 other types dropped", func(t *testing.T) {
		d, fd := coreDecoder(t, DecoderConfig{}, nil)
		var nals [][]byte
		for _, typ := range []byte{6, 9, 10, 11, 12, 13, 14, 15, 19, 20, 21, 22, 23, 24, 31} {
			nals = append(nals, []byte{typ, 0x80})
		}
		expect(t, d, pkt(append(nals, dSPS, dPPS, dIDR)...), nil)
		au := fd.aus[0]
		if len(au.slices) != 1 || len(au.sps) != 1 || len(au.pps) != 1 || !au.key {
			t.Fatalf("%+v", au)
		}
		if f := expect(t, d, pkt(nals...), nil); f != nil || len(fd.aus) != 1 {
			t.Fatal("a packet without slices reached the system")
		}
	})
	t.Run("4 emulation prevention", func(t *testing.T) {
		d, fd := coreDecoder(t, DecoderConfig{}, nil)
		sps := unhex(t, probeSPS[10].sps) // the Windows encoder's SPS has escapes
		s, _ := parseSPS(sps, DefaultMaxPixels)
		slice := testSliceHeader{idr: true, refIDC: 3, sliceType: 7, frameBits: int(s.f.log2MaxFrameNum4) + 4, idrPicID: 0}.nal()
		slice = append(slice[:len(slice)-1], 0, 0, 3, 1, slice[len(slice)-1])
		expect(t, d, pkt(sps, unhex(t, probeSPS[10].pps), slice), nil)
		au := fd.aus[0]
		if !bytes.Equal(au.sps[0], sps) || !bytes.Equal(au.slices[0], slice) || au.width != 320 || au.height != 240 {
			t.Fatal("units not passed as received")
		}
	})
	leftCrop := testSPS{f: spsFields{profileIDC: 66, constraints: 0xc0, levelIDC: 30, chromaFormatIDC: 1, pocType: 2, maxRefFrames: 1,
		widthMbs1: 39, heightMapUnits1: 29, cropping: true, cropL: 1}}
	rule("5 left crop", DecoderConfig{}, pkt(leftCrop.nal(), dPPS, dIDR), ErrUnsupportedStream)
	refs := leftCrop
	refs.f.cropping, refs.f.cropL, refs.f.maxRefFrames = false, 0, 7 // 8100 / 1200 = 6
	rule("5 references above MaxDpbFrames", DecoderConfig{}, pkt(refs.nal(), dPPS, dIDR), ErrUnsupportedStream)
	rule("5 frame above MaxFS", DecoderConfig{}, pkt(testSPSFor(1280, 720, 66, 0xc0, 30), dPPS, dIDR), ErrUnsupportedStream)
	rule("5 picture above MaxPixels", DecoderConfig{MaxPixels: 640 * 480}, pkt(testSPSFor(656, 480, 66, 0xc0, 30), dPPS, dIDR), ErrTooLarge)
	t.Run("5 MaxPixels bounds the coded size, not the cropped one", func(t *testing.T) {
		// 4096x2304 cropped to 48x48 at level 5.1: the system decodes and allocates 4096x2304.
		crop := testSPS{f: spsFields{profileIDC: 100, levelIDC: 51, chromaFormatIDC: 1, pocType: 2, maxRefFrames: 1,
			widthMbs1: 255, heightMapUnits1: 143, direct8x8: true, cropping: true, cropR: 2024, cropB: 1128}}
		d, fd := coreDecoder(t, DecoderConfig{MaxPixels: 4096}, nil)
		expect(t, d, pkt(crop.nal(), dPPS, dIDR), ErrTooLarge)
		if len(fd.aus) != 0 {
			t.Fatal("the system saw the packet")
		}
		d, fd = coreDecoder(t, DecoderConfig{}, nil)
		if f := expect(t, d, pkt(crop.nal(), dPPS, dIDR), nil); f.Width != 48 || f.Height != 48 || fd.aus[0].codedWidth != 4096 {
			t.Fatalf("default bound: %dx%d", f.Width, f.Height)
		}
		// The macOS encoder codes 48x48 as 192x96: that is the smallest bound for it.
		vt48 := unhex(t, probeSPS[8].sps)
		s, err := parseSPS(vt48, DefaultMaxPixels)
		if err != nil || s.widthMbs != 12 || s.heightMbs != 6 || s.f.pocType != 0 {
			t.Fatalf("48x48 SPS: %v %+v", err, s)
		}
		vtIDR := testSliceHeader{idr: true, refIDC: 3, sliceType: 7, frameBits: int(s.f.log2MaxFrameNum4) + 4, pocBits: int(s.f.log2MaxPocLsb4) + 4}.nal()
		d, _ = coreDecoder(t, DecoderConfig{MaxPixels: 192 * 96}, nil)
		expect(t, d, pkt(vt48, unhex(t, probeSPS[8].pps), vtIDR), nil)
		d, _ = coreDecoder(t, DecoderConfig{MaxPixels: 192*96 - 1}, nil)
		expect(t, d, pkt(vt48, unhex(t, probeSPS[8].pps), vtIDR), ErrTooLarge)
	})
	zeroTail := func(nal []byte) []byte { return append(bytes.Clone(nal), 0, 0, 3, 0, 0, 3, 0, 0, 3) }
	rule("5 zero bytes after the SPS trailing bits", DecoderConfig{}, pkt(zeroTail(dSPS), dPPS, dIDR), ErrCorrupt)
	rule("6 zero bytes after the PPS trailing bits", DecoderConfig{}, pkt(dSPS, zeroTail(dPPS), dIDR), ErrCorrupt)
	rule("6 PPS names an absent SPS", DecoderConfig{}, pkt(dSPS, testPPS(0, 1), dIDR), ErrCorrupt)
	rule("6 FMO", DecoderConfig{}, pkt(dSPS, testPPSFields{groups: 1}.nal(), dIDR), ErrUnsupportedStream)
	rule("7 IDR without PPS", DecoderConfig{}, pkt(dSPS, dIDR), ErrUnsupportedStream)
	rule("7 IDR without SPS", DecoderConfig{}, pkt(dPPS, dIDR), ErrUnsupportedStream)
	rule("7 new SPS in a delta packet", DecoderConfig{}, pkt(testSPSFor(320, 240, 66, 0xc0, 30), dDelta), ErrUnsupportedStream)
	rule("7 new PPS in a delta packet", DecoderConfig{}, pkt(testPPS(1, 0), dDelta), ErrUnsupportedStream)
	rule("7 parameter sets alone", DecoderConfig{}, pkt(testPPS(1, 0)), ErrUnsupportedStream)
	rule("7 twice the same SPS id", DecoderConfig{}, pkt(dSPS, dSPS, dPPS, dIDR), ErrUnsupportedStream)
	rule("7 twice the same PPS id", DecoderConfig{}, pkt(dSPS, dPPS, dPPS, dIDR), ErrUnsupportedStream)
	t.Run("7 repeated parameter sets in a delta packet", func(t *testing.T) {
		d, fd := coreDecoder(t, DecoderConfig{}, nil)
		expect(t, d, keyPkt(), nil)
		expect(t, d, pkt(dSPS, dPPS, dDelta), nil)
		expect(t, d, pkt(dPPS), nil)
		if au := fd.aus[1]; au.key || au.sps != nil || au.pps != nil || len(fd.aus) != 2 {
			t.Fatalf("%+v", au)
		}
	})
	t.Run("7 a keyframe replaces the parameter sets", func(t *testing.T) {
		d, _ := coreDecoder(t, DecoderConfig{}, nil)
		expect(t, d, keyPkt(), nil)
		expect(t, d, pkt(dSPS, testPPS(1, 0), testSlice(true, 0, 7, 1, 0, 2)), nil)
		expect(t, d, deltaPkt(), ErrCorrupt) // PPS 0 is no longer in force
	})
	t.Run("8 normalization", func(t *testing.T) {
		d, fd := coreDecoder(t, DecoderConfig{}, nil)
		vt := unhex(t, "2742c01fab402802dc80")
		slice := testSliceHeader{idr: true, refIDC: 3, sliceType: 7, frameBits: 5, pocBits: 6}.nal()
		expect(t, d, pkt(vt, unhex(t, "28ce3c80"), slice), nil)
		s, err := parseSPS(fd.aus[0].sps[0], DefaultMaxPixels)
		if err != nil || s.needsNormalize() || !s.f.vui.restriction || s.f.vui.videoSignalPresent {
			t.Fatalf("%v %+v", err, s.f.vui)
		}
		if sps, ok := d.SPS(); !ok || sps.Codec() != "avc3.42c01f" || sps.Matrix != 2 || sps.Width != 1280 {
			t.Fatal(sps)
		}
		// An SPS that already has no reordering is passed unchanged.
		d, fd = coreDecoder(t, DecoderConfig{}, nil)
		expect(t, d, keyPkt(), nil)
		expect(t, d, pkt(fdSPS(t), dPPS, testSlice(true, 0, 7, 0, 0, 3)), nil)
		if !bytes.Equal(fd.aus[1].sps[0], fdSPS(t)) {
			t.Fatal("normalized SPS rewritten again")
		}
		// No reordering but a buffer above max_num_ref_frames: rewritten to the refs.
		big := testSPS{f: spsFields{profileIDC: 66, constraints: 0xc0, levelIDC: 30, chromaFormatIDC: 1, pocType: 2, maxRefFrames: 1,
			widthMbs1: 39, heightMapUnits1: 29, direct8x8: true, vuiPresent: true}}
		setRestriction(&big.f.vui, 1)
		big.f.vui.maxDecFrameBuffer = 2
		expect(t, d, pkt(big.nal(), dPPS, testSlice(true, 0, 7, 0, 0, 4)), nil)
		if s, err := parseSPS(fd.aus[2].sps[0], DefaultMaxPixels); err != nil || s.f.vui.maxDecFrameBuffer != 1 || s.f.vui.maxNumReorderFrames != 0 {
			t.Fatalf("buffer above the refs: %v %+v", err, s)
		}
	})
	rule("9 B slice", DecoderConfig{}, pkt(testSlice(false, 0, 1, 0, 1, 0)), ErrUnsupportedStream)
	rule("9 IDR P slice", DecoderConfig{}, pkt(dSPS, dPPS, testSlice(true, 0, 5, 0, 0, 1)), ErrCorrupt)
	rule("9 absent PPS", DecoderConfig{}, pkt(testSlice(false, 0, 5, 1, 1, 0)), ErrCorrupt)
	rule("9 first_mb past the picture", DecoderConfig{}, pkt(testSlice(false, 1200, 5, 0, 1, 0)), ErrCorrupt)
	rule("9 first slice not at 0", DecoderConfig{}, pkt(testSlice(false, 1, 5, 0, 1, 0)), ErrUnsupportedStream)
	rule("9 second picture by restart", DecoderConfig{}, pkt(dDelta, testSlice(false, 0, 5, 0, 1, 0)), ErrUnsupportedStream)
	rule("9 second picture by frame_num", DecoderConfig{}, pkt(dDelta, testSlice(false, 600, 5, 0, 2, 0)), ErrUnsupportedStream)
	rule("9 second picture by idr_pic_id", DecoderConfig{}, pkt(dSPS, dPPS, dIDR, testSlice(true, 600, 7, 0, 0, 2)), ErrUnsupportedStream)
	// Two PPS with the same fields: a slice naming the other id starts a new picture (7.4.1.2.4).
	rule("9 second picture by pic_parameter_set_id", DecoderConfig{},
		pkt(dSPS, dPPS, testPPS(1, 0), dIDR, testSlice(true, 600, 7, 1, 0, 1)), ErrUnsupportedStream)
	t.Run("9 second picture by pic_order_cnt_lsb", func(t *testing.T) {
		d, _ := coreDecoder(t, DecoderConfig{}, nil)
		sps, pps := baseSPS().nal(), testPPS(0, 0) // pic_order_cnt_type 0, 6-bit lsb, 5-bit frame_num
		idr := func(first, poc uint32) []byte {
			return testSliceHeader{idr: true, refIDC: 3, firstMb: first, sliceType: 7, frameBits: 5, pocBits: 6, pocLsb: poc}.nal()
		}
		expect(t, d, pkt(sps, pps, idr(0, 0), idr(600, 2)), ErrUnsupportedStream)
		expect(t, d, pkt(sps, pps, idr(0, 2), idr(600, 2)), nil)
	})
	rule("9 arbitrary slice order", DecoderConfig{}, pkt(dDelta, testSlice(false, 600, 5, 0, 1, 0), testSlice(false, 300, 5, 0, 1, 0)), ErrUnsupportedStream)
	rule("9 IDR and non-IDR slices", DecoderConfig{}, pkt(dSPS, dPPS, dIDR, testSlice(false, 600, 5, 0, 0, 0)), ErrUnsupportedStream)
	t.Run("9 slices of one picture", func(t *testing.T) {
		d, fd := coreDecoder(t, DecoderConfig{}, nil)
		expect(t, d, pkt(dSPS, dPPS, dIDR, testSlice(true, 300, 7, 0, 0, 1), testSlice(true, 600, 2, 0, 0, 1)), nil)
		expect(t, d, pkt(dDelta, testSlice(false, 1, 0, 0, 1, 0), testSlice(false, 1199, 2, 0, 1, 0)), nil)
		if len(fd.aus[0].slices) != 3 || len(fd.aus[1].slices) != 3 {
			t.Fatal("slices")
		}
	})
	t.Run("9 SPS change without an IDR", func(t *testing.T) {
		d, _ := coreDecoder(t, DecoderConfig{}, nil)
		sps1 := testSPS{f: spsFields{profileIDC: 66, constraints: 0xc0, levelIDC: 30, chromaFormatIDC: 1, id: 1, pocType: 2,
			maxRefFrames: 1, widthMbs1: 19, heightMapUnits1: 14}}
		expect(t, d, pkt(dSPS, sps1.nal(), dPPS, testPPS(1, 1), dIDR), nil)
		expect(t, d, pkt(testSlice(false, 0, 5, 1, 1, 0)), ErrUnsupportedStream)
	})
	t.Run("10 keyframe wait", func(t *testing.T) {
		d, fd := coreDecoder(t, DecoderConfig{}, nil)
		expect(t, d, deltaPkt(), ErrNeedKeyframe)
		if f := expect(t, d, pkt([]byte{nalSEI, 0x80}), nil); f != nil {
			t.Fatal("picture from a packet without slices")
		}
		expect(t, d, keyPkt(), nil)
		expect(t, d, deltaPkt(), nil)
		fd.err = &codecError{engine: "fake", op: "decode", code: -12909, kind: ErrCorrupt}
		expect(t, d, deltaPkt(), ErrCorrupt)
		fd.err = nil
		expect(t, d, deltaPkt(), ErrNeedKeyframe)
		expect(t, d, keyPkt(), nil)
		fd.noPic = true
		if f := expect(t, d, deltaPkt(), nil); f != nil {
			t.Fatal("picture when the system gave none")
		}
		if _, ok := d.SPS(); !ok {
			t.Fatal("SPS")
		}
	})
	t.Run("11 padded 1920x1088 layout", func(t *testing.T) {
		const w, h, ph = 1920, 1080, 1088
		buf := make([]byte, w*ph*3/2)
		for i := range buf[:w*ph] {
			buf[i] = byte(i % 251)
		}
		u, v := buf[w*ph:w*ph+w*ph/4], buf[w*ph+w*ph/4:]
		for i := range u {
			u[i], v[i] = byte(i%7+1), byte(i%11+100)
		}
		d, _ := coreDecoder(t, DecoderConfig{}, func(d *fakeDec) {
			d.layout = func(au accessUnit) picture {
				return picture{width: w, height: ph, y: buf[:w*ph], u: u, v: v, yStride: w, uvStride: w / 2}
			}
		})
		f := expect(t, d, pkt(testSPSFor(w, h, 66, 0xc0, 40), dPPS, dIDR), nil)
		if f.Width != w || f.Height != h {
			t.Fatal(f.Width, f.Height)
		}
		for _, p := range []struct {
			got, want []byte
			stride    int
			w, h      int
		}{{f.Y, buf, w, w, h}, {f.U, u, w / 2, w / 2, h / 2}, {f.V, v, w / 2, w / 2, h / 2}} {
			for y := range p.h {
				if !bytes.Equal(p.got[y*p.stride:y*p.stride+p.w], p.want[y*p.stride:y*p.stride+p.w]) {
					t.Fatalf("row %d differs", y)
				}
			}
		}
		// The chroma plane starts after the padded height, row 1088.
		if f.U[0] != buf[w*ph] || f.U[0] == buf[w*h] {
			t.Fatal("chroma offset")
		}
	})
	layout := func(name string, mk func(au accessUnit) picture, want error) {
		t.Run("11 "+name, func(t *testing.T) {
			d, _ := coreDecoder(t, DecoderConfig{}, func(d *fakeDec) { d.layout = mk })
			if f := expect(t, d, keyPkt(), want); want == nil {
				for y := range f.Height {
					for x := range f.Width {
						if f.Y[y*f.YStride+x] != lumaAt(x, y) {
							t.Fatal("luma", x, y)
						}
					}
				}
				for y := range f.Height / 2 {
					for x := range f.Width / 2 {
						if f.U[y*f.UVStride+x] != byte(2*x+y) || f.V[y*f.UVStride+x] != byte(x+3*y) {
							t.Fatal("chroma", x, y)
						}
					}
				}
				return
			}
			expect(t, d, deltaPkt(), ErrNeedKeyframe)
		})
	}
	layout("smaller than the SPS", func(au accessUnit) picture { return patternPicture(638, 480, 638, 480) }, ErrCodec)
	layout("larger than coded", func(au accessUnit) picture { return patternPicture(640, 480, 656, 480) }, ErrCodec)
	layout("short buffer", func(au accessUnit) picture {
		p := patternPicture(640, 480, 640, 480)
		p.v = p.v[:len(p.v)-1]
		return p
	}, ErrCodec)
	layout("stride below width", func(au accessUnit) picture {
		p := patternPicture(640, 480, 640, 480)
		p.yStride = 639
		return p
	}, ErrCodec)
	layout("wide stride", func(au accessUnit) picture {
		p := patternPicture(640, 480, 640, 480)
		y := make([]byte, 700*480)
		for r := range 480 {
			copy(y[r*700:], p.y[r*640:r*640+640])
		}
		p.y, p.yStride = y, 700
		return p
	}, nil)
	layout("NV12", func(au accessUnit) picture {
		p := patternPicture(640, 480, 640, 480)
		uv := make([]byte, 640*240)
		for i := range p.u {
			uv[2*i], uv[2*i+1] = p.u[i], p.v[i]
		}
		p.u, p.v, p.uvStride, p.nv12 = uv, nil, 640, true
		return p
	}, nil)
	layout("NV12 short", func(au accessUnit) picture {
		p := patternPicture(640, 480, 640, 480)
		p.u, p.v, p.uvStride, p.nv12 = make([]byte, 640*240-1), nil, 640, true
		return p
	}, ErrCodec)
}

// fdSPS is a 640x480 SPS with the decoder normalization already applied.
func fdSPS(t testing.TB) []byte {
	ts := testSPS{f: spsFields{profileIDC: 66, constraints: 0xc0, levelIDC: 30, chromaFormatIDC: 1, pocType: 2, maxRefFrames: 1,
		widthMbs1: 39, heightMapUnits1: 29, direct8x8: true, vuiPresent: true}}
	setRestriction(&ts.f.vui, 1)
	return ts.nal()
}

func TestSplitJoinUV(t *testing.T) {
	for _, n := range []int{0, 1, 255, 256, 257, 700} {
		u, v, uv := make([]byte, n), make([]byte, n), make([]byte, 2*n)
		for i := range n {
			u[i], v[i] = byte(i), byte(3*i+1)
			uv[2*i], uv[2*i+1] = u[i], v[i]
		}
		gu, gv := make([]byte, n+1), make([]byte, n+1)
		splitUV(gu[:n], gv[:n], uv)
		if !bytes.Equal(gu[:n], u) || !bytes.Equal(gv[:n], v) || gu[n] != 0 || gv[n] != 0 {
			t.Errorf("splitUV of %d pairs", n)
		}
		guv := make([]byte, 2*n+1)
		joinUV(guv[:2*n], u, v)
		if !bytes.Equal(guv[:2*n], uv) || guv[2*n] != 0 {
			t.Errorf("joinUV of %d pairs", n)
		}
	}
}

func TestRoundTrip(t *testing.T) {
	for _, c := range []struct {
		w, h int
		p    Profile
	}{{640, 480, ConstrainedBaseline}, {1920, 1080, Main}, {162, 98, High}, {48, 48, ConstrainedBaseline}} {
		e, _ := coreEncoder(t, Config{Width: c.w, Height: c.h, Profile: c.p, KeyframeInterval: time.Second}, func(f *fakeEnc) {
			f.sliceCount, f.extra = 3, [][]byte{{nalAUD, 0x10}}
		})
		d, fd := coreDecoder(t, DecoderConfig{}, nil)
		for i := range 40 {
			pkts, err := e.Encode(newFrame(t, c.w, c.h, time.Duration(i)*time.Second/30), false)
			if err != nil || len(pkts) != 1 {
				t.Fatal(err)
			}
			f, err := d.Decode(pkts[0])
			if err != nil || f.Width != c.w || f.Height != c.h {
				t.Fatalf("%dx%d frame %d: %v", c.w, c.h, i, err)
			}
			if pkts[0].Keyframe != fd.aus[i].key {
				t.Fatal("keyframe flag")
			}
		}
		s, _ := d.SPS()
		if p, _ := s.Profile(); p != c.p || s.Matrix != 6 || s.Width != c.w {
			t.Fatal(s)
		}
	}
}

// readStream reads a testdata stream: for each packet a big-endian u32
// length, a big-endian i64 timestamp in nanoseconds, then the packet.
func readStream(t testing.TB, name string) []Packet {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	var pkts []Packet
	for len(b) > 0 {
		if len(b) < 12 {
			t.Fatalf("%s: truncated header", name)
		}
		n := binary.BigEndian.Uint32(b)
		ts := time.Duration(binary.BigEndian.Uint64(b[4:]))
		b = b[12:]
		if uint64(n) > uint64(len(b)) {
			t.Fatalf("%s: truncated packet", name)
		}
		pkts = append(pkts, Packet{Data: b[:n], Timestamp: ts})
		b = b[n:]
	}
	return pkts
}

func TestStreamFiles(t *testing.T) {
	names, err := filepath.Glob(filepath.Join("testdata", "*.h264f"))
	if err != nil || len(names) == 0 {
		t.Fatal(err, names)
	}
	for _, path := range names {
		name := filepath.Base(path)
		t.Run(name, func(t *testing.T) {
			pkts := readStream(t, name)
			s, err := ParseKeyframe(pkts[0].Data)
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := s.Profile(); !ok || s.Matrix != 6 {
				t.Fatalf("%s matrix %d", s.Codec(), s.Matrix)
			}
			d := &Decoder{cfg: DecoderConfig{MaxPixels: DefaultMaxPixels, MaxPacketBytes: DefaultMaxPacketBytes}, impl: &fakeDec{}, needKey: true}
			for i, p := range pkts {
				nals := units(t, p.Data)
				for _, n := range nals {
					if typ := nalType(n); typ != nalSlice && typ != nalIDR && typ != nalSPS && typ != nalPPS {
						t.Fatalf("packet %d holds NAL type %d", i, typ)
					}
				}
				f, err := d.Decode(p)
				if err != nil || f == nil || f.Width != s.Width || f.Height != s.Height || f.Timestamp != p.Timestamp {
					t.Fatalf("packet %d: %v", i, err)
				}
			}
		})
	}
	pkts := readStream(t, "vt-cb-48x48.h264f")
	if s, _ := ParseKeyframe(pkts[0].Data); len(pkts) != 30 || s.Codec() != "avc3.42c00b" || s.Width != 48 || s.Height != 48 {
		t.Fatalf("%d packets, %+v", len(pkts), s)
	}
}

type failingBitrate struct{ fakeEnc }

func (f *failingBitrate) setBitrate(int) error { return errors.New("kolovideo/h264: refused") }

func TestEncoderEngineFailures(t *testing.T) {
	fe := &fakeEngine{}
	useEngine(t, fe)
	e, err := NewEncoder(Config{Width: 640, Height: 480})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	e.mu.Lock()
	inner := e.impl.(*fakeEnc)
	e.impl = &failingBitrate{*inner}
	e.mu.Unlock()
	if err := e.SetBitrate(1000); err == nil || e.Config().Bitrate == 1000 {
		t.Fatal("engine failure ignored", err)
	}
	// After a failed first emit, a second emit is ignored and the first
	// error is returned.
	e2, _ := coreEncoder(t, Config{Width: 640, Height: 480}, func(f *fakeEnc) {
		f.twice, f.edit = true, func(n [][]byte) [][]byte { return n[2:] }
		f.sps, f.pps = nil, nil
	})
	if _, err := e2.Encode(newFrame(t, 640, 480, 0), false); !errors.Is(err, ErrCodec) || strings.Contains(err.Error(), "more than one") {
		t.Fatal(err)
	}
	// A short U plane from the system.
	d, _ := coreDecoder(t, DecoderConfig{}, func(d *fakeDec) {
		d.layout = func(au accessUnit) picture {
			p := patternPicture(640, 480, 640, 480)
			p.u = p.u[:10]
			return p
		}
	})
	if _, err := d.Decode(keyPkt()); !errors.Is(err, ErrCodec) {
		t.Fatal(err)
	}
	if (noEngine{}).name() != "" || (&fakeEngine{}).name() != "fake" {
		t.Fatal("name")
	}
}
