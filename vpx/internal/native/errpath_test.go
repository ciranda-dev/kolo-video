package native

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// checkAttemptPath requires the message to name the attempted path exactly
// once, after a separator, and never its symlink-resolved form.
func checkAttemptPath(t *testing.T, err error, src Source, path string) {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("err = %v", err)
	}
	var got *Attempt
	for i := range e.Attempts {
		if e.Attempts[i].Source == src {
			got = &e.Attempts[i]
		}
	}
	if got == nil || got.Path != path {
		t.Fatalf("attempts %+v, want %s at %q", e.Attempts, src, path)
	}
	msg := err.Error()
	if n := strings.Count(msg, path); n != 1 {
		t.Fatalf("message names %q %d times: %s", path, n, msg)
	}
	if !strings.Contains(msg, string(src)+" "+path+": ") {
		t.Fatalf("message lacks %q: %s", string(src)+" "+path+": ", msg)
	}
	if real, rerr := filepath.EvalSymlinks(path); rerr == nil && real != path && strings.Contains(msg, real) {
		t.Fatalf("message names the resolved path %q: %s", real, msg)
	}
	if strings.Contains(msg, "private<lib>") || strings.Contains(msg, "/<lib>") {
		t.Fatalf("mangled path in message: %s", msg)
	}
}

func TestErrorNamesPathOnce(t *testing.T) {
	const p = "/a/b/libkolovpx.so"
	err := &Error{GOOS: "x", GOARCH: "y", Attempts: []Attempt{
		{SourceCache, p, errors.New("load: " + p + ": invalid ELF header")},
		{SourceBundle, "/c/" + libName(), errors.New("open /c/" + libName() + ".tmp1: denied")},
	}}
	if n := strings.Count(err.Error(), p); n != 1 {
		t.Fatalf("path named %d times: %s", n, err)
	}
	if n := strings.Count(err.Error(), "/c/"+libName()); n != 1 {
		t.Fatalf("bundle path named %d times: %s", n, err)
	}
}

func TestCacheAttemptNamesPath(t *testing.T) {
	if !dlAvailable {
		t.Skip("no native loader in this build")
	}
	s := newSource([]byte("not a shared library"))
	root := t.TempDir()
	dir, _ := cacheDir(root, s.sum)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "decoy-entry"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := load(s, Config{CacheDir: root})
	checkAttemptPath(t, err, SourceCache, filepath.Join(dir, libName()))
	if strings.Contains(err.Error(), "decoy-entry") {
		t.Fatalf("message lists cache contents: %v", err)
	}
}

func TestCacheFailureNamesPath(t *testing.T) {
	if !dlAvailable {
		t.Skip("no native loader in this build")
	}
	s := newSource([]byte("not a shared library"))
	blocked := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocked, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	dir, _ := cacheDir(blocked, s.sum)
	_, err := load(s, Config{CacheDir: blocked})
	if !errors.Is(err, ErrCache) {
		t.Fatalf("err = %v", err)
	}
	checkAttemptPath(t, err, SourceCache, filepath.Join(dir, libName()))
}
