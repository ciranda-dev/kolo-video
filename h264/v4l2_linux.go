//go:build linux

package h264

import (
	"bytes"
	"errors"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

// V4L2 definitions from include/uapi/linux/videodev2.h, described in the
// kernel documentation pages userspace-api/media/v4l/vidioc-querycap,
// vidioc-enum-fmt, pixfmt-compressed, dev-encoder and dev-decoder.
const (
	v4l2BufTypeVideoCapture       = 1  // V4L2_BUF_TYPE_VIDEO_CAPTURE
	v4l2BufTypeVideoOutput        = 2  // V4L2_BUF_TYPE_VIDEO_OUTPUT
	v4l2BufTypeVideoCaptureMplane = 9  // V4L2_BUF_TYPE_VIDEO_CAPTURE_MPLANE
	v4l2BufTypeVideoOutputMplane  = 10 // V4L2_BUF_TYPE_VIDEO_OUTPUT_MPLANE
	v4l2CapVideoM2MMplane         = 0x00004000
	v4l2CapVideoM2M               = 0x00008000
	v4l2CapDeviceCaps             = 0x80000000
)

// v4l2PixFmtH264 is V4L2_PIX_FMT_H264, 'H264': Annex B access units with
// start codes, one access unit per buffer (stateful codecs).
var v4l2PixFmtH264 = fourcc("H264")

// v4l2MaxEnum bounds each ENUM_FMT loop so a driver that never returns
// EINVAL cannot hang the scan.
const v4l2MaxEnum = 1024

// struct v4l2_capability, 104 bytes.
type v4l2Capability struct {
	Driver       [16]byte
	Card         [32]byte
	BusInfo      [32]byte
	Version      uint32
	Capabilities uint32
	DeviceCaps   uint32
	Reserved     [3]uint32
}

// struct v4l2_fmtdesc, 64 bytes.
type v4l2FmtDesc struct {
	Index       uint32
	Type        uint32
	Flags       uint32
	Description [32]byte
	PixelFormat uint32
	MbusCode    uint32
	Reserved    [3]uint32
}

func fourcc(s string) uint32 {
	return uint32(s[0]) | uint32(s[1])<<8 | uint32(s[2])<<16 | uint32(s[3])<<24
}

// ioc encodes an ioctl request as asm/ioctl.h does: powerpc and mips use
// 3 direction bits with write 4, every other architecture 2 with write 1.
func ioc(goarch string, read, write bool, nr, size uintptr) uintptr {
	var dir, shift uintptr
	switch goarch {
	case "ppc64", "ppc64le", "mips", "mipsle", "mips64", "mips64le":
		shift = 29
		if read {
			dir |= 2
		}
		if write {
			dir |= 4
		}
	default:
		shift = 30
		if read {
			dir |= 2
		}
		if write {
			dir |= 1
		}
	}
	return dir<<shift | size<<16 | 'V'<<8 | nr
}

var (
	vidiocQueryCap = ioc(runtime.GOARCH, true, false, 0, unsafe.Sizeof(v4l2Capability{}))
	vidiocEnumFmt  = ioc(runtime.GOARCH, true, true, 2, unsafe.Sizeof(v4l2FmtDesc{}))
)

// v4l2Sys is the kernel side of the probe: tests replace it.
type v4l2Sys struct {
	open  func(path string) (int, error)
	ioctl func(fd int, req uintptr, arg unsafe.Pointer) error
	close func(fd int)
}

// osV4L2 opens nodes read-only and issues QUERYCAP and ENUM_FMT only.
var osV4L2 = v4l2Sys{
	open: func(path string) (int, error) {
		for {
			fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
			if err == unix.EINTR {
				continue
			}
			return fd, err
		}
	},
	ioctl: func(fd int, req uintptr, arg unsafe.Pointer) error {
		for {
			_, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), req, uintptr(arg))
			if e == unix.EINTR {
				continue
			}
			if e != 0 {
				return e
			}
			return nil
		}
	},
	close: func(fd int) { unix.Close(fd) },
}

var errEnumUnbounded = errors.New("format list does not end")

// v4l2Node is what a memory-to-memory node offers.
type v4l2Node struct {
	driver           string
	encoder, decoder bool
}

// probeNode reads the capabilities of path and, for a memory-to-memory
// device, whether V4L2_PIX_FMT_H264 is a coded format of its CAPTURE queue
// (encoder) or OUTPUT queue (decoder). It never starts streaming.
func probeNode(sys v4l2Sys, path string) (v4l2Node, error) {
	fd, err := sys.open(path)
	if err != nil {
		return v4l2Node{}, err
	}
	defer sys.close(fd)
	var c v4l2Capability
	if err := sys.ioctl(fd, vidiocQueryCap, unsafe.Pointer(&c)); err != nil {
		return v4l2Node{}, err
	}
	caps := c.Capabilities
	if caps&v4l2CapDeviceCaps != 0 {
		caps = c.DeviceCaps
	}
	n := v4l2Node{driver: printable(c.Driver[:])}
	var capture, output []uint32
	if caps&v4l2CapVideoM2M != 0 {
		capture = append(capture, v4l2BufTypeVideoCapture)
		output = append(output, v4l2BufTypeVideoOutput)
	}
	if caps&v4l2CapVideoM2MMplane != 0 {
		capture = append(capture, v4l2BufTypeVideoCaptureMplane)
		output = append(output, v4l2BufTypeVideoOutputMplane)
	}
	for _, t := range capture {
		ok, err := listsH264(sys, fd, t)
		if err != nil {
			return v4l2Node{}, err
		}
		n.encoder = n.encoder || ok
	}
	for _, t := range output {
		ok, err := listsH264(sys, fd, t)
		if err != nil {
			return v4l2Node{}, err
		}
		n.decoder = n.decoder || ok
	}
	return n, nil
}

// listsH264 enumerates the formats of one queue until EINVAL.
func listsH264(sys v4l2Sys, fd int, bufType uint32) (bool, error) {
	for i := uint32(0); i < v4l2MaxEnum; i++ {
		d := v4l2FmtDesc{Index: i, Type: bufType}
		if err := sys.ioctl(fd, vidiocEnumFmt, unsafe.Pointer(&d)); err != nil {
			if errors.Is(err, unix.EINVAL) {
				return false, nil
			}
			return false, err
		}
		if d.PixelFormat == v4l2PixFmtH264 {
			return true, nil
		}
	}
	return false, errEnumUnbounded
}

// printable cuts a driver string at its first NUL and replaces bytes outside
// printable ASCII, so what a driver reports cannot shape an error message.
func printable(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	out := make([]byte, len(b))
	for i, c := range b {
		if c < 0x20 || c > 0x7e {
			c = '?'
		}
		out[i] = c
	}
	return string(out)
}
