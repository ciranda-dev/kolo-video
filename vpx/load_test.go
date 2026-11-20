package vpx

import (
	"embed"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	kolovideo "github.com/ciranda-dev/kolo-video"
	"github.com/ciranda-dev/kolo-video/vpx/internal/native"
	"github.com/ciranda-dev/kolo-video/vpx/internal/portable"
)

//go:embed testdata/kvabi-*
var abiFixtures embed.FS

func hasNative() bool {
	_, err := testEngine(EngineNative)
	return !errors.Is(err, native.ErrUnsupported)
}

func wantKind() EngineKind {
	if hasNative() {
		return EngineNative
	}
	return EnginePortable
}

// writeABIFixture writes a library that exports the kv_* interface with ABI
// version 2 into a temp directory and returns its path.
func writeABIFixture(t *testing.T) string {
	t.Helper()
	ext := map[string]string{"darwin": "dylib", "windows": "dll"}[runtime.GOOS]
	if ext == "" {
		ext = "so"
	}
	name := fmt.Sprintf("testdata/kvabi-%s-%s.%s", runtime.GOOS, runtime.GOARCH, ext)
	b, err := abiFixtures.ReadFile(name)
	if err != nil {
		t.Skipf("no fixture for this target: %v", err)
	}
	p := filepath.Join(t.TempDir(), "libkolovpx."+ext)
	if err := os.WriteFile(p, b, 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadAuto(t *testing.T) {
	setLoaded(t, nil)
	cfg := LoadConfig{CacheDir: testCache}
	rt, err := Load(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("runtime: engine=%v version=%s native error: %v", rt.Engine, rt.Version, rt.NativeErr)
	if rt.Engine != wantKind() || rt.Version != "v1.16.0" || (rt.NativeErr == nil) != hasNative() {
		t.Fatalf("%+v", rt)
	}
	if !hasNative() && (!strings.Contains(rt.NativeErr.Error(), "kolovideo/vpx: ") || !errors.Is(rt.NativeErr, native.ErrUnsupported)) {
		t.Fatalf("native error %q", rt.NativeErr)
	}
	if Engine() != rt.Engine || LoadError() != rt.NativeErr {
		t.Fatalf("Engine %v, LoadError %v", Engine(), LoadError())
	}
	if again, err := Load(cfg); err != nil || again.Engine != rt.Engine {
		t.Fatalf("same configuration: %+v %v", again, err)
	}
	if _, err := Load(LoadConfig{Engine: EnginePortable}); !errors.Is(err, kolovideo.ErrInvalidConfig) {
		t.Fatalf("other configuration: %v", err)
	}
	if Engine() != rt.Engine {
		t.Fatal("the first engine did not stay")
	}
	enc, err := NewEncoder(Config{Codec: VP8, Width: 64, Height: 48})
	if err != nil {
		t.Fatalf("NewEncoder after Load with a custom configuration: %v", err)
	}
	enc.Close()
}

// TestLoadDefault loads the default configuration, whose cache is under the
// user cache directory; the environment points that into the test cache.
func TestLoadDefault(t *testing.T) {
	home := filepath.Join(testCache, "home")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	t.Setenv("LocalAppData", filepath.Join(home, "AppData"))
	if dir, err := os.UserCacheDir(); err != nil || !strings.HasPrefix(dir, home) {
		t.Fatalf("user cache dir %q (%v) is outside the test cache", dir, err)
	}
	setLoaded(t, nil)
	if Engine() != wantKind() {
		t.Fatalf("Engine() = %v", Engine())
	}
	if _, err := Load(LoadConfig{}); err != nil {
		t.Fatalf("default configuration after the implicit load: %v", err)
	}
}

func TestLoadPortable(t *testing.T) {
	setLoaded(t, nil)
	rt, err := Load(LoadConfig{Engine: EnginePortable})
	if err != nil || rt.Engine != EnginePortable || rt.NativeErr == nil || rt.Version != "v1.16.0" {
		t.Fatalf("%+v %v", rt, err)
	}
}

func TestLoadNativeForced(t *testing.T) {
	setLoaded(t, nil)
	rt, err := Load(LoadConfig{Engine: EngineNative, CacheDir: testCache})
	if !hasNative() {
		if !errors.Is(err, ErrUnavailable) || !errors.Is(err, native.ErrUnsupported) || !strings.HasPrefix(err.Error(), "kolovideo/vpx: libvpx unavailable: ") {
			t.Fatalf("err = %v", err)
		}
		loadMu.Lock()
		defer loadMu.Unlock()
		if loaded != nil {
			t.Fatal("a failed load left an engine active")
		}
		return
	}
	if err != nil || rt.Engine != EngineNative || rt.NativeErr != nil {
		t.Fatalf("%+v %v", rt, err)
	}
}

func TestLoadInvalidEngine(t *testing.T) {
	setLoaded(t, nil)
	if _, err := Load(LoadConfig{Engine: 9}); !errors.Is(err, kolovideo.ErrInvalidConfig) {
		t.Fatalf("err = %v", err)
	}
	if EngineKind(9).String() != "EngineKind(9)" || EngineAuto.String() != "auto" {
		t.Fatal("String")
	}
}

// roundTrip encodes and decodes a few frames with the active engine.
func roundTrip(t *testing.T, codec Codec) {
	t.Helper()
	pkts := encodeAll(t, Config{Codec: codec, Width: 96, Height: 64, Bitrate: 500_000}, 5)
	dec := newDecoder(t, DecoderConfig{Codec: codec})
	shown := 0
	for _, p := range pkts {
		f, err := dec.Decode(p)
		if err != nil {
			t.Fatal(err)
		}
		if f != nil {
			shown++
		}
	}
	if shown == 0 {
		t.Fatal("no frame decoded")
	}
}

// TestLoadFallback loads libraries that cannot work: the native load must fail
// with a typed error, the portable engine must take over on EngineAuto, and
// EngineNative must report ErrUnavailable.
func TestLoadFallback(t *testing.T) {
	dir := t.TempDir()
	garbage := filepath.Join(dir, "garbage")
	if err := os.WriteFile(garbage, []byte("not a shared library"), 0o755); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		path func(t *testing.T) string
		is   error
	}{
		{"wrong ABI", writeABIFixture, ErrNativeABI},
		{"not a library", func(*testing.T) string { return garbage }, nil},
		{"missing file", func(*testing.T) string { return filepath.Join(dir, "missing") }, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := LoadConfig{Library: c.path(t), CacheDir: testCache}
			setLoaded(t, nil)
			rt, err := Load(cfg)
			if err != nil || rt.Engine != EnginePortable || rt.NativeErr == nil {
				t.Fatalf("auto: %+v %v", rt, err)
			}
			t.Logf("native error: %v", rt.NativeErr)
			if !strings.HasPrefix(rt.NativeErr.Error(), "kolovideo/vpx: ") {
				t.Fatalf("native error %q", rt.NativeErr)
			}
			if c.is != nil && hasNative() && !errors.Is(rt.NativeErr, c.is) {
				t.Fatalf("native error %q does not match %v", rt.NativeErr, c.is)
			}
			if LoadError() != rt.NativeErr || Engine() != EnginePortable {
				t.Fatal("Engine and LoadError disagree with Load")
			}
			roundTrip(t, VP8)
			roundTrip(t, VP9)

			setLoaded(t, nil)
			cfg.Engine = EngineNative
			if _, err := Load(cfg); !errors.Is(err, ErrUnavailable) {
				t.Fatalf("native forced: %v", err)
			}
		})
	}
}

var (
	writeLibrary     sync.Once
	writeLibraryPath string
	writeLibraryErr  error
)

func TestWriteLibrary(t *testing.T) {
	// The library stays loaded for the life of the process, and Windows cannot
	// delete a loaded DLL, so it is written once, under the test cache.
	writeLibrary.Do(func() {
		writeLibraryPath, writeLibraryErr = WriteLibrary(filepath.Join(testCache, "write-library"))
	})
	p, err := writeLibraryPath, writeLibraryErr
	if !hasNative() {
		if !errors.Is(err, native.ErrUnsupported) {
			t.Fatalf("err = %v", err)
		}
		return
	}
	if err != nil || filepath.Dir(p) != filepath.Join(testCache, "write-library") {
		t.Fatalf("%q %v", p, err)
	}
	setLoaded(t, nil)
	rt, err := Load(LoadConfig{Engine: EngineNative, Library: p})
	if err != nil || rt.Engine != EngineNative || rt.Version != "v1.16.0" {
		t.Fatalf("%+v %v", rt, err)
	}
	roundTrip(t, VP9)
}

func TestCapabilities(t *testing.T) {
	var got [2]Limits
	for i, kind := range []EngineKind{EngineNative, EnginePortable} {
		t.Run(kind.String(), func(t *testing.T) {
			useEngine(t, kind)
			l := Capabilities()
			if l.Engine != kind {
				t.Fatalf("%+v", l)
			}
			got[i] = l
			t.Logf("%+v", l)
		})
	}
	if got[0] != (Limits{}) {
		if got[1].Encode.Width >= got[0].Encode.Width || got[1].Decode.Width > got[0].Decode.Width {
			t.Fatalf("portable limits %+v exceed native %+v", got[1], got[0])
		}
	}
	if got[1] != (Limits{EnginePortable, RealTime{640, 480, 30}, RealTime{1920, 1080, 30}}) {
		t.Fatalf("portable limits %+v", got[1])
	}
}

const (
	warmup      = 5
	minPictures = 35
	minRun      = 250 * time.Millisecond
	timedRuns   = 3
	framesPool  = 16
)

// perPicture calls step for pictures 0, 1, 2 and so on until at least minPictures
// pictures and minRun have passed, timedRuns times, and returns the smallest
// mean time per call: noise only adds time. It times a whole run, not each
// call, because the Windows clock advances in steps of up to 15.6 ms, so a
// picture of a few milliseconds reads as zero.
func perPicture(step func(i int)) time.Duration {
	best := time.Duration(math.MaxInt64)
	for range timedRuns {
		start := time.Now()
		n := 0
		for n < minPictures || time.Since(start) < minRun {
			step(warmup + n)
			n++
		}
		best = min(best, time.Since(start)/time.Duration(n))
	}
	return best
}

// encodeMean encodes test frames with the active engine and returns the mean
// time per picture and the packets of one run.
func encodeMean(t *testing.T, codec Codec, w, h int) (time.Duration, []Packet) {
	t.Helper()
	enc, err := NewEncoder(Config{Codec: codec, Width: w, Height: h})
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	frames := make([]*kolovideo.Frame, framesPool)
	for i := range frames {
		frames[i] = testFrame(t, w, h, i)
	}
	var pkts []Packet
	i := 0
	step := func(int) {
		f := frames[i%framesPool]
		f.Timestamp = time.Duration(i) * time.Second / DefaultFrameRate
		i++
		out, err := enc.Encode(f, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(pkts) < framesPool {
			pkts = append(pkts, out...)
		}
	}
	for range warmup {
		step(0)
	}
	return perPicture(step), pkts
}

// decodeMean decodes the packets, which start with a keyframe, over and over
// with the active engine and returns the mean time per packet.
func decodeMean(t *testing.T, codec Codec, pkts []Packet) time.Duration {
	t.Helper()
	dec := newDecoder(t, DecoderConfig{Codec: codec})
	step := func(i int) {
		if _, err := dec.Decode(pkts[i%len(pkts)]); err != nil {
			t.Fatal(err)
		}
	}
	for i := range warmup {
		step(i)
	}
	return perPicture(step)
}

// Bounds of the portable over native time ratio in TestEngineSpeed.
const (
	minSlowdown = 1.0
	maxSlowdown = 200.0
)

// TestEngineSpeed checks the portable engine against the native one on the same
// host, at the portable engine's encode limit and decode limit. The limits are
// measurements of one machine, and an absolute time says more about the host
// than about the library: an emulated or shared host runs the same code several
// times slower. The native library is the same libvpx compiled for the host,
// so it measures the host. The ratio on the reference machine is between 5 and
// 23 (VP8 and VP9, encode and decode); the test fails when it is below 1, a
// native engine slower than the translated one, or above 200, a portable
// engine that lost an order of magnitude.
func TestEngineSpeed(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector slows only the portable engine")
	}
	if !hasNative() {
		t.Skip("no native engine in this build to measure the host against")
	}
	useEngine(t, EnginePortable)
	limits := Capabilities()
	ew, eh := limits.Encode.Width, limits.Encode.Height
	dw, dh := limits.Decode.Width, limits.Decode.Height
	for _, codec := range []Codec{VP8, VP9} {
		t.Run(codec.String(), func(t *testing.T) {
			useEngine(t, EngineNative)
			_, big := encodeMean(t, codec, dw, dh)
			var enc, dec [2]time.Duration
			for i, kind := range []EngineKind{EngineNative, EnginePortable} {
				useEngine(t, kind)
				enc[i], _ = encodeMean(t, codec, ew, eh)
				dec[i] = decodeMean(t, codec, big)
			}
			for _, c := range []struct {
				what         string
				native, port time.Duration
			}{{fmt.Sprintf("encode %dx%d", ew, eh), enc[0], enc[1]}, {fmt.Sprintf("decode %dx%d", dw, dh), dec[0], dec[1]}} {
				ratio := float64(c.port) / float64(c.native)
				t.Logf("%s: native %v, portable %v, ratio %.1f", c.what, c.native, c.port, ratio)
				if ratio < minSlowdown || ratio > maxSlowdown {
					t.Errorf("%s: portable/native time ratio %.1f outside %.0f to %.0f", c.what, ratio, minSlowdown, maxSlowdown)
				}
			}
		})
	}
}

func TestNotices(t *testing.T) {
	n := Notices()
	for _, want := range []string{"LICENSE-libvpx", "PATENTS-libvpx", "LICENSE-wasi-libc", "LICENSE-wasm2go", "LICENSE-llvm-mingw", "Additional IP Rights Grant", "Redistribution and use in source and binary forms"} {
		if !strings.Contains(n, want) {
			t.Errorf("Notices lacks %q", want)
		}
	}
	for _, name := range []string{"LICENSE-libvpx", "PATENTS-libvpx", "LICENSE-wasi-libc", "LICENSE-wasm2go", "LICENSE-llvm-mingw"} {
		want, err := os.ReadFile(filepath.Join("..", "third_party", "licenses", name))
		if err != nil {
			t.Skipf("repository root files not available: %v", err)
		}
		if !strings.Contains(n, string(want)) {
			t.Errorf("notices/%s differs from the repository's %s", name, name)
		}
	}
}

func TestCodecErrors(t *testing.T) {
	for _, c := range []struct {
		code                  int32
		corrupt, invalid, gen bool
	}{
		{codecErrCorruptFrame, true, false, false},
		{codecErrUnsupBitstream, true, false, false},
		{codecErrInvalidParam, false, true, false},
		{codecErrIncapable, false, true, false},
		{codecErrMem, false, false, true},
		{1, false, false, true},
		{99, false, false, true},
	} {
		err := error(&codecError{"op", c.code})
		if errors.Is(err, ErrCorrupt) != c.corrupt || errors.Is(err, kolovideo.ErrInvalidConfig) != c.invalid || errors.Is(err, ErrCodec) != c.gen {
			t.Errorf("code %d: corrupt=%v invalid=%v codec=%v", c.code, errors.Is(err, ErrCorrupt), errors.Is(err, kolovideo.ErrInvalidConfig), errors.Is(err, ErrCodec))
		}
		if !strings.HasPrefix(err.Error(), "kolovideo/vpx: op: ") {
			t.Errorf("message %q", err)
		}
	}
	for _, c := range []struct {
		err  error
		want error
	}{
		{&portable.CodecError{Op: "decode", Code: codecErrCorruptFrame}, ErrCorrupt},
		{&portable.CodecError{Op: "encode", Code: codecErrInvalidParam}, kolovideo.ErrInvalidConfig},
		{&portable.CodecError{Op: "encode", Code: codecErrMem}, ErrCodec},
		{&portable.TrapError{Value: "trap"}, ErrCodec},
		{portable.ErrClosed, kolovideo.ErrClosed},
		{errors.New("other"), ErrCodec},
	} {
		if got := fromPortable("op", c.err); !errors.Is(got, c.want) || !strings.HasPrefix(got.Error(), "kolovideo") {
			t.Errorf("%v maps to %v, want %v", c.err, got, c.want)
		}
	}
}

// TestNativeErrorsExported checks that a user can tell why the native engine is
// not in use through the exported errors.
func TestNativeErrorsExported(t *testing.T) {
	if hasNative() {
		t.Skip("this build has a native engine")
	}
	setLoaded(t, nil)
	rt, err := Load(LoadConfig{})
	if err != nil || rt.Engine != EnginePortable {
		t.Fatalf("%+v %v", rt, err)
	}
	if !errors.Is(rt.NativeErr, ErrNativeUnsupported) || !errors.Is(LoadError(), ErrNativeUnsupported) {
		t.Fatalf("native error %v is not ErrNativeUnsupported", rt.NativeErr)
	}
	setLoaded(t, nil)
	if _, err := Load(LoadConfig{Engine: EngineNative}); !errors.Is(err, ErrUnavailable) || !errors.Is(err, ErrNativeUnsupported) {
		t.Fatalf("forced native: %v", err)
	}
}
