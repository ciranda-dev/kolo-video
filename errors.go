package kolovideo

import "errors"

var (
	// ErrInvalidConfig means a configuration field is out of range.
	ErrInvalidConfig = errors.New("kolovideo: invalid config")
	// ErrInvalidFrame means a frame or raw image has inconsistent sizes.
	ErrInvalidFrame = errors.New("kolovideo: invalid frame")
	// ErrNoDevice means no camera matches the request.
	ErrNoDevice = errors.New("kolovideo: no such device")
	// ErrUnsupportedFormat means the device or stream offers no format this
	// package can convert.
	ErrUnsupportedFormat = errors.New("kolovideo: unsupported format")
	// ErrPermission means the system has not granted camera access.
	ErrPermission = errors.New("kolovideo: camera access not granted")
	// ErrDeviceLost means the camera was disconnected, failed, or sent no
	// frame within the stall timeout.
	ErrDeviceLost = errors.New("kolovideo: device lost")
	// ErrClosed means the capture, encoder or decoder was closed.
	ErrClosed = errors.New("kolovideo: closed")
	// ErrUnsupported means this platform has no backend for the feature:
	// camera capture on a system other than macOS, Linux, and Windows on
	// amd64 or arm64, or a system whose camera framework is missing.
	ErrUnsupported = errors.New("kolovideo: not supported on this platform")
)
