//go:build linux && kolovideo_native && (amd64 || arm64)

package native

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// memfdLoad writes the library into a sealed anonymous memory file, checks its
// hash and loads it through /proc/self/fd, so nothing reaches the disk.
func memfdLoad(s *source) (*Lib, error) {
	fd, err := memfdCreate()
	if err != nil {
		return nil, fmt.Errorf("memfd_create: %w", err)
	}
	defer unix.Close(fd)
	for b := s.data; len(b) > 0; {
		n, err := unix.Write(fd, b)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("write memfd: %w", err)
		}
		b = b[n:]
	}
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_ADD_SEALS, unix.F_SEAL_SEAL|unix.F_SEAL_SHRINK|unix.F_SEAL_GROW|unix.F_SEAL_WRITE); err != nil {
		return nil, fmt.Errorf("seal memfd: %w", err)
	}
	if err := checkFd(fd, s); err != nil {
		return nil, err
	}
	return openFile(SourceMemfd, fmt.Sprintf("/proc/self/fd/%d", fd), false)
}

// memfdCreate asks for an executable, sealable file and falls back to the
// kernel's default flags where MFD_EXEC is unknown (before Linux 6.3) or
// refused (vm.memfd_noexec=2).
func memfdCreate() (int, error) {
	fd, err := unix.MemfdCreate("libkolovpx", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING|unix.MFD_EXEC)
	if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.EACCES) {
		fd, err = unix.MemfdCreate("libkolovpx", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
	}
	return fd, err
}

// checkFd hashes the sealed file contents.
func checkFd(fd int, s *source) error {
	h := sha256.New()
	buf := make([]byte, 1<<16)
	var off int64
	for {
		n, err := unix.Pread(fd, buf, off)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return fmt.Errorf("read memfd: %w", err)
		}
		if n == 0 {
			break
		}
		h.Write(buf[:n])
		off += int64(n)
	}
	var got [32]byte
	h.Sum(got[:0])
	if got != s.sum || off != int64(len(s.data)) {
		return fmt.Errorf("%w: memfd contents differ", ErrIntegrity)
	}
	return nil
}

// openCached loads the cache file p through one descriptor: the bytes that are
// hashed are the bytes that are loaded, whatever happens to the path after the
// check. Without /proc the path is loaded, and the checks on the directories
// above it are what remains.
func openCached(p string, s *source) (*Lib, error) {
	fd, err := unix.Open(p, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCache, err)
	}
	defer unix.Close(fd)
	if err := checkFd(fd, s); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCache, err)
	}
	fdPath := fmt.Sprintf("/proc/self/fd/%d", fd)
	if _, err := os.Stat(fdPath); err != nil {
		return openFile(SourceCache, p, false)
	}
	l, err := openFile(SourceCache, fdPath, false)
	if l != nil {
		l.Path = p
	}
	return l, err
}
