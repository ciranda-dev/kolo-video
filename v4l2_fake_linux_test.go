//go:build linux

package kolovideo

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// fakeV4L2 is a model of the V4L2 kernel interface for one capture driver: it
// checks the request numbers, the order of the calls and the buffer rules the
// way the kernel does, so that the streaming code runs without a camera.
type fakeV4L2 struct {
	mu   sync.Mutex
	node string

	// What the driver offers.
	card    string
	caps    uint32
	formats []fakeFormat
	maxBufs uint32 // buffers granted by REQBUFS
	bufLen  uint32 // length of each buffer
	// adjust lets the driver change the format that S_FMT asks for.
	adjust func(*v4l2PixFormat)
	// parmDen is the frame rate times 1000 that S_PARM reports; 0 reports no rate.
	parmDen uint32
	// failIoctl returns an error for a request, once armed.
	failIoctl map[uintptr]error
	failMmap  error
	// failMmapAt makes the nth mmap call (1-based) fail with failMmap; 0 makes every call fail.
	failMmapAt int
	mmapCalls  int
	// neverEnd makes ENUM_FMT list a format forever.
	neverEnd bool
	failOpen error
	// blockOpen, when set, makes open wait until it is closed, as a driver that never answers.
	blockOpen chan struct{}

	// State.
	cur       v4l2PixFormat
	bufs      []*fakeBuf
	done      []v4l2Buffer
	streaming bool
	unplugged bool
	seq       uint32
	notify    chan struct{}
	fds       map[int]bool
	readOnly  map[int]bool
	nextFD    int
	wakes     map[int]*fakeWake
	maps      int
	calls     []string
	problems  []string
	streamOns int
}

type fakeFormat struct {
	code  uint32
	sizes [][2]uint32 // discrete sizes
	steps *[6]uint32  // or a stepwise range: min w, max w, step w, min h, max h, step h
	rates [][2]uint32 // frame intervals as numerator and denominator
}

type fakeBuf struct {
	data   []byte
	queued bool
}

type fakeWake struct {
	ch   chan struct{}
	once sync.Once
}

func newFakeV4L2(node string) *fakeV4L2 {
	return &fakeV4L2{
		node: node, card: "Fake Capture", caps: v4l2CapVideoCapture | v4l2CapStreaming | v4l2CapDeviceCaps,
		maxBufs: v4l2Buffers, bufLen: 1 << 16, parmDen: 30000,
		formats: []fakeFormat{
			{code: fourcc("YUYV"), sizes: [][2]uint32{{64, 48}}, rates: [][2]uint32{{1, 30}}},
			{code: fourcc("NV12"), sizes: [][2]uint32{{64, 48}}, rates: [][2]uint32{{1, 30}}},
		},
		notify: make(chan struct{}, 1), fds: map[int]bool{}, readOnly: map[int]bool{}, nextFD: 100, wakes: map[int]*fakeWake{},
		failIoctl: map[uintptr]error{},
	}
}

// install makes the fake the kernel interface for the test.
func (f *fakeV4L2) install(t *testing.T) {
	t.Helper()
	old := v4l2OS
	v4l2OS = f
	t.Cleanup(func() { v4l2OS = old })
}

func (f *fakeV4L2) problem(format string, args ...any) {
	f.problems = append(f.problems, fmt.Sprintf(format, args...))
}

// check reports what the code under test left behind or did against the kernel's rules.
func (f *fakeV4L2) check(t *testing.T) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.problems {
		t.Errorf("kernel rule broken: %s", p)
	}
	if len(f.fds) != 0 {
		t.Errorf("%d descriptors left open", len(f.fds))
	}
	if f.maps != 0 {
		t.Errorf("%d buffer mappings left", f.maps)
	}
	if f.streaming {
		t.Error("streaming was left on")
	}
}

func (f *fakeV4L2) open(path string, write bool) (int, error) {
	f.mu.Lock()
	block := f.blockOpen
	f.mu.Unlock()
	if block != nil {
		<-block
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "open")
	if !write {
		f.calls[len(f.calls)-1] = "open-ro"
	}
	if f.failOpen != nil {
		return -1, f.failOpen
	}
	if path != f.node {
		return -1, unix.ENOENT
	}
	fd := f.nextFD
	f.nextFD++
	f.fds[fd] = true
	f.readOnly[fd] = !write
	return fd, nil
}

func (f *fakeV4L2) close(fd int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if w, ok := f.wakes[fd]; ok {
		w.once.Do(func() { close(w.ch) })
		delete(f.wakes, fd)
		return
	}
	if !f.fds[fd] {
		f.problem("close of descriptor %d that is not open", fd)
		return
	}
	delete(f.fds, fd)
}

func (f *fakeV4L2) newWake() (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fd := f.nextFD
	f.nextFD++
	f.wakes[fd] = &fakeWake{ch: make(chan struct{})}
	return fd, nil
}

func (f *fakeV4L2) signal(wake int) {
	f.mu.Lock()
	w := f.wakes[wake]
	f.mu.Unlock()
	if w != nil {
		w.once.Do(func() { close(w.ch) })
	}
}

func (f *fakeV4L2) poll(fd, wake int) (int16, bool, error) {
	f.mu.Lock()
	w := f.wakes[wake]
	f.mu.Unlock()
	for {
		f.mu.Lock()
		switch {
		case !f.fds[fd]:
			f.problem("poll on descriptor %d that is not open", fd)
			f.mu.Unlock()
			return unix.POLLNVAL, false, nil
		case f.unplugged:
			f.mu.Unlock()
			return unix.POLLERR | unix.POLLHUP, false, nil
		case len(f.done) > 0:
			f.mu.Unlock()
			return unix.POLLIN, false, nil
		}
		f.mu.Unlock()
		select {
		case <-w.ch:
			return 0, true, nil
		case <-f.notify:
		}
	}
}

func (f *fakeV4L2) wakeUp() {
	select {
	case f.notify <- struct{}{}:
	default:
	}
}

func (f *fakeV4L2) mmap(fd int, offset int64, length int) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "mmap")
	f.mmapCalls++
	if f.failMmap != nil && (f.failMmapAt == 0 || f.mmapCalls == f.failMmapAt) {
		return nil, f.failMmap
	}
	for i, b := range f.bufs {
		if offset == int64(i)*int64(f.bufLen) && length <= len(b.data) {
			f.maps++
			return b.data[:length:length], nil
		}
	}
	return nil, unix.EINVAL
}

func (f *fakeV4L2) munmap(b []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "munmap")
	for _, x := range f.bufs {
		if len(b) > 0 && &x.data[0] == &b[0] {
			f.maps--
			return
		}
	}
	f.problem("munmap of memory that is not a buffer mapping")
}

func (f *fakeV4L2) ioctl(fd int, req uintptr, arg unsafe.Pointer) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.fds[fd] {
		f.problem("ioctl %#x on descriptor %d that is not open", req, fd)
		return unix.EBADF
	}
	if f.readOnly[fd] {
		switch req {
		case vidiocQueryCap, vidiocEnumFmt, vidiocEnumFrameSizes, vidiocEnumFrameIntervals:
		default:
			f.problem("ioctl %#x on a read-only descriptor", req)
		}
	}
	if err := f.failIoctl[req]; err != nil {
		return err
	}
	if f.unplugged {
		return unix.ENODEV
	}
	switch req {
	case vidiocQueryCap:
		c := (*v4l2Capability)(arg)
		copy(c.Card[:], f.card)
		c.Capabilities, c.DeviceCaps = f.caps, f.caps
	case vidiocEnumFmt:
		d := (*v4l2FmtDesc)(arg)
		if f.neverEnd {
			d.PixelFormat = f.formats[0].code
			return nil
		}
		if d.Type != v4l2BufTypeVideoCapture || int(d.Index) >= len(f.formats) {
			return unix.EINVAL
		}
		d.PixelFormat = f.formats[d.Index].code
	case vidiocEnumFrameSizes:
		s := (*v4l2FrmSizeEnum)(arg)
		ft := f.format(s.PixelFormat)
		switch {
		case ft == nil:
			return unix.EINVAL
		case ft.steps != nil && s.Index == 0:
			s.Type = 2
			s.U = *ft.steps
		case ft.steps == nil && int(s.Index) < len(ft.sizes):
			s.Type = v4l2FrmSizeDiscrete
			s.U[0], s.U[1] = ft.sizes[s.Index][0], ft.sizes[s.Index][1]
		default:
			return unix.EINVAL
		}
	case vidiocEnumFrameIntervals:
		iv := (*v4l2FrmIvalEnum)(arg)
		ft := f.format(iv.PixelFormat)
		if ft == nil || int(iv.Index) >= len(ft.rates) {
			return unix.EINVAL
		}
		iv.Type = v4l2FrmIvalDiscrete
		iv.U[0], iv.U[1] = ft.rates[iv.Index][0], ft.rates[iv.Index][1]
	case vidiocSFmt:
		fm := (*v4l2Format)(arg)
		if fm.Type != v4l2BufTypeVideoCapture {
			return unix.EINVAL
		}
		if f.streaming || len(f.bufs) > 0 {
			return unix.EBUSY
		}
		p := fm.pix()
		if f.format(p.PixelFormat) == nil {
			return unix.EINVAL
		}
		if f.adjust != nil {
			f.adjust(p)
		}
		p.BytesPerLine = p.Width * 2
		if p.PixelFormat == fourcc("NV12") {
			p.BytesPerLine = p.Width
		}
		p.SizeImage = f.bufLen
		f.cur = *p
		f.calls = append(f.calls, "s_fmt")
	case vidiocSParm:
		sp := (*v4l2StreamParm)(arg)
		if sp.Type != v4l2BufTypeVideoCapture {
			return unix.EINVAL
		}
		f.calls = append(f.calls, "s_parm")
		if f.parmDen == 0 {
			sp.Capability = 0
		} else {
			sp.Capability, sp.TimeNum, sp.TimeDen = v4l2CapTimePerFrame, 1000, f.parmDen
		}
	case vidiocReqBufs:
		r := (*v4l2RequestBuffers)(arg)
		if r.Type != v4l2BufTypeVideoCapture || r.Memory != v4l2MemoryMmap {
			return unix.EINVAL
		}
		if f.streaming {
			return unix.EBUSY
		}
		f.calls = append(f.calls, "reqbufs")
		n := min(r.Count, f.maxBufs)
		f.bufs = nil
		f.done = nil
		for range n {
			f.bufs = append(f.bufs, &fakeBuf{data: make([]byte, f.bufLen)})
		}
		r.Count = n
	case vidiocQueryBuf:
		b := (*v4l2Buffer)(arg)
		if b.Type != v4l2BufTypeVideoCapture || int(b.Index) >= len(f.bufs) {
			return unix.EINVAL
		}
		b.Length = f.bufLen
		*(*uint32)(unsafe.Pointer(&b.M)) = b.Index * f.bufLen
	case vidiocQBuf:
		b := (*v4l2Buffer)(arg)
		if b.Type != v4l2BufTypeVideoCapture || b.Memory != v4l2MemoryMmap || int(b.Index) >= len(f.bufs) {
			return unix.EINVAL
		}
		if f.bufs[b.Index].queued {
			f.problem("buffer %d queued twice", b.Index)
			return unix.EINVAL
		}
		f.bufs[b.Index].queued = true
		f.calls = append(f.calls, "qbuf")
	case vidiocDQBuf:
		b := (*v4l2Buffer)(arg)
		if !f.streaming {
			return unix.EINVAL
		}
		if len(f.done) == 0 {
			return unix.EAGAIN
		}
		*b = f.done[0]
		f.done = f.done[1:]
		if int(b.Index) < len(f.bufs) {
			f.bufs[b.Index].queued = false
		}
	case vidiocStreamOn:
		if *(*int32)(arg) != v4l2BufTypeVideoCapture {
			return unix.EINVAL
		}
		queued := 0
		for _, b := range f.bufs {
			if b.queued {
				queued++
			}
		}
		if queued < 1 {
			return unix.EINVAL
		}
		f.streaming = true
		f.streamOns++
		f.calls = append(f.calls, "streamon")
	case vidiocStreamOff:
		f.streaming = false
		f.done = nil
		for _, b := range f.bufs {
			b.queued = false
		}
		f.calls = append(f.calls, "streamoff")
	default:
		return unix.ENOTTY
	}
	return nil
}

func (f *fakeV4L2) format(code uint32) *fakeFormat {
	for i := range f.formats {
		if f.formats[i].code == code {
			return &f.formats[i]
		}
	}
	return nil
}

// frame hands the driver a frame: it fills a queued buffer, or drops the frame
// when none is queued, as a driver does. It reports whether a buffer took it.
func (f *fakeV4L2) frame(data []byte, flags uint32, ts time.Duration) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.streaming {
		return false
	}
	for i, b := range f.bufs {
		if !b.queued {
			continue
		}
		inQueue := false
		for _, d := range f.done {
			inQueue = inQueue || int(d.Index) == i
		}
		if inQueue {
			continue
		}
		n := copy(b.data, data)
		f.seq++
		buf := v4l2Buffer{Index: uint32(i), Type: v4l2BufTypeVideoCapture, Memory: v4l2MemoryMmap, BytesUsed: uint32(n), Flags: flags, Sequence: f.seq}
		buf.Timestamp = unix.NsecToTimeval(int64(ts))
		f.done = append(f.done, buf)
		f.wakeUp()
		return true
	}
	return false
}

// raw puts exactly this buffer record on the done list, whatever it says.
func (f *fakeV4L2) raw(b v4l2Buffer) {
	b.Memory = v4l2MemoryMmap
	f.mu.Lock()
	f.done = append(f.done, b)
	f.mu.Unlock()
	f.wakeUp()
}

func (f *fakeV4L2) unplug() {
	f.mu.Lock()
	f.unplugged = true
	f.streaming = false // a disconnected driver stops its queue by itself
	f.mu.Unlock()
	f.wakeUp()
}

func (f *fakeV4L2) callList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// v4l2Sink records what the capture loop reports.
type v4l2Sink struct {
	frames   chan *Frame
	failed   chan error
	invalidN atomic.Int32
}

func newV4L2Sink() *v4l2Sink {
	return &v4l2Sink{frames: make(chan *Frame, 64), failed: make(chan error, 4)}
}

func (s *v4l2Sink) deliver(f *Frame) {
	select {
	case s.frames <- f:
	default:
	}
}
func (s *v4l2Sink) invalid()          { s.invalidN.Add(1) }
func (s *v4l2Sink) fail(err error)    { s.failed <- err }
func (s *v4l2Sink) invalidCount() int { return int(s.invalidN.Load()) }
