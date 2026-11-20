//go:build linux

package kolovideo

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

func TestV4L2StructSizes(t *testing.T) {
	ptr := unsafe.Sizeof(uintptr(0))
	want := map[string][2]uintptr{
		"capability":     {unsafe.Sizeof(v4l2Capability{}), 104},
		"fmtdesc":        {unsafe.Sizeof(v4l2FmtDesc{}), 64},
		"frmsizeenum":    {unsafe.Sizeof(v4l2FrmSizeEnum{}), 44},
		"frmivalenum":    {unsafe.Sizeof(v4l2FrmIvalEnum{}), 52},
		"pix_format":     {unsafe.Sizeof(v4l2PixFormat{}), 48},
		"requestbuffers": {unsafe.Sizeof(v4l2RequestBuffers{}), 20},
		"streamparm":     {unsafe.Sizeof(v4l2StreamParm{}), 204},
		"format":         {unsafe.Sizeof(v4l2Format{}), 200 + ptr},
		"buffer":         {unsafe.Sizeof(v4l2Buffer{}), map[uintptr]uintptr{4: 68, 8: 88}[ptr]},
	}
	for name, s := range want {
		if s[0] != s[1] {
			t.Errorf("struct v4l2_%s is %d bytes, want %d", name, s[0], s[1])
		}
	}
	if iocDirShift != 30 || ptr != 8 {
		return
	}
	// Request numbers on 64-bit systems with the generic encoding.
	for name, c := range map[string][2]uintptr{
		"QUERYCAP":            {vidiocQueryCap, 0x80685600},
		"ENUM_FMT":            {vidiocEnumFmt, 0xc0405602},
		"S_FMT":               {vidiocSFmt, 0xc0d05605},
		"REQBUFS":             {vidiocReqBufs, 0xc0145608},
		"QUERYBUF":            {vidiocQueryBuf, 0xc0585609},
		"QBUF":                {vidiocQBuf, 0xc058560f},
		"DQBUF":               {vidiocDQBuf, 0xc0585611},
		"STREAMON":            {vidiocStreamOn, 0x40045612},
		"STREAMOFF":           {vidiocStreamOff, 0x40045613},
		"S_PARM":              {vidiocSParm, 0xc0cc5616},
		"ENUM_FRAMESIZES":     {vidiocEnumFrameSizes, 0xc02c564a},
		"ENUM_FRAMEINTERVALS": {vidiocEnumFrameIntervals, 0xc034564b},
	} {
		if c[0] != c[1] {
			t.Errorf("VIDIOC_%s = %#x, want %#x", name, c[0], c[1])
		}
	}
}

func TestStepSize(t *testing.T) {
	for _, c := range [][5]uint32{
		{640, 16, 1920, 8, 640},
		{643, 16, 1920, 8, 640},
		{10, 16, 1920, 8, 16},
		{4000, 16, 1920, 8, 1920},
		{641, 1, 1920, 1, 641},
		{641, 1, 1920, 0, 641},
	} {
		if got := stepSize(c[0], c[1], c[2], c[3]); got != c[4] {
			t.Errorf("stepSize(%d, %d, %d, %d) = %d, want %d", c[0], c[1], c[2], c[3], got, c[4])
		}
	}
}

func TestConvertV4L2(t *testing.T) {
	const w, h = 4, 2
	i420 := make([]byte, w*h+2*2*1)
	for i := range i420 {
		i420[i] = byte(i)
	}
	f, err := convertV4L2(FormatI420, i420, w, h, 0)
	if err != nil || f.U[1] != 9 || f.V[0] != 10 {
		t.Fatalf("I420: %v %v", f, err)
	}
	if _, err := convertV4L2(FormatNV12, make([]byte, w*h+w), w, h, w); err != nil {
		t.Fatal(err)
	}
	if _, err := convertV4L2(FormatYUY2, make([]byte, 2*w*h), w, h, 0); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		pf  PixelFormat
		n   int
		bpl int
	}{{FormatI420, 9, 0}, {FormatNV12, 9, 4}, {FormatNV12, 100, 2}, {FormatYUY2, 15, 0}, {FormatUnknown, 100, 0}} {
		if _, err := convertV4L2(c.pf, make([]byte, c.n), w, h, c.bpl); err == nil {
			t.Errorf("%v with %d bytes accepted", c.pf, c.n)
		}
	}
	if _, err := convertV4L2(FormatMJPEG, []byte{1, 2}, w, h, 0); !errors.Is(err, ErrInvalidFrame) {
		t.Fatal(err)
	}
}

func FuzzConvertV4L2(f *testing.F) {
	f.Add(uint8(0), 4, 2, 0, make([]byte, 12))
	f.Fuzz(func(t *testing.T, kind uint8, w, h, bpl int, data []byte) {
		if w <= 0 || h <= 0 || w > 4096 || h > 4096 {
			return
		}
		pf := []PixelFormat{FormatI420, FormatNV12, FormatYUY2, FormatUYVY, FormatMJPEG}[kind%5]
		if fr, err := convertV4L2(pf, data, w, h, bpl); err == nil && fr.Validate() != nil {
			t.Fatal("invalid frame")
		}
	})
}

func TestV4L2PixelFormat(t *testing.T) {
	for code, want := range map[string]PixelFormat{
		"YUYV": FormatYUY2, "UYVY": FormatUYVY, "NV12": FormatNV12, "YU12": FormatI420,
		"MJPG": FormatMJPEG, "JPEG": FormatMJPEG, "RGB3": FormatUnknown, "H264": FormatUnknown,
	} {
		if got := v4l2PixelFormat(fourcc(code)); got != want {
			t.Errorf("%s = %v, want %v", code, got, want)
		}
	}
	if got := v4l2PixelFormat(0); got != FormatUnknown {
		t.Errorf("0 = %v", got)
	}
}

func fakeV4L2Dir(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestListV4L2(t *testing.T) {
	dir := fakeV4L2Dir(t, "video10", "video2", "video0", "video1", "video01", "video+1", "video-1", "videoX", "video", "vbi0", "media0", "xvideo3")
	var mu sync.Mutex
	var probed []string
	devs, err := listV4L2(dir, time.Second, func(path string) (Device, bool, error) {
		mu.Lock()
		probed = append(probed, filepath.Base(path))
		mu.Unlock()
		switch filepath.Base(path) {
		case "video1":
			return Device{}, false, nil
		case "video2":
			return Device{ID: path, Name: "cam", Modes: []Mode{{640, 480, 30, FormatYUY2}}}, true, nil
		}
		return Device{ID: path, Name: "meta"}, true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(probed)
	if want := []string{"video0", "video1", "video10", "video2"}; !slices.Equal(probed, want) {
		t.Fatalf("probed %v, want %v", probed, want)
	}
	if len(devs) != 3 {
		t.Fatalf("%d devices", len(devs))
	}
	for i, d := range devs {
		if want := d.Name == "cam"; d.Default != want {
			t.Errorf("device %d %q default=%v", i, d.Name, d.Default)
		}
	}
	if devs[1].ID != filepath.Join(dir, "video2") {
		t.Errorf("ID %q", devs[1].ID)
	}
}

func TestListV4L2HungNode(t *testing.T) {
	dir := fakeV4L2Dir(t, "video0", "video1")
	release := make(chan struct{})
	defer close(release)
	probe := func(path string) (Device, bool, error) {
		if filepath.Base(path) == "video0" {
			<-release
		}
		return Device{ID: path, Modes: []Mode{{640, 480, 30, FormatNV12}}}, true, nil
	}
	start := time.Now()
	devs, err := listV4L2(dir, 50*time.Millisecond, probe)
	if err != nil || len(devs) != 1 || filepath.Base(devs[0].ID) != "video1" {
		t.Fatalf("%+v %v", devs, err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("listing took %v", time.Since(start))
	}
	only := fakeV4L2Dir(t, "video0")
	if _, err := listV4L2(only, 50*time.Millisecond, probe); !errors.Is(err, ErrDeviceLost) {
		t.Fatalf("only node hung: %v", err)
	}
}

func TestListV4L2NoModes(t *testing.T) {
	dir := fakeV4L2Dir(t, "video0")
	devs, err := listV4L2(dir, time.Second, func(path string) (Device, bool, error) { return Device{ID: path}, true, nil })
	if err != nil || len(devs) != 1 || devs[0].Default {
		t.Fatalf("%+v %v", devs, err)
	}
}

func TestListV4L2Errors(t *testing.T) {
	dir := fakeV4L2Dir(t, "video0", "video1")
	denied := func(string) (Device, bool, error) { return Device{}, false, unix.EACCES }
	if _, err := listV4L2(dir, time.Second, denied); !errors.Is(err, ErrPermission) {
		t.Errorf("all denied: %v", err)
	}
	mixed := func(path string) (Device, bool, error) {
		if filepath.Base(path) == "video0" {
			return Device{}, false, unix.EPERM
		}
		return Device{ID: path, Modes: []Mode{{640, 480, 30, FormatNV12}}}, true, nil
	}
	if devs, err := listV4L2(dir, time.Second, mixed); err != nil || len(devs) != 1 {
		t.Errorf("one denied: %v %v", devs, err)
	}
	broken := func(string) (Device, bool, error) { return Device{}, false, unix.EIO }
	if devs, err := listV4L2(dir, time.Second, broken); err != nil || len(devs) != 0 {
		t.Errorf("probe failure: %v %v", devs, err)
	}
	if _, err := listV4L2(filepath.Join(dir, "missing"), time.Second, broken); err == nil {
		t.Error("missing directory listed")
	}
	if devs, err := listV4L2(t.TempDir(), time.Second, broken); err != nil || len(devs) != 0 {
		t.Errorf("empty directory: %v %v", devs, err)
	}
}

func TestProbeV4L2NotADevice(t *testing.T) {
	dir := fakeV4L2Dir(t, "video0")
	if d, ok, err := probeV4L2(osV4L2{}, filepath.Join(dir, "video0")); ok || err == nil {
		t.Errorf("regular file probed as a camera: %+v %v", d, err)
	}
	if _, ok, err := probeV4L2(osV4L2{}, filepath.Join(dir, "gone")); ok || !errors.Is(err, unix.ENOENT) {
		t.Errorf("missing node: %v %v", ok, err)
	}
}
