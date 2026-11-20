//go:build darwin

package kolovideo

import (
	"math"
	"math/big"
	"math/bits"
)

// avfAPI is the part of AVFoundation the backend uses. The real one calls
// the system frameworks; tests replace it.
type avfAPI interface {
	// authorizationStatus never prompts.
	authorizationStatus() int
	devices() ([]avfDevice, error)
	// requestAccess shows the system prompt if access is undecided and calls
	// done once, on a foreign thread.
	requestAccess(done func(granted bool)) error
	// openDevice returns nil without error when the id is unknown.
	openDevice(uid string) (avfSession, error)
}

// avfSession is one camera with its capture session, output and queue.
// Every method that can raise an Objective-C exception has a precondition
// the caller checks first; see openSession.
type avfSession interface {
	// formats lists the device's formats; setActiveFormat takes an index.
	formats() []avfFormat
	// newInput creates the device input. On failure code is the NSError code.
	newInput() (code int64, ok bool)
	beginConfiguration()
	commitConfiguration()
	canAddInput() bool
	addInput()
	canAddOutput() bool
	addOutput()
	// setDelegate makes the delegate for ident receive sample buffers on the
	// session's serial queue.
	setDelegate(ident uintptr)
	pixelFormats() []uint32
	setPixelFormat(fourCC uint32)
	// lockDevice is lockForConfiguration. On failure code is the NSError code.
	lockDevice() (code int64, ok bool)
	unlockDevice()
	setActiveFormat(index int)
	setMinFrameDuration(d avfTime)
	activeMinFrameDuration() avfTime
	// observe reports a disconnect (kind avfLostDisconnect) or a session
	// runtime error (kind avfLostAccess or avfLostSession) with its code.
	observe(lost func(kind int, code int64))
	start()
	running() bool
	// syncClock is the session clock, 0 if the system has none.
	syncClock() uintptr
	// stop removes the observers, stops the session, clears the delegate and
	// waits for a callback already running on the queue. It is idempotent.
	stop()
	// release frees every object; it calls stop first.
	release()
}

// avfDevice is a camera as listed.
type avfDevice struct {
	ID, Name string
	Default  bool
	// Builtin marks a camera of the device type built-in wide angle.
	Builtin bool
	Formats []avfFormat
}

type avfFormat struct {
	Width, Height int
	FourCC        uint32
	Ranges        []avfRange
}

type avfRange struct {
	MaxRate                  float64
	MinDuration, MaxDuration avfTime
}

// avfTime is a CMTime.
type avfTime struct {
	Value     int64
	Timescale int32
	Flags     uint32
	Epoch     int64
}

const (
	avfTimeValid    = 1 << 0
	avfTimeImplied  = 1<<2 | 1<<3 | 1<<4
	avfTimeScaleUS  = 1000000
	avfTimeHostNano = 1000000000
)

// avfAuthorizationStatus values.
const (
	avAuthNotDetermined = 0
	avAuthRestricted    = 1
	avAuthDenied        = 2
	avAuthAuthorized    = 3
)

// avfNotAuthorized is AVErrorApplicationIsNotAuthorizedToUseDevice.
const avfNotAuthorized = -11852

// Kinds of avfSession.observe notifications.
const (
	avfLostDisconnect = iota
	avfLostAccess
	avfLostSession
)

func (t avfTime) numeric() bool { return t.Flags&(avfTimeValid|avfTimeImplied) == avfTimeValid }

func (t avfTime) seconds() float64 {
	if !t.numeric() || t.Timescale <= 0 {
		return 0
	}
	return float64(t.Value) / float64(t.Timescale)
}

// cmp compares two numeric times exactly.
func (t avfTime) cmp(u avfTime) int {
	a := new(big.Int).Mul(big.NewInt(t.Value), big.NewInt(int64(u.Timescale)))
	b := new(big.Int).Mul(big.NewInt(u.Value), big.NewInt(int64(t.Timescale)))
	return a.Cmp(b)
}

// nanos converts t to nanoseconds, rounding half away from zero. It reports
// false for a non-numeric, negative or overflowing time.
func (t avfTime) nanos() (int64, bool) {
	if !t.numeric() || t.Timescale <= 0 || t.Value < 0 {
		return 0, false
	}
	hi, lo := bits.Mul64(uint64(t.Value), avfTimeHostNano)
	ts := uint64(t.Timescale)
	if hi >= ts {
		return 0, false
	}
	q, r := bits.Div64(hi, lo, ts)
	if r >= ts-r {
		q++
	}
	if q > math.MaxInt64 {
		return 0, false
	}
	return int64(q), true
}

// FourCC codes of CVPixelFormatType and CMVideoCodecType values.
const (
	fccI420      = 0x79343230 // y420
	fccI420Full  = 0x66343230 // f420
	fccNV12      = 0x34323076 // 420v
	fccNV12Full  = 0x34323066 // 420f
	fccYUY2      = 0x79757673 // yuvs
	fccYUY2Full  = 0x79757666 // yuvf
	fccUYVY      = 0x32767579 // 2vuy
	fccBGRA      = 0x42475241 // BGRA
	fccJPEG      = 0x6a706567 // jpeg
	fccJPEGOpenD = 0x646d6231 // dmb1
)

// avfPixelFormat maps a FourCC to a PixelFormat.
func avfPixelFormat(c uint32) PixelFormat {
	switch c {
	case fccI420, fccI420Full:
		return FormatI420
	case fccNV12, fccNV12Full:
		return FormatNV12
	case fccYUY2, fccYUY2Full:
		return FormatYUY2
	case fccUYVY:
		return FormatUYVY
	case fccBGRA:
		return FormatBGRA
	case fccJPEG, fccJPEGOpenD:
		return FormatMJPEG
	}
	return FormatUnknown
}

// avfModes lists the modes of formats with the highest frame rate of each.
func avfModes(formats []avfFormat) []Mode {
	var modes []Mode
	seen := map[Mode]bool{}
	for _, f := range formats {
		fps := 0.0
		for _, r := range f.Ranges {
			fps = max(fps, r.MaxRate)
		}
		m := Mode{f.Width, f.Height, fps, avfPixelFormat(f.FourCC)}
		if !seen[m] {
			seen[m] = true
			modes = append(modes, m)
		}
	}
	return modes
}

// avfFrameDuration returns the frame time for fps inside range r.
func avfFrameDuration(r avfRange, fps float64) avfTime {
	if fps*1001/1000 >= r.MaxRate {
		return r.MinDuration
	}
	d := avfTime{Value: int64(math.Round(avfTimeScaleUS / fps)), Timescale: avfTimeScaleUS, Flags: avfTimeValid}
	if d.cmp(r.MinDuration) < 0 {
		return r.MinDuration
	}
	if d.cmp(r.MaxDuration) > 0 {
		return r.MaxDuration
	}
	return d
}
