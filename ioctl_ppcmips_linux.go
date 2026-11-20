//go:build linux && (ppc64 || ppc64le || mips || mipsle || mips64 || mips64le)

package kolovideo

// ioctl request encoding from the powerpc and mips uapi asm/ioctl.h.
const (
	iocWrite    = 4
	iocRead     = 2
	iocDirShift = 29
)
