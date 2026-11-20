package native

import (
	"crypto/sha256"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// realPath resolves the symbolic links of p: the cache returns the resolved path
// it loads from.
func realPath(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func countFiles(t *testing.T, dir string) (files []string) {
	t.Helper()
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestExtractHelper(t *testing.T) {
	dir := os.Getenv("KV_T2_EXTRACT")
	if dir == "" {
		t.Skip("helper process only")
	}
	s := stubSource(t)
	if _, err := ensureFile(dir, libName(), s.data, s.sum); err != nil {
		t.Fatal(err)
	}
}

func TestCacheConcurrent(t *testing.T) {
	s := stubSource(t)
	root := t.TempDir()
	dir, err := cacheDir(root, s.sum)
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	paths := make(chan string, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			p, err := ensureFile(dir, libName(), s.data, s.sum)
			errs <- err
			paths <- p
		}()
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	procs := make([]*exec.Cmd, 4)
	var outs [4]strings.Builder
	for i := range procs {
		c := exec.Command(self, "-test.run=^TestExtractHelper$")
		c.Env = append(os.Environ(), "KV_T2_EXTRACT="+dir)
		c.Stdout, c.Stderr = &outs[i], &outs[i]
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		procs[i] = c
	}
	close(start)
	wg.Wait()
	for i, c := range procs {
		if err := c.Wait(); err != nil {
			t.Errorf("subprocess %d: %v\n%s", i, err, outs[i].String())
		}
	}
	close(errs)
	close(paths)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
	for p := range paths {
		if p != filepath.Join(realPath(t, dir), libName()) {
			t.Errorf("path %q", p)
		}
	}
	if f := countFiles(t, root); len(f) != 1 {
		t.Fatalf("files %v, want exactly one", f)
	}
	if !fileHasSum(filepath.Join(dir, libName()), s.sum, len(s.data)) {
		t.Fatal("cached file does not match")
	}
}

func TestCacheTamperedReplaced(t *testing.T) {
	s := stubSource(t)
	dir, _ := cacheDir(t.TempDir(), s.sum)
	p, err := ensureFile(dir, libName(), s.data, s.sum)
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func([]byte) []byte{
		"flipped byte": func(b []byte) []byte { b[len(b)/2] ^= 0xff; return b },
		"truncated":    func(b []byte) []byte { return b[:len(b)-1] },
		"extended":     func(b []byte) []byte { return append(b, 0) },
		"empty":        func(b []byte) []byte { return nil },
	} {
		if err := os.Chmod(p, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, mutate(append([]byte(nil), s.data...)), 0o600); err != nil {
			t.Fatal(err)
		}
		if fileHasSum(p, s.sum, len(s.data)) {
			t.Fatalf("%s: tampered file passes", name)
		}
		if _, err := ensureFile(dir, libName(), s.data, s.sum); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !fileHasSum(p, s.sum, len(s.data)) {
			t.Fatalf("%s: file not replaced", name)
		}
		if f := countFiles(t, dir); len(f) != 1 {
			t.Fatalf("%s: files %v", name, f)
		}
	}
}

func TestCacheSymlinkReplaced(t *testing.T) {
	s := stubSource(t)
	dir, _ := cacheDir(t.TempDir(), s.sum)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, s.data, 0o600); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, libName())
	if err := os.Symlink(target, p); err != nil {
		t.Skipf("symlinks: %v", err)
	}
	if _, err := ensureFile(dir, libName(), s.data, s.sum); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(p); err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("symlink not replaced by a regular file: %v %v", fi, err)
	}
}

func TestCacheNotADirectory(t *testing.T) {
	s := stubSource(t)
	dir, _ := cacheDir(t.TempDir(), s.sum)
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureFile(dir, libName(), s.data, s.sum); !errors.Is(err, ErrCache) {
		t.Fatalf("err = %v", err)
	}
}

func TestCacheModes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes")
	}
	s := stubSource(t)
	root := t.TempDir()
	dir, _ := cacheDir(root, s.sum)
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	p, err := ensureFile(dir, libName(), s.data, s.sum)
	if err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o700 {
		t.Fatalf("directory mode %v", fi.Mode().Perm())
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o500 {
		t.Fatalf("file mode %v", fi.Mode().Perm())
	}
}

func TestCacheReadOnly(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs permission checks that apply to this user")
	}
	s := stubSource(t)
	root := t.TempDir()
	if err := os.Chmod(root, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(root, 0o700)
	dir, _ := cacheDir(root, s.sum)
	if _, err := ensureFile(dir, libName(), s.data, s.sum); !errors.Is(err, ErrCache) {
		t.Fatalf("err = %v", err)
	}
	if runtime.GOOS == "darwin" {
		_, err := load(s, Config{CacheDir: root})
		var e *Error
		if !errors.Is(err, ErrCache) || !errors.As(err, &e) || e.Attempts[len(e.Attempts)-1].Source != SourceCache {
			t.Fatalf("load err = %v", err)
		}
		if !strings.HasPrefix(err.Error(), "kolovideo/vpx: load native libvpx ("+Target()+")") {
			t.Fatalf("message %q", err)
		}
	}
}

func TestCacheExistingFileInReadOnlyDir(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs permission checks that apply to this user")
	}
	s := stubSource(t)
	dir, _ := cacheDir(t.TempDir(), s.sum)
	if _, err := ensureFile(dir, libName(), s.data, s.sum); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)
	if _, err := ensureFile(dir, libName(), s.data, s.sum); err != nil {
		t.Fatal(err)
	}
}

func TestCacheDefaultDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", home)
	t.Setenv("LocalAppData", home)
	sum := sha256.Sum256([]byte("x"))
	dir, err := cacheDir("", sum)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(dir, home) || !strings.Contains(dir, filepath.Join("kolo-video", "vpx", LibvpxVersion+"-")) {
		t.Fatalf("dir %q", dir)
	}
}

func TestWriteLibrary(t *testing.T) {
	s := stubSource(t)
	dir := filepath.Join(t.TempDir(), "App.app", "Contents", "Frameworks")
	for i := 0; i < 2; i++ {
		p, err := writeLibrary(s, dir)
		if err != nil {
			t.Fatal(err)
		}
		if p != filepath.Join(dir, libName()) || !fileHasSum(p, s.sum, len(s.data)) {
			t.Fatalf("path %q", p)
		}
	}
	if f := countFiles(t, dir); len(f) != 1 {
		t.Fatalf("files %v", f)
	}
}
