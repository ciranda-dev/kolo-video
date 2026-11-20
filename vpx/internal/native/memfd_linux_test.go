//go:build linux && kolovideo_native && (amd64 || arm64)

package native

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func openFds(t *testing.T) int {
	t.Helper()
	e, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skip("no /proc")
	}
	return len(e)
}

func TestMemfdLoad(t *testing.T) {
	s := stubSource(t)
	before := openFds(t)
	l, err := memfdLoad(s)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.unload)
	if l.Source != SourceMemfd || l.Path != "" || l.Version != "v0.0.0-stub" {
		t.Fatalf("%+v", l)
	}
	if after := openFds(t); after != before {
		t.Fatalf("file descriptors %d before, %d after", before, after)
	}
}

func TestMemfdNothingOnDisk(t *testing.T) {
	s := stubSource(t)
	root := filepath.Join(t.TempDir(), "cache")
	l, err := load(s, Config{CacheDir: root})
	if err != nil || l.Source != SourceMemfd {
		t.Fatalf("%v %v", l, err)
	}
	t.Cleanup(l.unload)
	if _, err := os.Stat(root); err == nil {
		t.Fatal("cache created although memfd worked")
	}
}

func TestMemfdSealedAndChecked(t *testing.T) {
	s := stubSource(t)
	fd, err := memfdCreate()
	if err != nil {
		t.Skip(err)
	}
	defer unix.Close(fd)
	if _, err := unix.Write(fd, s.data); err != nil {
		t.Fatal(err)
	}
	if err := checkFd(fd, s); err != nil {
		t.Fatal(err)
	}
	other := &source{data: s.data, sum: s.sum}
	other.sum[3] ^= 1
	if err := checkFd(fd, other); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("err = %v", err)
	}
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_ADD_SEALS, unix.F_SEAL_WRITE); err != nil {
		t.Fatal(err)
	}
	if _, err := unix.Write(fd, []byte{0}); err == nil {
		t.Fatal("write after seal succeeded")
	}
}
