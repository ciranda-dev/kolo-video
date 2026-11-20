package kolovideo

// The Media Foundation capture logic. It has no operating system
// dependency: every call into Windows goes through the mf* interfaces below,
// implemented by mf_windows.go and by a fake in the tests, so this file
// builds and is tested on every platform.

import (
	"errors"
	"fmt"
	"math"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// mfMaxModes bounds the native types read from one camera, so a driver
// reporting a huge count cannot hang a listing.
const mfMaxModes = 1024

// mfModes reads min(count, mfMaxModes) native types in index order.
func mfModes(count uint32, read func(index uint32) Mode) []Mode {
	modes := make([]Mode, min(count, mfMaxModes))
	for i := range modes {
		modes[i] = read(uint32(i))
	}
	return modes
}

// The interfaces below are the Media Foundation objects the backend uses.
// Every method runs on a thread that called coInit and startup, except
// mfReader.flush and mfSource.shutdown, which also run on a thread that only
// called coInit, while the reading thread is inside read.

type mfAPI interface {
	// coInit enters a multithreaded apartment on the calling thread, which
	// must be locked to it. The returned function leaves it.
	coInit() (func(), error)
	// startup starts Media Foundation for the process. The returned
	// function shuts it down.
	startup() (func(), error)
	// devices lists the video capture devices without activating them.
	devices() ([]mfDevice, error)
	// source activates the device with the symbolic link id.
	source(id string) (mfSource, error)
}

type mfDevice struct{ id, name string }

type mfSource interface {
	// modes lists the native types of the first stream in index order; a
	// type that cannot be read is the zero Mode.
	modes() ([]Mode, error)
	// start selects native type index and opens a synchronous reader that
	// converts to NV12.
	start(index int) (mfReader, mfLayout, error)
	// shutdown ends the source and unblocks a read in progress. It may be
	// called more than once.
	shutdown() error
	release()
}

type mfReader interface {
	// read blocks for the next sample. The sample is returned also on
	// error, with its flags and time set.
	read() (mfSample, error)
	// layout returns the current output type.
	layout() (mfLayout, error)
	// flush cancels the read in progress, which then returns.
	flush() error
	release()
}

// mfSample is one result of a read. data is nil when the read produced no
// picture; otherwise it is valid until release.
type mfSample struct {
	flags uint32
	data  []byte
	// pitch is the row stride in bytes, 0 if the buffer has no 2D layout.
	pitch int
	// ts is the host clock when the read returned.
	ts   time.Duration
	done func()
}

func (s mfSample) release() {
	if s.done != nil {
		s.done()
	}
}

// mfLayout is the reader's output picture: the buffer size, the visible
// size inside it, and whether the YCbCr matrix is BT.709.
type mfLayout struct {
	w, h, bufw, bufh int
	bt709            bool
}

// mfOffset and mfVideoArea have the layout of MFOffset and MFVideoArea.
type mfOffset struct {
	_     uint16
	value int16
}

type mfVideoArea struct {
	x, y   mfOffset
	cx, cy int32
}

// mfMode builds a Mode from the attributes of a native type; a nil argument
// is an attribute the type lacks.
func mfMode(size, rate *uint64, sub *mfGUID) Mode {
	var m Mode
	if size != nil {
		m.Width = int(*size >> 32)
		m.Height = int(*size & 0xffffffff)
	}
	if rate != nil && *rate&0xffffffff != 0 {
		m.FrameRate = float64(*rate>>32) / float64(*rate&0xffffffff)
	}
	if sub != nil {
		m.Format = mfPixelFormat(*sub)
	}
	return m
}

// mfLayoutOf builds the layout from the frame size attribute, the display
// aperture if the type has one, and the YCbCr matrix if it has one. The
// aperture is used only when it starts at the origin and lies inside the
// buffer.
func mfLayoutOf(size uint64, area *mfVideoArea, matrix *uint32) mfLayout {
	l := mfLayout{w: int(size >> 32), h: int(size & 0xffffffff)}
	l.bufw, l.bufh = l.w, l.h
	if area != nil && area.x.value == 0 && area.y.value == 0 &&
		area.cx > 0 && area.cy > 0 && int64(area.cx) <= int64(l.w) && int64(area.cy) <= int64(l.h) {
		l.w, l.h = int(area.cx), int(area.cy)
	}
	l.bt709 = matrix != nil && *matrix == mfMatrixBT709
	return l
}

// qpcDuration converts a performance counter value at freq ticks per second
// to a duration without overflow.
func qpcDuration(count, freq int64) time.Duration {
	if freq <= 0 {
		return 0
	}
	return time.Duration(count/freq)*time.Second + time.Duration(count%freq)*time.Second/time.Duration(freq)
}

func mfCode(err error) (mfHRESULT, bool) {
	var h mfHRESULT
	return h, errors.As(err, &h)
}

// mfFail describes a failed call; an access denied result means the camera
// privacy setting.
func mfFail(what string, err error) error {
	if errors.Is(err, ErrUnsupported) || errors.Is(err, ErrPermission) {
		return err
	}
	if h, ok := mfCode(err); ok && h == hrAccessDenied {
		return fmt.Errorf("%w: %s: camera access is off in Windows privacy settings (%w)", ErrPermission, what, err)
	}
	return fmt.Errorf("kolovideo: %s: %w", what, err)
}

// mfThread runs fn on a new OS thread in a multithreaded apartment with
// Media Foundation started, and returns once that is done or failed. The
// thread exits with the goroutine, so no COM state leaks.
func mfThread(api mfAPI, fn func()) error {
	errc := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		uninit, err := api.coInit()
		if err != nil {
			errc <- mfFail("start COM", err)
			return
		}
		defer uninit()
		shutdown, err := api.startup()
		if err != nil {
			errc <- mfFail("start Media Foundation", err)
			return
		}
		defer shutdown()
		errc <- nil
		fn()
	}()
	return <-errc
}

func mfDevices(api mfAPI) ([]Device, error) {
	type result struct {
		devs []Device
		err  error
	}
	res := make(chan result, 1)
	if err := mfThread(api, func() {
		list, err := api.devices()
		if err != nil {
			res <- result{err: mfFail("list cameras", err)}
			return
		}
		var devs []Device
		for i, d := range list {
			devs = append(devs, Device{ID: d.id, Name: d.name, Default: i == 0})
		}
		res <- result{devs: devs}
	}); err != nil {
		return nil, err
	}
	r := <-res
	return r.devs, r.err
}

// mfCloseWait is how long close waits for the reading thread after each
// step: a camera silent for this long counts as lost.
var mfCloseWait = DefaultStallTimeout

// mfGate passes the backend's reports to the sink until it is closed; after
// close returns, the sink is never called again.
type mfGate struct {
	mu     sync.Mutex
	closed bool
	sink   frameSink
}

func (g *mfGate) deliver(f *Frame) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.closed {
		g.sink.deliver(f)
	}
}

func (g *mfGate) invalid() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.closed {
		g.sink.invalid()
	}
}

func (g *mfGate) fail(err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.closed {
		g.sink.fail(err)
	}
}

func (g *mfGate) close() {
	g.mu.Lock()
	g.closed = true
	g.mu.Unlock()
}

type mfBackend struct {
	api      mfAPI
	gate     *mfGate
	done     chan struct{}
	stop     atomic.Bool
	once     sync.Once
	closeErr error

	// live is held shared while another thread calls the reader or the
	// source, and exclusively to release them.
	live     sync.RWMutex
	src      mfSource
	rd       mfReader
	released bool
}

type mfOpened struct {
	mode Mode
	err  error
}

func mfOpen(api mfAPI, dev Device, cfg CaptureConfig, sink frameSink) (backend, Mode, error) {
	b := &mfBackend{api: api, gate: &mfGate{sink: sink}, done: make(chan struct{})}
	ready := make(chan mfOpened, 1)
	if err := mfThread(api, func() { b.run(dev, cfg, ready) }); err != nil {
		return nil, Mode{}, err
	}
	r := <-ready
	if r.err != nil {
		<-b.done
		return nil, Mode{}, r.err
	}
	return b, r.mode, nil
}

// run opens the camera and reads frames until close; it owns the reader.
func (b *mfBackend) run(dev Device, cfg CaptureConfig, ready chan<- mfOpened) {
	defer close(b.done)
	defer func() {
		b.live.Lock()
		if b.rd != nil {
			b.rd.release()
		}
		if b.src != nil {
			b.src.shutdown()
			b.src.release()
		}
		b.released = true
		b.live.Unlock()
	}()
	src, err := b.api.source(dev.ID)
	if err != nil {
		if h, ok := mfCode(err); ok && h == hrAccessDenied {
			ready <- mfOpened{err: mfFail("open camera", err)}
		} else {
			ready <- mfOpened{err: fmt.Errorf("%w: %q: %w", ErrNoDevice, dev.ID, err)}
		}
		return
	}
	b.live.Lock()
	b.src = src
	b.live.Unlock()
	modes, err := src.modes()
	if err != nil {
		ready <- mfOpened{err: mfFail("list camera modes", err)}
		return
	}
	mode, index, err := chooseMode(modes, cfg)
	if err != nil {
		ready <- mfOpened{err: err}
		return
	}
	rd, layout, err := src.start(index)
	if err != nil {
		ready <- mfOpened{err: mfFail("start camera", err)}
		return
	}
	b.live.Lock()
	b.rd = rd
	b.live.Unlock()
	if !fitsPixels(layout.bufw, layout.bufh, cfg.MaxPixels) {
		ready <- mfOpened{err: fmt.Errorf("%w: reader output %dx%d", ErrUnsupportedFormat, layout.bufw, layout.bufh)}
		return
	}
	if modes[index].FrameRate > 0 {
		mode.FrameRate = modes[index].FrameRate
	}
	mode.Width, mode.Height = layout.w, layout.h
	ready <- mfOpened{mode: mode}

	dur := frameDuration(mode.FrameRate)
	for !b.stop.Load() {
		s, err := rd.read()
		if b.stop.Load() {
			s.release()
			return
		}
		if err != nil || s.flags&(mfReadError|mfReadEndOfStream) != 0 {
			s.release()
			b.gate.fail(mfReadFailure(err))
			return
		}
		if s.flags&mfReadTypeChanged != 0 {
			l, err := rd.layout()
			if err != nil {
				s.release()
				b.gate.fail(fmt.Errorf("%w: read format: %w", ErrDeviceLost, err))
				return
			}
			if !fitsPixels(l.bufw, l.bufh, cfg.MaxPixels) {
				s.release()
				b.gate.fail(fmt.Errorf("%w: format change to %dx%d", ErrDeviceLost, l.bufw, l.bufh))
				return
			}
			layout = l
		}
		if s.data != nil {
			if f, err := mfFrame(s, layout); err != nil {
				b.gate.invalid()
			} else {
				f.Timestamp = s.ts
				f.Duration = dur
				b.gate.deliver(f)
			}
		}
		s.release()
	}
}

// mfReadFailure maps a failed or ended read: the camera was lost, or its
// access was withdrawn.
func mfReadFailure(err error) error {
	if err == nil {
		return fmt.Errorf("%w: stream ended", ErrDeviceLost)
	}
	if h, ok := mfCode(err); ok && h == hrAccessDenied {
		return mfFail("read frame", err)
	}
	return fmt.Errorf("%w: read frame: %w", ErrDeviceLost, err)
}

// mfFrame converts an NV12 sample whose chroma plane follows bufh rows of
// luma.
func mfFrame(s mfSample, l mfLayout) (*Frame, error) {
	pitch := s.pitch
	if pitch == 0 {
		pitch = l.bufw
	}
	if pitch <= 0 || l.bufh <= 0 || l.h > l.bufh || uint64(len(s.data)) > math.MaxInt32 {
		return nil, fmt.Errorf("%w: NV12 layout", ErrInvalidFrame)
	}
	if len(s.data)/pitch < l.bufh {
		return nil, fmt.Errorf("%w: short NV12 buffer", ErrInvalidFrame)
	}
	f, err := FromNV12(l.w, l.h, s.data, pitch, s.data[pitch*l.bufh:], pitch)
	if err == nil && l.bt709 {
		f.rec709to601()
	}
	return f, err
}

// close stops the reading thread and waits for it to release everything.
// A read in progress is cancelled with Flush from a thread of its own; if
// the reader does not return, the source is shut down as well. A source
// that does not unblock either leaves its objects to the process, and close
// says so, but the sink is never called after close returns.
func (b *mfBackend) close() error {
	b.once.Do(func() {
		b.stop.Store(true)
		b.gate.close()
		for _, step := range []func(src mfSource, rd mfReader){
			func(_ mfSource, rd mfReader) {
				if rd != nil {
					rd.flush()
				}
			},
			func(src mfSource, _ mfReader) {
				if src != nil {
					src.shutdown()
				}
			},
		} {
			if b.finished() {
				return
			}
			b.fromThread(step)
			if b.waitDone() {
				return
			}
		}
		b.closeErr = fmt.Errorf("kolovideo: the camera did not stop reading within %v of each step", mfCloseWait)
	})
	return b.closeErr
}

func (b *mfBackend) finished() bool {
	select {
	case <-b.done:
		return true
	default:
		return false
	}
}

func (b *mfBackend) waitDone() bool {
	t := time.NewTimer(mfCloseWait)
	defer t.Stop()
	select {
	case <-b.done:
		return true
	case <-t.C:
		return false
	}
}

// fromThread runs fn on a thread that only entered an apartment, while the
// objects are still alive.
func (b *mfBackend) fromThread(fn func(src mfSource, rd mfReader)) {
	go func() {
		runtime.LockOSThread()
		if uninit, err := b.api.coInit(); err == nil {
			defer uninit()
		}
		b.live.RLock()
		defer b.live.RUnlock()
		if !b.released {
			fn(b.src, b.rd)
		}
	}()
}
