package h264

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"time"
)

var (
	// ErrUnavailable means this system has no H.264 codec kolo-video can use;
	// the error returned is an *UnavailableError that wraps it.
	ErrUnavailable = errors.New("kolovideo/h264: no H.264 codec on this system")
	// ErrCorrupt means a packet is not valid H.264 or the system decoder
	// reported bad data.
	ErrCorrupt = errors.New("kolovideo/h264: corrupt packet")
	// ErrTooLarge means a packet, its NAL unit count or its picture exceeds a
	// bound.
	ErrTooLarge = errors.New("kolovideo/h264: packet or picture too large")
	// ErrNeedKeyframe means the decoder drops packets until an IDR with its
	// SPS and PPS.
	ErrNeedKeyframe = errors.New("kolovideo/h264: waiting for a keyframe")
	// ErrUnsupportedStream means the stream uses a profile, level or tool this
	// package refuses.
	ErrUnsupportedStream = errors.New("kolovideo/h264: unsupported stream")
	// ErrProfile means the system encoder cannot produce the requested
	// profile.
	ErrProfile = errors.New("kolovideo/h264: profile not available from the system encoder")
	// ErrCodec means the system codec failed for a reason other than bad
	// input.
	ErrCodec = errors.New("kolovideo/h264: system codec error")
)

// UnavailableError says why H.264 is absent.
type UnavailableError struct {
	GOOS, GOARCH string
	// Engine is "VideoToolbox", "Media Foundation", or "" when none is built in.
	Engine string
	// Reason is the system's message, or why no engine exists for this platform.
	Reason string
	// Code is the OSStatus or HRESULT, 0 if none.
	Code int64
	// Devices lists, on Linux, the V4L2 stateful H.264 devices found and not used.
	Devices []string
}

// Error returns the reason with the platform and code.
func (e *UnavailableError) Error() string {
	var b strings.Builder
	b.WriteString("kolovideo/h264: ")
	if e.Engine != "" {
		b.WriteString(e.Engine + ": ")
	}
	b.WriteString(e.Reason)
	if e.Engine != "" || e.Code != 0 {
		b.WriteString(" (" + e.GOOS + "/" + e.GOARCH)
		if e.Code != 0 {
			b.WriteString(", code " + codeText(e.Code))
		}
		b.WriteString(")")
	}
	if e.GOOS == "linux" {
		devs := "none"
		if len(e.Devices) > 0 {
			devs = strings.Join(e.Devices, ", ")
		}
		b.WriteString("; V4L2 stateful H.264 devices: " + devs)
	}
	return b.String()
}

// Unwrap returns ErrUnavailable.
func (e *UnavailableError) Unwrap() error { return ErrUnavailable }

// codeText prints an HRESULT (stored as its unsigned value) in hex and an
// OSStatus (negative) in decimal.
func codeText(c int64) string {
	if c > 0 {
		return fmt.Sprintf("0x%08x", c)
	}
	return fmt.Sprint(c)
}

// Profile is an H.264 profile.
type Profile int

// Profiles.
const (
	ConstrainedBaseline Profile = iota + 1 // profile_idc 66 with constraint_set1
	Main                                   // profile_idc 77
	High                                   // profile_idc 100
)

// String returns the profile name.
func (p Profile) String() string {
	switch p {
	case ConstrainedBaseline:
		return "Constrained Baseline"
	case Main:
		return "Main"
	case High:
		return "High"
	}
	return fmt.Sprintf("Profile(%d)", int(p))
}

// Support describes one direction of the system codec.
type Support struct {
	Available bool
	// Hardware reports that the system has a hardware codec for this direction.
	Hardware bool
	// Engine is "VideoToolbox" or "Media Foundation".
	Engine string
	// Profiles lists what the engine produces (encode) or accepts (decode). On
	// Windows the encode list is probed at run time; on macOS it comes from
	// the API and each encoder confirms its profile at the first keyframe.
	Profiles []Profile
	// MaxWidth and MaxHeight are the limits the system vendor documents, 0
	// when it documents none. No system documents or reports a frame-rate
	// limit, so none is reported.
	MaxWidth, MaxHeight int
	// Err says why Available is false.
	Err error
}

// Limits is the encode and decode support of this system.
type Limits struct{ Encode, Decode Support }

// Packet is one access unit: Annex B, 4-byte start codes, NAL types 1, 5, 7
// and 8 only, one picture. A keyframe starts with an SPS and a PPS, then an
// IDR picture.
type Packet struct {
	Data []byte
	// Keyframe is set by the encoder; Decode ignores it and reads the NAL types.
	Keyframe bool
	// Timestamp and Duration pass through unchanged.
	Timestamp time.Duration
	Duration  time.Duration
}

// engine is one system codec. The engine file built for a target sets
// platform in its init function.
type engine interface {
	name() string
	caps() Limits
	newEncoder(p encParams) (encImpl, error)
	newDecoder(p decParams) (decImpl, error)
}

type encParams struct {
	width, height int
	fps           float64
	bps           int
	profile       Profile
	keyInterval   time.Duration // negative: on request only
}

type encImpl interface {
	// params returns the SPS and PPS the engine knows before the first frame
	// (Windows), or nil.
	params() [][]byte
	// encode submits one I420 picture and calls emit at most once with the
	// access unit's NAL units (no start codes or length prefixes) and the SPS
	// and PPS the engine holds; the slices are valid only during emit. pts
	// and dur are in 90 kHz ticks.
	encode(y, u, v []byte, yStride, uvStride int, pts, dur int64, key bool,
		emit func(nals, params [][]byte, key bool)) error
	setBitrate(bps int) error
	hardware() bool
	close()
}

type decParams struct{ maxPixels int }

// accessUnit is one validated picture. Every NAL unit is without start code
// or length prefix and valid only during decode.
type accessUnit struct {
	key bool
	// sps and pps are set on a keyframe only: every SPS normalized, every PPS
	// as received.
	sps, pps [][]byte
	slices   [][]byte // as received, emulation prevention bytes kept
	// width and height are the cropped picture size of the active SPS;
	// codedWidth and codedHeight are its size in macroblocks times 16.
	width, height           int
	codedWidth, codedHeight int
}

// picture is a decoded image in the engine's memory, valid until the next
// call. width and height are the buffer's frame size (Windows: the padded
// coded size); the cropped picture is its top-left corner. For NV12, u holds
// the interleaved chroma plane and v is nil.
type picture struct {
	width, height     int
	y, u, v           []byte
	yStride, uvStride int
	nv12              bool
}

type decImpl interface {
	// decode takes one validated access unit and returns ok false when the
	// system gives no picture for it.
	decode(au accessUnit) (pic picture, ok bool, err error)
	hardware() bool
	close()
}

// codecError is a failure of a system codec call, or a check of its output
// when code is 0. kind is ErrCorrupt, ErrTooLarge, ErrProfile or ErrCodec.
type codecError struct {
	engine, op string
	code       int64
	kind       error
}

func (e *codecError) Error() string {
	if e.code == 0 {
		return fmt.Sprintf("kolovideo/h264: %s: %s", e.engine, e.op)
	}
	return fmt.Sprintf("kolovideo/h264: %s: %s failed with code %s", e.engine, e.op, codeText(e.code))
}

func (e *codecError) Unwrap() error { return e.kind }

// platform is the engine of this target, set by its engine file.
var platform engine

var engines struct {
	sync.Mutex
	eng  engine
	caps *Limits
}

// current returns the engine of this target, or one that reports absence.
func current() engine {
	engines.Lock()
	defer engines.Unlock()
	if engines.eng == nil {
		engines.eng = platform
		if engines.eng == nil {
			engines.eng = noEngine{&UnavailableError{
				GOOS: runtime.GOOS, GOARCH: runtime.GOARCH,
				Reason: "no H.264 engine on " + runtime.GOOS + "/" + runtime.GOARCH,
			}}
		}
	}
	return engines.eng
}

// noEngine reports err for every direction.
type noEngine struct{ err *UnavailableError }

func (n noEngine) name() string { return "" }

func (n noEngine) caps() Limits {
	return Limits{Encode: Support{Err: n.err}, Decode: Support{Err: n.err}}
}

func (n noEngine) newEncoder(encParams) (encImpl, error) { return nil, n.err }
func (n noEngine) newDecoder(decParams) (decImpl, error) { return nil, n.err }
