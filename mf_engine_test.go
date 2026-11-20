package kolovideo

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"
)

// HRESULTs the fake returns.
const (
	testDeviceInvalidated mfHRESULT = 0xC00D3EA2 // MF_E_VIDEO_RECORDING_DEVICE_INVALIDATED, mferror.h:136
	testShutdown          mfHRESULT = 0xC00D3E85 // MF_E_SHUTDOWN, mferror.h:115
	testStartStreaming    mfHRESULT = 0xC00D3704 // MF_E_HW_MFT_FAILED_START_STREAMING, mferror.h:95
)

func mfOpenFake(t *testing.T, f *mfFake, cfg CaptureConfig) (backend, Mode, *mfSink, error) {
	t.Helper()
	cfg, err := cfg.withDefaults()
	if err != nil {
		t.Fatal(err)
	}
	sink := newMFSink()
	be, mode, err := mfOpen(f, Device{ID: "id0", Name: "Front"}, cfg, sink)
	if be != nil {
		t.Cleanup(func() { be.close() })
	}
	return be, mode, sink, err
}

func mfIndexOf(log []string, s string, from int) int {
	for i := from; i < len(log); i++ {
		if log[i] == s {
			return i
		}
	}
	return -1
}

func waitLog(t *testing.T, f *mfFake, s string) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); mfIndexOf(f.events(), s, 0) < 0; {
		if time.Now().After(end) {
			t.Fatalf("%q never logged: %v", s, f.events())
		}
		time.Sleep(time.Millisecond)
	}
}

func recvFrame(t *testing.T, s *mfSink) *Frame {
	t.Helper()
	select {
	case f := <-s.frames:
		return f
	case <-time.After(5 * time.Second):
		t.Fatal("no frame")
	}
	return nil
}

func recvFail(t *testing.T, s *mfSink) error {
	t.Helper()
	select {
	case err := <-s.failed:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("no failure reported")
	}
	return nil
}

// sample queues an NV12 sample of the default 6x4 layout at pitch 8.
func (f *mfFake) sample(ts time.Duration, y, u, v byte) *atomic.Int32 {
	released := new(atomic.Int32)
	data := nv12Data(8, 4, 6, 4, y, u, v)
	f.reads <- mfFakeRead{s: mfSample{data: data, pitch: 8, ts: ts, done: func() {
		released.Add(1)
		for i := range data {
			data[i] = 0
		}
	}}}
	return released
}

func TestMFDevices(t *testing.T) {
	f := newMFFake()
	devs, err := mfDevices(f)
	if err != nil {
		t.Fatal(err)
	}
	want := []Device{{ID: "id0", Name: "Front", Default: true}, {ID: "id1", Name: "USB"}}
	if !slices.EqualFunc(devs, want, func(a, b Device) bool {
		return a.ID == b.ID && a.Name == b.Name && a.Default == b.Default && len(a.Modes) == 0
	}) {
		t.Errorf("devices %+v, want %+v", devs, want)
	}
	f.waitIdle(t)
	if got, want := f.events(), []string{"coInit", "startup", "devices", "mfShutdown", "coUninit"}; !slices.Equal(got, want) {
		t.Errorf("calls %v, want %v", got, want)
	}
}

func TestMFDevicesNone(t *testing.T) {
	f := newMFFake()
	f.list = nil
	devs, err := mfDevices(f)
	if err != nil || devs != nil {
		t.Errorf("devices %v, %v", devs, err)
	}
	f.waitIdle(t)
}

func TestMFDevicesErrors(t *testing.T) {
	missing := fmt.Errorf("%w: Failed to load mfplat.dll", ErrUnsupported)
	for _, tc := range []struct {
		name  string
		set   func(*mfFake)
		is    error
		wants string
	}{
		{"list fails", func(f *mfFake) { f.listErr = testStartStreaming }, nil, "list cameras: HRESULT 0xc00d3704"},
		{"list denied", func(f *mfFake) { f.listErr = hrAccessDenied }, ErrPermission, "privacy"},
		{"Media Foundation missing", func(f *mfFake) { f.startErr = missing }, ErrUnsupported, "mfplat.dll"},
		{"COM fails", func(f *mfFake) { f.coErr = mfHRESULT(0x80040154) }, nil, "start COM: HRESULT 0x80040154"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newMFFake()
			tc.set(f)
			devs, err := mfDevices(f)
			if err == nil || devs != nil {
				t.Fatalf("devices %v, %v", devs, err)
			}
			if tc.is != nil && !errors.Is(err, tc.is) {
				t.Errorf("%v is not %v", err, tc.is)
			}
			if !strings.Contains(err.Error(), tc.wants) {
				t.Errorf("error %q lacks %q", err, tc.wants)
			}
			f.waitIdle(t)
		})
	}
}

func TestMFThreadOrder(t *testing.T) {
	f := newMFFake()
	ran := make(chan struct{})
	if err := mfThread(f, func() { f.add("fn"); close(ran) }); err != nil {
		t.Fatal(err)
	}
	<-ran
	f.waitIdle(t)
	if got, want := f.events(), []string{"coInit", "startup", "fn", "mfShutdown", "coUninit"}; !slices.Equal(got, want) {
		t.Errorf("calls %v, want %v", got, want)
	}
}

func TestMFModeSelection(t *testing.T) {
	modes := []Mode{
		{},
		{640, 480, 30, FormatMJPEG},
		{1280, 720, 15, FormatNV12},
		{1280, 720, 30, FormatYUY2},
		{1280, 720, 30, FormatNV12},
		{1280, 720, 60, FormatNV12},
	}
	for _, tc := range []struct {
		name       string
		cfg        CaptureConfig
		layout     mfLayout
		wantStart  string
		wantMode   Mode
		wantLayout bool
	}{
		{"exact size and rate, the first usable native type counts from index 0", CaptureConfig{Width: 1280, Height: 720, FrameRate: 30},
			mfLayout{w: 1280, h: 720, bufw: 1280, bufh: 720}, "start 4", Mode{1280, 720, 30, FormatNV12}, true},
		{"the rate is the camera's, not the request", CaptureConfig{Width: 1280, Height: 720, FrameRate: 60},
			mfLayout{w: 1280, h: 720, bufw: 1280, bufh: 720}, "start 5", Mode{1280, 720, 60, FormatNV12}, true},
		{"nearest size", CaptureConfig{Width: 800, Height: 600, FrameRate: 30},
			mfLayout{w: 640, h: 480, bufw: 640, bufh: 480}, "start 1", Mode{640, 480, 30, FormatMJPEG}, true},
		{"the visible size comes from the reader", CaptureConfig{Width: 1280, Height: 720, FrameRate: 30},
			mfLayout{w: 1280, h: 720, bufw: 1280, bufh: 736}, "start 4", Mode{1280, 720, 30, FormatNV12}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newMFFake()
			f.modes, f.layout = modes, tc.layout
			_, mode, _, err := mfOpenFake(t, f, tc.cfg)
			if err != nil {
				t.Fatal(err)
			}
			if mode != tc.wantMode {
				t.Errorf("mode %+v, want %+v", mode, tc.wantMode)
			}
			if mfIndexOf(f.events(), tc.wantStart, 0) < 0 {
				t.Errorf("calls %v lack %q", f.events(), tc.wantStart)
			}
		})
	}
}

func TestMFOpenErrors(t *testing.T) {
	for _, tc := range []struct {
		name  string
		set   func(*mfFake)
		cfg   CaptureConfig
		is    error
		not   error
		wants string
	}{
		{"privacy off at activation", func(f *mfFake) { f.srcErr = hrAccessDenied }, CaptureConfig{}, ErrPermission, ErrNoDevice, "privacy"},
		{"device gone at activation", func(f *mfFake) { f.srcErr = mfHRESULT(0x80070490) }, CaptureConfig{}, ErrNoDevice, ErrPermission, "0x80070490"},
		{"modes unreadable", func(f *mfFake) { f.modesErr = testStartStreaming }, CaptureConfig{}, nil, ErrNoDevice, "list camera modes: HRESULT 0xc00d3704"},
		{"no usable mode", func(f *mfFake) { f.modes = []Mode{{}, {640, 480, 30, FormatUnknown}} }, CaptureConfig{}, ErrUnsupportedFormat, nil, "no usable mode"},
		{"every mode too large", func(f *mfFake) {}, CaptureConfig{MaxPixels: 10}, ErrUnsupportedFormat, nil, "no usable mode"},
		{"privacy off at start", func(f *mfFake) { f.startReadErr = hrAccessDenied }, CaptureConfig{}, ErrPermission, nil, "start camera"},
		{"start fails", func(f *mfFake) { f.startReadErr = testStartStreaming }, CaptureConfig{}, nil, ErrNoDevice, "start camera: HRESULT 0xc00d3704"},
		{"reader output too large", func(f *mfFake) { f.layout = mfLayout{w: 5000, h: 5000, bufw: 5000, bufh: 5000} }, CaptureConfig{}, ErrUnsupportedFormat, nil, "reader output 5000x5000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newMFFake()
			tc.set(f)
			be, _, _, err := mfOpenFake(t, f, tc.cfg)
			if err == nil || be != nil {
				t.Fatalf("opened: %v", err)
			}
			if tc.is != nil && !errors.Is(err, tc.is) {
				t.Errorf("%v is not %v", err, tc.is)
			}
			if tc.not != nil && errors.Is(err, tc.not) {
				t.Errorf("%v is %v", err, tc.not)
			}
			if !strings.Contains(err.Error(), tc.wants) {
				t.Errorf("error %q lacks %q", err, tc.wants)
			}
			f.waitIdle(t)
			log := f.events()
			if mfIndexOf(log, "source.release", 0) >= 0 != (mfIndexOf(log, "source id0", 0) >= 0 && f.srcErr == nil) {
				t.Errorf("source not released exactly when opened: %v", log)
			}
		})
	}
}

func TestMFOpenFailureReleasesInOrder(t *testing.T) {
	f := newMFFake()
	f.layout = mfLayout{w: 5000, h: 5000, bufw: 5000, bufh: 5000}
	if _, _, _, err := mfOpenFake(t, f, CaptureConfig{}); !errors.Is(err, ErrUnsupportedFormat) {
		t.Fatal(err)
	}
	f.waitIdle(t)
	want := []string{"coInit", "startup", "source id0", "modes", "start 0", "reader.release", "source.shutdown", "source.release", "mfShutdown", "coUninit"}
	if got := f.events(); !slices.Equal(got, want) {
		t.Errorf("calls %v, want %v", got, want)
	}
}

func TestMFOpenStartupErrors(t *testing.T) {
	f := newMFFake()
	f.startErr = fmt.Errorf("%w: Failed to load mfreadwrite.dll", ErrUnsupported)
	_, _, _, err := mfOpenFake(t, f, CaptureConfig{})
	if !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), "mfreadwrite.dll") {
		t.Errorf("error %v", err)
	}
	f.waitIdle(t)
}

func TestMFFrames(t *testing.T) {
	f := newMFFake()
	be, mode, sink, err := mfOpenFake(t, f, CaptureConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if mode != (Mode{6, 4, 30, FormatNV12}) {
		t.Fatalf("mode %+v", mode)
	}
	freed := f.sample(11*time.Millisecond, 0x40, 0x70, 0x90)
	fr := recvFrame(t, sink)
	if err := fr.Validate(); err != nil {
		t.Fatal(err)
	}
	if fr.Width != 6 || fr.Height != 4 {
		t.Errorf("frame %dx%d", fr.Width, fr.Height)
	}
	if fr.Timestamp != 11*time.Millisecond || fr.Duration != frameDuration(30) {
		t.Errorf("timestamp %v duration %v", fr.Timestamp, fr.Duration)
	}
	be.close()
	if freed.Load() != 1 {
		t.Errorf("sample released %d times", freed.Load())
	}
	// The release zeroed the sample, so the frame must own its pixels.
	for r := range fr.Height {
		for x := range fr.Width {
			if fr.Y[r*fr.YStride+x] != 0x40 {
				t.Fatalf("luma %#x at %d,%d", fr.Y[r*fr.YStride+x], x, r)
			}
		}
	}
	for r := range 2 {
		for x := range 3 {
			if fr.U[r*fr.UVStride+x] != 0x70 || fr.V[r*fr.UVStride+x] != 0x90 {
				t.Fatalf("chroma %#x %#x", fr.U[r*fr.UVStride+x], fr.V[r*fr.UVStride+x])
			}
		}
	}
}

func TestMFTimestampsFollowTheHostClock(t *testing.T) {
	f := newMFFake()
	_, _, sink, err := mfOpenFake(t, f, CaptureConfig{})
	if err != nil {
		t.Fatal(err)
	}
	want := []time.Duration{3 * time.Second, 3*time.Second + 33*time.Millisecond, 3*time.Second + 70*time.Millisecond}
	for _, ts := range want {
		f.sample(ts, 1, 2, 3)
	}
	for _, ts := range want {
		if got := recvFrame(t, sink).Timestamp; got != ts {
			t.Errorf("timestamp %v, want %v", got, ts)
		}
	}
}

func TestMFFrameLayouts(t *testing.T) {
	bt := mfLayout{w: 6, h: 4, bufw: 6, bufh: 4, bt709: true}
	for _, tc := range []struct {
		name    string
		layout  mfLayout
		data    []byte
		pitch   int
		invalid bool
	}{
		{"padded rows", mfLayout{w: 6, h: 4, bufw: 6, bufh: 4}, nv12Data(8, 4, 6, 4, 9, 8, 7), 8, false},
		{"no 2D layout takes the buffer width", mfLayout{w: 6, h: 4, bufw: 6, bufh: 4}, nv12Data(6, 4, 6, 4, 9, 8, 7), 0, false},
		{"visible area inside a taller buffer", mfLayout{w: 6, h: 4, bufw: 8, bufh: 8}, nv12Data(8, 8, 6, 4, 9, 8, 7), 8, false},
		{"BT.709 is converted", bt, nv12Data(8, 4, 6, 4, 9, 8, 7), 8, false},
		{"negative pitch", mfLayout{w: 6, h: 4, bufw: 6, bufh: 4}, nv12Data(8, 4, 6, 4, 9, 8, 7), -8, true},
		{"buffer shorter than its layout", mfLayout{w: 6, h: 4, bufw: 6, bufh: 4}, nv12Data(8, 4, 6, 4, 9, 8, 7)[:40], 8, true},
		{"pitch narrower than the picture", mfLayout{w: 6, h: 4, bufw: 6, bufh: 4}, make([]byte, 48), 4, true},
		{"visible height above the buffer", mfLayout{w: 6, h: 6, bufw: 6, bufh: 4}, nv12Data(8, 4, 6, 4, 9, 8, 7), 8, true},
		{"empty buffer", mfLayout{w: 6, h: 4, bufw: 6, bufh: 4}, []byte{}, 8, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newMFFake()
			f.layout = tc.layout
			f.modes = []Mode{{tc.layout.bufw, tc.layout.bufh, 30, FormatNV12}}
			_, _, sink, err := mfOpenFake(t, f, CaptureConfig{})
			if err != nil {
				t.Fatal(err)
			}
			f.reads <- mfFakeRead{s: mfSample{data: tc.data, pitch: tc.pitch}}
			if tc.invalid {
				for end := time.Now().Add(5 * time.Second); sink.invalidN.Load() == 0; time.Sleep(time.Millisecond) {
					if time.Now().After(end) {
						t.Fatal("the frame was not reported invalid")
					}
				}
				if len(sink.frames) != 0 || len(sink.failed) != 0 {
					t.Error("an invalid frame was delivered or ended the capture")
				}
				return
			}
			got := recvFrame(t, sink)
			pitch := max(tc.pitch, tc.layout.bufw)
			want, err := FromNV12(tc.layout.w, tc.layout.h, tc.data, pitch, tc.data[pitch*tc.layout.bufh:], pitch)
			if err != nil {
				t.Fatal(err)
			}
			if tc.layout.bt709 {
				want.rec709to601()
			}
			if !slices.Equal(got.Y, want.Y) || !slices.Equal(got.U, want.U) || !slices.Equal(got.V, want.V) {
				t.Errorf("frame differs from FromNV12:\n%v\n%v", got.Y, want.Y)
			}
			if sink.invalidN.Load() != 0 {
				t.Error("valid frame counted invalid")
			}
		})
	}
}

func TestMFGapsAreSkipped(t *testing.T) {
	f := newMFFake()
	_, _, sink, err := mfOpenFake(t, f, CaptureConfig{})
	if err != nil {
		t.Fatal(err)
	}
	f.reads <- mfFakeRead{s: mfSample{flags: 0x100}} // MF_SOURCE_READERF_STREAMTICK: no picture
	f.sample(time.Second, 1, 2, 3)
	if got := recvFrame(t, sink); got.Timestamp != time.Second {
		t.Errorf("timestamp %v", got.Timestamp)
	}
	if sink.invalidN.Load() != 0 || len(sink.failed) != 0 {
		t.Error("a gap was reported")
	}
}

func TestMFReadFailures(t *testing.T) {
	for _, tc := range []struct {
		name  string
		read  mfFakeRead
		is    error
		not   error
		wants string
	}{
		{"device unplugged", mfFakeRead{err: testDeviceInvalidated}, ErrDeviceLost, ErrPermission, "HRESULT 0xc00d3ea2"},
		{"reader error flag", mfFakeRead{s: mfSample{flags: mfReadError}}, ErrDeviceLost, nil, "stream ended"},
		{"end of stream", mfFakeRead{s: mfSample{flags: mfReadEndOfStream}}, ErrDeviceLost, nil, "stream ended"},
		{"access withdrawn", mfFakeRead{err: hrAccessDenied}, ErrPermission, ErrDeviceLost, "privacy"},
		{"no sample buffer", mfFakeRead{err: hrUnexpected}, ErrDeviceLost, nil, "0x8000ffff"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newMFFake()
			_, _, sink, err := mfOpenFake(t, f, CaptureConfig{})
			if err != nil {
				t.Fatal(err)
			}
			released := new(atomic.Int32)
			tc.read.s.done = func() { released.Add(1) }
			f.reads <- tc.read
			err = recvFail(t, sink)
			if !errors.Is(err, tc.is) || (tc.not != nil && errors.Is(err, tc.not)) || !strings.Contains(err.Error(), tc.wants) {
				t.Errorf("error %v, want %v containing %q", err, tc.is, tc.wants)
			}
			if h, ok := mfCode(err); tc.read.err != nil && (!ok || h != tc.read.err) {
				t.Errorf("HRESULT %v %v lost in %v", h, ok, err)
			}
			waitLog(t, f, "source.release")
			if released.Load() != 1 {
				t.Errorf("failed sample released %d times", released.Load())
			}
			f.reads <- mfFakeRead{} // never read: the loop ended
			if len(sink.failed) != 0 {
				t.Error("failure reported twice")
			}
		})
	}
}

func TestMFFormatChange(t *testing.T) {
	changed := mfSample{flags: mfReadTypeChanged}
	t.Run("new layout is used", func(t *testing.T) {
		f := newMFFake()
		f.layoutNext = mfLayout{w: 8, h: 2, bufw: 8, bufh: 2}
		_, _, sink, err := mfOpenFake(t, f, CaptureConfig{})
		if err != nil {
			t.Fatal(err)
		}
		changed.data, changed.pitch = nv12Data(8, 2, 8, 2, 0x55, 0x66, 0x77), 8
		f.reads <- mfFakeRead{s: changed}
		got := recvFrame(t, sink)
		if got.Width != 8 || got.Height != 2 || got.Y[0] != 0x55 {
			t.Errorf("frame %dx%d y %#x", got.Width, got.Height, got.Y[0])
		}
	})
	for _, tc := range []struct {
		name string
		set  func(*mfFake)
		want string
	}{
		{"layout unreadable", func(f *mfFake) { f.layoutErr = testDeviceInvalidated }, "read format"},
		{"layout too large", func(f *mfFake) { f.layoutNext = mfLayout{w: 5000, h: 5000, bufw: 5000, bufh: 5000} }, "format change to 5000x5000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newMFFake()
			tc.set(f)
			_, _, sink, err := mfOpenFake(t, f, CaptureConfig{})
			if err != nil {
				t.Fatal(err)
			}
			f.reads <- mfFakeRead{s: changed}
			if err := recvFail(t, sink); !errors.Is(err, ErrDeviceLost) || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %v", err)
			}
		})
	}
}

func TestMFCloseOrder(t *testing.T) {
	f := newMFFake()
	be, _, sink, err := mfOpenFake(t, f, CaptureConfig{})
	if err != nil {
		t.Fatal(err)
	}
	f.sample(time.Second, 1, 2, 3)
	recvFrame(t, sink)
	if err := be.close(); err != nil {
		t.Fatal(err)
	}
	sink.closed.Store(true)
	f.waitIdle(t)
	log := f.events()
	// A thread with an apartment of its own, entered after the reading
	// thread's, flushes the blocked read; only then does the reading thread
	// release the reader, shut the source down and release it.
	second := mfIndexOf(log, "coInit", mfIndexOf(log, "coInit", 0)+1)
	flush := mfIndexOf(log, "reader.flush", 0)
	rel := mfIndexOf(log, "reader.release", 0)
	shut := mfIndexOf(log, "source.shutdown", 0)
	src := mfIndexOf(log, "source.release", 0)
	down := mfIndexOf(log, "mfShutdown", 0)
	if !(second > 0 && second < flush && flush < rel && rel < shut && shut < src && src < down) {
		t.Errorf("order %v: coInit %d, flush %d, reader.release %d, shutdown %d, source.release %d, mfShutdown %d", log, second, flush, rel, shut, src, down)
	}
	if n := f.shutdowns.Load(); n != 1 {
		t.Errorf("%d shutdowns, want 1: the source must be shut down after the reader is idle", n)
	}
	if got, want := strings.Count(strings.Join(log, " "), "coUninit"), strings.Count(strings.Join(log, " "), "coInit"); got != want {
		t.Errorf("apartments not balanced: %v", log)
	}
	select {
	case err := <-sink.failed:
		t.Errorf("close reported %v", err)
	default:
	}
	f.sample(2*time.Second, 1, 2, 3)
	time.Sleep(settle)
	if sink.late.Load() != 0 {
		t.Errorf("%d sink calls after close", sink.late.Load())
	}
}

func setCloseWait(t *testing.T, d time.Duration) {
	old := mfCloseWait
	mfCloseWait = d
	t.Cleanup(func() { mfCloseWait = old })
}

// TestMFCloseShutsTheSourceWhenFlushIsNotEnough covers a reader that stays
// blocked after Flush.
func TestMFCloseShutsTheSourceWhenFlushIsNotEnough(t *testing.T) {
	setCloseWait(t, 30*time.Millisecond)
	f := newMFFake()
	f.flushNoop = true
	be, _, sink, err := mfOpenFake(t, f, CaptureConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if err := be.close(); err != nil {
		t.Fatal(err)
	}
	sink.closed.Store(true)
	f.waitIdle(t)
	log := f.events()
	flush := mfIndexOf(log, "reader.flush", 0)
	first := mfIndexOf(log, "source.shutdown", 0)
	rel := mfIndexOf(log, "reader.release", 0)
	if !(flush > 0 && flush < first && first < rel) {
		t.Errorf("order %v", log)
	}
	if f.flushes.Load() != 1 || f.shutdowns.Load() != 2 {
		t.Errorf("%d flushes and %d shutdowns, want 1 and 2 (closing thread, release)", f.flushes.Load(), f.shutdowns.Load())
	}
}

// TestMFCloseWithAWedgedReader covers a driver that never returns from the
// read: close reports it, the sink is not called afterwards, and everything
// is released when the read finally returns.
func TestMFCloseWithAWedgedReader(t *testing.T) {
	setCloseWait(t, 20*time.Millisecond)
	f := newMFFake()
	f.flushNoop, f.wedge = true, true
	be, _, sink, err := mfOpenFake(t, f, CaptureConfig{})
	if err != nil {
		t.Fatal(err)
	}
	err = be.close()
	if err == nil || !strings.Contains(err.Error(), "did not stop") || !strings.HasPrefix(err.Error(), "kolovideo: ") {
		t.Fatalf("close: %v", err)
	}
	sink.closed.Store(true)
	if err2 := be.close(); err2 != err {
		t.Errorf("second close: %v, want the first error", err2)
	}
	if f.flushes.Load() != 1 || f.shutdowns.Load() != 1 {
		t.Errorf("%d flushes and %d shutdowns, want 1 and 1", f.flushes.Load(), f.shutdowns.Load())
	}
	if mfIndexOf(f.events(), "reader.release", 0) >= 0 {
		t.Error("the reader was released while a read was in progress")
	}
	f.reads <- mfFakeRead{s: mfSample{data: nv12Data(8, 4, 6, 4, 1, 2, 3), pitch: 8}}
	close(f.unwedge)
	f.waitIdle(t)
	if sink.late.Load() != 0 || len(sink.frames) != 0 || len(sink.failed) != 0 {
		t.Errorf("%d sink calls after close", sink.late.Load())
	}
	if mfIndexOf(f.events(), "source.release", 0) < 0 {
		t.Errorf("not released after the read returned: %v", f.events())
	}
}

func TestMFCloseTwiceAndConcurrently(t *testing.T) {
	f := newMFFake()
	be, _, sink, err := mfOpenFake(t, f, CaptureConfig{})
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var feed sync.WaitGroup
	feed.Add(1)
	go func() {
		defer feed.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			select {
			case f.reads <- mfFakeRead{s: mfSample{data: nv12Data(8, 4, 6, 4, 1, 2, 3), pitch: 8}}:
			case <-time.After(time.Millisecond):
			}
		}
	}()
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := be.close(); err != nil {
				t.Error(err)
			}
			select {
			case <-be.(*mfBackend).done:
			default:
				t.Error("close returned before the reading thread finished")
			}
		}()
	}
	wg.Wait()
	sink.closed.Store(true)
	close(stop)
	feed.Wait()
	f.waitIdle(t)
	if n := f.shutdowns.Load(); n != 1 {
		t.Errorf("%d shutdowns, want 1 (the release)", n)
	}
	if n := f.flushes.Load(); n > 1 {
		t.Errorf("%d flushes", n)
	}
	time.Sleep(settle)
	if sink.late.Load() != 0 {
		t.Errorf("%d sink calls after close", sink.late.Load())
	}
}

func TestMFCloseAfterLoss(t *testing.T) {
	f := newMFFake()
	be, _, sink, err := mfOpenFake(t, f, CaptureConfig{})
	if err != nil {
		t.Fatal(err)
	}
	f.reads <- mfFakeRead{err: testDeviceInvalidated}
	recvFail(t, sink)
	<-be.(*mfBackend).done
	if err := be.close(); err != nil {
		t.Fatal(err)
	}
	f.waitIdle(t)
	if n := f.shutdowns.Load(); n != 1 || f.flushes.Load() != 0 {
		t.Errorf("%d shutdowns and %d flushes: close touched a released source", n, f.flushes.Load())
	}
	if n := strings.Count(strings.Join(f.events(), " "), "coInit"); n != 1 {
		t.Errorf("close entered an apartment for a released source: %v", f.events())
	}
}

func TestMFCloseWhenSourceNeverStartedReading(t *testing.T) {
	f := newMFFake()
	be, _, _, err := mfOpenFake(t, f, CaptureConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if err := be.close(); err != nil {
		t.Fatal(err)
	}
	if err := be.close(); err != nil {
		t.Fatal(err)
	}
	f.waitIdle(t)
}

// TestMFCapture drives the backend through the public Capture, as the
// platform hooks do.
func TestMFCapture(t *testing.T) {
	f := newMFFake()
	oldList, oldOpen := listDevices, openBackend
	listDevices = func() ([]Device, error) { return mfDevices(f) }
	openBackend = func(d Device, cfg CaptureConfig, s frameSink) (backend, Mode, error) { return mfOpen(f, d, cfg, s) }
	t.Cleanup(func() { listDevices, openBackend = oldList, oldOpen })

	c, err := OpenCapture(CaptureConfig{DeviceID: "id1", Buffer: 4})
	if err != nil {
		t.Fatal(err)
	}
	if c.Device().ID != "id1" || c.Mode() != (Mode{6, 4, 30, FormatNV12}) {
		t.Errorf("device %+v mode %+v", c.Device(), c.Mode())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for i := range 3 {
		f.sample(time.Duration(i+1)*time.Millisecond, byte(i), 2, 3)
	}
	for i := range 3 {
		fr, err := c.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := fr.Validate(); err != nil || fr.Timestamp != time.Duration(i+1)*time.Millisecond {
			t.Errorf("frame %d: %v, timestamp %v", i, err, fr.Timestamp)
		}
	}
	if st := c.Stats(); st.Frames != 3 || st.Dropped != 0 || st.Invalid != 0 {
		t.Errorf("stats %+v", st)
	}
	f.reads <- mfFakeRead{err: testDeviceInvalidated}
	if _, err := c.Read(ctx); !errors.Is(err, ErrDeviceLost) {
		t.Errorf("read after unplug: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Read(ctx); !errors.Is(err, ErrClosed) {
		t.Errorf("read after close: %v", err)
	}
	f.waitIdle(t)
}

func TestMFCaptureCloseUnblocksReader(t *testing.T) {
	f := newMFFake()
	oldList, oldOpen := listDevices, openBackend
	listDevices = func() ([]Device, error) { return mfDevices(f) }
	openBackend = func(d Device, cfg CaptureConfig, s frameSink) (backend, Mode, error) { return mfOpen(f, d, cfg, s) }
	t.Cleanup(func() { listDevices, openBackend = oldList, oldOpen })
	c, err := OpenCapture(CaptureConfig{})
	if err != nil {
		t.Fatal(err)
	}
	errc := make(chan error, 1)
	go func() { _, err := c.Read(context.Background()); errc <- err }()
	time.Sleep(settle)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-errc; !errors.Is(err, ErrClosed) {
		t.Errorf("blocked read: %v", err)
	}
	f.waitIdle(t)
}

func TestMFMode(t *testing.T) {
	u := func(w, h uint32) *uint64 { v := uint64(w)<<32 | uint64(h); return &v }
	g := func(x mfGUID) *mfGUID { return &x }
	huge := uint32(1<<32 - 1)
	for _, tc := range []struct {
		name       string
		size, rate *uint64
		sub        *mfGUID
		want       Mode
	}{
		{"NV12 30 fps", u(1280, 720), u(30, 1), g(mfFmtNV12), Mode{1280, 720, 30, FormatNV12}},
		{"NTSC rate", u(640, 480), u(30000, 1001), g(mfFmtYUY2), Mode{640, 480, 30000.0 / 1001, FormatYUY2}},
		{"zero denominator", u(640, 480), u(30, 0), g(mfFmtMJPG), Mode{640, 480, 0, FormatMJPEG}},
		{"no rate", u(640, 480), nil, g(mfFmtUYVY), Mode{640, 480, 0, FormatUYVY}},
		{"no size", nil, u(30, 1), g(mfFmtRGB32), Mode{0, 0, 30, FormatBGRA}},
		{"no subtype", u(640, 480), u(30, 1), nil, Mode{640, 480, 30, FormatUnknown}},
		{"H.264 is not a raw format", u(640, 480), u(30, 1), g(mfGUID{0x34363248, 0, 0x10, [8]byte{0x80, 0, 0, 0xAA, 0, 0x38, 0x9B, 0x71}}), Mode{640, 480, 30, FormatUnknown}},
		{"huge size is the converted halves", u(huge, huge), nil, nil, Mode{int(huge), int(huge), 0, FormatUnknown}},
	} {
		if got := mfMode(tc.size, tc.rate, tc.sub); got != tc.want {
			t.Errorf("%s: %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

func TestMFModesBound(t *testing.T) {
	for _, tc := range []struct {
		count uint32
		want  int
	}{{0, 0}, {3, 3}, {mfMaxModes, mfMaxModes}, {mfMaxModes + 1, mfMaxModes}, {1<<32 - 1, mfMaxModes}} {
		reads := 0
		modes := mfModes(tc.count, func(i uint32) Mode { reads++; return Mode{Width: int(i)} })
		if len(modes) != tc.want || reads != tc.want {
			t.Errorf("count %d: %d modes, %d reads, want %d", tc.count, len(modes), reads, tc.want)
		}
		if len(modes) > 2 && modes[2].Width != 2 {
			t.Errorf("modes out of index order: %+v", modes[2])
		}
	}
}

func TestMFPixelFormats(t *testing.T) {
	for _, tc := range []struct {
		g    mfGUID
		want PixelFormat
	}{
		{mfFmtI420, FormatI420}, {mfFmtIYUV, FormatI420}, {mfFmtNV12, FormatNV12}, {mfFmtYUY2, FormatYUY2},
		{mfFmtUYVY, FormatUYVY}, {mfFmtRGB32, FormatBGRA}, {mfFmtMJPG, FormatMJPEG}, {mfMediaVideo, FormatUnknown}, {mfGUID{}, FormatUnknown},
	} {
		if got := mfPixelFormat(tc.g); got != tc.want {
			t.Errorf("%v: %v, want %v", tc.g, got, tc.want)
		}
	}
}

func TestMFLayoutOf(t *testing.T) {
	size := uint64(1280)<<32 | 736
	m709, m601 := uint32(mfMatrixBT709), uint32(2)
	area := func(x, y int16, cx, cy int32) *mfVideoArea {
		return &mfVideoArea{x: mfOffset{value: x}, y: mfOffset{value: y}, cx: cx, cy: cy}
	}
	for _, tc := range []struct {
		name   string
		area   *mfVideoArea
		matrix *uint32
		want   mfLayout
	}{
		{"no aperture", nil, nil, mfLayout{1280, 736, 1280, 736, false}},
		{"aperture crops the buffer", area(0, 0, 1280, 720), nil, mfLayout{1280, 720, 1280, 736, false}},
		{"BT.709", nil, &m709, mfLayout{1280, 736, 1280, 736, true}},
		{"another matrix", nil, &m601, mfLayout{1280, 736, 1280, 736, false}},
		{"offset aperture is ignored", area(8, 0, 1272, 720), nil, mfLayout{1280, 736, 1280, 736, false}},
		{"vertical offset is ignored", area(0, 8, 1280, 720), nil, mfLayout{1280, 736, 1280, 736, false}},
		{"aperture wider than the buffer", area(0, 0, 1290, 720), nil, mfLayout{1280, 736, 1280, 736, false}},
		{"aperture taller than the buffer", area(0, 0, 1280, 740), nil, mfLayout{1280, 736, 1280, 736, false}},
		{"empty aperture", area(0, 0, 0, 0), nil, mfLayout{1280, 736, 1280, 736, false}},
		{"negative aperture", area(0, 0, -1, 720), nil, mfLayout{1280, 736, 1280, 736, false}},
	} {
		if got := mfLayoutOf(size, tc.area, tc.matrix); got != tc.want {
			t.Errorf("%s: %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

func TestQPCDuration(t *testing.T) {
	for _, tc := range []struct {
		count, freq int64
		want        time.Duration
	}{
		{0, 10_000_000, 0},
		{10_000_000, 10_000_000, time.Second},
		{15_000_000, 10_000_000, 1500 * time.Millisecond},
		{1, 10_000_000, 100},
		{24_000_000 * 3600 * 24 * 365, 24_000_000, 365 * 24 * time.Hour},
		{1<<50 + 5, 10_000_000, 112589990*time.Second + 684262900},
		{5, 0, 0},
		{5, -1, 0},
	} {
		if got := qpcDuration(tc.count, tc.freq); got != tc.want {
			t.Errorf("qpcDuration(%d, %d) = %v, want %v", tc.count, tc.freq, got, tc.want)
		}
	}
	prev := time.Duration(-1)
	for c := int64(1) << 40; c < 1<<40+100; c += 7 {
		if d := qpcDuration(c, 10_000_000); d < prev {
			t.Fatalf("not monotonic at %d", c)
		}
		prev = qpcDuration(c, 10_000_000)
	}
}

func TestMFStructLayouts(t *testing.T) {
	if n := unsafe.Sizeof(mfGUID{}); n != 16 {
		t.Errorf("GUID is %d bytes", n)
	}
	// MFOffset is a WORD and a short; MFVideoArea is two of them and a SIZE
	// (mfobjects.h:2811 to 2819).
	if n := unsafe.Sizeof(mfOffset{}); n != 4 {
		t.Errorf("MFOffset is %d bytes", n)
	}
	var a mfVideoArea
	if unsafe.Sizeof(a) != 16 || unsafe.Offsetof(a.y) != 4 || unsafe.Offsetof(a.cx) != 8 || unsafe.Offsetof(a.cy) != 12 {
		t.Errorf("MFVideoArea size %d, offsets %d %d %d", unsafe.Sizeof(a), unsafe.Offsetof(a.y), unsafe.Offsetof(a.cx), unsafe.Offsetof(a.cy))
	}
}

func TestMFErrorsKeepTheirCause(t *testing.T) {
	err := mfFail("start camera", testStartStreaming)
	if h, ok := mfCode(err); !ok || h != testStartStreaming {
		t.Errorf("HRESULT lost in %v", err)
	}
	if errors.Is(err, ErrPermission) {
		t.Error("an ordinary failure is a permission error")
	}
	perm := mfFail("open camera", hrAccessDenied)
	if !errors.Is(perm, ErrPermission) {
		t.Errorf("%v", perm)
	}
	if h, ok := mfCode(perm); !ok || h != hrAccessDenied {
		t.Errorf("HRESULT lost in %v", perm)
	}
	if got := mfFail("x", perm); got != perm {
		t.Error("a permission error is wrapped twice")
	}
	if s := mfHRESULT(0x80070005).Error(); s != "HRESULT 0x80070005" {
		t.Error(s)
	}
}
