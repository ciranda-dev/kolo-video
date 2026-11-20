//go:build linux && !(ppc64 || ppc64le || mips || mipsle || mips64 || mips64le)

package kolovideo

// ioctl request encoding from include/uapi/asm-generic/ioctl.h.
const (
	iocWrite    = 1
	iocRead     = 2
	iocDirShift = 30
)
