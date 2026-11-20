//go:build !linux && !darwin && !(windows && (amd64 || arm64))

package kolovideo

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
)

func TestUnsupportedPlatformMessage(t *testing.T) {
	want := runtime.GOOS + "/" + runtime.GOARCH
	for name, err := range map[string]error{
		"Devices":       func() error { _, err := Devices(); return err }(),
		"RequestAccess": RequestAccess(context.Background()),
		"OpenCapture":   func() error { _, err := OpenCapture(CaptureConfig{}); return err }(),
	} {
		if !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want ErrUnsupported naming %s", name, err, want)
		}
	}
}
