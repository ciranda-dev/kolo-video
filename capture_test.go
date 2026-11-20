package kolovideo

import (
	"context"
	"errors"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// settle is the pause that lets goroutines run when a test checks that nothing
// more happens or races Close against readers; the assertions hold in any order.
const settle = 10 * time.Millisecond

// fakeCamera replaces the platform hooks for one test.
type fakeCamera struct {
	devices []Device
	listErr error
	openErr error
	// period > 0 makes each backend deliver a frame every period.
	period time.Duration
	// early delivers one frame before openBackend returns.
	early    bool
	closeErr error

	mu    sync.Mutex
	sinks []frameSink
	opens []*fakeBackend
}

type fakeBackend struct {
	stop   chan struct{}
	done   chan struct{}
	closes atomic.Int32
	err    error
}

func (b *fakeBackend) close() error {
	if b.closes.Add(1) == 1 {
		close(b.stop)
	}
	<-b.done
	return b.err
}

var testModes = []Mode{
	{1280, 720, 30, FormatMJPEG},
	{1280, 720, 10, FormatYUY2},
	{640, 480, 30, FormatYUY2},
	{640, 480, 30, FormatNV12},
	{320, 240, 30000.0 / 1001, FormatNV12},
}

func useFake(t *testing.T, fc *fakeCamera) *fakeCamera {
	t.Helper()
	if fc.devices == nil {
		fc.devices = []Device{
			{ID: "cam0", Name: "Front", Modes: testModes},
			{ID: "cam1", Name: "USB", Default: true, Modes: testModes[:2]},
		}
	}
	oldList, oldOpen, oldAccess := listDevices, openBackend, requestAccess
	listDevices = func() ([]Device, error) { return fc.devices, fc.listErr }
	openBackend = fc.open
	requestAccess = func(context.Context) error { return ErrPermission }
	t.Cleanup(func() { listDevices, openBackend, requestAccess = oldList, oldOpen, oldAccess })
	return fc
}

func (fc *fakeCamera) open(dev Device, cfg CaptureConfig, sink frameSink) (backend, Mode, error) {
	if fc.openErr != nil {
		return nil, Mode{}, fc.openErr
	}
	mode, _, err := chooseMode(dev.Modes, cfg)
	if err != nil {
		return nil, Mode{}, err
	}
	b := &fakeBackend{stop: make(chan struct{}), done: make(chan struct{}), err: fc.closeErr}
	if fc.early {
		f, _ := NewFrame(4, 4)
		f.Timestamp = 42
		sink.deliver(f)
	}
	fc.mu.Lock()
	fc.sinks = append(fc.sinks, sink)
	fc.opens = append(fc.opens, b)
	fc.mu.Unlock()
	if fc.period <= 0 {
		close(b.done)
		return b, mode, nil
	}
	go func() {
		defer close(b.done)
		t := time.NewTicker(fc.period)
		defer t.Stop()
		for n := 0; ; n++ {
			select {
			case <-b.stop:
				return
			case <-t.C:
				// Small frames keep the producer fast under -race.
				f, _ := NewFrame(16, 16)
				f.Timestamp = time.Duration(n) * fc.period
				sink.deliver(f)
			}
		}
	}()
	return b, mode, nil
}

// push delivers frames with timestamps from..to-1 to the last open capture.
func (fc *fakeCamera) push(from, to int) {
	fc.mu.Lock()
	s := fc.sinks[len(fc.sinks)-1]
	fc.mu.Unlock()
	for i := from; i < to; i++ {
		f, _ := NewFrame(4, 4)
		f.Timestamp = time.Duration(i)
		s.deliver(f)
	}
}

func (fc *fakeCamera) sink() frameSink {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	return fc.sinks[len(fc.sinks)-1]
}

func readN(t *testing.T, c *Capture, n int) []*Frame {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out []*Frame
	for range n {
		f, err := c.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, f)
	}
	return out
}

func TestCaptureFramesFlow(t *testing.T) {
	useFake(t, &fakeCamera{period: time.Millisecond})
	c, err := OpenCapture(CaptureConfig{DeviceID: "cam0", Buffer: 100})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.Device().ID != "cam0" {
		t.Fatalf("device %+v", c.Device())
	}
	if m := c.Mode(); m != (Mode{640, 480, 30, FormatNV12}) {
		t.Fatalf("mode %+v", m)
	}
	frames := readN(t, c, 20)
	for i := 1; i < len(frames); i++ {
		if frames[i].Timestamp <= frames[i-1].Timestamp {
			t.Fatal("timestamps not increasing")
		}
		if err := frames[i].Validate(); err != nil || frames[i].Width != 16 {
			t.Fatal("bad frame")
		}
	}
	if s := c.Stats(); s.Frames < 20 || s.Invalid != 0 {
		t.Fatalf("stats %+v", s)
	}
}

func TestCaptureDropsOldest(t *testing.T) {
	fc := useFake(t, &fakeCamera{})
	c, err := OpenCapture(CaptureConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.Device().ID != "cam1" {
		t.Fatalf("default device %q", c.Device().ID)
	}
	fc.push(0, 5)
	got := readN(t, c, 2)
	if got[0].Timestamp != 3 || got[1].Timestamp != 4 {
		t.Fatalf("read %v %v, want the 2 newest", got[0].Timestamp, got[1].Timestamp)
	}
	fc.sink().invalid()
	if s := c.Stats(); s != (Stats{Frames: 6, Dropped: 3, Invalid: 1}) {
		t.Fatalf("stats %+v", s)
	}
}

func TestChooseMode(t *testing.T) {
	cases := []struct {
		w, h int
		fps  float64
		want Mode
	}{
		{640, 480, 30, Mode{640, 480, 30, FormatNV12}},
		{1280, 720, 30, Mode{1280, 720, 30, FormatMJPEG}},
		{1280, 720, 5, Mode{1280, 720, 5, FormatYUY2}},
		{1920, 1080, 30, Mode{1280, 720, 30, FormatMJPEG}},
		{1280, 720, 60, Mode{1280, 720, 30, FormatMJPEG}},
		{320, 240, 30, Mode{320, 240, 30000.0 / 1001, FormatNV12}},
		{100, 100, 15, Mode{320, 240, 15, FormatNV12}},
	}
	for _, c := range cases {
		got, _, err := chooseMode(testModes, CaptureConfig{Width: c.w, Height: c.h, FrameRate: c.fps, MaxPixels: DefaultMaxPixels})
		if err != nil || got != c.want {
			t.Errorf("%dx%d@%v: %+v %v, want %+v", c.w, c.h, c.fps, got, err, c.want)
		}
	}
	// An unknown rate is assumed to reach the request.
	got, _, err := chooseMode([]Mode{{640, 480, 0, FormatYUY2}}, CaptureConfig{Width: 640, Height: 480, FrameRate: 25, MaxPixels: DefaultMaxPixels})
	if err != nil || got.FrameRate != 25 {
		t.Fatalf("unknown rate: %+v %v", got, err)
	}
	for _, modes := range [][]Mode{nil, {{640, 480, 30, FormatUnknown}}, {{0, 480, 30, FormatNV12}}} {
		if _, _, err := chooseMode(modes, CaptureConfig{Width: 1, Height: 1, FrameRate: 1, MaxPixels: DefaultMaxPixels}); !errors.Is(err, ErrUnsupportedFormat) {
			t.Errorf("%v: %v", modes, err)
		}
	}
}

func TestOpenCaptureErrors(t *testing.T) {
	fc := useFake(t, &fakeCamera{})
	for _, cfg := range []CaptureConfig{
		{Width: -1, Height: 10},
		{Width: 640},
		{Width: 1 << 20, Height: 10},
		{FrameRate: -1},
		{FrameRate: math.Inf(1)},
		{FrameRate: math.NaN()},
		{FrameRate: 1e-300},
		{FrameRate: 1e-9},
		{FrameRate: 1e9},
		{FrameRate: MaxFrameRate * 1.001},
		{Buffer: -1},
	} {
		if _, err := OpenCapture(cfg); !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("%+v: %v", cfg, err)
		}
	}
	if _, err := OpenCapture(CaptureConfig{DeviceID: "nope"}); !errors.Is(err, ErrNoDevice) {
		t.Errorf("unknown device: %v", err)
	}
	fc.devices = []Device{{ID: "raw", Modes: []Mode{{640, 480, 30, FormatUnknown}}}}
	if _, err := OpenCapture(CaptureConfig{}); !errors.Is(err, ErrUnsupportedFormat) {
		t.Errorf("no usable mode: %v", err)
	}
	fc.devices = []Device{}
	if _, err := OpenCapture(CaptureConfig{}); !errors.Is(err, ErrNoDevice) {
		t.Errorf("no devices: %v", err)
	}
	fc.listErr = ErrPermission
	if _, err := OpenCapture(CaptureConfig{}); !errors.Is(err, ErrPermission) {
		t.Errorf("list error: %v", err)
	}
	if _, err := Devices(); !errors.Is(err, ErrPermission) {
		t.Errorf("Devices: %v", err)
	}
	fc.listErr, fc.devices = nil, nil
	useFake(t, fc)
	fc.openErr = ErrDeviceLost
	if _, err := OpenCapture(CaptureConfig{}); !errors.Is(err, ErrDeviceLost) {
		t.Errorf("open error: %v", err)
	}
	if err := RequestAccess(context.Background()); !errors.Is(err, ErrPermission) {
		t.Errorf("RequestAccess: %v", err)
	}
}

func TestDeviceLost(t *testing.T) {
	fc := useFake(t, &fakeCamera{})
	c, err := OpenCapture(CaptureConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fc.push(0, 1)
	fc.sink().fail(errors.Join(ErrDeviceLost, errors.New("unplugged")))
	fc.push(1, 2)
	if f, err := c.Read(context.Background()); err != nil || f.Timestamp != 0 {
		t.Fatalf("buffered frame: %v", err)
	}
	for range 2 {
		if _, err := c.Read(context.Background()); !errors.Is(err, ErrDeviceLost) {
			t.Fatalf("after loss: %v", err)
		}
	}
	if s := c.Stats(); s.Frames != 1 {
		t.Fatalf("frame after loss counted: %+v", s)
	}
}

func TestStallTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		useFake(t, &fakeCamera{})
		c, err := OpenCapture(CaptureConfig{StallTimeout: 50 * time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		start := time.Now()
		_, err = c.Read(context.Background())
		if !errors.Is(err, ErrDeviceLost) {
			t.Fatalf("stall: %v", err)
		}
		if d := time.Since(start); d != 50*time.Millisecond {
			t.Fatalf("stall reported after %v, want 50ms", d)
		}
	})
}

func TestNoStallWhileFramesFlow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		useFake(t, &fakeCamera{period: 5 * time.Millisecond})
		c, err := OpenCapture(CaptureConfig{StallTimeout: 200 * time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		deadline := time.Now().Add(time.Second)
		reads := 0
		for time.Now().Before(deadline) {
			if _, err := c.Read(context.Background()); err != nil {
				t.Fatal(err)
			}
			reads++
		}
		if reads != 200 {
			t.Fatalf("%d reads in 1s of 5ms frames, want 200", reads)
		}
	})
}

func TestStallAfterFramesStop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fc := useFake(t, &fakeCamera{})
		c, err := OpenCapture(CaptureConfig{StallTimeout: 200 * time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		for range 10 {
			time.Sleep(150 * time.Millisecond)
			fc.push(0, 1)
			if _, err := c.Read(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
		last := time.Now()
		if _, err := c.Read(context.Background()); !errors.Is(err, ErrDeviceLost) {
			t.Fatalf("stall: %v", err)
		}
		if d := time.Since(last); d != 200*time.Millisecond {
			t.Fatalf("stall reported %v after the last frame, want 200ms", d)
		}
	})
}

func TestStallDisabled(t *testing.T) {
	useFake(t, &fakeCamera{})
	c, err := OpenCapture(CaptureConfig{StallTimeout: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := c.Read(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
}

func TestReadCancel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fc := useFake(t, &fakeCamera{})
		c, err := OpenCapture(CaptureConfig{})
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		ctx, cancel := context.WithCancel(context.Background())
		errc := make(chan error, 1)
		go func() {
			_, err := c.Read(ctx)
			errc <- err
		}()
		synctest.Wait() // the Read is blocked
		cancel()
		if err := <-errc; !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
		fc.push(7, 8)
		if f := readN(t, c, 1)[0]; f.Timestamp != 7 {
			t.Fatal("capture unusable after cancel")
		}
	})
}

func TestCloseWhileReading(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fc := useFake(t, &fakeCamera{})
		c, err := OpenCapture(CaptureConfig{StallTimeout: -1})
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		for range 3 {
			wg.Go(func() {
				if _, err := c.Read(context.Background()); !errors.Is(err, ErrClosed) {
					t.Errorf("blocked Read: %v", err)
				}
			})
		}
		synctest.Wait() // every Read is blocked
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
		wg.Wait()
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Read(context.Background()); !errors.Is(err, ErrClosed) {
			t.Fatalf("Read after Close: %v", err)
		}
		fc.push(0, 1)
		if n := fc.opens[0].closes.Load(); n != 1 {
			t.Fatalf("backend closed %d times", n)
		}
	})
}

func TestCloseDiscardsBuffered(t *testing.T) {
	fc := useFake(t, &fakeCamera{})
	c, _ := OpenCapture(CaptureConfig{})
	fc.push(0, 2)
	c.Close()
	if _, err := c.Read(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("got %v", err)
	}
}

func TestConcurrentCapture(t *testing.T) {
	useFake(t, &fakeCamera{period: 100 * time.Microsecond})
	c, err := OpenCapture(CaptureConfig{Buffer: 3})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for {
				if _, err := c.Read(context.Background()); err != nil {
					if !errors.Is(err, ErrClosed) {
						t.Error(err)
					}
					return
				}
				c.Stats()
			}
		})
	}
	time.Sleep(settle)
	wg.Go(func() { c.Close() })
	c.Close()
	wg.Wait()
}

func TestNoGoroutineLeak(t *testing.T) {
	useFake(t, &fakeCamera{period: 200 * time.Microsecond})
	before := runtime.NumGoroutine()
	for range 20 {
		c, err := OpenCapture(CaptureConfig{})
		if err != nil {
			t.Fatal(err)
		}
		readN(t, c, 2)
		ctx, cancel := context.WithCancel(context.Background())
		go func() { time.Sleep(settle); cancel() }()
		c.Read(ctx)
		c.Close()
	}
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > before {
		t.Fatalf("%d goroutines before, %d after", before, n)
	}
}

func TestLongRunning(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		useFake(t, &fakeCamera{period: 500 * time.Microsecond})
		c, err := OpenCapture(CaptureConfig{Buffer: 4})
		if err != nil {
			t.Fatal(err)
		}
		var read uint64
		prev := time.Duration(-1)
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			f, err := c.Read(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if f.Timestamp <= prev {
				t.Fatalf("timestamp %v after %v", f.Timestamp, prev)
			}
			prev = f.Timestamp
			read++
			if read%50 == 0 {
				time.Sleep(20 * time.Millisecond) // 40 frames arrive, 4 fit
			}
		}
		s := c.Stats()
		c.Close()
		if s.Dropped == 0 || read+s.Dropped > s.Frames || s.Frames-read-s.Dropped > 4 {
			t.Fatalf("read %d, stats %+v", read, s)
		}
		t.Logf("read %d, stats %+v", read, s)
	})
}

func TestPixelFormatString(t *testing.T) {
	if FormatNV12.String() != "NV12" || FormatMJPEG.String() != "MJPEG" || PixelFormat(99).String() != "PixelFormat(99)" {
		t.Fatal("names")
	}
}

func TestCaptureEdges(t *testing.T) {
	fc := useFake(t, &fakeCamera{early: true, closeErr: errors.New("backend close failed")})
	c, err := OpenCapture(CaptureConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if f := readN(t, c, 1)[0]; f.Timestamp != 42 {
		t.Fatal("frame delivered during open was lost")
	}
	d := c.Device()
	d.Modes[0].Width = 1
	if c.Device().Modes[0].Width == 1 {
		t.Fatal("Device shares its Modes slice")
	}
	fc.sink().fail(ErrPermission)
	if _, err := c.Read(context.Background()); !errors.Is(err, ErrPermission) {
		t.Fatalf("permission loss: %v", err)
	}
	if err := c.Close(); err == nil || err.Error() != "backend close failed" {
		t.Fatalf("Close: %v", err)
	}
	if err := c.Close(); err == nil {
		t.Fatal("second Close lost the first error")
	}
}

func TestCaptureMaxPixels(t *testing.T) {
	useFake(t, &fakeCamera{})
	if _, err := OpenCapture(CaptureConfig{MaxPixels: -1}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("negative: %v", err)
	}
	c, err := OpenCapture(CaptureConfig{DeviceID: "cam0", Width: 1280, Height: 720, MaxPixels: 640 * 480})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if m := c.Mode(); m.Width != 640 {
		t.Fatalf("mode over MaxPixels chosen: %+v", m)
	}
	if _, err := OpenCapture(CaptureConfig{MaxPixels: 100}); !errors.Is(err, ErrUnsupportedFormat) {
		t.Fatalf("all modes too large: %v", err)
	}
}

// needSources skips a test that inspects the package files when the test
// binary runs away from the source directory.
func needSources(t *testing.T) {
	t.Helper()
	if _, err := os.Stat("capture_other.go"); err != nil {
		t.Skip("run from the package directory: ", err)
	}
}

// TestOneCaptureBackendPerTarget checks that for every GOOS/GOARCH the go
// tool knows, with cgo on and off, exactly one file defines each platform
// function and the package has no cgo file.
func TestOneCaptureBackendPerTarget(t *testing.T) {
	needSources(t)
	out, err := exec.Command("go", "tool", "dist", "list").Output()
	if err != nil {
		t.Skipf("go tool dist list: %v", err)
	}
	targets := strings.Fields(string(out))
	if len(targets) < 40 {
		t.Fatalf("only %d targets listed", len(targets))
	}
	want := map[string]bool{"platformDevices": true, "platformOpen": true, "platformRequestAccess": true}
	for _, target := range targets {
		goos, goarch, _ := strings.Cut(target, "/")
		for _, cgo := range []bool{false, true} {
			ctx := build.Default
			ctx.GOOS, ctx.GOARCH, ctx.CgoEnabled = goos, goarch, cgo
			pkg, err := ctx.ImportDir(".", 0)
			if err != nil {
				t.Errorf("%s cgo=%v: %v", target, cgo, err)
				continue
			}
			defined := map[string][]string{}
			fset := token.NewFileSet()
			for _, name := range pkg.GoFiles {
				f, err := parser.ParseFile(fset, filepath.Join(pkg.Dir, name), nil, parser.SkipObjectResolution)
				if err != nil {
					t.Fatal(err)
				}
				for _, d := range f.Decls {
					if fn, ok := d.(*ast.FuncDecl); ok && fn.Recv == nil && want[fn.Name.Name] {
						defined[fn.Name.Name] = append(defined[fn.Name.Name], name)
					}
				}
			}
			for fn := range want {
				if len(defined[fn]) != 1 {
					t.Errorf("%s cgo=%v: %s defined in %v, want one file", target, cgo, fn, defined[fn])
				}
			}
			if len(pkg.CgoFiles) > 0 {
				t.Errorf("%s cgo=%v: cgo files %v", target, cgo, pkg.CgoFiles)
			}
		}
	}
}

// TestLinuxRootHasNoPurego keeps the root package static on Linux: purego
// would make a CGO_ENABLED=0 binary dynamically linked to glibc.
func TestLinuxRootHasNoPurego(t *testing.T) {
	needSources(t)
	out, err := exec.Command("go", "tool", "dist", "list").Output()
	if err != nil {
		t.Skipf("go tool dist list: %v", err)
	}
	for _, target := range strings.Fields(string(out)) {
		goos, goarch, _ := strings.Cut(target, "/")
		if goos != "linux" {
			continue
		}
		for _, tags := range []string{"", "kolovideo_native"} {
			cmd := exec.Command("go", "list", "-deps", "-tags", tags, "-f", "{{.ImportPath}}", ".")
			cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH="+goarch, "CGO_ENABLED=0", "GOFLAGS=")
			deps, err := cmd.Output()
			if err != nil {
				var stderr []byte
				if ee, ok := err.(*exec.ExitError); ok {
					stderr = ee.Stderr
				}
				t.Fatalf("%s tags=%q: go list (it needs the module cache, GOMODCACHE): %v\n%s", target, tags, err, stderr)
			}
			for _, d := range strings.Fields(string(deps)) {
				if strings.Contains(d, "purego") || d == "runtime/cgo" {
					t.Errorf("%s tags=%q: root package depends on %s", target, tags, d)
				}
			}
		}
	}
}

func TestFrameRateRange(t *testing.T) {
	for _, rate := range []float64{MinFrameRate, 1, MaxFrameRate} {
		cfg, err := CaptureConfig{FrameRate: rate}.withDefaults()
		if err != nil || cfg.FrameRate != rate {
			t.Errorf("rate %v: %+v %v", rate, cfg, err)
		}
	}
	if _, err := (CaptureConfig{FrameRate: MinFrameRate / 1.001}).withDefaults(); !errors.Is(err, ErrInvalidConfig) {
		t.Errorf("below the minimum: %v", err)
	}
}

func TestFrameDuration(t *testing.T) {
	for rate, want := range map[float64]time.Duration{
		30: time.Second / 30, 1: time.Second, MinFrameRate: time.Hour, 1e-300: time.Hour, 1e-9: time.Hour,
		math.Inf(1): 0, 0: 0, -1: 0, math.NaN(): 0,
	} {
		if got := frameDuration(rate); got < want-time.Microsecond || got > want+time.Microsecond {
			t.Errorf("frameDuration(%v) = %v, want %v", rate, got, want)
		}
	}
}

func TestZeroCapture(t *testing.T) {
	var c Capture
	if _, err := c.Read(context.Background()); !errors.Is(err, ErrClosed) {
		t.Errorf("Read: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	if _, err := c.Read(context.Background()); !errors.Is(err, ErrClosed) {
		t.Errorf("Read after Close: %v", err)
	}
	_ = c.Stats()
	_ = c.Mode()
	_ = c.Device()
}
