//go:build linux

package kolovideo

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestV4L2Vivid runs the streaming code against the kernel's vivid virtual capture driver, which makes
// test pictures and has no camera behind it. It runs only with KOLO_VIDEO_V4L2_TEST=1 on a machine
// where `modprobe vivid` has been done, and it never opens a device that is not a vivid one.
func TestV4L2Vivid(t *testing.T) {
	if os.Getenv("KOLO_VIDEO_V4L2_TEST") != "1" {
		t.Skip("set KOLO_VIDEO_V4L2_TEST=1 on a machine with the vivid driver loaded")
	}
	devs, err := platformDevices()
	if err != nil {
		t.Fatal(err)
	}
	opened := 0
	for _, d := range devs {
		if !strings.Contains(strings.ToLower(d.Name), "vivid") || len(d.Modes) == 0 {
			continue
		}
		t.Logf("%s %q: %d modes", d.ID, d.Name, len(d.Modes))
		for _, m := range d.Modes {
			if m.Format == FormatUnknown {
				t.Errorf("mode %+v", m)
			}
		}
		c, err := OpenCapture(CaptureConfig{DeviceID: d.ID, Width: 640, Height: 480, FrameRate: 30})
		if err != nil {
			t.Fatalf("%s: %v", d.ID, err)
		}
		mode := c.Mode()
		t.Logf("%s: mode %+v", d.ID, mode)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		var last time.Duration
		for i := range 40 {
			f, err := c.Read(ctx)
			if err != nil {
				t.Fatalf("%s frame %d: %v", d.ID, i, err)
			}
			if err := f.Validate(); err != nil || f.Width != mode.Width || f.Height != mode.Height {
				t.Fatalf("%s frame %d: %dx%d %v", d.ID, i, f.Width, f.Height, err)
			}
			if f.Timestamp <= last {
				t.Fatalf("%s frame %d: timestamp %v after %v", d.ID, i, f.Timestamp, last)
			}
			last = f.Timestamp
		}
		cancel()
		st := c.Stats()
		t.Logf("%s: %+v", d.ID, st)
		if st.Invalid != 0 {
			t.Errorf("%s: %d invalid frames", d.ID, st.Invalid)
		}
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Read(context.Background()); err == nil {
			t.Fatal("Read after Close succeeded")
		}
		opened++
	}
	if opened == 0 {
		t.Fatal("no vivid capture device: run `sudo modprobe vivid` first")
	}
}
