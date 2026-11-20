//go:build !linux && !darwin && !(windows && (amd64 || arm64))

package kolovideo

import (
	"context"
	"fmt"
	"runtime"
)

func errNoCapture() error {
	return fmt.Errorf("%w: camera capture is not available on %s/%s", ErrUnsupported, runtime.GOOS, runtime.GOARCH)
}

func platformDevices() ([]Device, error) { return nil, errNoCapture() }

func platformOpen(Device, CaptureConfig, frameSink) (backend, Mode, error) {
	return nil, Mode{}, errNoCapture()
}

func platformRequestAccess(context.Context) error { return errNoCapture() }
