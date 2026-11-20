package kolovideo

import (
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// mfFake is a Media Foundation with no operating system behind it. It logs
// every call that matters for ordering.
type mfFake struct {
	mu  sync.Mutex
	log []string

	coErr, startErr error
	list            []mfDevice
	listErr         error
	srcErr          error
	modes           []Mode
	modesErr        error
	startReadErr    error
	layout          mfLayout
	layoutNext      mfLayout
	layoutErr       error

	// flushNoop makes flush leave a blocked read alone; wedge makes the
	// read ignore flush and shutdown until unwedge is closed.
	flushNoop bool
	wedge     bool
	unwedge   chan struct{}

	live  atomic.Int32
	reads chan mfFakeRead
	// flushed and stopped are closed by the first flush and shutdown.
	flushed   chan struct{}
	flushOnce sync.Once
	flushes   atomic.Int32
	stopped   chan struct{}
	stopOnce  sync.Once
	shutdowns atomic.Int32
	started   atomic.Int32
	src       *mfFakeSource
}

type mfFakeRead struct {
	s   mfSample
	err error
}

func newMFFake() *mfFake {
	return &mfFake{
		list:    []mfDevice{{"id0", "Front"}, {"id1", "USB"}},
		modes:   []Mode{{6, 4, 30, FormatNV12}},
		layout:  mfLayout{w: 6, h: 4, bufw: 6, bufh: 4},
		reads:   make(chan mfFakeRead, 16),
		flushed: make(chan struct{}),
		stopped: make(chan struct{}),
		unwedge: make(chan struct{}),
	}
}

func (f *mfFake) add(s string) {
	f.mu.Lock()
	f.log = append(f.log, s)
	f.mu.Unlock()
}

func (f *mfFake) events() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.log)
}

// waitIdle waits until every apartment entered was left.
func (f *mfFake) waitIdle(t *testing.T) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); f.live.Load() != 0; {
		if time.Now().After(end) {
			t.Fatalf("%d apartments still entered; log %v", f.live.Load(), f.events())
		}
		time.Sleep(time.Millisecond)
	}
}

func (f *mfFake) coInit() (func(), error) {
	if f.coErr != nil {
		return nil, f.coErr
	}
	f.live.Add(1)
	f.add("coInit")
	return func() { f.add("coUninit"); f.live.Add(-1) }, nil
}

func (f *mfFake) startup() (func(), error) {
	if f.startErr != nil {
		return nil, f.startErr
	}
	f.add("startup")
	return func() { f.add("mfShutdown") }, nil
}

func (f *mfFake) devices() ([]mfDevice, error) {
	f.add("devices")
	return f.list, f.listErr
}

func (f *mfFake) source(id string) (mfSource, error) {
	f.add("source " + id)
	if f.srcErr != nil {
		return nil, f.srcErr
	}
	f.src = &mfFakeSource{f: f}
	return f.src, nil
}

type mfFakeSource struct {
	f      *mfFake
	reader *mfFakeReader
}

func (s *mfFakeSource) modes() ([]Mode, error) {
	s.f.add("modes")
	return s.f.modes, s.f.modesErr
}

func (s *mfFakeSource) start(index int) (mfReader, mfLayout, error) {
	s.f.started.Add(1)
	s.f.add("start " + string(rune('0'+index)))
	if s.f.startReadErr != nil {
		return nil, mfLayout{}, s.f.startReadErr
	}
	s.reader = &mfFakeReader{f: s.f}
	return s.reader, s.f.layout, nil
}

func (s *mfFakeSource) shutdown() error {
	s.f.add("source.shutdown")
	s.f.shutdowns.Add(1)
	s.f.stopOnce.Do(func() { close(s.f.stopped) })
	return nil
}

func (s *mfFakeSource) release() { s.f.add("source.release") }

type mfFakeReader struct{ f *mfFake }

// read blocks like ReadSample until a sample is queued, the read is
// flushed (it returns no sample) or the source is shut down (MF_E_SHUTDOWN).
func (r *mfFakeReader) read() (mfSample, error) {
	if r.f.wedge {
		select {
		case x := <-r.f.reads:
			return x.s, x.err
		case <-r.f.unwedge:
			return mfSample{}, testShutdown
		}
	}
	select {
	case x := <-r.f.reads:
		return x.s, x.err
	case <-r.f.flushed:
		return mfSample{}, nil
	case <-r.f.stopped:
		return mfSample{}, testShutdown
	}
}

func (r *mfFakeReader) flush() error {
	r.f.add("reader.flush")
	r.f.flushes.Add(1)
	if !r.f.flushNoop {
		r.f.flushOnce.Do(func() { close(r.f.flushed) })
	}
	return nil
}

func (r *mfFakeReader) layout() (mfLayout, error) {
	r.f.add("layout")
	if r.f.layoutNext != (mfLayout{}) {
		return r.f.layoutNext, r.f.layoutErr
	}
	return r.f.layout, r.f.layoutErr
}

func (r *mfFakeReader) release() { r.f.add("reader.release") }

// mfSink records what a backend reports.
type mfSink struct {
	frames   chan *Frame
	failed   chan error
	invalidN atomic.Int32
	// closed is set when close returned; a call after it is late.
	closed atomic.Bool
	late   atomic.Int32
}

func newMFSink() *mfSink { return &mfSink{frames: make(chan *Frame, 64), failed: make(chan error, 4)} }

func (s *mfSink) check() {
	if s.closed.Load() {
		s.late.Add(1)
	}
}

func (s *mfSink) deliver(f *Frame) {
	s.check()
	select {
	case s.frames <- f:
	default:
	}
}

func (s *mfSink) invalid() { s.check(); s.invalidN.Add(1) }

func (s *mfSink) fail(err error) {
	s.check()
	s.failed <- err
}

// nv12Data builds a luma plane of bufh rows at pitch bytes, then the
// interleaved chroma plane, with 0xEE in the padding.
func nv12Data(pitch, bufh int, w, h int, y, u, v byte) []byte {
	d := make([]byte, pitch*bufh*3/2)
	for i := range d {
		d[i] = 0xEE
	}
	for r := range bufh {
		for x := range pitch {
			if x < w && r < h {
				d[r*pitch+x] = y
			}
		}
	}
	for r := range (h + 1) / 2 {
		for x := range (w + 1) / 2 {
			d[pitch*bufh+r*pitch+2*x] = u
			d[pitch*bufh+r*pitch+2*x+1] = v
		}
	}
	return d
}
