package native

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"testing"
)

//go:embed libkolovpx-darwin-amd64.dylib libkolovpx-darwin-arm64.dylib libkolovpx-linux-amd64.so libkolovpx-linux-arm64.so libkolovpx-windows-amd64.dll libkolovpx-windows-arm64.dll
var sumsAllLibs embed.FS

var sumsLibFiles = map[string]string{
	"darwin/amd64":  "libkolovpx-darwin-amd64.dylib",
	"darwin/arm64":  "libkolovpx-darwin-arm64.dylib",
	"linux/amd64":   "libkolovpx-linux-amd64.so",
	"linux/arm64":   "libkolovpx-linux-arm64.so",
	"windows/amd64": "libkolovpx-windows-amd64.dll",
	"windows/arm64": "libkolovpx-windows-arm64.dll",
}

func sumsCheck(t *testing.T, target string, data []byte) {
	t.Helper()
	want, ok := sums[target]
	if !ok {
		t.Fatalf("no checksum for %s", target)
	}
	got := sha256.Sum256(data)
	if hex.EncodeToString(got[:]) != want.SHA256 || len(data) != want.Size {
		t.Errorf("%s: sha256 %x size %d, sums.go has %s size %d", target, got, len(data), want.SHA256, want.Size)
	}
}

func TestSumsCoverEveryLibrary(t *testing.T) {
	if len(sums) != len(sumsLibFiles) {
		t.Errorf("sums has %d entries, want %d", len(sums), len(sumsLibFiles))
	}
	for target, name := range sumsLibFiles {
		data, err := sumsAllLibs.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		sumsCheck(t, target, data)
	}
}

func TestEmbeddedLibraryMatchesSums(t *testing.T) {
	if len(libData) == 0 {
		t.Skipf("no library is embedded for %s in this build", Target())
	}
	sumsCheck(t, Target(), libData)
}
