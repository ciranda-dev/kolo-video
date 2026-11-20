//go:build darwin

package kolovideo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"github.com/ebitengine/purego"
	"github.com/ebitengine/purego/objc"
)

func tm(value int64, scale int32) avfTime {
	return avfTime{Value: value, Timescale: scale, Flags: avfTimeValid}
}

// fakeAVF is a strict AVFoundation: a call that would raise an Objective-C
// exception records a violation instead.
type fakeAVF struct {
	status      int
	devs        []avfDevice
	openCalls   atomic.Int32
	accessCalls atomic.Int32
	access      func(done func(bool)) error

	mu       sync.Mutex
	sessions []*fakeSession

	// Behaviour of the next sessions.
	inputCode       int64
	inputFails      bool
	cannotAddInput  bool
	cannotAddOutput bool
	lockFails       bool
	notRunning      bool
	outputFormats   []uint32
	clock           uintptr
	activeDuration  avfTime
}

func (f *fakeAVF) authorizationStatus() int { return f.status }
func (f *fakeAVF) devices() ([]avfDevice, error) {
	return f.devs, nil
}

func (f *fakeAVF) requestAccess(done func(bool)) error {
	f.accessCalls.Add(1)
	if f.access != nil {
		return f.access(done)
	}
	return nil
}

func (f *fakeAVF) openDevice(uid string) (avfSession, error) {
	f.openCalls.Add(1)
	for _, d := range f.devs {
		if d.ID == uid {
			s := &fakeSession{f: f, dev: d}
			f.mu.Lock()
			f.sessions = append(f.sessions, s)
			f.mu.Unlock()
			return s, nil
		}
	}
	return nil, nil
}

func (f *fakeAVF) last() *fakeSession {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sessions[len(f.sessions)-1]
}

type fakeSession struct {
	f   *fakeAVF
	dev avfDevice

	mu          sync.Mutex
	log         []string
	violations  []string
	depth       int
	locked      bool
	inputOK     bool
	outputOK    bool
	inputAdded  bool
	outputAdded bool
	active      int
	activeSet   bool
	minDur      avfTime
	pixelFormat uint32
	ident       uintptr
	delegate    bool
	lost        func(kind int, code int64)
	started     bool
	stopped     bool
	released    bool
}

func (s *fakeSession) rec(call string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.released {
		s.violations = append(s.violations, call+" after release")
	}
	s.log = append(s.log, call)
}

func (s *fakeSession) violate(format string, args ...any) {
	s.mu.Lock()
	s.violations = append(s.violations, fmt.Sprintf(format, args...))
	s.mu.Unlock()
}

func (s *fakeSession) formats() []avfFormat { s.rec("formats"); return s.dev.Formats }

func (s *fakeSession) newInput() (int64, bool) {
	s.rec("newInput")
	if s.f.inputFails {
		return s.f.inputCode, false
	}
	return 0, true
}

func (s *fakeSession) beginConfiguration() { s.rec("begin"); s.depth++ }

func (s *fakeSession) commitConfiguration() {
	s.rec("commit")
	if s.depth == 0 {
		s.violate("commitConfiguration without beginConfiguration")
		return
	}
	s.depth--
}

func (s *fakeSession) canAddInput() bool {
	s.rec("canAddInput")
	s.inputOK = !s.f.cannotAddInput
	return s.inputOK
}

func (s *fakeSession) addInput() {
	s.rec("addInput")
	if !s.inputOK {
		s.violate("addInput without canAddInput")
	}
	s.inputOK, s.inputAdded = false, true
}

func (s *fakeSession) canAddOutput() bool {
	s.rec("canAddOutput")
	s.outputOK = !s.f.cannotAddOutput
	return s.outputOK
}

func (s *fakeSession) addOutput() {
	s.rec("addOutput")
	if !s.outputOK {
		s.violate("addOutput without canAddOutput")
	}
	s.outputOK, s.outputAdded = false, true
}

func (s *fakeSession) setDelegate(ident uintptr) {
	s.rec("setDelegate")
	s.ident, s.delegate = ident, true
}

func (s *fakeSession) pixelFormats() []uint32 {
	s.rec("pixelFormats")
	return s.available()
}

func (s *fakeSession) available() []uint32 {
	if s.f.outputFormats != nil {
		return s.f.outputFormats
	}
	return []uint32{fccBGRA, fccNV12, fccYUY2}
}

func (s *fakeSession) setPixelFormat(c uint32) {
	s.rec("setPixelFormat")
	if !slices.Contains(s.available(), c) {
		s.violate("videoSettings with unavailable format %#x", c)
	}
	s.pixelFormat = c
}

func (s *fakeSession) lockDevice() (int64, bool) {
	s.rec("lock")
	if s.f.lockFails {
		return 7, false
	}
	if s.locked {
		s.violate("lockForConfiguration twice")
	}
	s.locked = true
	return 0, true
}

func (s *fakeSession) unlockDevice() {
	s.rec("unlock")
	if !s.locked {
		s.violate("unlockForConfiguration without the lock")
	}
	s.locked = false
}

func (s *fakeSession) setActiveFormat(i int) {
	s.rec("setActiveFormat")
	if !s.locked {
		s.violate("activeFormat set without lockForConfiguration")
	}
	if i < 0 || i >= len(s.dev.Formats) {
		s.violate("activeFormat %d is not one of the device's %d formats", i, len(s.dev.Formats))
		return
	}
	s.active, s.activeSet = i, true
}

func (s *fakeSession) setMinFrameDuration(d avfTime) {
	s.rec("setMinFrameDuration")
	if !s.locked {
		s.violate("frame duration set without lockForConfiguration")
	}
	if !s.activeSet {
		s.violate("frame duration set before the active format")
		return
	}
	for _, r := range s.dev.Formats[s.active].Ranges {
		if d.cmp(r.MinDuration) >= 0 && d.cmp(r.MaxDuration) <= 0 {
			s.minDur = d
			return
		}
	}
	s.violate("frame duration %d/%d is outside every range of the active format", d.Value, d.Timescale)
}

func (s *fakeSession) activeMinFrameDuration() avfTime {
	s.rec("activeMinFrameDuration")
	if s.f.activeDuration.Flags != 0 {
		return s.f.activeDuration
	}
	return s.minDur
}

func (s *fakeSession) observe(lost func(int, int64)) { s.rec("observe"); s.lost = lost }

func (s *fakeSession) start() {
	s.rec("start")
	if !s.inputAdded || !s.outputAdded {
		s.violate("startRunning without input and output")
	}
	s.started = !s.f.notRunning
}

func (s *fakeSession) running() bool { s.rec("running"); return s.started }

func (s *fakeSession) syncClock() uintptr { s.rec("syncClock"); return s.f.clock }

func (s *fakeSession) stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.stopped {
		s.log = append(s.log, "stop")
	}
	s.stopped, s.delegate, s.started = true, false, false
}

func (s *fakeSession) release() {
	s.stop()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.released {
		s.violations = append(s.violations, "released twice")
		return
	}
	s.released = true
	s.log = append(s.log, "release")
}

// check reports every violation and the state a session must end in.
func (s *fakeSession) check(t *testing.T) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range s.violations {
		t.Errorf("Objective-C exception: %s", v)
	}
	if s.depth != 0 {
		t.Errorf("beginConfiguration and commitConfiguration unbalanced (%d open)", s.depth)
	}
	if s.locked {
		t.Error("device left locked for configuration")
	}
	if !s.released || !s.stopped {
		t.Errorf("session not stopped and released: %v", s.log)
	}
}

func (s *fakeSession) calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.log)
}

func (s *fakeSession) called(name string) bool { return slices.Contains(s.calls(), name) }

func useFakeAVF(t *testing.T, f *fakeAVF) *fakeAVF {
	t.Helper()
	old := avf
	avf = func() (avfAPI, error) { return f, nil }
	t.Cleanup(func() { avf = old })
	return f
}

type recSink struct {
	frames   chan *Frame
	invalids atomic.Int32
	fails    chan error
}

func newRecSink() *recSink {
	return &recSink{frames: make(chan *Frame, 256), fails: make(chan error, 8)}
}

func (r *recSink) deliver(f *Frame) { r.frames <- f }
func (r *recSink) invalid()         { r.invalids.Add(1) }
func (r *recSink) fail(err error)   { r.fails <- err }

func fmtOf(w, h int, fcc uint32, ranges ...avfRange) avfFormat {
	return avfFormat{Width: w, Height: h, FourCC: fcc, Ranges: ranges}
}

// rng is a frame rate range of 1/max to 1/min seconds per frame.
func rng(minFPS, maxFPS float64) avfRange {
	return avfRange{
		MaxRate:     maxFPS,
		MinDuration: tm(1, int32(maxFPS)),
		MaxDuration: tm(1, int32(minFPS)),
	}
}

func camera(formats ...avfFormat) *fakeAVF {
	return &fakeAVF{
		status: avAuthAuthorized,
		devs:   []avfDevice{{ID: "cam", Name: "Cam", Default: true, Formats: formats}},
	}
}

func openFake(t *testing.T, f *fakeAVF, cfg CaptureConfig) (backend, Mode, *recSink, error) {
	t.Helper()
	cfg, err := cfg.withDefaults()
	if err != nil {
		t.Fatal(err)
	}
	devs, err := platformDevicesFrom(f)
	if err != nil {
		t.Fatal(err)
	}
	sink := newRecSink()
	be, mode, err := openSession(f, devs[0], cfg, sink)
	return be, mode, sink, err
}

func platformDevicesFrom(f *fakeAVF) ([]Device, error) {
	old := avf
	avf = func() (avfAPI, error) { return f, nil }
	defer func() { avf = old }()
	return platformDevices()
}

func TestAVFListing(t *testing.T) {
	f := &fakeAVF{devs: []avfDevice{
		{ID: "a", Name: "First", Default: false, Formats: []avfFormat{
			fmtOf(640, 480, fccNV12, rng(1, 15), rng(1, 30)),
			fmtOf(640, 480, fccNV12, rng(1, 30)),
			fmtOf(1280, 720, fccBGRA, rng(1, 60)),
			fmtOf(1280, 720, fccYUY2),
			fmtOf(320, 240, fccUYVY, rng(1, 30)),
			fmtOf(320, 240, fccJPEG, rng(1, 30)),
			fmtOf(320, 240, fccI420Full, rng(1, 30)),
			fmtOf(320, 240, 0x12345678, rng(1, 30)),
		}},
		{ID: "b", Name: "Second", Default: true},
	}}
	useFakeAVF(t, f)
	devs, err := Devices()
	if err != nil {
		t.Fatal(err)
	}
	want := []Device{
		{ID: "a", Name: "First", Modes: []Mode{
			{640, 480, 30, FormatNV12}, {1280, 720, 60, FormatBGRA}, {1280, 720, 0, FormatYUY2},
			{320, 240, 30, FormatUYVY}, {320, 240, 30, FormatMJPEG}, {320, 240, 30, FormatI420},
			{320, 240, 30, FormatUnknown},
		}},
		{ID: "b", Name: "Second", Default: true},
	}
	if len(devs) != len(want) {
		t.Fatalf("got %d devices", len(devs))
	}
	for i := range want {
		if devs[i].ID != want[i].ID || devs[i].Name != want[i].Name || devs[i].Default != want[i].Default ||
			!slices.Equal(devs[i].Modes, want[i].Modes) {
			t.Errorf("device %d: got %+v want %+v", i, devs[i], want[i])
		}
	}
	if f.openCalls.Load() != 0 || f.accessCalls.Load() != 0 {
		t.Error("listing opened a device or asked for access")
	}
}

func TestAVFUnavailable(t *testing.T) {
	old := avf
	avf = func() (avfAPI, error) { return nil, fmt.Errorf("%w: test", ErrUnsupported) }
	defer func() { avf = old }()
	if _, err := Devices(); !errors.Is(err, ErrUnsupported) {
		t.Errorf("Devices: %v", err)
	}
	if err := RequestAccess(context.Background()); !errors.Is(err, ErrUnsupported) {
		t.Errorf("RequestAccess: %v", err)
	}
	if _, _, err := platformOpen(Device{}, CaptureConfig{}, newRecSink()); !errors.Is(err, ErrUnsupported) {
		t.Errorf("platformOpen: %v", err)
	}
}

func TestAVFFormatHelpers(t *testing.T) {
	cases := map[uint32]PixelFormat{
		fccI420: FormatI420, fccI420Full: FormatI420, fccNV12: FormatNV12, fccNV12Full: FormatNV12,
		fccYUY2: FormatYUY2, fccYUY2Full: FormatYUY2, fccUYVY: FormatUYVY, fccBGRA: FormatBGRA,
		fccJPEG: FormatMJPEG, fccJPEGOpenD: FormatMJPEG, 0: FormatUnknown,
	}
	for c, want := range cases {
		if got := avfPixelFormat(c); got != want {
			t.Errorf("%#x: got %v want %v", c, got, want)
		}
	}
	for name, c := range map[string]uint32{"y420": fccI420, "420v": fccNV12, "BGRA": fccBGRA, "2vuy": fccUYVY, "yuvs": fccYUY2} {
		if got := uint32(name[0])<<24 | uint32(name[1])<<16 | uint32(name[2])<<8 | uint32(name[3]); got != c {
			t.Errorf("%s: constant %#x, code %#x", name, c, got)
		}
	}
}

func TestAVFTime(t *testing.T) {
	if !tm(1, 30).numeric() || (avfTime{Value: 1, Timescale: 30}).numeric() || (avfTime{Value: 1, Timescale: 30, Flags: avfTimeValid | 1<<4}).numeric() {
		t.Error("numeric")
	}
	nanos := []struct {
		t    avfTime
		want int64
		ok   bool
	}{
		{tm(1, 30), 33333333, true},
		{tm(5, 30), 166666667, true},
		{tm(0, 30), 0, true},
		{tm(123456789, 1000000000), 123456789, true},
		{tm(1, 2), 500000000, true},
		{tm(1, 3), 333333333, true},
		{tm(2, 3), 666666667, true},
		{tm(-1, 30), 0, false},
		{tm(1, 0), 0, false},
		{avfTime{Value: 5, Timescale: 30}, 0, false},
		{tm(1<<62, 1), 0, false},
		{tm(9223372036, 1), 9223372036000000000, true},
		{tm(9223372037, 1), 0, false},
	}
	for _, c := range nanos {
		got, ok := c.t.nanos()
		if got != c.want || ok != c.ok {
			t.Errorf("%+v: got %d %v want %d %v", c.t, got, ok, c.want, c.ok)
		}
	}
	if tm(1, 30).cmp(tm(2, 60)) != 0 || tm(1, 30).cmp(tm(1, 31)) <= 0 || tm(1, 31).cmp(tm(1, 30)) >= 0 {
		t.Error("cmp")
	}
}

func TestAVFFrameDuration(t *testing.T) {
	r := rng(5, 30)
	cases := []struct {
		fps  float64
		want avfTime
	}{
		{30, r.MinDuration},
		{29.97, tm(33367, 1000000)},
		{60, r.MinDuration},
		{1000, r.MinDuration},
		{15, tm(66667, 1000000)},
		{5, r.MaxDuration},
		{1, r.MaxDuration},
		{0.001, r.MaxDuration},
	}
	for _, c := range cases {
		got := avfFrameDuration(r, c.fps)
		if got.cmp(c.want) != 0 {
			t.Errorf("%v fps: got %d/%d want %d/%d", c.fps, got.Value, got.Timescale, c.want.Value, c.want.Timescale)
		}
		if got.cmp(r.MinDuration) < 0 || got.cmp(r.MaxDuration) > 0 {
			t.Errorf("%v fps: %d/%d outside the range", c.fps, got.Value, got.Timescale)
		}
	}
}

func TestOpenSessionOrder(t *testing.T) {
	f := camera(fmtOf(1280, 720, fccNV12, rng(1, 30)))
	be, mode, _, err := openFake(t, f, CaptureConfig{Width: 1280, Height: 720})
	if err != nil {
		t.Fatal(err)
	}
	s := f.last()
	want := []string{
		"formats", "newInput", "begin", "canAddInput", "addInput", "setDelegate", "canAddOutput", "addOutput",
		"pixelFormats", "setPixelFormat", "lock", "setActiveFormat", "setMinFrameDuration", "commit",
		"observe", "start", "unlock", "running", "syncClock", "activeMinFrameDuration",
	}
	if got := s.calls(); !slices.Equal(got, want) {
		t.Errorf("calls\n got %v\nwant %v", got, want)
	}
	if mode.Width != 1280 || mode.Height != 720 || mode.Format != FormatNV12 || mode.FrameRate != 30 {
		t.Errorf("mode %+v", mode)
	}
	if s.pixelFormat != fccNV12 || s.ident == 0 {
		t.Errorf("pixel format %#x ident %d", s.pixelFormat, s.ident)
	}
	if darwinCallbacks.get(s.ident) == nil {
		t.Error("delegate ident is not registered")
	}
	if err := be.close(); err != nil {
		t.Fatal(err)
	}
	s.check(t)
	if darwinCallbacks.get(s.ident) != nil {
		t.Error("ident still registered after close")
	}
}

func TestPreconditionFormatFromDevice(t *testing.T) {
	f := camera(
		fmtOf(640, 480, fccNV12, rng(1, 30)),
		fmtOf(1280, 720, fccBGRA, rng(1, 60)),
		fmtOf(1280, 720, fccNV12, rng(1, 15), rng(1, 24)),
		fmtOf(1280, 720, fccNV12, rng(1, 30), rng(1, 10)),
		fmtOf(1280, 720, fccYUY2, rng(1, 60)),
	)
	be, mode, _, err := openFake(t, f, CaptureConfig{Width: 1280, Height: 720, FrameRate: 30})
	if err != nil {
		t.Fatal(err)
	}
	s := f.last()
	if s.active != 3 {
		t.Errorf("active format %d, want 3 (the NV12 1280x720 format with the highest rate)", s.active)
	}
	if mode.Format != FormatNV12 || mode.FrameRate != 30 {
		t.Errorf("mode %+v", mode)
	}
	be.close()
	s.check(t)
}

func TestPreconditionDurationClamped(t *testing.T) {
	for _, fps := range []float64{0.5, 1, 7, 15, 24, 29.97, 30, 60, 240} {
		f := camera(fmtOf(640, 480, fccNV12, rng(5, 30)))
		be, _, _, err := openFake(t, f, CaptureConfig{Width: 640, Height: 480, FrameRate: fps})
		if err != nil {
			t.Fatalf("%v fps: %v", fps, err)
		}
		be.close()
		f.last().check(t)
	}
}

func TestPreconditionDurationChosenRange(t *testing.T) {
	f := camera(fmtOf(640, 480, fccNV12, rng(15, 30), rng(1, 60)))
	f.devs[0].Formats[0].Ranges = []avfRange{rng(1, 30), rng(24, 60)}
	be, mode, _, err := openFake(t, f, CaptureConfig{Width: 640, Height: 480, FrameRate: 60})
	if err != nil {
		t.Fatal(err)
	}
	if got := f.last().minDur; got.cmp(tm(1, 60)) != 0 {
		t.Errorf("duration %d/%d, want 1/60", got.Value, got.Timescale)
	}
	if mode.FrameRate != 60 {
		t.Errorf("rate %v", mode.FrameRate)
	}
	be.close()
	f.last().check(t)
}

func TestPreconditionCannotAddInput(t *testing.T) {
	f := camera(fmtOf(640, 480, fccNV12, rng(1, 30)))
	f.cannotAddInput = true
	_, _, _, err := openFake(t, f, CaptureConfig{})
	if err == nil || !strings.HasPrefix(err.Error(), "kolovideo: start capture") {
		t.Fatalf("err %v", err)
	}
	s := f.last()
	if s.called("addInput") || s.called("addOutput") || s.called("start") || s.called("setActiveFormat") {
		t.Errorf("configured past a refused input: %v", s.calls())
	}
	s.check(t)
}

func TestPreconditionCannotAddOutput(t *testing.T) {
	f := camera(fmtOf(640, 480, fccNV12, rng(1, 30)))
	f.cannotAddOutput = true
	_, _, _, err := openFake(t, f, CaptureConfig{})
	if err == nil || !strings.HasPrefix(err.Error(), "kolovideo: start capture") {
		t.Fatalf("err %v", err)
	}
	s := f.last()
	if s.called("addOutput") || s.called("start") || s.called("lock") {
		t.Errorf("configured past a refused output: %v", s.calls())
	}
	s.check(t)
}

func TestPreconditionLockFails(t *testing.T) {
	f := camera(fmtOf(640, 480, fccNV12, rng(1, 30)))
	f.lockFails = true
	_, _, _, err := openFake(t, f, CaptureConfig{})
	if err == nil || !strings.Contains(err.Error(), "error 7") {
		t.Fatalf("err %v", err)
	}
	s := f.last()
	if s.called("setActiveFormat") || s.called("setMinFrameDuration") || s.called("unlock") || s.called("start") {
		t.Errorf("set the format without the lock: %v", s.calls())
	}
	s.check(t)
}

func TestPreconditionOutputFormats(t *testing.T) {
	cases := []struct {
		name      string
		available []uint32
		want      uint32
		fail      bool
	}{
		{"both", []uint32{fccBGRA, fccNV12}, fccNV12, false},
		{"bgra only", []uint32{fccYUY2, fccBGRA}, fccBGRA, false},
		{"neither", []uint32{fccYUY2, fccUYVY}, 0, true},
		{"empty", []uint32{}, 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := camera(fmtOf(640, 480, fccNV12, rng(1, 30)))
			f.outputFormats = c.available
			be, _, _, err := openFake(t, f, CaptureConfig{})
			s := f.last()
			if c.fail {
				if !errors.Is(err, ErrUnsupportedFormat) {
					t.Fatalf("err %v", err)
				}
				if s.called("lock") || s.called("setPixelFormat") {
					t.Errorf("continued without an output format: %v", s.calls())
				}
				s.check(t)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if s.pixelFormat != c.want {
				t.Errorf("pixel format %#x, want %#x", s.pixelFormat, c.want)
			}
			be.close()
			s.check(t)
		})
	}
}

func TestPreconditionBalancedOnEveryFailure(t *testing.T) {
	for _, mutate := range []func(*fakeAVF){
		func(f *fakeAVF) { f.cannotAddInput = true },
		func(f *fakeAVF) { f.cannotAddOutput = true },
		func(f *fakeAVF) { f.lockFails = true },
		func(f *fakeAVF) { f.outputFormats = []uint32{} },
		func(f *fakeAVF) { f.notRunning = true },
		func(f *fakeAVF) { f.inputFails = true },
	} {
		f := camera(fmtOf(640, 480, fccNV12, rng(1, 30)))
		mutate(f)
		if _, _, _, err := openFake(t, f, CaptureConfig{}); err == nil {
			t.Fatal("open succeeded")
		}
		f.last().check(t)
		if n := darwinCallbacks.size(); n != 0 {
			t.Errorf("%d targets left registered", n)
		}
	}
}

func TestOpenSessionNotRunning(t *testing.T) {
	f := camera(fmtOf(640, 480, fccNV12, rng(1, 30)))
	f.notRunning = true
	_, _, _, err := openFake(t, f, CaptureConfig{})
	if err == nil {
		t.Fatal("no error")
	}
	s := f.last()
	if !s.called("unlock") {
		t.Error("device not unlocked")
	}
	s.check(t)
}

func TestOpenSessionInputErrors(t *testing.T) {
	f := camera(fmtOf(640, 480, fccNV12, rng(1, 30)))
	f.inputFails, f.inputCode = true, avfNotAuthorized
	_, _, _, err := openFake(t, f, CaptureConfig{})
	if !errors.Is(err, ErrPermission) {
		t.Fatalf("not authorized: %v", err)
	}
	f.last().check(t)

	f = camera(fmtOf(640, 480, fccNV12, rng(1, 30)))
	f.inputFails, f.inputCode = true, -11814
	_, _, _, err = openFake(t, f, CaptureConfig{})
	if err == nil || errors.Is(err, ErrPermission) || !strings.Contains(err.Error(), "-11814") {
		t.Fatalf("other: %v", err)
	}
	if f.last().called("begin") {
		t.Error("configured a session without an input")
	}
	f.last().check(t)
}

func TestOpenSessionAuthorization(t *testing.T) {
	for _, status := range []int{avAuthNotDetermined, avAuthRestricted, avAuthDenied, 42} {
		f := camera(fmtOf(640, 480, fccNV12, rng(1, 30)))
		f.status = status
		_, _, _, err := openFake(t, f, CaptureConfig{})
		if !errors.Is(err, ErrPermission) {
			t.Errorf("status %d: %v", status, err)
		}
		if f.openCalls.Load() != 0 || f.accessCalls.Load() != 0 {
			t.Errorf("status %d: opened a device or asked for access", status)
		}
	}
}

func TestOpenSessionErrors(t *testing.T) {
	f := camera(fmtOf(640, 480, fccNV12, rng(1, 30)))
	sink := newRecSink()
	if _, _, err := openSession(f, Device{ID: "gone", Modes: []Mode{{640, 480, 30, FormatNV12}}}, mustDefaults(t, CaptureConfig{}), sink); !errors.Is(err, ErrNoDevice) {
		t.Errorf("unknown device: %v", err)
	}
	if _, _, err := openSession(f, Device{ID: "cam"}, mustDefaults(t, CaptureConfig{}), sink); !errors.Is(err, ErrUnsupportedFormat) {
		t.Errorf("no modes: %v", err)
	}
	// The listing offered a mode the device no longer has.
	dev := Device{ID: "cam", Modes: []Mode{{1280, 720, 30, FormatNV12}}}
	if _, _, err := openSession(f, dev, mustDefaults(t, CaptureConfig{Width: 1280, Height: 720}), sink); !errors.Is(err, ErrUnsupportedFormat) {
		t.Errorf("mode missing from the formats: %v", err)
	}
	s := f.last()
	if s.called("newInput") || s.called("begin") {
		t.Errorf("configured a session for a missing format: %v", s.calls())
	}
	s.check(t)
}

func mustDefaults(t *testing.T, c CaptureConfig) CaptureConfig {
	t.Helper()
	c, err := c.withDefaults()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestOpenSessionActiveRate(t *testing.T) {
	f := camera(fmtOf(640, 480, fccNV12, rng(1, 30)))
	f.activeDuration = tm(1, 24)
	be, mode, _, err := openFake(t, f, CaptureConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if mode.FrameRate != 24 {
		t.Errorf("rate %v, want the camera's 24", mode.FrameRate)
	}
	be.close()

	f = camera(fmtOf(640, 480, fccNV12, rng(1, 30)))
	f.activeDuration = avfTime{Flags: 0}
	f.activeDuration.Flags = 1 << 4 // indefinite
	be, mode, _, err = openFake(t, f, CaptureConfig{FrameRate: 25})
	if err != nil {
		t.Fatal(err)
	}
	if mode.FrameRate != 25 {
		t.Errorf("rate %v, want the requested 25", mode.FrameRate)
	}
	be.close()
}

func TestSessionNotifications(t *testing.T) {
	cases := []struct {
		kind    int
		code    int64
		want    error
		message string
	}{
		{avfLostDisconnect, 0, ErrDeviceLost, "disconnected"},
		{avfLostAccess, avfNotAuthorized, ErrPermission, "revoked"},
		{avfLostSession, -11800, ErrDeviceLost, "-11800"},
	}
	for _, c := range cases {
		f := camera(fmtOf(640, 480, fccNV12, rng(1, 30)))
		be, _, sink, err := openFake(t, f, CaptureConfig{})
		if err != nil {
			t.Fatal(err)
		}
		f.last().lost(c.kind, c.code)
		select {
		case err := <-sink.fails:
			if !errors.Is(err, c.want) || !strings.Contains(err.Error(), c.message) {
				t.Errorf("kind %d: %v", c.kind, err)
			}
		default:
			t.Errorf("kind %d: no failure reported", c.kind)
		}
		be.close()
	}
}

func TestBackendCloseOnce(t *testing.T) {
	f := camera(fmtOf(640, 480, fccNV12, rng(1, 30)))
	be, _, _, err := openFake(t, f, CaptureConfig{})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); be.close() }()
	}
	wg.Wait()
	f.last().check(t)
	n := 0
	for _, c := range f.last().calls() {
		if c == "release" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("released %d times", n)
	}
}

func TestRequestAccess(t *testing.T) {
	for _, status := range []int{avAuthAuthorized, avAuthRestricted, avAuthDenied} {
		f := useFakeAVF(t, &fakeAVF{status: status})
		err := RequestAccess(context.Background())
		if (status == avAuthAuthorized) != (err == nil) || (err != nil && !errors.Is(err, ErrPermission)) {
			t.Errorf("status %d: %v", status, err)
		}
		if f.accessCalls.Load() != 0 {
			t.Errorf("status %d: prompted", status)
		}
	}

	f := useFakeAVF(t, &fakeAVF{status: avAuthNotDetermined})
	f.access = func(done func(bool)) error { go done(true); return nil }
	if err := RequestAccess(context.Background()); err != nil {
		t.Errorf("granted: %v", err)
	}
	f.access = func(done func(bool)) error { go func() { done(false); done(true) }(); return nil }
	if err := RequestAccess(context.Background()); !errors.Is(err, ErrPermission) {
		t.Errorf("denied: %v", err)
	}
	f.access = func(done func(bool)) error { return errors.New("boom") }
	if err := RequestAccess(context.Background()); err == nil || err.Error() != "boom" {
		t.Errorf("error: %v", err)
	}
	f.access = func(done func(bool)) error { return nil }
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := RequestAccess(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("timeout: %v", err)
	}
}

func TestOpenCaptureThroughFake(t *testing.T) {
	f := useFakeAVF(t, camera(fmtOf(640, 480, fccNV12, rng(1, 30)), fmtOf(1280, 720, fccBGRA, rng(1, 30))))
	c, err := OpenCapture(CaptureConfig{Width: 1280, Height: 720})
	if err != nil {
		t.Fatal(err)
	}
	if m := c.Mode(); m.Width != 1280 || m.Format != FormatBGRA {
		t.Errorf("mode %+v", m)
	}
	s := f.last()
	// A frame converted by the real conversion reaches Read.
	tg := darwinCallbacks.get(s.ident)
	if tg == nil {
		t.Fatal("no target")
	}
	fr, _ := NewFrame(16, 16)
	tg.sink.deliver(fr)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := c.Read(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	s.check(t)
}

// Real AVFoundation, no camera: the delegate method, the class, the blocks
// and the session objects are exercised without opening a device.

func realAVF(t *testing.T) *avfReal {
	t.Helper()
	api, err := loadAVF()
	if err != nil {
		t.Skip(err)
	}
	return api
}

var (
	synthOnce        sync.Once
	cvCreate         func(alloc, w, h uintptr, format uint32, attrs uintptr, out *uintptr) int32
	cmFormatForImage func(alloc, pb uintptr, out *uintptr) int32
	cmSampleForImage func(alloc, pb, desc uintptr, timing *synthTiming, out *uintptr) int32
	cvSetAttachment  func(buf, key, value uintptr, mode uint32)
)

type synthTiming struct{ Duration, PTS, DTS avfTime }

func bindSynth(t *testing.T) {
	t.Helper()
	realAVF(t)
	synthOnce.Do(func() {
		cv, err := purego.Dlopen("/System/Library/Frameworks/CoreVideo.framework/CoreVideo", purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil {
			t.Fatal(err)
		}
		cm, err := purego.Dlopen("/System/Library/Frameworks/CoreMedia.framework/CoreMedia", purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil {
			t.Fatal(err)
		}
		purego.RegisterLibFunc(&cvCreate, cv, "CVPixelBufferCreate")
		purego.RegisterLibFunc(&cvSetAttachment, cv, "CVBufferSetAttachment")
		purego.RegisterLibFunc(&cmFormatForImage, cm, "CMVideoFormatDescriptionCreateForImageBuffer")
		purego.RegisterLibFunc(&cmSampleForImage, cm, "CMSampleBufferCreateReadyWithImageBuffer")
	})
}

// synthSample is a CMSampleBuffer around a pixel buffer filled from planes.
type synthSample struct {
	pb, desc, sb uintptr
	planes       [][]byte
	w, h         int
}

// newSynth creates a w by h pixel buffer. NV12 gets a Y plane and a UV
// plane, BGRA one plane; each plane is tightly packed in planes and written
// row by row into the buffer's padded rows.
func newSynth(t *testing.T, w, h int, format uint32, planes [][]byte, pts avfTime, bt709 bool) *synthSample {
	t.Helper()
	bindSynth(t)
	s := &synthSample{planes: planes, w: w, h: h}
	if r := cvCreate(0, uintptr(w), uintptr(h), format, 0, &s.pb); r != 0 {
		t.Fatalf("CVPixelBufferCreate: %d", r)
	}
	if cvLock(s.pb, 0) != 0 {
		t.Fatal("lock")
	}
	for i, tight := range planes {
		var base unsafe.Pointer
		var stride, rows int
		if len(planes) == 1 {
			base, stride, rows = cvBase(s.pb), int(cvStride(s.pb)), h
		} else {
			base, stride, rows = cvPlaneBase(s.pb, uintptr(i)), int(cvPlaneStride(s.pb, uintptr(i))), int(cvPlaneHeight(s.pb, uintptr(i)))
		}
		row := len(tight) / rows
		dst := unsafe.Slice((*byte)(base), stride*rows)
		for r := range rows {
			copy(dst[r*stride:r*stride+row], tight[r*row:(r+1)*row])
		}
	}
	cvUnlock(s.pb, 0)
	if bt709 {
		cvSetAttachment(s.pb, cvMatrixKey, cvMatrix709, 1)
	}
	if r := cmFormatForImage(0, s.pb, &s.desc); r != 0 {
		t.Fatalf("format description: %d", r)
	}
	timing := synthTiming{Duration: tm(1, 30), PTS: pts}
	if r := cmSampleForImage(0, s.pb, s.desc, &timing, &s.sb); r != 0 {
		t.Fatalf("sample buffer: %d", r)
	}
	t.Cleanup(func() { cfRelease(s.sb); cfRelease(s.desc); cfRelease(s.pb) })
	return s
}

func pattern(n, seed int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + seed*13 + i/251)
	}
	return b
}

func nv12Planes(w, h, seed int) [][]byte {
	return [][]byte{pattern(w*h, seed), pattern(w*(h/2), seed+1)}
}

func nv12Frame(t *testing.T, w, h int, planes [][]byte, bt709 bool) *Frame {
	t.Helper()
	f, err := FromNV12(w, h, planes[0], w, planes[1], w)
	if err != nil {
		t.Fatal(err)
	}
	if bt709 {
		f.rec709to601()
	}
	return f
}

func sameFrame(t *testing.T, got, want *Frame) {
	t.Helper()
	if got.Width != want.Width || got.Height != want.Height ||
		!slices.Equal(got.Y, want.Y) || !slices.Equal(got.U, want.U) || !slices.Equal(got.V, want.V) {
		t.Errorf("frame differs (%dx%d against %dx%d)", got.Width, got.Height, want.Width, want.Height)
	}
}

// newDelegate creates a delegate instance of the real class for ident.
func newDelegate(t *testing.T, ident uintptr) objc.ID {
	t.Helper()
	realAVF(t)
	var d objc.ID
	avfPool(func() {
		d = objc.ID(avDelegateClass).Send(sel("new"))
		d.SetIvar(avDelegateIdent, objc.ID(ident))
	})
	return d
}

// deliver sends sample buffer sb to delegate the way the capture queue
// does: from a thread of an NSOperationQueue, through NSInvocation to the
// method implementation, and waits until it returned.
func deliver(delegate objc.ID, sb ...uintptr) {
	avfPool(func() {
		sig := delegate.Send(sel("methodSignatureForSelector:"), avSampleBufferHandler)
		q := avfClass("NSOperationQueue").Send(sel("new"))
		q.Send(sel("setMaxConcurrentOperationCount:"), int64(1))
		args := make([]*uintptr, len(sb))
		for i := range sb {
			v := sb[i]
			args[i] = &v
			inv := avfClass("NSInvocation").Send(sel("invocationWithMethodSignature:"), sig)
			inv.Send(sel("setTarget:"), delegate)
			inv.Send(sel("setSelector:"), avSampleBufferHandler)
			var nilID objc.ID
			inv.Send(sel("setArgument:atIndex:"), unsafe.Pointer(&nilID), int64(2))
			inv.Send(sel("setArgument:atIndex:"), unsafe.Pointer(args[i]), int64(3))
			inv.Send(sel("setArgument:atIndex:"), unsafe.Pointer(&nilID), int64(4))
			op := avfClass("NSInvocationOperation").Send(sel("alloc")).Send(sel("initWithInvocation:"), inv)
			q.Send(sel("addOperation:"), op)
			op.Send(sel("release"))
		}
		q.Send(sel("waitUntilAllOperationsAreFinished"))
		q.Send(sel("release"))
		runtime.KeepAlive(args)
	})
}

func newTarget(sink frameSink, maxPixels int) (*darwinTarget, uintptr) {
	tg := &darwinTarget{sink: sink, maxPixels: maxPixels}
	tg.dur.Store(int64(frameDuration(30)))
	return tg, darwinCallbacks.add(tg)
}

func TestDelegateClass(t *testing.T) {
	realAVF(t)
	again, err := loadAVF()
	if err != nil || again != avfInst {
		t.Fatalf("second load: %v", err)
	}
	proto := objc.GetProtocol("AVCaptureVideoDataOutputSampleBufferDelegate")
	d := newDelegate(t, 1)
	var ok, responds bool
	avfPool(func() {
		ok = proto != nil && objc.Send[bool](d, sel("conformsToProtocol:"), proto)
		responds = objc.Send[bool](d, sel("respondsToSelector:"), avSampleBufferHandler)
	})
	if !ok || !responds {
		t.Errorf("conforms %v responds %v", ok, responds)
	}
	if got := uintptr(d.GetIvar(avDelegateIdent)); got != 1 {
		t.Errorf("ident ivar %d", got)
	}
	avfPool(func() { d.Send(sel("release")) })
}

func TestDelegateNV12(t *testing.T) {
	const w, h = 64, 48
	sink := newRecSink()
	_, ident := newTarget(sink, DefaultMaxPixels)
	defer darwinCallbacks.remove(ident)
	d := newDelegate(t, ident)

	planes := nv12Planes(w, h, 1)
	planes709 := nv12Planes(w, h, 2)
	a := newSynth(t, w, h, fccNV12, planes, tm(5, 30), false)
	b := newSynth(t, w, h, fccNV12, planes709, tm(0, 30), true)
	deliver(d, a.sb, b.sb)

	got := <-sink.frames
	sameFrame(t, got, nv12Frame(t, w, h, planes, false))
	if got.Timestamp != 166666667*time.Nanosecond || got.Duration != frameDuration(30) {
		t.Errorf("timestamp %v duration %v", got.Timestamp, got.Duration)
	}
	got = <-sink.frames
	sameFrame(t, got, nv12Frame(t, w, h, planes709, true))
	if got.Timestamp != 0 {
		t.Errorf("timestamp %v", got.Timestamp)
	}
	if err := got.Validate(); err != nil {
		t.Error(err)
	}
	if n := sink.invalids.Load(); n != 0 {
		t.Errorf("%d invalid", n)
	}
}

func TestDelegateBGRA(t *testing.T) {
	const w, h = 40, 30
	sink := newRecSink()
	_, ident := newTarget(sink, DefaultMaxPixels)
	defer darwinCallbacks.remove(ident)
	d := newDelegate(t, ident)
	pix := pattern(w*h*4, 3)
	s := newSynth(t, w, h, fccBGRA, [][]byte{pix}, tm(1, 30), false)
	deliver(d, s.sb)
	want, err := FromBGRA(w, h, pix, w*4)
	if err != nil {
		t.Fatal(err)
	}
	sameFrame(t, <-sink.frames, want)
}

func TestDelegateTimestamps(t *testing.T) {
	const w, h = 16, 16
	sink := newRecSink()
	tg, ident := newTarget(sink, DefaultMaxPixels)
	defer darwinCallbacks.remove(ident)
	d := newDelegate(t, ident)
	planes := nv12Planes(w, h, 4)

	// The session clock is converted to the host clock; converting the host
	// clock to itself leaves the time unchanged.
	tg.clock.Store(cmHostClock())
	s := newSynth(t, w, h, fccNV12, planes, tm(123456789, 1000000000), false)
	deliver(d, s.sb)
	if got := <-sink.frames; got.Timestamp != 123456789 {
		t.Errorf("converted timestamp %v", got.Timestamp)
	}
	tg.clock.Store(0)

	// A buffer without a numeric time takes the host time.
	s = newSynth(t, w, h, fccNV12, planes, avfTime{Value: 7, Timescale: 30}, false)
	before := uptimeNanos(clockUptimeRaw)
	deliver(d, s.sb)
	after := uptimeNanos(clockUptimeRaw)
	if got := <-sink.frames; got.Timestamp < time.Duration(before) || got.Timestamp > time.Duration(after) {
		t.Errorf("fallback timestamp %v outside [%d, %d]", got.Timestamp, before, after)
	}
}

func TestDelegateInvalid(t *testing.T) {
	sink := newRecSink()
	_, ident := newTarget(sink, 100*100)
	defer darwinCallbacks.remove(ident)
	d := newDelegate(t, ident)

	big := newSynth(t, 128, 128, fccNV12, nv12Planes(128, 128, 5), tm(0, 30), false)
	other := newSynth(t, 16, 16, 0x32767579 /* 2vuy */, [][]byte{pattern(16*16*2, 6)}, tm(0, 30), false)
	deliver(d, big.sb, other.sb, 0)
	if n := sink.invalids.Load(); n != 3 {
		t.Errorf("%d invalid, want 3 (too large, unsupported format, nil sample)", n)
	}
	if len(sink.frames) != 0 {
		t.Error("a frame was delivered")
	}

	tg := darwinCallbacks.get(ident)
	avfPool(func() { tg.image(uintptr(avfNSString("not a pixel buffer")), 0) })
	tg.image(0, 0)
	if n := sink.invalids.Load(); n != 5 {
		t.Errorf("%d invalid, want 5", n)
	}
}

func TestDelegateDispatchByIdent(t *testing.T) {
	const w, h = 16, 16
	sa, sb := newRecSink(), newRecSink()
	_, ia := newTarget(sa, DefaultMaxPixels)
	_, ib := newTarget(sb, DefaultMaxPixels)
	da, db := newDelegate(t, ia), newDelegate(t, ib)
	pa, pb := nv12Planes(w, h, 7), nv12Planes(w, h, 8)
	a := newSynth(t, w, h, fccNV12, pa, tm(0, 30), false)
	b := newSynth(t, w, h, fccNV12, pb, tm(0, 30), false)
	deliver(da, a.sb)
	deliver(db, b.sb)
	sameFrame(t, <-sa.frames, nv12Frame(t, w, h, pa, false))
	sameFrame(t, <-sb.frames, nv12Frame(t, w, h, pb, false))

	// A callback after remove finds nothing and is ignored.
	darwinCallbacks.remove(ia)
	deliver(da, a.sb)
	if len(sa.frames) != 0 || sa.invalids.Load() != 0 {
		t.Error("a removed capture received a frame")
	}
	darwinCallbacks.remove(ib)
}

func TestDelegateConcurrentThreads(t *testing.T) {
	const w, h = 32, 24
	sink := newRecSink()
	_, ident := newTarget(sink, DefaultMaxPixels)
	defer darwinCallbacks.remove(ident)
	d := newDelegate(t, ident)
	planes := nv12Planes(w, h, 9)
	s := newSynth(t, w, h, fccNV12, planes, tm(0, 30), false)
	want := nv12Frame(t, w, h, planes, false)
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			deliver(d, s.sb, s.sb, s.sb, s.sb, s.sb)
		}()
	}
	wg.Wait()
	if len(sink.frames) != 20 {
		t.Fatalf("%d frames", len(sink.frames))
	}
	for range 20 {
		sameFrame(t, <-sink.frames, want)
	}
}

// TestAVFStructABI passes and returns a CMTime by value through the
// Objective-C runtime and through a C function.
func TestAVFStructABI(t *testing.T) {
	realAVF(t)
	want := avfTime{Value: 1001, Timescale: 30000, Flags: avfTimeValid, Epoch: 3}
	var got avfTime
	avfPool(func() {
		v := avfClass("NSValue").Send(sel("valueWithCMTime:"), want)
		got = objc.Send[avfTime](v, sel("CMTimeValue"))
	})
	if got != want {
		t.Errorf("NSValue round trip: got %+v want %+v", got, want)
	}
	if h := cmSyncConvert(tm(42, 1000), cmHostClock(), cmHostClock()); h.cmp(tm(42, 1000)) != 0 {
		t.Errorf("CMSyncConvertTime: %+v", h)
	}
}

func TestAVFSyncCallback(t *testing.T) {
	realAVF(t)
	label := avfCString("kolovideo.test")
	q := dispatchQueueCreate(&label[0], 0)
	var ran atomic.Bool
	cb := purego.NewCallback(func(uintptr) { ran.Store(true) })
	dispatchSyncF(q, 0, cb)
	if !ran.Load() {
		t.Error("dispatch_sync_f returned before the function ran")
	}
	dispatchSyncF(q, 0, avSyncDone)
	dispatchRelease(q)
}

// TestRealSessionObjects drives the real session type on objects that need
// no camera: a session and an output made by makeObjects, and a plain object
// in place of the device.
func TestRealSessionObjects(t *testing.T) {
	realAVF(t)
	s := &avfRealSession{}
	avfPool(func() {
		s.dev = avfRetain(avfClass("NSObject").Send(sel("new")))
		s.makeObjects()
	})
	defer s.release()

	if !s.canAddOutput() {
		t.Fatal("canAddOutput")
	}
	s.beginConfiguration()
	s.addOutput()
	formats := s.pixelFormats()
	if pf, ok := outputFormat(formats); !ok || pf != fccNV12 && pf != fccBGRA {
		t.Errorf("no usable output format in %#x", formats)
	}
	for _, want := range []uint32{fccNV12, fccBGRA} {
		if slices.Contains(formats, want) {
			s.setPixelFormat(want)
		}
	}
	sink := newRecSink()
	tg, ident := newTarget(sink, DefaultMaxPixels)
	defer darwinCallbacks.remove(ident)
	s.setDelegate(ident)
	s.commitConfiguration()
	if s.running() {
		t.Error("running without start")
	}
	s.syncClock()

	// The delegate set on the output is the real class instance with the
	// right ident.
	var set objc.ID
	avfPool(func() { set = s.output.Send(sel("sampleBufferDelegate")) })
	if set != s.delegate || uintptr(set.GetIvar(avDelegateIdent)) != ident {
		t.Errorf("delegate %v ident %d", set, set.GetIvar(avDelegateIdent))
	}

	s.observe(tg.lost)
	post := func(name objc.ID, object objc.ID, info objc.ID) {
		avfPool(func() {
			nc := avfClass("NSNotificationCenter").Send(sel("defaultCenter"))
			nc.Send(sel("postNotificationName:object:userInfo:"), name, object, info)
		})
	}
	errorInfo := func(code int64) objc.ID {
		var info objc.ID
		avfPool(func() {
			e := avfClass("NSError").Send(sel("errorWithDomain:code:userInfo:"), avfNSString("kolovideo.test"), code, objc.ID(0))
			info = avfRetain(avfClass("NSDictionary").Send(sel("dictionaryWithObject:forKey:"), e, avSessionErrorKey))
		})
		return info
	}
	expect := func(want error, text string) {
		t.Helper()
		select {
		case err := <-sink.fails:
			if !errors.Is(err, want) || !strings.Contains(err.Error(), text) {
				t.Errorf("got %v, want %v containing %q", err, want, text)
			}
		case <-time.After(2 * time.Second):
			t.Errorf("no notification for %q", text)
		}
	}
	post(avDisconnectedNote, s.dev, 0)
	expect(ErrDeviceLost, "disconnected")
	post(avRuntimeErrorNote, s.session, errorInfo(avfNotAuthorized))
	expect(ErrPermission, "revoked")
	post(avRuntimeErrorNote, s.session, errorInfo(-11800))
	expect(ErrDeviceLost, "-11800")
	post(avRuntimeErrorNote, s.session, 0)
	expect(ErrDeviceLost, "error 0")

	// A notification about another object is not ours.
	post(avDisconnectedNote, s.output, 0)
	select {
	case err := <-sink.fails:
		t.Errorf("notification for another object: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	s.stop()
	s.stop()
	post(avDisconnectedNote, s.dev, 0)
	select {
	case err := <-sink.fails:
		t.Errorf("notification after stop: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	avfPool(func() { set = s.output.Send(sel("sampleBufferDelegate")) })
	if set != 0 {
		t.Error("delegate still set after stop")
	}
}

func TestRealAccessBlock(t *testing.T) {
	realAVF(t)
	got := make(chan bool, 1)
	var b objc.Block
	avfPool(func() {
		b = objc.NewBlock(func(_ objc.Block, granted bool) {
			avfPool(func() { got <- granted })
		})
	})
	b.Invoke(true)
	if !<-got {
		t.Error("block did not receive the argument")
	}
	b.Release()
}

// TestRealListing checks the real camera list. With KOLO_VIDEO_DEVICE_GOLDEN set to a JSON file holding
// the Devices() list it also compares the list with it. Keep that file outside the repository: camera
// names and IDs identify the machine.
func TestRealListing(t *testing.T) {
	api := realAVF(t)
	devs, err := api.devices()
	if err != nil {
		t.Fatal(err)
	}
	platform, err := platformDevices()
	if err != nil {
		t.Fatal(err)
	}
	if len(platform) != len(devs) {
		t.Fatalf("%d against %d devices", len(platform), len(devs))
	}
	defaults := 0
	for i, d := range devs {
		if d.ID == "" || d.Name == "" {
			t.Errorf("device %+v has no id or name", d)
		}
		if d.Default {
			defaults++
		}
		if platform[i].ID != d.ID || !slices.Equal(platform[i].Modes, avfModes(d.Formats)) {
			t.Errorf("device %q: modes differ", d.ID)
		}
		for _, f := range d.Formats {
			if f.Width <= 0 || f.Height <= 0 || f.FourCC == 0 {
				t.Errorf("device %q: format %+v", d.ID, f)
			}
		}
	}
	if len(devs) > 0 && defaults > 1 {
		t.Errorf("%d default devices", defaults)
	}
	if s, err := api.openDevice(""); s != nil || err != nil {
		t.Error("empty id opened a device")
	}
	for _, id := range []string{"no such camera id", "\xff\xfe", "a\x00b"} {
		if s, err := api.openDevice(id); s != nil || err != nil {
			t.Errorf("id %q: %v %v", id, s, err)
		}
	}
	if st := api.authorizationStatus(); st < avAuthNotDetermined || st > avAuthAuthorized {
		t.Errorf("status %d", st)
	}
	path := os.Getenv("KOLO_VIDEO_DEVICE_GOLDEN")
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var want []Device
	if err := json.Unmarshal(data, &want); err != nil {
		t.Fatal(err)
	}
	if !slices.EqualFunc(platform, want, func(a, b Device) bool {
		return a.ID == b.ID && a.Name == b.Name && a.Default == b.Default && slices.Equal(a.Modes, b.Modes)
	}) {
		t.Errorf("devices differ from %s: got %d, want %d", path, len(platform), len(want))
	}
}

// rssKB returns the resident size of this process.
func rssKB(t *testing.T) int {
	t.Helper()
	out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(os.Getpid())).Output()
	if err != nil {
		t.Skip("ps unavailable:", err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// TestAVFPoolsKeepMemoryFlat runs Objective-C from Go threads for a long
// time. Every call that autoreleases objects must sit in a pool; without
// pools the objects pile up for the life of the thread.
func TestAVFPoolsKeepMemoryFlat(t *testing.T) {
	api := realAVF(t)
	iterations, limitKB := 40, 4<<10
	if testing.Short() {
		iterations = 5
	}
	if v := os.Getenv("KOLO_VIDEO_LEAK_LISTINGS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			t.Fatalf("KOLO_VIDEO_LEAK_LISTINGS=%q", v)
		}
		iterations = n
	}
	list := func() {
		if _, err := api.devices(); err != nil {
			t.Fatal(err)
		}
		api.authorizationStatus()
	}
	// Plain Objective-C work that autoreleases: strings, arrays, numbers.
	churn := func() {
		for range 2000 {
			avfPool(func() {
				a := avfClass("NSMutableArray").Send(sel("array"))
				for i := range 16 {
					a.Send(sel("addObject:"), avfNSString(strconv.Itoa(i)))
					a.Send(sel("addObject:"), avfClass("NSNumber").Send(sel("numberWithInt:"), int32(i)))
				}
				avfGoString(a.Send(sel("description")))
			})
		}
	}
	for range 3 {
		list()
	}
	churn()
	runtime.GC()
	before := rssKB(t)
	start := time.Now()
	for i := range iterations {
		list()
		if i%10 == 0 {
			churn()
		}
	}
	runtime.GC()
	after := rssKB(t)
	t.Logf("%d listings and %d churn rounds in %v: RSS %d KB before, %d KB after", iterations, (iterations+9)/10, time.Since(start).Round(time.Millisecond), before, after)
	if after-before > limitKB {
		t.Errorf("RSS grew by %d KB", after-before)
	}
}

// TestAVFHardwareCapture opens the built-in camera. It runs only with
// KOLO_VIDEO_HW_TEST=1 and only when the system already granted camera
// access, because opening a camera turns it on. It never opens a phone,
// never prompts and never stores a frame.
func TestAVFHardwareCapture(t *testing.T) {
	if os.Getenv("KOLO_VIDEO_HW_TEST") != "1" {
		t.Skip("set KOLO_VIDEO_HW_TEST=1 to capture from the built-in camera")
	}
	api, err := loadAVF()
	if err != nil {
		t.Skip(err)
	}
	if st := api.authorizationStatus(); st != avAuthAuthorized {
		t.Skipf("camera access is not granted (authorization status %d); this test never asks", st)
	}
	raw, err := api.devices()
	if err != nil {
		t.Fatal(err)
	}
	id := ""
	for _, d := range raw {
		if d.Builtin {
			id = d.ID
		}
	}
	if id == "" {
		t.Skip("no built-in camera")
	}
	const want = 45
	c, err := OpenCapture(CaptureConfig{DeviceID: id, Width: 1280, Height: 720, FrameRate: 30, Buffer: want})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	t.Logf("device %q mode %+v", c.Device().Name, c.Mode())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var last time.Duration
	start := time.Now()
	for i := range want {
		f, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if err := f.Validate(); err != nil {
			t.Fatal(err)
		}
		if f.Width != 1280 || f.Height != 720 {
			t.Fatalf("frame %dx%d", f.Width, f.Height)
		}
		if i > 0 && f.Timestamp <= last {
			t.Fatalf("timestamp %v after %v", f.Timestamp, last)
		}
		last = f.Timestamp
	}
	st := c.Stats()
	t.Logf("%d frames in %v, stats %+v", want, time.Since(start).Round(time.Millisecond), st)
	if st.Dropped != 0 || st.Invalid != 0 {
		t.Errorf("dropped %d invalid %d", st.Dropped, st.Invalid)
	}
	if err := c.Close(); err != nil {
		t.Error(err)
	}
}
