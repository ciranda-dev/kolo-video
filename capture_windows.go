//go:build windows && (amd64 || arm64)

package kolovideo

import "context"

func platformRequestAccess(context.Context) error { return nil }

func platformDevices() ([]Device, error) { return mfDevices(winMF{}) }

func platformOpen(dev Device, cfg CaptureConfig, sink frameSink) (backend, Mode, error) {
	return mfOpen(winMF{}, dev, cfg, sink)
}
