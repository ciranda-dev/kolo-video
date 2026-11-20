package native

import (
	"path/filepath"
	"testing"
)

func TestBundleAttemptNamesPath(t *testing.T) {
	s := newSource([]byte("not a shared library"))
	bundle := fakeApp(t, s)
	_, err := load(s, Config{CacheDir: t.TempDir()})
	real, rerr := filepath.EvalSymlinks(bundle)
	if rerr != nil {
		t.Fatal(rerr)
	}
	checkAttemptPath(t, err, SourceBundle, real)
}
