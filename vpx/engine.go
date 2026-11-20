package vpx

import (
	"fmt"

	kolovideo "github.com/ciranda-dev/kolo-video"
)

type engine interface {
	kind() EngineKind
	version() string
	newEncoder(p encParams) (encImpl, error)
	newDecoder(vp9 bool, threads int) (decImpl, error)
}

type encParams struct {
	vp9                           bool
	width, height, threads, speed int
	kbps, maxIntraPct             uint32
}

type encImpl interface {
	// encode wraps the planes, keeps none of them after it returns, and calls
	// emit for each packet; data is valid only during emit.
	encode(y, u, v []byte, yStride, uvStride int, pts int64, dur uint32, key bool, emit func(data []byte, key bool)) error
	setBitrate(kbps uint32) error
	close()
}

// plane420 is a decoded picture whose planes are valid until the next call.
type plane420 struct {
	width, height     int
	y, u, v           []byte
	yStride, uvStride int
}

type decImpl interface {
	// decode returns ok=false when no frame is shown.
	decode(data []byte) (img plane420, ok bool, err error)
	corrupted() bool
	close()
}

// libvpx error codes (vpx_codec_err_t).
const (
	codecErrMem            = 2
	codecErrIncapable      = 4
	codecErrUnsupBitstream = 5
	codecErrCorruptFrame   = 7
	codecErrInvalidParam   = 8
)

var codecText = [...]string{
	"ok",
	"unspecified internal error",
	"out of memory",
	"ABI version mismatch",
	"codec does not implement the requested capability",
	"bitstream not supported by this decoder",
	"bitstream requires a feature this decoder does not support",
	"corrupt frame detected",
	"invalid parameter",
	"end of iterated list",
}

// codecError is a libvpx error code. It matches ErrCorrupt for a corrupt frame
// or an unsupported bitstream, kolovideo.ErrInvalidConfig for an invalid
// parameter or an unsupported capability, and ErrCodec otherwise.
type codecError struct {
	op   string
	code int32
}

func (e *codecError) Error() string {
	text := "unknown error"
	if e.code >= 0 && int(e.code) < len(codecText) {
		text = codecText[e.code]
	}
	return fmt.Sprintf("kolovideo/vpx: %s: %s (libvpx error %d)", e.op, text, e.code)
}

func (e *codecError) Is(target error) bool {
	corrupt := e.code == codecErrCorruptFrame || e.code == codecErrUnsupBitstream
	invalid := e.code == codecErrInvalidParam || e.code == codecErrIncapable
	switch target {
	case ErrCorrupt:
		return corrupt
	case kolovideo.ErrInvalidConfig:
		return invalid
	case ErrCodec:
		return !corrupt && !invalid
	}
	return false
}

// unavailable is ErrUnavailable with the reason a requested engine cannot run.
type unavailable struct{ cause error }

func (e *unavailable) Error() string {
	const p = "kolovideo/vpx: "
	msg := e.cause.Error()
	if len(msg) > len(p) && msg[:len(p)] == p {
		msg = msg[len(p):]
	}
	return ErrUnavailable.Error() + ": " + msg
}

func (e *unavailable) Is(target error) bool { return target == ErrUnavailable }
func (e *unavailable) Unwrap() error        { return e.cause }
