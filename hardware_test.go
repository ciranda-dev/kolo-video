package kolovideo

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// TestRealDevices lists the real cameras. Listing never opens a camera or
// asks for access.
func TestRealDevices(t *testing.T) {
	devs, err := platformDevices()
	if errors.Is(err, ErrUnsupported) || errors.Is(err, ErrPermission) {
		t.Skip(err)
	}
	if err != nil {
		t.Fatal(err)
	}
	defaults := 0
	for _, d := range devs {
		if d.ID == "" {
			t.Errorf("device %q has no ID", d.Name)
		}
		if d.Default {
			defaults++
		}
		t.Logf("%q %q default=%v modes=%d", d.ID, d.Name, d.Default, len(d.Modes))
		for _, m := range d.Modes {
			t.Logf("  %dx%d up to %.2f fps %v", m.Width, m.Height, m.FrameRate, m.Format)
			if m.Width <= 0 || m.Height <= 0 || m.FrameRate < 0 {
				t.Errorf("bad mode %+v", m)
			}
		}
	}
	if len(devs) > 0 && defaults > 1 {
		t.Errorf("%d default devices", defaults)
	}
}

// TestRealCapture opens the default camera, or the first one that is not a
// phone. It runs only with KOLO_VIDEO_HW_TEST=1 because it turns the camera on.
func TestRealCapture(t *testing.T) {
	if os.Getenv("KOLO_VIDEO_HW_TEST") != "1" {
		t.Skip("set KOLO_VIDEO_HW_TEST=1 to capture from a real camera")
	}
	devs, err := platformDevices()
	if err != nil {
		t.Skip(err)
	}
	id := ""
	for _, d := range devs {
		if strings.Contains(d.Name, "iPhone") || strings.Contains(d.Name, "iPad") {
			continue // a phone camera wakes the phone
		}
		if id == "" || d.Default {
			id = d.ID
		}
	}
	if id == "" {
		t.Skip("no camera other than a phone")
	}
	c, err := OpenCapture(CaptureConfig{DeviceID: id})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	t.Logf("device %q mode %+v", c.Device().Name, c.Mode())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var last time.Duration
	for i := range 30 {
		f, err := c.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Validate(); err != nil {
			t.Fatal(err)
		}
		if i > 0 && f.Timestamp <= last {
			t.Fatalf("timestamp %v after %v", f.Timestamp, last)
		}
		last = f.Timestamp
	}
	t.Logf("stats %+v", c.Stats())
}
