package native

import (
	"crypto/sha256"
	"embed"
	"fmt"
	"runtime"
	"testing"
)

//go:embed testdata/kvstub-*
var fixtures embed.FS

func fixtureName() string {
	ext := "so"
	switch runtime.GOOS {
	case "darwin":
		ext = "dylib"
	case "windows":
		ext = "dll"
	}
	return fmt.Sprintf("testdata/kvstub-%s-%s.%s", runtime.GOOS, runtime.GOARCH, ext)
}

// stubSource returns the kv_* stub library for this target as an embedded
// library, skipping the test where no loader is built.
func stubSource(t testing.TB) *source {
	t.Helper()
	if !dlAvailable {
		t.Skip("no native loader in this build")
	}
	b, err := fixtures.ReadFile(fixtureName())
	if err != nil {
		t.Skipf("no fixture: %v", err)
	}
	return newSource(b)
}

func newSource(b []byte) *source { return &source{data: b, sum: sha256.Sum256(b)} }
