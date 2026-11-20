//go:build darwin

package kolovideo

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
)

// avf is the AVFoundation the backend uses; tests replace it.
var avf = func() (avfAPI, error) { return loadAVF() }

func platformDevices() ([]Device, error) {
	api, err := avf()
	if err != nil {
		return nil, err
	}
	list, err := api.devices()
	if err != nil {
		return nil, err
	}
	devs := make([]Device, 0, len(list))
	for _, d := range list {
		devs = append(devs, Device{ID: d.ID, Name: d.Name, Default: d.Default, Modes: avfModes(d.Formats)})
	}
	return devs, nil
}

func authError(status int) error {
	switch status {
	case avAuthNotDetermined:
		return fmt.Errorf("%w: not requested yet; call RequestAccess", ErrPermission)
	case avAuthRestricted:
		return fmt.Errorf("%w: restricted by policy", ErrPermission)
	case avAuthDenied:
		return fmt.Errorf("%w: denied in System Settings", ErrPermission)
	}
	return nil
}

func platformRequestAccess(ctx context.Context) error {
	api, err := avf()
	if err != nil {
		return err
	}
	if s := api.authorizationStatus(); s != avAuthNotDetermined {
		return authError(s)
	}
	ch := make(chan bool, 1)
	if err := api.requestAccess(func(granted bool) {
		select {
		case ch <- granted:
		default:
		}
	}); err != nil {
		return err
	}
	select {
	case granted := <-ch:
		if !granted {
			return authError(avAuthDenied)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// darwinTarget is what the delegate of a capture resolves to.
type darwinTarget struct {
	sink      frameSink
	maxPixels int
	dur       atomic.Int64
	clock     atomic.Uintptr
}

// lost reports a notification from the session.
func (t *darwinTarget) lost(kind int, code int64) {
	switch kind {
	case avfLostDisconnect:
		t.sink.fail(fmt.Errorf("%w: camera disconnected", ErrDeviceLost))
	case avfLostAccess:
		t.sink.fail(fmt.Errorf("%w: revoked while capturing", ErrPermission))
	default:
		t.sink.fail(fmt.Errorf("%w: capture session error %d", ErrDeviceLost, code))
	}
}

// darwinCallbacks maps the ident stored in each delegate to its capture. A
// late callback after remove finds nothing and is ignored.
var darwinCallbacks = &targetRegistry{m: map[uintptr]*darwinTarget{}}

type targetRegistry struct {
	mu   sync.RWMutex
	next uintptr
	m    map[uintptr]*darwinTarget
}

func (r *targetRegistry) add(t *darwinTarget) uintptr {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.next++
	r.m[r.next] = t
	return r.next
}

func (r *targetRegistry) get(id uintptr) *darwinTarget {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.m[id]
}

func (r *targetRegistry) size() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.m)
}

func (r *targetRegistry) remove(id uintptr) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.m, id)
}

type darwinBackend struct {
	session avfSession
	id      uintptr
	once    sync.Once
}

func platformOpen(dev Device, cfg CaptureConfig, sink frameSink) (backend, Mode, error) {
	api, err := avf()
	if err != nil {
		return nil, Mode{}, err
	}
	return openSession(api, dev, cfg, sink)
}

// openSession starts capture with the format matching the chosen mode. It
// checks the authorization status first and never asks for access.
//
// An Objective-C exception ends the process and cannot be caught, so every
// documented precondition is checked before the call that raises: canAddInput
// and canAddOutput before adding, the active format taken from this device's
// formats, the frame duration inside the format's range, the configuration
// lock held while those are set, and begin and commit paired.
func openSession(api avfAPI, dev Device, cfg CaptureConfig, sink frameSink) (backend, Mode, error) {
	mode, _, err := chooseMode(dev.Modes, cfg)
	if err != nil {
		return nil, Mode{}, err
	}
	if status := api.authorizationStatus(); status != avAuthAuthorized {
		if err := authError(status); err != nil {
			return nil, Mode{}, err
		}
		return nil, Mode{}, fmt.Errorf("%w: error %d", ErrPermission, status)
	}
	s, err := api.openDevice(dev.ID)
	if err != nil {
		return nil, Mode{}, err
	}
	if s == nil {
		return nil, Mode{}, fmt.Errorf("%w: %q", ErrNoDevice, dev.ID)
	}
	format, rng := bestFormat(s.formats(), mode)
	if format < 0 {
		s.release()
		return nil, Mode{}, fmt.Errorf("%w: %dx%d %v", ErrUnsupportedFormat, mode.Width, mode.Height, mode.Format)
	}
	dur := avfFrameDuration(rng, mode.FrameRate)

	t := &darwinTarget{sink: sink, maxPixels: cfg.MaxPixels}
	id := darwinCallbacks.add(t)
	done := false
	defer func() {
		if !done {
			s.release()
			darwinCallbacks.remove(id)
		}
	}()
	if err := startSession(s, id, format, dur, t, dev.ID); err != nil {
		return nil, Mode{}, err
	}
	t.clock.Store(s.syncClock())
	mode.FrameRate = activeRate(s, mode.FrameRate)
	t.dur.Store(int64(frameDuration(mode.FrameRate)))
	done = true
	return &darwinBackend{session: s, id: id}, mode, nil
}

// bestFormat returns the index of the format with the mode's size and pixel
// format that reaches the highest frame rate, and that rate range.
func bestFormat(formats []avfFormat, m Mode) (int, avfRange) {
	best := -1
	var rng avfRange
	for i, f := range formats {
		if f.Width != m.Width || f.Height != m.Height || avfPixelFormat(f.FourCC) != m.Format {
			continue
		}
		for _, r := range f.Ranges {
			if best < 0 || r.MaxRate > rng.MaxRate {
				best, rng = i, r
			}
		}
	}
	return best, rng
}

func activeRate(s avfSession, fallback float64) float64 {
	if d := s.activeMinFrameDuration(); d.numeric() && d.seconds() > 0 {
		return 1 / d.seconds()
	}
	return fallback
}

func startFailed(id string, code int64) error {
	return fmt.Errorf("kolovideo: start capture on %q failed (error %d)", id, code)
}

// startSession configures and starts s; on error the caller releases it.
func startSession(s avfSession, ident uintptr, format int, dur avfTime, t *darwinTarget, devID string) error {
	if code, ok := s.newInput(); !ok {
		if code == avfNotAuthorized {
			return fmt.Errorf("%w: error %d", ErrPermission, code)
		}
		return startFailed(devID, code)
	}
	s.beginConfiguration()
	if !s.canAddInput() {
		s.commitConfiguration()
		return startFailed(devID, 0)
	}
	s.addInput()
	s.setDelegate(ident)
	if !s.canAddOutput() {
		s.commitConfiguration()
		return startFailed(devID, 0)
	}
	s.addOutput()
	pf, ok := outputFormat(s.pixelFormats())
	if !ok {
		s.commitConfiguration()
		return fmt.Errorf("%w: the camera output offers neither NV12 nor BGRA", ErrUnsupportedFormat)
	}
	s.setPixelFormat(pf)
	if code, ok := s.lockDevice(); !ok {
		s.commitConfiguration()
		return startFailed(devID, code)
	}
	s.setActiveFormat(format)
	s.setMinFrameDuration(dur)
	s.commitConfiguration()
	s.observe(t.lost)
	s.start()
	s.unlockDevice()
	if !s.running() {
		return startFailed(devID, 0)
	}
	return nil
}

// outputFormat prefers NV12, the cheapest format to convert, and falls back
// to BGRA; a format the output does not list would raise an exception.
func outputFormat(available []uint32) (uint32, bool) {
	for _, want := range []uint32{fccNV12, fccBGRA} {
		for _, f := range available {
			if f == want {
				return want, true
			}
		}
	}
	return 0, false
}

func (b *darwinBackend) close() error {
	b.once.Do(func() {
		b.session.release()
		darwinCallbacks.remove(b.id)
	})
	return nil
}
