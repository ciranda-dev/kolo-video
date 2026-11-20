//go:build linux

package kolovideo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// V4L2 definitions from include/uapi/linux/videodev2.h.
const (
	v4l2BufTypeVideoCapture = 1
	v4l2MemoryMmap          = 1
	v4l2FieldNone           = 1
	v4l2CapVideoCapture     = 0x00000001
	v4l2CapStreaming        = 0x04000000
	v4l2CapDeviceCaps       = 0x80000000
	v4l2CapTimePerFrame     = 0x1000
	v4l2FrmSizeDiscrete     = 1
	v4l2FrmIvalDiscrete     = 1
	v4l2BufFlagError        = 0x00000040
	v4l2BufFlagTsMask       = 0x0000e000
	v4l2BufFlagTsMonotonic  = 0x00002000
	v4l2ColorspaceRec709    = 3
	v4l2ColorspaceJPEG      = 7
	v4l2ColorspaceDCIP3     = 12
	v4l2YCbCrEnc709         = 2
	v4l2YCbCrEncXV709       = 4
	v4l2QuantFullRange      = 1
	v4l2PixFmtPrivMagic     = 0xfeedcafe
)

// v4l2Buffers is the number of driver buffers requested, as in the V4L2
// reference capture example.
const v4l2Buffers = 4

// v4l2MaxEnum bounds each enumeration loop so a driver that never returns
// EINVAL cannot hang Devices.
const v4l2MaxEnum = 1024

type v4l2Capability struct {
	Driver       [16]byte
	Card         [32]byte
	BusInfo      [32]byte
	Version      uint32
	Capabilities uint32
	DeviceCaps   uint32
	Reserved     [3]uint32
}

type v4l2FmtDesc struct {
	Index       uint32
	Type        uint32
	Flags       uint32
	Description [32]byte
	PixelFormat uint32
	MbusCode    uint32
	Reserved    [3]uint32
}

// v4l2FrmSizeEnum holds the discrete size in U[0:2] or the stepwise range
// min_width, max_width, step_width, min_height, max_height, step_height.
type v4l2FrmSizeEnum struct {
	Index       uint32
	PixelFormat uint32
	Type        uint32
	U           [6]uint32
	Reserved    [2]uint32
}

// v4l2FrmIvalEnum holds the discrete interval in U[0:2] or the stepwise
// min, max and step intervals.
type v4l2FrmIvalEnum struct {
	Index       uint32
	PixelFormat uint32
	Width       uint32
	Height      uint32
	Type        uint32
	U           [6]uint32
	Reserved    [2]uint32
}

type v4l2PixFormat struct {
	Width        uint32
	Height       uint32
	PixelFormat  uint32
	Field        uint32
	BytesPerLine uint32
	SizeImage    uint32
	Colorspace   uint32
	Priv         uint32
	Flags        uint32
	YCbCrEnc     uint32
	Quantization uint32
	XferFunc     uint32
}

// v4l2Format's union holds pointers in some members, so it is pointer
// aligned and 200 bytes long.
type v4l2Format struct {
	Type uint32
	Fmt  [200 / unsafe.Sizeof(uintptr(0))]uintptr
}

func (f *v4l2Format) pix() *v4l2PixFormat {
	return (*v4l2PixFormat)(unsafe.Pointer(&f.Fmt[0]))
}

type v4l2RequestBuffers struct {
	Count        uint32
	Type         uint32
	Memory       uint32
	Capabilities uint32
	Flags        uint8
	Reserved     [3]uint8
}

type v4l2Timecode struct {
	Type     uint32
	Flags    uint32
	Frames   uint8
	Seconds  uint8
	Minutes  uint8
	Hours    uint8
	UserBits [4]uint8
}

type v4l2Buffer struct {
	Index     uint32
	Type      uint32
	BytesUsed uint32
	Flags     uint32
	Field     uint32
	Timestamp unix.Timeval
	Timecode  v4l2Timecode
	Sequence  uint32
	Memory    uint32
	M         uintptr
	Length    uint32
	Reserved2 uint32
	RequestFD uint32
}

// offset reads the union member m.offset, the first 4 bytes of M.
func (b *v4l2Buffer) offset() uint32 {
	return *(*uint32)(unsafe.Pointer(&b.M))
}

type v4l2StreamParm struct {
	Type         uint32
	Capability   uint32
	CaptureMode  uint32
	TimeNum      uint32
	TimeDen      uint32
	ExtendedMode uint32
	ReadBuffers  uint32
	Reserved     [4]uint32
	_            [160]byte
}

func ioc(dir, nr, size uintptr) uintptr {
	return dir<<iocDirShift | size<<16 | 'V'<<8 | nr
}

var (
	vidiocQueryCap           = ioc(iocRead, 0, unsafe.Sizeof(v4l2Capability{}))
	vidiocEnumFmt            = ioc(iocRead|iocWrite, 2, unsafe.Sizeof(v4l2FmtDesc{}))
	vidiocSFmt               = ioc(iocRead|iocWrite, 5, unsafe.Sizeof(v4l2Format{}))
	vidiocReqBufs            = ioc(iocRead|iocWrite, 8, unsafe.Sizeof(v4l2RequestBuffers{}))
	vidiocQueryBuf           = ioc(iocRead|iocWrite, 9, unsafe.Sizeof(v4l2Buffer{}))
	vidiocQBuf               = ioc(iocRead|iocWrite, 15, unsafe.Sizeof(v4l2Buffer{}))
	vidiocDQBuf              = ioc(iocRead|iocWrite, 17, unsafe.Sizeof(v4l2Buffer{}))
	vidiocStreamOn           = ioc(iocWrite, 18, unsafe.Sizeof(int32(0)))
	vidiocStreamOff          = ioc(iocWrite, 19, unsafe.Sizeof(int32(0)))
	vidiocSParm              = ioc(iocRead|iocWrite, 22, unsafe.Sizeof(v4l2StreamParm{}))
	vidiocEnumFrameSizes     = ioc(iocRead|iocWrite, 74, unsafe.Sizeof(v4l2FrmSizeEnum{}))
	vidiocEnumFrameIntervals = ioc(iocRead|iocWrite, 75, unsafe.Sizeof(v4l2FrmIvalEnum{}))
)

func ioctl(fd int, req uintptr, arg unsafe.Pointer) error {
	for {
		_, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), req, uintptr(arg))
		if e == unix.EINTR {
			continue
		}
		if e != 0 {
			return e
		}
		return nil
	}
}

// v4l2Sys is the kernel interface the backend uses: a device node, its
// ioctls, memory mapping and polling. Tests replace it with a model of a driver.
type v4l2Sys interface {
	// open opens a node without blocking. Listing opens read-only; capture
	// opens read-write.
	open(path string, write bool) (fd int, err error)
	ioctl(fd int, req uintptr, arg unsafe.Pointer) error
	mmap(fd int, offset int64, length int) ([]byte, error)
	munmap(b []byte)
	close(fd int)
	// newWake makes a descriptor that signal makes readable, for Close.
	newWake() (int, error)
	signal(wake int)
	// poll waits until fd is readable or in error, or wake is signalled. It
	// returns the events of fd and whether wake fired.
	poll(fd, wake int) (revents int16, woken bool, err error)
}

// osV4L2 is the real kernel.
type osV4L2 struct{}

func (osV4L2) open(path string, write bool) (int, error) {
	flags := unix.O_RDONLY
	if write {
		flags = unix.O_RDWR
	}
	for {
		fd, err := unix.Open(path, flags|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
		if err == unix.EINTR {
			continue
		}
		return fd, err
	}
}

func (osV4L2) ioctl(fd int, req uintptr, arg unsafe.Pointer) error { return ioctl(fd, req, arg) }

func (osV4L2) mmap(fd int, offset int64, length int) ([]byte, error) {
	return unix.Mmap(fd, offset, length, unix.PROT_READ, unix.MAP_SHARED)
}

func (osV4L2) munmap(b []byte) { unix.Munmap(b) }
func (osV4L2) close(fd int)    { unix.Close(fd) }

func (osV4L2) newWake() (int, error) { return unix.Eventfd(0, unix.EFD_CLOEXEC|unix.EFD_NONBLOCK) }

func (osV4L2) signal(wake int) {
	one := [8]byte{1}
	unix.Write(wake, one[:])
}

func (osV4L2) poll(fd, wake int) (int16, bool, error) {
	fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}, {Fd: int32(wake), Events: unix.POLLIN}}
	for {
		if _, err := unix.Poll(fds, -1); err != nil {
			if err == unix.EINTR {
				continue
			}
			return 0, false, err
		}
		return fds[0].Revents, fds[1].Revents != 0, nil
	}
}

// v4l2OS is the kernel interface in use.
var v4l2OS v4l2Sys = osV4L2{}

func fourcc(s string) uint32 {
	return uint32(s[0]) | uint32(s[1])<<8 | uint32(s[2])<<16 | uint32(s[3])<<24
}

func v4l2PixelFormat(code uint32) PixelFormat {
	switch code {
	case fourcc("YUYV"):
		return FormatYUY2
	case fourcc("UYVY"):
		return FormatUYVY
	case fourcc("NV12"):
		return FormatNV12
	case fourcc("YU12"):
		return FormatI420
	case fourcc("MJPG"), fourcc("JPEG"):
		return FormatMJPEG
	}
	return FormatUnknown
}

func platformRequestAccess(context.Context) error { return nil }

func platformDevices() ([]Device, error) {
	sys := v4l2OS
	return listV4L2("/dev", v4l2ProbeTimeout, func(path string) (Device, bool, error) { return probeV4L2(sys, path) })
}

// v4l2ProbeTimeout bounds the listing of all nodes together, as the stall
// timeout bounds a frame: a node that does not answer in time is skipped.
var v4l2ProbeTimeout = DefaultStallTimeout

type v4l2Probe struct {
	dev Device
	ok  bool
	err error
}

// errV4L2Timeout marks a node that did not answer within the listing deadline.
var errV4L2Timeout = errors.New("no answer in time")

// listV4L2 probes the videoN nodes of dir all at once under one deadline and
// lists them in numeric order.
func listV4L2(dir string, timeout time.Duration, probe func(path string) (Device, bool, error)) ([]Device, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("kolovideo: list %s: %w", dir, err)
	}
	var nums []int
	for _, e := range entries {
		if n, ok := strings.CutPrefix(e.Name(), "video"); ok {
			if i, err := strconv.Atoi(n); err == nil && i >= 0 && strconv.Itoa(i) == n {
				nums = append(nums, i)
			}
		}
	}
	slices.Sort(nums)
	results := make([]chan v4l2Probe, len(nums))
	for i, n := range nums {
		results[i] = make(chan v4l2Probe, 1)
		go func(path string, out chan<- v4l2Probe) {
			d, ok, err := probe(path)
			out <- v4l2Probe{d, ok, err}
		}(dir+"/video"+strconv.Itoa(n), results[i])
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	expired := false
	var devs []Device
	denied, hung := false, ""
	for i, ch := range results {
		var r v4l2Probe
		if expired {
			select {
			case r = <-ch:
			default:
				r.err = errV4L2Timeout
			}
		} else {
			select {
			case r = <-ch:
			case <-deadline.C:
				expired = true
				r.err = errV4L2Timeout
			}
		}
		switch {
		case errors.Is(r.err, unix.EACCES) || errors.Is(r.err, unix.EPERM):
			denied = true
		case r.err == errV4L2Timeout && hung == "":
			hung = dir + "/video" + strconv.Itoa(nums[i])
		}
		if r.ok {
			devs = append(devs, r.dev)
		}
	}
	// The default is the first node with a usable mode; metadata and
	// infrared nodes often list none.
	for i := range devs {
		if len(devs[i].Modes) > 0 {
			devs[i].Default = true
			break
		}
	}
	if len(devs) == 0 && denied {
		return nil, fmt.Errorf("%w: no read access to %s/video*", ErrPermission, dir)
	}
	if len(devs) == 0 && hung != "" {
		return nil, fmt.Errorf("%w: %s did not answer within %v", ErrDeviceLost, hung, timeout)
	}
	return devs, nil
}

// probeV4L2 reports whether path is a streaming capture device and lists
// its modes. It opens the node read-only and only queries it: the camera is
// not started. A node that is busy is listed without modes.
func probeV4L2(sys v4l2Sys, path string) (Device, bool, error) {
	busy := Device{ID: path, Name: filepath.Base(path)}
	fd, err := sys.open(path, false)
	if errors.Is(err, unix.EBUSY) {
		return busy, true, nil
	}
	if err != nil {
		return Device{}, false, err
	}
	defer sys.close(fd)
	var c v4l2Capability
	if err := sys.ioctl(fd, vidiocQueryCap, unsafe.Pointer(&c)); errors.Is(err, unix.EBUSY) {
		return busy, true, nil
	} else if err != nil {
		return Device{}, false, err
	}
	caps := c.Capabilities
	if caps&v4l2CapDeviceCaps != 0 {
		caps = c.DeviceCaps
	}
	if caps&v4l2CapVideoCapture == 0 || caps&v4l2CapStreaming == 0 {
		return Device{}, false, nil
	}
	name, _, _ := bytes.Cut(c.Card[:], []byte{0})
	return Device{ID: path, Name: string(name), Modes: v4l2Modes(sys, fd, nil)}, true, nil
}

// v4l2Modes lists the modes of fd. A stepwise or continuous size range is
// listed as its largest size and, when want is set, also as the wanted size
// fitted to the range.
func v4l2Modes(sys v4l2Sys, fd int, want *CaptureConfig) []Mode {
	var modes []Mode
	seen := map[Mode]bool{}
	add := func(m Mode) {
		if !seen[m] {
			seen[m] = true
			modes = append(modes, m)
		}
	}
	for _, code := range v4l2Formats(sys, fd) {
		pf := v4l2PixelFormat(code)
		for j := range uint32(v4l2MaxEnum) {
			s := v4l2FrmSizeEnum{Index: j, PixelFormat: code}
			if sys.ioctl(fd, vidiocEnumFrameSizes, unsafe.Pointer(&s)) != nil {
				break
			}
			if s.Type != v4l2FrmSizeDiscrete {
				w, h := s.U[1], s.U[4]
				add(Mode{int(w), int(h), v4l2MaxRate(sys, fd, code, w, h), pf})
				if want != nil {
					w = stepSize(uint32(want.Width), s.U[0], s.U[1], s.U[2])
					h = stepSize(uint32(want.Height), s.U[3], s.U[4], s.U[5])
					add(Mode{int(w), int(h), v4l2MaxRate(sys, fd, code, w, h), pf})
				}
				break
			}
			w, h := s.U[0], s.U[1]
			add(Mode{int(w), int(h), v4l2MaxRate(sys, fd, code, w, h), pf})
		}
	}
	return modes
}

// stepSize fits v to the range lo..hi in steps of step from lo.
func stepSize(v, lo, hi, step uint32) uint32 {
	v = min(max(v, lo), hi)
	if step > 1 {
		v = lo + (v-lo)/step*step
	}
	return v
}

// v4l2Formats lists the capture pixel formats this package can convert.
func v4l2Formats(sys v4l2Sys, fd int) []uint32 {
	var codes []uint32
	for i := range uint32(v4l2MaxEnum) {
		d := v4l2FmtDesc{Index: i, Type: v4l2BufTypeVideoCapture}
		if sys.ioctl(fd, vidiocEnumFmt, unsafe.Pointer(&d)) != nil {
			break
		}
		if v4l2PixelFormat(d.PixelFormat) != FormatUnknown {
			codes = append(codes, d.PixelFormat)
		}
	}
	return codes
}

// v4l2MaxRate returns the highest frame rate for a size, or 0 if the
// driver does not list intervals.
func v4l2MaxRate(sys v4l2Sys, fd int, code, w, h uint32) float64 {
	best := 0.0
	for k := range uint32(v4l2MaxEnum) {
		iv := v4l2FrmIvalEnum{Index: k, PixelFormat: code, Width: w, Height: h}
		if sys.ioctl(fd, vidiocEnumFrameIntervals, unsafe.Pointer(&iv)) != nil {
			break
		}
		// Discrete: U[0:2] is the interval; stepwise: U[0:2] is the minimum.
		if iv.U[0] > 0 && iv.U[1] > 0 {
			best = max(best, float64(iv.U[1])/float64(iv.U[0]))
		}
		if iv.Type != v4l2FrmIvalDiscrete {
			break
		}
	}
	return best
}

type v4l2Backend struct {
	sys      v4l2Sys
	fd, wake int
	maps     [][]byte
	once     sync.Once
	done     chan struct{}
}

type v4l2Opened struct {
	b    *v4l2Backend
	conv func([]byte) (*Frame, error)
	mode Mode
	err  error
}

// platformOpen sets the camera up under the stall timeout, so that a driver
// that never answers cannot hang OpenCapture; a negative timeout waits
// forever. A setup that finishes late is released.
func platformOpen(dev Device, cfg CaptureConfig, sink frameSink) (backend, Mode, error) {
	sys := v4l2OS
	done := make(chan v4l2Opened, 1)
	go func() {
		b, conv, mode, err := openV4L2(sys, dev, cfg)
		done <- v4l2Opened{b, conv, mode, err}
	}()
	var expired <-chan time.Time
	if cfg.StallTimeout > 0 {
		t := time.NewTimer(cfg.StallTimeout)
		defer t.Stop()
		expired = t.C
	}
	var r v4l2Opened
	select {
	case r = <-done:
	case <-expired:
		go func() {
			if late := <-done; late.b != nil {
				late.b.release()
			}
		}()
		return nil, Mode{}, fmt.Errorf("%w: %s did not answer within %v", ErrDeviceLost, dev.ID, cfg.StallTimeout)
	}
	if r.err != nil {
		return nil, Mode{}, r.err
	}
	go r.b.loop(sink, r.conv, frameDuration(r.mode.FrameRate))
	return r.b, r.mode, nil
}

func openV4L2(sys v4l2Sys, dev Device, cfg CaptureConfig) (*v4l2Backend, func([]byte) (*Frame, error), Mode, error) {
	fd, err := sys.open(dev.ID, true)
	if err != nil {
		switch {
		case errors.Is(err, unix.EACCES), errors.Is(err, unix.EPERM):
			return nil, nil, Mode{}, fmt.Errorf("%w: %s: %v", ErrPermission, dev.ID, err)
		case errors.Is(err, unix.ENOENT), errors.Is(err, unix.ENODEV), errors.Is(err, unix.ENXIO):
			return nil, nil, Mode{}, fmt.Errorf("%w: %s: %v", ErrNoDevice, dev.ID, err)
		}
		return nil, nil, Mode{}, fmt.Errorf("kolovideo: open %s: %w", dev.ID, err)
	}
	b := &v4l2Backend{sys: sys, fd: fd, wake: -1, done: make(chan struct{})}
	mode, _, err := chooseMode(v4l2Modes(sys, fd, &cfg), cfg)
	if err != nil {
		b.release()
		return nil, nil, Mode{}, err
	}
	conv, mode, err := b.start(mode, cfg.MaxPixels)
	if err != nil {
		b.release()
		return nil, nil, Mode{}, err
	}
	return b, conv, mode, nil
}

// start sets the format and rate, maps the buffers and starts streaming.
func (b *v4l2Backend) start(want Mode, maxPixels int) (func([]byte) (*Frame, error), Mode, error) {
	var code uint32
	for _, c := range v4l2Formats(b.sys, b.fd) {
		if v4l2PixelFormat(c) == want.Format {
			code = c
			break
		}
	}
	if code == 0 {
		return nil, Mode{}, fmt.Errorf("%w: %v", ErrUnsupportedFormat, want.Format)
	}
	f := v4l2Format{Type: v4l2BufTypeVideoCapture}
	p := f.pix()
	p.Width, p.Height, p.PixelFormat, p.Field = uint32(want.Width), uint32(want.Height), code, v4l2FieldNone
	if err := b.sys.ioctl(b.fd, vidiocSFmt, unsafe.Pointer(&f)); err != nil {
		return nil, Mode{}, fmt.Errorf("kolovideo: set format on %dx%d %v: %w", want.Width, want.Height, want.Format, err)
	}
	if p.PixelFormat != code || p.Width > maxDimension || p.Height > maxDimension || !fitsPixels(int(p.Width), int(p.Height), maxPixels) {
		return nil, Mode{}, fmt.Errorf("%w: driver chose %dx%d format %#x", ErrUnsupportedFormat, p.Width, p.Height, p.PixelFormat)
	}
	got := Mode{Width: int(p.Width), Height: int(p.Height), FrameRate: want.FrameRate, Format: want.Format}

	parm := v4l2StreamParm{Type: v4l2BufTypeVideoCapture, TimeNum: 1000, TimeDen: uint32(min(want.FrameRate*1000+0.5, math.MaxUint32))}
	if b.sys.ioctl(b.fd, vidiocSParm, unsafe.Pointer(&parm)) == nil &&
		parm.Capability&v4l2CapTimePerFrame != 0 && parm.TimeNum > 0 && parm.TimeDen > 0 {
		got.FrameRate = float64(parm.TimeDen) / float64(parm.TimeNum)
	}

	req := v4l2RequestBuffers{Count: v4l2Buffers, Type: v4l2BufTypeVideoCapture, Memory: v4l2MemoryMmap}
	if err := b.sys.ioctl(b.fd, vidiocReqBufs, unsafe.Pointer(&req)); err != nil {
		return nil, Mode{}, fmt.Errorf("kolovideo: request buffers: %w", err)
	}
	if req.Count < 2 {
		return nil, Mode{}, fmt.Errorf("kolovideo: driver gave %d buffers, need 2", req.Count)
	}
	for i := range req.Count {
		buf := v4l2Buffer{Index: i, Type: v4l2BufTypeVideoCapture, Memory: v4l2MemoryMmap}
		if err := b.sys.ioctl(b.fd, vidiocQueryBuf, unsafe.Pointer(&buf)); err != nil {
			return nil, Mode{}, fmt.Errorf("kolovideo: query buffer: %w", err)
		}
		m, err := b.sys.mmap(b.fd, int64(buf.offset()), int(buf.Length))
		if err != nil {
			return nil, Mode{}, fmt.Errorf("kolovideo: map buffer: %w", err)
		}
		b.maps = append(b.maps, m)
		if err := b.sys.ioctl(b.fd, vidiocQBuf, unsafe.Pointer(&buf)); err != nil {
			return nil, Mode{}, fmt.Errorf("kolovideo: queue buffer: %w", err)
		}
	}
	var err error
	if b.wake, err = b.sys.newWake(); err != nil {
		return nil, Mode{}, fmt.Errorf("kolovideo: eventfd: %w", err)
	}
	typ := int32(v4l2BufTypeVideoCapture)
	if err := b.sys.ioctl(b.fd, vidiocStreamOn, unsafe.Pointer(&typ)); err != nil {
		return nil, Mode{}, fmt.Errorf("kolovideo: start streaming: %w", err)
	}

	w, h, bpl := got.Width, got.Height, int(p.BytesPerLine)
	// The extended fields are valid only when the driver sets the magic.
	var enc, quant uint32
	if p.Priv == v4l2PixFmtPrivMagic {
		enc, quant = p.YCbCrEnc, p.Quantization
	}
	if enc == 0 && (p.Colorspace == v4l2ColorspaceRec709 || p.Colorspace == v4l2ColorspaceDCIP3) {
		enc = v4l2YCbCrEnc709
	}
	full := quant == v4l2QuantFullRange || (quant == 0 && p.Colorspace == v4l2ColorspaceJPEG)
	conv := func(data []byte) (*Frame, error) {
		f, err := convertV4L2(want.Format, data, w, h, bpl)
		if err != nil || want.Format == FormatMJPEG {
			return f, err
		}
		if full {
			f.fullToLimited()
		}
		if enc == v4l2YCbCrEnc709 || enc == v4l2YCbCrEncXV709 {
			f.rec709to601()
		}
		return f, nil
	}
	return conv, got, nil
}

// convertV4L2 converts one buffer; bpl is the driver's bytes per line of
// the first plane, 0 if unset.
func convertV4L2(pf PixelFormat, data []byte, w, h, bpl int) (*Frame, error) {
	switch pf {
	case FormatYUY2, FormatUYVY:
		if bpl == 0 {
			bpl = 2 * w
		}
		if pf == FormatYUY2 {
			return FromYUY2(w, h, data, bpl)
		}
		return FromUYVY(w, h, data, bpl)
	case FormatNV12:
		if bpl == 0 {
			bpl = w
		}
		if bpl < w || len(data)/bpl < h {
			return nil, fmt.Errorf("%w: short NV12 buffer", ErrInvalidFrame)
		}
		return FromNV12(w, h, data, bpl, data[bpl*h:], bpl)
	case FormatI420:
		if bpl == 0 {
			bpl = w
		}
		cs, ch := bpl/2, (h+1)/2
		if bpl < w || len(data)/bpl < h || len(data)-bpl*h < 2*cs*ch {
			return nil, fmt.Errorf("%w: short I420 buffer", ErrInvalidFrame)
		}
		u := bpl * h
		f := &Frame{Width: w, Height: h, Y: data, U: data[u:], V: data[u+cs*ch:], YStride: bpl, UVStride: cs}
		if err := f.Validate(); err != nil {
			return nil, err
		}
		return copyFrame(f), nil
	case FormatMJPEG:
		return decodeMJPEG(data, w, h)
	}
	return nil, fmt.Errorf("%w: %v", ErrUnsupportedFormat, pf)
}

func (b *v4l2Backend) loop(sink frameSink, conv func([]byte) (*Frame, error), dur time.Duration) {
	defer close(b.done)
	for {
		revents, woken, err := b.sys.poll(b.fd, b.wake)
		if err != nil {
			sink.fail(fmt.Errorf("%w: poll: %w", ErrDeviceLost, err))
			return
		}
		if woken {
			return
		}
		buf := v4l2Buffer{Type: v4l2BufTypeVideoCapture, Memory: v4l2MemoryMmap}
		err = b.sys.ioctl(b.fd, vidiocDQBuf, unsafe.Pointer(&buf))
		if err == unix.EAGAIN && revents&(unix.POLLERR|unix.POLLHUP|unix.POLLNVAL) == 0 {
			continue
		}
		if err != nil {
			sink.fail(fmt.Errorf("%w: dequeue buffer: %w", ErrDeviceLost, err))
			return
		}
		if uint64(buf.Index) >= uint64(len(b.maps)) || uint64(buf.BytesUsed) > uint64(len(b.maps[buf.Index])) || buf.Flags&v4l2BufFlagError != 0 {
			sink.invalid()
		} else if f, err := conv(b.maps[buf.Index][:buf.BytesUsed]); err != nil {
			sink.invalid()
		} else {
			if buf.Flags&v4l2BufFlagTsMask == v4l2BufFlagTsMonotonic {
				f.Timestamp = time.Duration(buf.Timestamp.Nano())
			} else {
				f.Timestamp = monotonicNow()
			}
			f.Duration = dur
			sink.deliver(f)
		}
		if err := b.sys.ioctl(b.fd, vidiocQBuf, unsafe.Pointer(&buf)); err != nil {
			sink.fail(fmt.Errorf("%w: queue buffer: %w", ErrDeviceLost, err))
			return
		}
	}
}

func monotonicNow() time.Duration {
	var ts unix.Timespec
	if unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts) != nil {
		return 0
	}
	return time.Duration(ts.Nano())
}

func (b *v4l2Backend) close() error {
	b.once.Do(func() {
		b.sys.signal(b.wake)
		<-b.done
		b.release()
	})
	return nil
}

// release stops streaming and frees everything; the loop must not run.
func (b *v4l2Backend) release() {
	typ := int32(v4l2BufTypeVideoCapture)
	b.sys.ioctl(b.fd, vidiocStreamOff, unsafe.Pointer(&typ))
	for _, m := range b.maps {
		b.sys.munmap(m)
	}
	b.maps = nil
	if b.wake >= 0 {
		b.sys.close(b.wake)
	}
	b.sys.close(b.fd)
}
