//go:build linux && kolovideo_native && (amd64 || arm64)

package native

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestCacheSwapNeverLoadsAnotherFile swaps the cache directory for one that holds
// a library with another ABI while the loader runs. The loader loads the
// descriptor it hashed, so that library must never be the one that is loaded.
func TestCacheSwapNeverLoadsAnotherFile(t *testing.T) {
	s := stubSource(t)
	planted, err := os.ReadFile(fmt.Sprintf("../../testdata/kvabi-linux-%s.so", Target()[len("linux/"):]))
	if err != nil {
		t.Skipf("no planted fixture: %v", err)
	}
	root := realPath(t, t.TempDir())
	dir, err := cacheDir(root, s.sum)
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	swaps := 0
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			old := fmt.Sprintf("%s.old%d", dir, i%2)
			os.RemoveAll(old)
			if os.Rename(dir, old) != nil {
				continue
			}
			if os.MkdirAll(dir, 0o700) == nil {
				os.WriteFile(filepath.Join(dir, libName()), planted, 0o500)
				swaps++
			}
			time.Sleep(150 * time.Microsecond)
		}
	}()
	foreign, loads := 0, 0
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		l, err := cacheLoad(s, dir)
		switch {
		case err == nil:
			loads++
			l.unload()
		case errors.Is(err, ErrABI):
			foreign++
		}
	}
	close(stop)
	wg.Wait()
	t.Logf("%d loads, %d directory swaps, %d foreign loads", loads, swaps, foreign)
	if foreign != 0 {
		t.Fatalf("%d loads ran the planted library", foreign)
	}
	if loads == 0 {
		t.Fatal("nothing loaded")
	}
}
