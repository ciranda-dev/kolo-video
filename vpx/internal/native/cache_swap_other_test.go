//go:build !(linux && kolovideo_native && (amd64 || arm64))

package native

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestCacheFileSwappedBeforeLoad replaces the cache file with another one between
// the check and the load. On Windows the open handle keeps the file in place and
// the load goes on; elsewhere the swap is noticed and the load refused.
func TestCacheFileSwappedBeforeLoad(t *testing.T) {
	s := stubSource(t)
	root := realPath(t, t.TempDir())
	dir, _ := cacheDir(root, s.sum)
	p, err := ensureFile(dir, filepath.Base(libName()), s.data, s.sum)
	if err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(root, "other")
	if err := os.WriteFile(other, append(append([]byte(nil), s.data...), 0), 0o500); err != nil {
		t.Fatal(err)
	}
	swapped := false
	beforeLoad = func() { swapped = os.Rename(other, p) == nil }
	t.Cleanup(func() { beforeLoad = func() {} })
	l, err := openCached(p, s)
	if runtime.GOOS == "windows" {
		if err != nil || swapped {
			t.Fatalf("an open handle must keep the file in place: err=%v swapped=%v", err, swapped)
		}
		l.unload()
		return
	}
	if !swapped {
		t.Fatal("the test could not swap the file")
	}
	if !errors.Is(err, ErrCache) {
		if l != nil {
			l.unload()
		}
		t.Fatalf("err = %v, want ErrCache", err)
	}
}
