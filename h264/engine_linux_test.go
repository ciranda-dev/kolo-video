//go:build linux

package h264

import (
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// fakeNode is the driver behind one node of the fake directory.
type fakeNode struct {
	openErr error
	capErr  error
	driver  string
	caps    uint32
	devCaps uint32
	// formats lists the coded formats by buffer type; a type that is absent
	// answers EINVAL at index 0.
	formats map[uint32][]uint32
	// enumErr is returned for a buffer type at the index in enumErrAt.
	enumErr   map[uint32]error
	enumErrAt uint32
	neverEnd  bool
	// block makes QUERYCAP wait until it is closed.
	block chan struct{}
}

// fakeKernel answers open and ioctl for the nodes of a fake directory and
// fails the test on anything but QUERYCAP and ENUM_FMT.
type fakeKernel struct {
	t     testing.TB
	mu    sync.Mutex
	nodes map[string]*fakeNode // by file name
	open  map[int]*fakeNode
	next  int
	opens int
	reqs  []uintptr
}

func newFakeKernel(t testing.TB) *fakeKernel {
	return &fakeKernel{t: t, nodes: map[string]*fakeNode{}, open: map[int]*fakeNode{}, next: 3}
}

func (k *fakeKernel) sys() v4l2Sys {
	return v4l2Sys{
		open: func(path string) (int, error) {
			k.mu.Lock()
			defer k.mu.Unlock()
			n := k.nodes[filepath.Base(path)]
			if n == nil {
				return -1, unix.ENOENT
			}
			if n.openErr != nil {
				return -1, n.openErr
			}
			k.opens++
			fd := k.next
			k.next++
			k.open[fd] = n
			return fd, nil
		},
		ioctl: func(fd int, req uintptr, arg unsafe.Pointer) error {
			k.mu.Lock()
			n := k.open[fd]
			k.reqs = append(k.reqs, req)
			k.mu.Unlock()
			if n == nil {
				k.t.Errorf("ioctl on closed or unknown fd %d", fd)
				return unix.EBADF
			}
			switch req {
			case vidiocQueryCap:
				if n.block != nil {
					<-n.block
				}
				if n.capErr != nil {
					return n.capErr
				}
				c := (*v4l2Capability)(arg)
				*c = v4l2Capability{Capabilities: n.caps, DeviceCaps: n.devCaps}
				copy(c.Driver[:], n.driver)
				copy(c.Card[:], "Fake card")
				return nil
			case vidiocEnumFmt:
				d := (*v4l2FmtDesc)(arg)
				if n.neverEnd {
					d.PixelFormat = fourcc("NV12")
					return nil
				}
				if err := n.enumErr[d.Type]; err != nil && d.Index == n.enumErrAt {
					return err
				}
				list := n.formats[d.Type]
				if int(d.Index) >= len(list) {
					return unix.EINVAL
				}
				d.PixelFormat = list[d.Index]
				return nil
			}
			k.t.Errorf("unexpected ioctl request %#x", req)
			return unix.ENOTTY
		},
		close: func(fd int) {
			k.mu.Lock()
			defer k.mu.Unlock()
			if k.open[fd] == nil {
				k.t.Errorf("close of unknown fd %d", fd)
			}
			delete(k.open, fd)
		},
	}
}

func (k *fakeKernel) openFDs() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.open)
}

// dir makes a directory with a regular file per entry of names and returns
// an engine over it.
func (k *fakeKernel) engine(t *testing.T, names ...string) *linuxEngine {
	t.Helper()
	dir := t.TempDir()
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return &linuxEngine{dir: dir, sys: k.sys(), timeout: 5 * time.Second}
}

const (
	capM2M       = v4l2CapVideoM2M
	capM2MMplane = v4l2CapVideoM2MMplane
	capCapture   = 0x1
	capStreaming = 0x04000000
)

func h264Formats(types ...uint32) map[uint32][]uint32 {
	m := map[uint32][]uint32{}
	for _, t := range types {
		m[t] = []uint32{fourcc("NV12"), v4l2PixFmtH264}
	}
	return m
}

func lines(e *linuxEngine) []string { return e.unavailable().Devices }

func TestNoVideoNodes(t *testing.T) {
	k := newFakeKernel(t)
	names := []string{"video", "video01", "videoX", "video-1", "video+1", "video0x", "videos", "vid", "video 1", "video1.5", "video99999999999999999999", "Video3"}
	for _, n := range names {
		k.nodes[n] = &fakeNode{caps: capM2M, formats: h264Formats(v4l2BufTypeVideoCapture)}
	}
	e := k.engine(t, append(names, "null", "tty0")...)
	if got := lines(e); len(got) != 0 {
		t.Fatalf("devices %q", got)
	}
	if k.opens != 0 {
		t.Fatalf("%d nodes opened, want none", k.opens)
	}
	want := "kolovideo/h264: no H.264 engine on linux/" + runtime.GOARCH + " in this release; V4L2 stateful H.264 devices: none"
	if got := e.unavailable().Error(); got != want {
		t.Fatalf("message\n got %q\nwant %q", got, want)
	}
}

func TestDirectoryErrors(t *testing.T) {
	k := newFakeKernel(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, dir, want string
	}{
		{"missing", filepath.Join(dir, "missing"), ""},
		{"not a directory", file, file + ": not probed (not a directory)"},
	} {
		e := &linuxEngine{dir: tc.dir, sys: k.sys(), timeout: time.Second}
		got := strings.Join(lines(e), "|")
		if got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
	if os.Geteuid() != 0 {
		locked := filepath.Join(dir, "locked")
		if err := os.Mkdir(locked, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(locked, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(locked, 0o700) })
		e := &linuxEngine{dir: locked, sys: k.sys(), timeout: time.Second}
		if got, want := strings.Join(lines(e), "|"), locked+": not probed (permission denied)"; got != want {
			t.Errorf("unreadable directory: %q, want %q", got, want)
		}
	}
	if k.opens != 0 {
		t.Fatal("a node was opened")
	}
}

func TestPermissionDenied(t *testing.T) {
	k := newFakeKernel(t)
	k.nodes["video0"] = &fakeNode{openErr: unix.EACCES}
	k.nodes["video1"] = &fakeNode{caps: capM2M, driver: "codec", formats: h264Formats(v4l2BufTypeVideoCapture)}
	k.nodes["video2"] = &fakeNode{openErr: unix.EPERM}
	e := k.engine(t, "video0", "video1", "video2")
	got := lines(e)
	d := e.dir
	want := []string{
		d + "/video0: not probed (permission denied)",
		d + "/video1: encoder (codec)",
		d + "/video2: not probed (operation not permitted)",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	if k.openFDs() != 0 {
		t.Fatal("descriptors left open")
	}
}

func TestNotMemoryToMemory(t *testing.T) {
	k := newFakeKernel(t)
	// A camera, a capture node that lists H.264 (a hardware-encoding camera),
	// an output node and a metadata node: none is a codec.
	k.nodes["video0"] = &fakeNode{caps: capCapture | capStreaming, formats: h264Formats(v4l2BufTypeVideoCapture)}
	k.nodes["video1"] = &fakeNode{caps: capCapture | capStreaming | v4l2CapDeviceCaps, devCaps: capCapture | capStreaming, formats: h264Formats(v4l2BufTypeVideoCapture)}
	k.nodes["video2"] = &fakeNode{caps: 0x2, formats: h264Formats(v4l2BufTypeVideoOutput)}
	k.nodes["video3"] = &fakeNode{}
	e := k.engine(t, "video0", "video1", "video2", "video3")
	if got := lines(e); len(got) != 0 {
		t.Fatalf("devices %q", got)
	}
	for _, r := range k.reqs {
		if r != vidiocQueryCap {
			t.Fatalf("request %#x on a node that is not memory to memory", r)
		}
	}
	if k.opens != 4 || k.openFDs() != 0 {
		t.Fatalf("opens %d, open now %d", k.opens, k.openFDs())
	}
}

func TestMemoryToMemoryDevices(t *testing.T) {
	cap1, out1 := uint32(v4l2BufTypeVideoCapture), uint32(v4l2BufTypeVideoOutput)
	capM, outM := uint32(v4l2BufTypeVideoCaptureMplane), uint32(v4l2BufTypeVideoOutputMplane)
	other := []uint32{fourcc("NV12"), fourcc("S264"), fourcc("AVC1"), fourcc("H265"), fourcc("MJPG"), fourcc("VP80")}
	for _, tc := range []struct {
		name string
		node fakeNode
		want string
	}{
		{"encoder", fakeNode{caps: capM2M, formats: h264Formats(cap1)}, "encoder (drv)"},
		{"decoder", fakeNode{caps: capM2M, formats: h264Formats(out1)}, "decoder (drv)"},
		{"both queues", fakeNode{caps: capM2M, formats: h264Formats(cap1, out1)}, "encoder and decoder (drv)"},
		{"encoder mplane", fakeNode{caps: capM2MMplane, formats: h264Formats(capM)}, "encoder (drv)"},
		{"decoder mplane", fakeNode{caps: capM2MMplane, formats: h264Formats(outM)}, "decoder (drv)"},
		{"both flags, mplane lists it", fakeNode{caps: capM2M | capM2MMplane, formats: h264Formats(capM)}, "encoder (drv)"},
		{"mplane flag, single plane types listed", fakeNode{caps: capM2MMplane, formats: h264Formats(cap1)}, ""},
		{"other coded formats", fakeNode{caps: capM2M, formats: map[uint32][]uint32{cap1: other, out1: other}}, ""},
		{"no formats", fakeNode{caps: capM2M}, ""},
		{"device caps win", fakeNode{caps: capM2M | v4l2CapDeviceCaps, devCaps: capM2M, formats: h264Formats(cap1)}, "encoder (drv)"},
		{"device caps without m2m", fakeNode{caps: capM2M | v4l2CapDeviceCaps, devCaps: capCapture, formats: h264Formats(cap1)}, ""},
		{"caps without the flag", fakeNode{caps: capM2M, devCaps: capCapture, formats: h264Formats(cap1)}, "encoder (drv)"},
		{"no driver name", fakeNode{caps: capM2M, driver: "\x00", formats: h264Formats(cap1)}, "encoder"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k := newFakeKernel(t)
			n := tc.node
			if n.driver == "" {
				n.driver = "drv"
			}
			if n.driver == "\x00" {
				n.driver = ""
			}
			k.nodes["video11"] = &n
			e := k.engine(t, "video11")
			got := strings.Join(lines(e), "|")
			want := ""
			if tc.want != "" {
				want = e.dir + "/video11: " + tc.want
			}
			if got != want {
				t.Fatalf("got %q, want %q", got, want)
			}
			if k.openFDs() != 0 {
				t.Fatal("descriptor left open")
			}
		})
	}
}

func TestOrderAndMessage(t *testing.T) {
	k := newFakeKernel(t)
	cap1 := uint32(v4l2BufTypeVideoCapture)
	k.nodes["video12"] = &fakeNode{caps: capM2M, driver: "b", formats: h264Formats(v4l2BufTypeVideoOutput)}
	k.nodes["video2"] = &fakeNode{caps: capM2M, driver: "a", formats: h264Formats(cap1)}
	k.nodes["video100"] = &fakeNode{caps: capM2M, driver: "c", formats: h264Formats(cap1)}
	k.nodes["video0"] = &fakeNode{caps: capCapture | capStreaming}
	e := k.engine(t, "video12", "video2", "video100", "video0", "video003")
	d := e.dir
	want := "kolovideo/h264: no H.264 engine on linux/" + runtime.GOARCH + " in this release; V4L2 stateful H.264 devices: " +
		d + "/video2: encoder (a), " + d + "/video12: decoder (b), " + d + "/video100: encoder (c)"
	if got := e.unavailable().Error(); got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

func TestIoctlErrors(t *testing.T) {
	cap1, out1 := uint32(v4l2BufTypeVideoCapture), uint32(v4l2BufTypeVideoOutput)
	for _, tc := range []struct {
		name string
		node fakeNode
		want string
	}{
		{"querycap ENOTTY", fakeNode{capErr: unix.ENOTTY}, "not probed (inappropriate ioctl for device)"},
		{"querycap EIO", fakeNode{capErr: unix.EIO}, "not probed (input/output error)"},
		{"querycap ENODEV", fakeNode{capErr: unix.ENODEV}, "not probed (no such device)"},
		{"querycap EACCES", fakeNode{capErr: unix.EACCES}, "not probed (permission denied)"},
		{"querycap error with control bytes", fakeNode{capErr: errors.New("bad\x1b[31m\nline")}, "not probed (bad?[31m?line)"},
		{"enum EIO on the first format", fakeNode{caps: capM2M, formats: h264Formats(cap1), enumErr: map[uint32]error{cap1: unix.EIO}}, "not probed (input/output error)"},
		{"enum EIO after one format", fakeNode{caps: capM2M, formats: map[uint32][]uint32{cap1: {fourcc("NV12"), fourcc("MJPG"), v4l2PixFmtH264}}, enumErr: map[uint32]error{cap1: unix.EIO}, enumErrAt: 1}, "not probed (input/output error)"},
		{"enum error on the output queue", fakeNode{caps: capM2M, formats: h264Formats(cap1), enumErr: map[uint32]error{out1: unix.ENOTTY}}, "not probed (inappropriate ioctl for device)"},
		{"enum never ends", fakeNode{caps: capM2M, neverEnd: true}, "not probed (format list does not end)"},
		{"enum wrapped EINVAL ends the list", fakeNode{caps: capM2M, formats: h264Formats(cap1), enumErr: map[uint32]error{out1: fmt.Errorf("x: %w", unix.EINVAL)}}, "encoder (drv)"},
		{"H.264 at the last listed index", fakeNode{caps: capM2M, formats: map[uint32][]uint32{cap1: append(make([]uint32, v4l2MaxEnum-1), v4l2PixFmtH264)}}, "encoder (drv)"},
		{"H.264 past the bound", fakeNode{caps: capM2M, formats: map[uint32][]uint32{cap1: append(make([]uint32, v4l2MaxEnum), v4l2PixFmtH264)}}, "not probed (format list does not end)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k := newFakeKernel(t)
			n := tc.node
			n.driver = "drv"
			k.nodes["video0"] = &n
			e := k.engine(t, "video0")
			got := strings.Join(lines(e), "|")
			if want := e.dir + "/video0: " + tc.want; got != want {
				t.Fatalf("got %q, want %q", got, want)
			}
			if k.openFDs() != 0 {
				t.Fatal("descriptor left open")
			}
		})
	}
}

func TestOnlyQueryCapAndEnumFmt(t *testing.T) {
	k := newFakeKernel(t)
	k.nodes["video0"] = &fakeNode{caps: capM2M | capM2MMplane, driver: "d", formats: h264Formats(1, 2, 9, 10)}
	e := k.engine(t, "video0")
	lines(e)
	for _, r := range k.reqs {
		if r != vidiocQueryCap && r != vidiocEnumFmt {
			t.Fatalf("request %#x", r)
		}
	}
	if len(k.reqs) == 0 || k.reqs[0] != vidiocQueryCap {
		t.Fatalf("first request %#x", k.reqs)
	}
}

func TestDriverTextIsPrintable(t *testing.T) {
	k := newFakeKernel(t)
	k.nodes["video0"] = &fakeNode{caps: capM2M, driver: "ab\x1b[2J\xff\x80cd", formats: h264Formats(v4l2BufTypeVideoCapture)}
	// A 16-byte name has no NUL.
	k.nodes["video1"] = &fakeNode{caps: capM2M, driver: "0123456789abcdef", formats: h264Formats(v4l2BufTypeVideoCapture)}
	e := k.engine(t, "video0", "video1")
	got := lines(e)
	if len(got) != 2 || got[0] != e.dir+"/video0: encoder (ab?[2J??cd)" || got[1] != e.dir+"/video1: encoder (0123456789abcdef)" {
		t.Fatalf("got %q", got)
	}
}

func TestProbeTimeout(t *testing.T) {
	k := newFakeKernel(t)
	release := make(chan struct{})
	k.nodes["video0"] = &fakeNode{caps: capM2M, driver: "a", formats: h264Formats(v4l2BufTypeVideoCapture)}
	k.nodes["video1"] = &fakeNode{block: release}
	k.nodes["video2"] = &fakeNode{caps: capM2M, driver: "c", formats: h264Formats(v4l2BufTypeVideoOutput)}
	k.nodes["video3"] = &fakeNode{block: release}
	e := k.engine(t, "video0", "video1", "video2", "video3")
	e.timeout = 100 * time.Millisecond
	start := time.Now()
	got := lines(e)
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("scan took %v with a 100 ms timeout", took)
	}
	d := e.dir
	want := []string{d + "/video0: encoder (a)", d + "/video1: not probed (timed out)", d + "/video2: decoder (c)", d + "/video3: not probed (timed out)"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for k.openFDs() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("blocked probes did not finish and close their descriptors")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestEngineIsUnavailable(t *testing.T) {
	k := newFakeKernel(t)
	k.nodes["video0"] = &fakeNode{caps: capM2M, driver: "drv", formats: h264Formats(v4l2BufTypeVideoCapture)}
	e := k.engine(t, "video0")
	c := e.caps()
	for _, s := range []Support{c.Encode, c.Decode} {
		var ue *UnavailableError
		if s.Available || s.Hardware || len(s.Profiles) != 0 || !errors.As(s.Err, &ue) || !errors.Is(s.Err, ErrUnavailable) {
			t.Fatalf("support %+v", s)
		}
		if len(ue.Devices) != 1 || ue.GOOS != runtime.GOOS || ue.GOARCH != runtime.GOARCH || ue.Engine != "" || ue.Code != 0 {
			t.Fatalf("error %+v", ue)
		}
	}
	if c.Encode.Err.Error() != c.Decode.Err.Error() {
		t.Fatal("encode and decode report different reasons")
	}
	if enc, err := e.newEncoder(encParams{}); enc != nil || !errors.Is(err, ErrUnavailable) {
		t.Fatalf("newEncoder: %v %v", enc, err)
	}
	if dec, err := e.newDecoder(decParams{}); dec != nil || !errors.Is(err, ErrUnavailable) {
		t.Fatalf("newDecoder: %v %v", dec, err)
	}
	if e.name() != "" {
		t.Fatalf("name %q", e.name())
	}
	// The scan runs once; the error is a copy.
	ue := e.unavailable()
	ue.Devices[0] = "changed"
	if got := e.unavailable().Devices[0]; got == "changed" {
		t.Fatal("the error shares the device list")
	}
	if k.opens != 1 {
		t.Fatalf("%d opens for 6 calls, want 1", k.opens)
	}
}

func TestConcurrentCalls(t *testing.T) {
	k := newFakeKernel(t)
	for i := range 8 {
		k.nodes[fmt.Sprintf("video%d", i)] = &fakeNode{caps: capM2M, driver: "d", formats: h264Formats(v4l2BufTypeVideoCapture)}
	}
	e := k.engine(t, "video0", "video1", "video2", "video3", "video4", "video5", "video6", "video7")
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if len(e.caps().Encode.Err.(*UnavailableError).Devices) != 8 {
				t.Error("wrong device count")
			}
			e.newDecoder(decParams{})
		}()
	}
	wg.Wait()
	if k.opens != 8 {
		t.Fatalf("%d opens, want 8", k.opens)
	}
}

// TestRandomDriverAnswers feeds arbitrary capability words, format codes and
// errors and checks the scan returns without a panic and with sane text.
func TestRandomDriverAnswers(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	errs := []error{nil, unix.EINVAL, unix.EIO, unix.ENOTTY, unix.EACCES, unix.ENODEV, unix.EBUSY, unix.Errno(9999)}
	for i := 0; i < 300; i++ {
		k := newFakeKernel(t)
		n := &fakeNode{caps: rng.Uint32(), devCaps: rng.Uint32(), formats: map[uint32][]uint32{}, enumErr: map[uint32]error{}}
		drv := make([]byte, rng.Intn(20))
		rng.Read(drv)
		n.driver = string(drv)
		for _, bt := range []uint32{0, 1, 2, 9, 10, rng.Uint32()} {
			list := make([]uint32, rng.Intn(5))
			for j := range list {
				if rng.Intn(3) == 0 {
					list[j] = v4l2PixFmtH264
				} else {
					list[j] = rng.Uint32()
				}
			}
			n.formats[bt] = list
			n.enumErr[bt] = errs[rng.Intn(len(errs))]
		}
		n.enumErrAt = uint32(rng.Intn(4))
		if rng.Intn(10) == 0 {
			n.capErr = errs[rng.Intn(len(errs))]
		}
		k.nodes["video0"] = n
		e := k.engine(t, "video0")
		for _, l := range lines(e) {
			if !strings.HasPrefix(l, e.dir+"/video0: ") {
				t.Fatalf("line %q", l)
			}
			for _, c := range []byte(strings.TrimPrefix(l, e.dir)) {
				if c < 0x20 || c > 0x7e {
					t.Fatalf("line %q has byte %#x", l, c)
				}
			}
		}
		if k.openFDs() != 0 {
			t.Fatal("descriptor left open")
		}
	}
}

func TestStructsAndRequests(t *testing.T) {
	if s := unsafe.Sizeof(v4l2Capability{}); s != 104 {
		t.Fatalf("v4l2_capability is %d bytes, want 104", s)
	}
	if s := unsafe.Sizeof(v4l2FmtDesc{}); s != 64 {
		t.Fatalf("v4l2_fmtdesc is %d bytes, want 64", s)
	}
	// VIDIOC_QUERYCAP _IOR('V', 0, 104 bytes) and VIDIOC_ENUM_FMT
	// _IOWR('V', 2, 64 bytes) by asm-generic/ioctl.h and by powerpc and mips.
	for _, goarch := range []string{"amd64", "arm64", "386", "riscv64", "ppc64le", "mips64"} {
		want := [2]uintptr{0x80685600, 0xc0405602}
		if goarch == "ppc64le" || goarch == "mips64" {
			want[0] = 0x40685600
		}
		q, f := ioc(goarch, true, false, 0, 104), ioc(goarch, true, true, 2, 64)
		if q != want[0] || f != want[1] {
			t.Errorf("%s: QUERYCAP %#x ENUM_FMT %#x, want %#x %#x", goarch, q, f, want[0], want[1])
		}
	}
	if vidiocQueryCap != ioc(runtime.GOARCH, true, false, 0, 104) || vidiocEnumFmt != ioc(runtime.GOARCH, true, true, 2, 64) {
		t.Fatal("request numbers of this architecture")
	}
	if v4l2PixFmtH264 != 0x34363248 {
		t.Fatalf("V4L2_PIX_FMT_H264 %#x", v4l2PixFmtH264)
	}
	if v4l2CapVideoM2M != 0x8000 || v4l2CapVideoM2MMplane != 0x4000 || v4l2CapDeviceCaps != 0x80000000 {
		t.Fatal("capability bits")
	}
}

func TestLinuxPlatformIsRegistered(t *testing.T) {
	le, ok := platform.(*linuxEngine)
	if !ok {
		t.Fatalf("platform is %T", platform)
	}
	if le.dir != "/dev" || le.timeout <= 0 || le.sys.open == nil || le.sys.ioctl == nil || le.sys.close == nil {
		t.Fatalf("engine %+v", le)
	}
}
