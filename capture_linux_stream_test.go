//go:build linux

package kolovideo

import (
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const fakeNode = "/dev/video7"

func fakeConfig(t *testing.T, w, h int, rate float64) CaptureConfig {
	t.Helper()
	cfg, err := CaptureConfig{DeviceID: fakeNode, Width: w, Height: h, FrameRate: rate}.withDefaults()
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// openFake opens the fake device through the real backend code.
func openFake(t *testing.T, f *fakeV4L2, w, h int) (backend, Mode, *v4l2Sink, error) {
	t.Helper()
	f.install(t)
	sink := newV4L2Sink()
	b, m, err := platformOpen(Device{ID: fakeNode}, fakeConfig(t, w, h, 30), sink)
	return b, m, sink, err
}

func yuyvData(w, h int, y byte) []byte {
	d := make([]byte, 2*w*h)
	for i := 0; i < len(d); i += 2 {
		d[i], d[i+1] = y, 128
	}
	return d
}

func nv12Frame(w, h int, y byte) []byte {
	d := make([]byte, w*h*3/2)
	for i := range w * h {
		d[i] = y
	}
	for i := w * h; i < len(d); i++ {
		d[i] = 128
	}
	return d
}

// feed hands the driver one frame, waiting for a queued buffer, and returns the delivered frame.
func feed(t *testing.T, f *fakeV4L2, s *v4l2Sink, data []byte, flags uint32, ts time.Duration) *Frame {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for !f.frame(data, flags, ts) {
		select {
		case <-deadline:
			t.Fatal("the driver never had a queued buffer")
		case <-time.After(time.Millisecond):
		}
	}
	return next(t, s)
}

func next(t *testing.T, s *v4l2Sink) *Frame {
	t.Helper()
	select {
	case fr := <-s.frames:
		return fr
	case err := <-s.failed:
		t.Fatalf("capture failed: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("no frame")
	}
	return nil
}

func waitFail(t *testing.T, s *v4l2Sink) error {
	t.Helper()
	select {
	case err := <-s.failed:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("the capture did not fail")
	}
	return nil
}

func TestV4L2LoopDelivers(t *testing.T) {
	for name, tc := range map[string]struct {
		code uint32
		data func(y byte) []byte
	}{
		"YUYV": {fourcc("YUYV"), func(y byte) []byte { return yuyvData(64, 48, y) }},
		"NV12": {fourcc("NV12"), func(y byte) []byte { return nv12Frame(64, 48, y) }},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeV4L2(fakeNode)
			f.formats = f.formats[:0]
			f.formats = append(f.formats, fakeFormat{code: tc.code, sizes: [][2]uint32{{64, 48}}, rates: [][2]uint32{{1, 30}}})
			b, mode, sink, err := openFake(t, f, 64, 48)
			if err != nil {
				t.Fatal(err)
			}
			if mode.Width != 64 || mode.Height != 48 || mode.FrameRate != 30 {
				t.Fatalf("mode %+v", mode)
			}
			want := []string{"open", "s_fmt", "s_parm", "reqbufs", "mmap", "qbuf", "mmap", "qbuf", "mmap", "qbuf", "mmap", "qbuf", "streamon"}
			if got := f.callList(); !slices.Equal(got[:len(want)], want) {
				t.Fatalf("calls %v, want %v", got, want)
			}
			// More frames than buffers: every buffer must come back to the driver.
			for i := range 3 * v4l2Buffers {
				y := byte(40 + i)
				fr := feed(t, f, sink, tc.data(y), v4l2BufFlagTsMonotonic, time.Duration(i+1)*time.Second)
				if fr.Width != 64 || fr.Height != 48 || fr.Timestamp != time.Duration(i+1)*time.Second ||
					fr.Duration != frameDuration(30) {
					t.Fatalf("frame %d: %dx%d ts %v dur %v", i, fr.Width, fr.Height, fr.Timestamp, fr.Duration)
				}
				if got := fr.Y[0]; got < y-2 || got > y+2 {
					t.Fatalf("frame %d: luma %d, want about %d", i, got, y)
				}
			}
			if sink.invalidCount() != 0 {
				t.Fatalf("%d invalid frames", sink.invalidCount())
			}
			if err := b.close(); err != nil {
				t.Fatal(err)
			}
			calls := f.callList()
			off := slices.Index(calls, "streamoff")
			if off < 0 || slices.Index(calls, "munmap") < off {
				t.Fatalf("close did not stop streaming before unmapping: %v", calls)
			}
			f.check(t)
		})
	}
}

func TestV4L2LoopTimestamps(t *testing.T) {
	f := newFakeV4L2(fakeNode)
	b, _, sink, err := openFake(t, f, 64, 48)
	if err != nil {
		t.Fatal(err)
	}
	defer b.close()
	data := yuyvData(64, 48, 90)
	if got := feed(t, f, sink, data, v4l2BufFlagTsMonotonic, 7*time.Second).Timestamp; got != 7*time.Second {
		t.Errorf("monotonic driver timestamp %v", got)
	}
	// Another clock is not trusted: the arrival time on the monotonic clock is used.
	before := monotonicNow()
	got := feed(t, f, sink, data, 0x00004000, 3*time.Second).Timestamp
	if got < before || got > monotonicNow() {
		t.Errorf("timestamp %v is not the arrival time (%v)", got, before)
	}
}

func TestV4L2LoopRejectsBadBuffers(t *testing.T) {
	f := newFakeV4L2(fakeNode)
	b, _, sink, err := openFake(t, f, 64, 48)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		b.close()
		f.check(t)
	}()
	good := yuyvData(64, 48, 100)
	f.mu.Lock()
	buflen := f.bufLen
	f.mu.Unlock()
	for name, rec := range map[string]v4l2Buffer{
		"driver error flag":      {Index: 0, BytesUsed: 6144, Flags: v4l2BufFlagError},
		"more bytes than mapped": {Index: 1, BytesUsed: buflen + 1},
		"huge byte count":        {Index: 2, BytesUsed: 0xffffffff},
		"short image":            {Index: 3, BytesUsed: 10},
	} {
		before := sink.invalidCount()
		f.mu.Lock()
		f.bufs[rec.Index].queued = true
		f.mu.Unlock()
		rec.Type = v4l2BufTypeVideoCapture
		f.raw(rec)
		deadline := time.After(5 * time.Second)
		for sink.invalidCount() == before {
			select {
			case <-deadline:
				t.Fatalf("%s: not counted as invalid", name)
			case err := <-sink.failed:
				t.Fatalf("%s: capture failed: %v", name, err)
			case <-time.After(time.Millisecond):
			}
		}
		// The capture goes on: the next good frame is delivered.
		if fr := feed(t, f, sink, good, v4l2BufFlagTsMonotonic, time.Second); fr.Width != 64 {
			t.Fatalf("%s: next frame %dx%d", name, fr.Width, fr.Height)
		}
	}
}

func TestV4L2LoopBufferIndexOutOfRange(t *testing.T) {
	// Equal to the buffer count (the first index past the end) and far past it.
	for _, past := range []uint32{0, 95} {
		f := newFakeV4L2(fakeNode)
		b, _, sink, err := openFake(t, f, 64, 48)
		if err != nil {
			t.Fatal(err)
		}
		f.mu.Lock()
		index := uint32(len(f.bufs)) + past
		f.mu.Unlock()
		f.raw(v4l2Buffer{Index: index, Type: v4l2BufTypeVideoCapture, BytesUsed: 100})
		if err := waitFail(t, sink); !errors.Is(err, ErrDeviceLost) {
			t.Fatalf("index %d: err = %v, want ErrDeviceLost", index, err)
		}
		if sink.invalidCount() != 1 {
			t.Errorf("index %d: %d invalid frames, want 1", index, sink.invalidCount())
		}
		b.close()
		f.check(t)
	}
}

func TestV4L2LoopUnplug(t *testing.T) {
	f := newFakeV4L2(fakeNode)
	b, _, sink, err := openFake(t, f, 64, 48)
	if err != nil {
		t.Fatal(err)
	}
	feed(t, f, sink, yuyvData(64, 48, 90), v4l2BufFlagTsMonotonic, time.Second)
	f.unplug()
	if err := waitFail(t, sink); !errors.Is(err, ErrDeviceLost) || !errors.Is(err, unix.ENODEV) {
		t.Fatalf("err = %v, want ErrDeviceLost wrapping ENODEV", err)
	}
	if err := b.close(); err != nil {
		t.Fatal(err)
	}
	f.check(t)
}

func TestV4L2LoopQueueFailureEndsCapture(t *testing.T) {
	f := newFakeV4L2(fakeNode)
	b, _, sink, err := openFake(t, f, 64, 48)
	if err != nil {
		t.Fatal(err)
	}
	feed(t, f, sink, yuyvData(64, 48, 90), v4l2BufFlagTsMonotonic, time.Second)
	f.mu.Lock()
	f.failIoctl[vidiocQBuf] = unix.EIO
	f.mu.Unlock()
	f.frame(yuyvData(64, 48, 90), v4l2BufFlagTsMonotonic, 2*time.Second)
	if err := waitFail(t, sink); !errors.Is(err, ErrDeviceLost) || !errors.Is(err, unix.EIO) {
		t.Fatalf("err = %v", err)
	}
	b.close()
	f.mu.Lock()
	delete(f.failIoctl, vidiocQBuf)
	f.mu.Unlock()
	f.check(t)
}

func TestV4L2CloseWhileWaiting(t *testing.T) {
	f := newFakeV4L2(fakeNode)
	b, _, _, err := openFake(t, f, 64, 48)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		b.close()
		b.close() // idempotent
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return while the loop waits for a frame")
	}
	f.check(t)
}

func TestV4L2CloseRacingFrames(t *testing.T) {
	f := newFakeV4L2(fakeNode)
	b, _, sink, err := openFake(t, f, 64, 48)
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		data := yuyvData(64, 48, 80)
		for {
			select {
			case <-stop:
				return
			default:
				f.frame(data, v4l2BufFlagTsMonotonic, time.Second)
				time.Sleep(50 * time.Microsecond)
			}
		}
	}()
	next(t, sink)
	b.close()
	close(stop)
	wg.Wait()
	f.check(t)
}

func TestV4L2DriverChangesFormat(t *testing.T) {
	t.Run("smaller size", func(t *testing.T) {
		f := newFakeV4L2(fakeNode)
		f.adjust = func(p *v4l2PixFormat) { p.Width, p.Height = 32, 24 }
		b, mode, sink, err := openFake(t, f, 64, 48)
		if err != nil {
			t.Fatal(err)
		}
		if mode.Width != 32 || mode.Height != 24 {
			t.Fatalf("mode %+v, want the driver's 32x24", mode)
		}
		fr := feed(t, f, sink, yuyvData(32, 24, 70), v4l2BufFlagTsMonotonic, time.Second)
		if fr.Width != 32 || fr.Height != 24 {
			t.Fatalf("frame %dx%d", fr.Width, fr.Height)
		}
		b.close()
		f.check(t)
	})
	for name, adjust := range map[string]func(*v4l2PixFormat){
		"another pixel format":  func(p *v4l2PixFormat) { p.PixelFormat = fourcc("MJPG") },
		"above MaxPixels":       func(p *v4l2PixFormat) { p.Width, p.Height = 65536, 65536 },
		"above the frame limit": func(p *v4l2PixFormat) { p.Width = 1 << 20 },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeV4L2(fakeNode)
			f.adjust = adjust
			f.install(t)
			cfg := fakeConfig(t, 64, 48, 30)
			_, _, err := platformOpen(Device{ID: fakeNode}, cfg, newV4L2Sink())
			if !errors.Is(err, ErrUnsupportedFormat) {
				t.Fatalf("err = %v, want ErrUnsupportedFormat", err)
			}
			if slices.Contains(f.callList(), "streamon") {
				t.Fatal("streaming started")
			}
			f.check(t)
		})
	}
}

func TestV4L2FrameRate(t *testing.T) {
	for name, tc := range map[string]struct {
		den  uint32
		want float64
	}{"driver reports 25": {25000, 25}, "driver reports none": {0, 30}} {
		t.Run(name, func(t *testing.T) {
			f := newFakeV4L2(fakeNode)
			f.parmDen = tc.den
			b, mode, _, err := openFake(t, f, 64, 48)
			if err != nil {
				t.Fatal(err)
			}
			if mode.FrameRate != tc.want {
				t.Errorf("rate %v, want %v", mode.FrameRate, tc.want)
			}
			b.close()
			f.check(t)
		})
	}
}

func TestV4L2StartFailuresRelease(t *testing.T) {
	for name, arm := range map[string]func(*fakeV4L2){
		"set format":         func(f *fakeV4L2) { f.failIoctl[vidiocSFmt] = unix.EBUSY },
		"request buffers":    func(f *fakeV4L2) { f.failIoctl[vidiocReqBufs] = unix.ENOMEM },
		"one buffer granted": func(f *fakeV4L2) { f.maxBufs = 1 },
		"no buffers granted": func(f *fakeV4L2) { f.maxBufs = 0 },
		"query buffer":       func(f *fakeV4L2) { f.failIoctl[vidiocQueryBuf] = unix.EINVAL },
		"map the first":      func(f *fakeV4L2) { f.failMmap = unix.ENOMEM },
		"map the third":      func(f *fakeV4L2) { f.failMmap, f.failMmapAt = unix.ENOMEM, 3 },
		"queue buffer":       func(f *fakeV4L2) { f.failIoctl[vidiocQBuf] = unix.EIO },
		"start streaming":    func(f *fakeV4L2) { f.failIoctl[vidiocStreamOn] = unix.ENOSPC },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeV4L2(fakeNode)
			arm(f)
			_, _, _, err := openFake(t, f, 64, 48)
			if err == nil {
				t.Fatal("open succeeded")
			}
			f.check(t)
		})
	}
}

func TestV4L2OpenErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want error
	}{
		"permission": {unix.EACCES, ErrPermission},
		"not root":   {unix.EPERM, ErrPermission},
		"unplugged":  {unix.ENODEV, ErrNoDevice},
		"missing":    {unix.ENOENT, ErrNoDevice},
		"no address": {unix.ENXIO, ErrNoDevice},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeV4L2(fakeNode)
			f.failOpen = tc.err
			if _, _, _, err := openFake(t, f, 64, 48); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			f.check(t)
		})
	}
	t.Run("busy", func(t *testing.T) {
		f := newFakeV4L2(fakeNode)
		f.failOpen = unix.EBUSY
		_, _, _, err := openFake(t, f, 64, 48)
		if err == nil || errors.Is(err, ErrNoDevice) || errors.Is(err, ErrPermission) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestV4L2ProbeFake(t *testing.T) {
	t.Run("modes", func(t *testing.T) {
		f := newFakeV4L2(fakeNode)
		f.formats = append(f.formats, fakeFormat{code: fourcc("MJPG"), sizes: [][2]uint32{{64, 48}, {128, 96}}, rates: [][2]uint32{{1, 30}, {1, 15}}})
		f.formats = append(f.formats, fakeFormat{code: fourcc("BA81")}) // a format this package cannot convert
		f.install(t)
		d, ok, err := probeV4L2(f, fakeNode)
		if err != nil || !ok || d.ID != fakeNode || d.Name != "Fake Capture" {
			t.Fatalf("%+v %v %v", d, ok, err)
		}
		if calls := f.callList(); !slices.Equal(calls, []string{"open-ro"}) {
			t.Fatalf("listing opened %v, want one read-only open", calls)
		}
		want := []Mode{{64, 48, 30, FormatYUY2}, {64, 48, 30, FormatNV12}, {64, 48, 30, FormatMJPEG}, {128, 96, 30, FormatMJPEG}}
		if !slices.Equal(d.Modes, want) {
			t.Fatalf("modes %v, want %v", d.Modes, want)
		}
		f.check(t)
	})
	t.Run("stepwise", func(t *testing.T) {
		f := newFakeV4L2(fakeNode)
		f.formats = []fakeFormat{{code: fourcc("YUYV"), steps: &[6]uint32{16, 1920, 8, 16, 1080, 8}, rates: [][2]uint32{{1, 60}}}}
		f.install(t)
		d, ok, err := probeV4L2(f, fakeNode)
		if err != nil || !ok || len(d.Modes) != 1 || d.Modes[0].Width != 1920 || d.Modes[0].Height != 1080 || d.Modes[0].FrameRate != 60 {
			t.Fatalf("%+v %v %v", d, ok, err)
		}
	})
	t.Run("not a capture device", func(t *testing.T) {
		f := newFakeV4L2(fakeNode)
		f.caps = v4l2CapVideoCapture | v4l2CapDeviceCaps // no streaming
		f.install(t)
		if _, ok, err := probeV4L2(f, fakeNode); ok || err != nil {
			t.Fatalf("%v %v", ok, err)
		}
		f.check(t)
	})
	t.Run("an enumeration that never ends", func(t *testing.T) {
		f := newFakeV4L2(fakeNode)
		f.neverEnd = true
		f.install(t)
		fd, err := f.open(fakeNode, false)
		if err != nil {
			t.Fatal(err)
		}
		if codes := v4l2Formats(f, fd); len(codes) != v4l2MaxEnum {
			t.Fatalf("%d formats, want the bound %d", len(codes), v4l2MaxEnum)
		}
		f.close(fd)
		f.check(t)
	})
	t.Run("busy", func(t *testing.T) {
		f := newFakeV4L2(fakeNode)
		f.failOpen = unix.EBUSY
		f.install(t)
		d, ok, err := probeV4L2(f, fakeNode)
		if err != nil || !ok || d.ID != fakeNode || len(d.Modes) != 0 {
			t.Fatalf("%+v %v %v", d, ok, err)
		}
	})
	t.Run("busy query", func(t *testing.T) {
		f := newFakeV4L2(fakeNode)
		f.failIoctl[vidiocQueryCap] = unix.EBUSY
		f.install(t)
		if d, ok, err := probeV4L2(f, fakeNode); err != nil || !ok || len(d.Modes) != 0 {
			t.Fatalf("%+v %v %v", d, ok, err)
		}
		f.check(t)
	})
	t.Run("denied", func(t *testing.T) {
		f := newFakeV4L2(fakeNode)
		f.failOpen = unix.EACCES
		f.install(t)
		if _, ok, err := probeV4L2(f, fakeNode); ok || !errors.Is(err, unix.EACCES) {
			t.Fatalf("%v %v", ok, err)
		}
	})
}

func TestV4L2ListingDoesNotHang(t *testing.T) {
	f := newFakeV4L2(fakeNode)
	f.blockOpen = make(chan struct{})
	f.install(t)
	dir := fakeV4L2Dir(t, "video7")
	probe := func(string) (Device, bool, error) { return probeV4L2(f, fakeNode) }
	_, err := listV4L2(dir, 50*time.Millisecond, probe)
	if !errors.Is(err, ErrDeviceLost) {
		t.Fatalf("hung open: %v", err)
	}
	close(f.blockOpen)
	deadline := time.Now().Add(5 * time.Second)
	for {
		f.mu.Lock()
		left := len(f.fds)
		f.mu.Unlock()
		if left == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the late probe left its descriptor open")
		}
		time.Sleep(time.Millisecond)
	}
	f.check(t)
}

func TestV4L2OpenCaptureDoesNotHang(t *testing.T) {
	f := newFakeV4L2(fakeNode)
	f.blockOpen = make(chan struct{})
	f.install(t)
	cfg := fakeConfig(t, 64, 48, 30)
	cfg.StallTimeout = 50 * time.Millisecond
	_, _, err := platformOpen(Device{ID: fakeNode}, cfg, newV4L2Sink())
	if !errors.Is(err, ErrDeviceLost) {
		t.Fatalf("hung open: %v", err)
	}
	close(f.blockOpen)
	// The setup that finishes late is released: no descriptor, mapping or stream is left.
	deadline := time.Now().Add(5 * time.Second)
	for {
		f.mu.Lock()
		left := len(f.fds) + f.maps + len(f.wakes)
		f.mu.Unlock()
		if left == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the late open left resources behind")
		}
		time.Sleep(time.Millisecond)
	}
	f.check(t)
}
