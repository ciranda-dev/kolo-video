package kolovideo

import (
	"context"
	"fmt"
	"math"
	"slices"
	"sync"
	"time"
)

// Capture defaults.
const (
	// DefaultWidth, DefaultHeight and DefaultFrameRate are the common
	// browser capture default, 640x480 at 30 fps. vpx.DefaultFrameRate and
	// h264.DefaultFrameRate are the encoders' rate for frames without a
	// Duration, the same value.
	DefaultWidth     = 640
	DefaultHeight    = 480
	DefaultFrameRate = 30
	// MinFrameRate and MaxFrameRate bound a wanted frame rate: one frame per
	// hour, so that a frame duration in 90 kHz ticks fits the 32-bit duration of
	// libvpx, and 1000 frames per second, the finest rate a 90 kHz clock gives
	// 90 ticks to.
	MinFrameRate = 1.0 / 3600
	MaxFrameRate = 1000
	// DefaultBuffer frames are held for a slow reader: one late read is
	// absorbed without adding latency.
	DefaultBuffer = 2
	// DefaultStallTimeout is the wait for a frame before the device counts
	// as lost, the timeout of the V4L2 reference capture loop.
	DefaultStallTimeout = 2 * time.Second
	// DefaultMaxPixels bounds the capture size: 8,912,896 pixels, the VP9
	// level 5 picture size (4096x2176), which holds 4K UHD. vpx.DefaultMaxPixels
	// bounds decoded VP8 and VP9 frames with the same value; h264.DefaultMaxPixels
	// bounds the coded size of H.264 pictures and is a different value.
	DefaultMaxPixels = 8912896
)

// PixelFormat is a camera's native pixel layout.
type PixelFormat int

// Pixel formats a camera may deliver.
const (
	FormatUnknown PixelFormat = iota
	FormatI420
	FormatNV12
	FormatYUY2
	FormatUYVY
	FormatBGRA
	FormatMJPEG
)

var formatNames = [...]string{"unknown", "I420", "NV12", "YUY2", "UYVY", "BGRA", "MJPEG"}

// String returns the format name.
func (p PixelFormat) String() string {
	if p < 0 || int(p) >= len(formatNames) {
		return fmt.Sprintf("PixelFormat(%d)", int(p))
	}
	return formatNames[p]
}

// Mode is one capture size and frame rate a camera supports.
type Mode struct {
	Width, Height int
	// FrameRate is the highest rate in Device.Modes, 0 if unknown. In
	// Capture.Mode it is the rate the camera was set to, or the requested
	// rate if the camera does not report one.
	FrameRate float64
	// Format is the camera's native format; captured frames are always
	// I420.
	Format PixelFormat
}

// Device is a camera.
type Device struct {
	// ID identifies the camera to OpenCapture. It is stable while the
	// camera stays connected.
	ID   string
	Name string
	// Default marks the system's preferred camera.
	Default bool
	// Modes lists supported modes when the platform reports them without
	// starting the camera; it is empty on Windows. A Linux node held exclusively
	// by another program is listed with no modes.
	Modes []Mode
}

// Devices lists the cameras connected now.
func Devices() ([]Device, error) {
	return listDevices()
}

// RequestAccess asks the user for camera access where the system requires
// it (macOS) and returns ErrPermission if it is refused. It may show a
// system prompt. On Linux and Windows it returns nil at once; where
// capture is not supported it returns ErrUnsupported.
func RequestAccess(ctx context.Context) error {
	return requestAccess(ctx)
}

// CaptureConfig selects a camera and mode. Zero fields take defaults.
type CaptureConfig struct {
	// DeviceID is a Device.ID; empty means the default camera.
	DeviceID string
	// Width and Height are the wanted size, DefaultWidth by DefaultHeight
	// if both are zero. The nearest supported mode is used.
	Width, Height int
	// FrameRate is the wanted rate, DefaultFrameRate if zero. It is capped
	// at the chosen mode's highest rate.
	FrameRate float64
	// Buffer is the number of frames held while the reader is slow; when
	// it is full the oldest frame is dropped. Default DefaultBuffer.
	Buffer int
	// StallTimeout is how long Read waits for a frame before it returns
	// ErrDeviceLost. Default DefaultStallTimeout; negative disables it.
	StallTimeout time.Duration
	// MaxPixels is the largest mode accepted, so a faulty camera cannot
	// make every frame a huge allocation. Default DefaultMaxPixels.
	MaxPixels int
}

func (c CaptureConfig) withDefaults() (CaptureConfig, error) {
	if (c.Width == 0) != (c.Height == 0) || c.Width < 0 || c.Height < 0 ||
		c.Width > maxDimension || c.Height > maxDimension {
		return c, fmt.Errorf("%w: size %dx%d", ErrInvalidConfig, c.Width, c.Height)
	}
	if c.FrameRate != 0 && !(c.FrameRate >= MinFrameRate && c.FrameRate <= MaxFrameRate) {
		return c, fmt.Errorf("%w: frame rate %v", ErrInvalidConfig, c.FrameRate)
	}
	if c.Buffer < 0 {
		return c, fmt.Errorf("%w: buffer %d", ErrInvalidConfig, c.Buffer)
	}
	if c.MaxPixels < 0 {
		return c, fmt.Errorf("%w: max pixels %d", ErrInvalidConfig, c.MaxPixels)
	}
	if c.MaxPixels == 0 {
		c.MaxPixels = DefaultMaxPixels
	}
	if c.Width == 0 {
		c.Width, c.Height = DefaultWidth, DefaultHeight
	}
	if c.FrameRate == 0 {
		c.FrameRate = DefaultFrameRate
	}
	if c.Buffer == 0 {
		c.Buffer = DefaultBuffer
	}
	if c.StallTimeout == 0 {
		c.StallTimeout = DefaultStallTimeout
	}
	return c, nil
}

// Stats counts frames since OpenCapture.
type Stats struct {
	// Frames is the number of frames received from the camera.
	Frames uint64
	// Dropped is the number of frames discarded because Buffer was full.
	Dropped uint64
	// Invalid is the number of camera frames that could not be converted.
	Invalid uint64
}

// backend is a running platform capture. close stops it and returns only
// when it will make no further calls to its sink.
type backend interface {
	close() error
}

// Platform hooks, replaced by tests.
var (
	listDevices   = platformDevices
	openBackend   = platformOpen
	requestAccess = platformRequestAccess
)

// Capture is an open camera. Its methods are safe for concurrent use.
type Capture struct {
	device Device
	mode   Mode
	stall  time.Duration
	limit  int
	be     backend

	closeOnce sync.Once
	closeErr  error

	mu     sync.Mutex
	queue  []*Frame
	err    error
	wake   chan struct{}
	last   time.Time
	stats  Stats
	closed bool
}

// OpenCapture opens a camera and starts capturing.
func OpenCapture(cfg CaptureConfig) (*Capture, error) {
	cfg, err := cfg.withDefaults()
	if err != nil {
		return nil, err
	}
	devs, err := listDevices()
	if err != nil {
		return nil, err
	}
	dev, err := pickDevice(devs, cfg.DeviceID)
	if err != nil {
		return nil, err
	}
	c := &Capture{
		device: dev,
		stall:  cfg.StallTimeout,
		limit:  cfg.Buffer,
		wake:   make(chan struct{}),
		last:   time.Now(),
	}
	be, mode, err := openBackend(dev, cfg, c)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.be, c.mode = be, mode
	c.last = time.Now()
	c.mu.Unlock()
	return c, nil
}

func pickDevice(devs []Device, id string) (Device, error) {
	if len(devs) == 0 {
		return Device{}, fmt.Errorf("%w: no camera connected", ErrNoDevice)
	}
	for _, d := range devs {
		if (id == "" && d.Default) || (id != "" && d.ID == id) {
			return d, nil
		}
	}
	if id == "" {
		return devs[0], nil
	}
	return Device{}, fmt.Errorf("%w: %q", ErrNoDevice, id)
}

// chooseMode picks the mode nearest in size to the request, preferring
// modes that reach the frame rate, then formats that are cheaper to
// convert, and returns it with its index. The returned rate is capped at
// the mode's highest rate.
func chooseMode(modes []Mode, cfg CaptureConfig) (Mode, int, error) {
	best := -1
	var bestKey [4]float64
	for i, m := range modes {
		if m.Width <= 0 || m.Height <= 0 || m.FrameRate < 0 || m.Format == FormatUnknown ||
			!fitsPixels(m.Width, m.Height, cfg.MaxPixels) {
			continue
		}
		dist := math.Abs(float64(m.Width-cfg.Width)) + math.Abs(float64(m.Height-cfg.Height))
		// A zero rate is unknown and assumed to reach the request. NTSC
		// rates such as 29.97 count as reaching 30.
		rate := rateOrRequest(m.FrameRate, cfg.FrameRate)
		short := 0.0
		if rate*1001/1000 < cfg.FrameRate-0.001 {
			short = 1
		}
		key := [4]float64{dist, short, -rate, formatCost(m.Format)}
		if best < 0 || less(key, bestKey) {
			best, bestKey = i, key
		}
	}
	if best < 0 {
		return Mode{}, -1, fmt.Errorf("%w: the camera offers no usable mode", ErrUnsupportedFormat)
	}
	m := modes[best]
	m.FrameRate = rateOrRequest(m.FrameRate, cfg.FrameRate)
	return m, best, nil
}

// fitsPixels reports whether w by h is at most maxPixels, without overflow.
func fitsPixels(w, h, maxPixels int) bool {
	return w > 0 && h > 0 && h <= maxPixels/w
}

// rateOrRequest caps the requested rate at a mode's highest rate, if known.
func rateOrRequest(modeRate, want float64) float64 {
	if modeRate == 0 {
		return want
	}
	return min(modeRate, want)
}

func less(a, b [4]float64) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

func formatCost(f PixelFormat) float64 {
	switch f {
	case FormatI420, FormatNV12:
		return 0
	case FormatYUY2, FormatUYVY:
		return 1
	case FormatBGRA:
		return 2
	default:
		return 3
	}
}

// Device returns the camera in use.
func (c *Capture) Device() Device {
	d := c.device
	d.Modes = slices.Clone(d.Modes)
	return d
}

// Mode returns the mode in use.
func (c *Capture) Mode() Mode {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.mode
}

// Stats returns the frame counters.
func (c *Capture) Stats() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stats
}

// Read returns the oldest buffered frame, waiting for one if needed. After
// the camera is lost, buffered frames are returned first, then an error
// wrapping ErrDeviceLost or ErrPermission. After Close it returns ErrClosed.
func (c *Capture) Read(ctx context.Context) (*Frame, error) {
	for {
		c.mu.Lock()
		if c.closed || c.wake == nil { // a zero Capture was never opened
			c.mu.Unlock()
			return nil, ErrClosed
		}
		if len(c.queue) > 0 {
			f := c.queue[0]
			c.queue[0] = nil
			c.queue = c.queue[1:]
			c.mu.Unlock()
			return f, nil
		}
		if c.err != nil {
			err := c.err
			c.mu.Unlock()
			return nil, err
		}
		wake := c.wake
		var timer *time.Timer
		var timeout <-chan time.Time
		if c.stall > 0 {
			timer = time.NewTimer(time.Until(c.last.Add(c.stall)))
			timeout = timer.C
		}
		c.mu.Unlock()

		var ctxErr error
		select {
		case <-wake:
		case <-ctx.Done():
			ctxErr = ctx.Err()
		case <-timeout:
			c.mu.Lock()
			if c.err == nil && len(c.queue) == 0 && time.Since(c.last) >= c.stall {
				c.failLocked(fmt.Errorf("%w: no frame for %v", ErrDeviceLost, c.stall))
			}
			c.mu.Unlock()
		}
		if timer != nil {
			timer.Stop()
		}
		if ctxErr != nil {
			return nil, ctxErr
		}
	}
}

// Close stops the camera. Blocked and later Read calls return ErrClosed.
// A zero Capture, which holds no camera, can be closed and read too.
// Close is idempotent; only the first call can return an error.
func (c *Capture) Close() error {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		c.queue = nil
		c.signalLocked()
		be := c.be
		c.mu.Unlock()
		if be != nil {
			c.closeErr = be.close()
		}
	})
	return c.closeErr
}

// deliver is called by the backend for each converted frame.
func (c *Capture) deliver(f *Frame) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.err != nil {
		return
	}
	c.stats.Frames++
	c.last = time.Now()
	if len(c.queue) >= c.limit {
		c.queue[0] = nil
		c.queue = c.queue[1:]
		c.stats.Dropped++
	}
	c.queue = append(c.queue, f)
	c.signalLocked()
}

// invalid is called by the backend for a frame it could not convert.
func (c *Capture) invalid() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.err != nil {
		return
	}
	c.stats.Frames++
	c.stats.Invalid++
	c.last = time.Now()
}

// fail is called by the backend when the camera stops for good.
func (c *Capture) fail(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failLocked(err)
}

func (c *Capture) failLocked(err error) {
	if c.closed || c.err != nil {
		return
	}
	c.err = err
	c.signalLocked()
}

func (c *Capture) signalLocked() {
	if c.wake == nil {
		return
	}
	close(c.wake)
	c.wake = make(chan struct{})
}

// frameSink is what a backend reports to.
type frameSink interface {
	deliver(*Frame)
	invalid()
	fail(error)
}

// frameDuration returns the nominal frame time at rate fps.
func frameDuration(fps float64) time.Duration {
	if !(fps > 0) {
		return 0
	}
	// A device may report any rate: one hour is the longest frame time.
	return time.Duration(min(float64(time.Second)/fps, float64(time.Hour)))
}
