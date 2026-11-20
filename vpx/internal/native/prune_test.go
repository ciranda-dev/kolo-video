package native

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPrune(t *testing.T) {
	root := filepath.Join(t.TempDir(), "vpx")
	sum := strings.Repeat("ab", 32)
	cur := filepath.Join(root, "v1.16.0-"+sum)
	oldDir := filepath.Join(root, "v1.15.0-"+sum)
	newDir := filepath.Join(root, "v1.14.0-"+sum)
	odd := filepath.Join(root, "notes")
	for _, d := range []string{cur, oldDir, newDir, odd} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	write := func(p string, age time.Duration) {
		t.Helper()
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		when := time.Now().Add(-age)
		if err := os.Chtimes(p, when, when); err != nil {
			t.Fatal(err)
		}
	}
	lib := libName()
	write(filepath.Join(cur, lib), 90*24*time.Hour) // the library in use, however old
	write(filepath.Join(cur, lib+".tmp1"), time.Hour)
	write(filepath.Join(cur, lib+".tmp2"), time.Second)
	write(filepath.Join(cur, "other.tmp3"), time.Hour)
	write(filepath.Join(oldDir, lib), 90*24*time.Hour)
	write(filepath.Join(oldDir, lib+".tmp9"), 90*24*time.Hour)
	write(filepath.Join(newDir, lib), time.Hour)
	write(filepath.Join(odd, lib), 90*24*time.Hour)
	for _, d := range []string{oldDir, newDir} {
		when := time.Now().Add(-90 * 24 * time.Hour)
		if d == newDir {
			when = time.Now().Add(-time.Hour)
		}
		if err := os.Chtimes(d, when, when); err != nil {
			t.Fatal(err)
		}
	}
	// A foreign file keeps its directory.
	keep := filepath.Join(root, "v1.13.0-"+sum)
	if err := os.MkdirAll(keep, 0o700); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(keep, "mine.txt"), time.Hour)
	when := time.Now().Add(-90 * 24 * time.Hour)
	if err := os.Chtimes(keep, when, when); err != nil {
		t.Fatal(err)
	}

	prune(cur)

	exists := func(p string) bool { _, err := os.Lstat(p); return err == nil }
	for p, want := range map[string]bool{
		filepath.Join(cur, lib):          true,
		filepath.Join(cur, lib+".tmp1"):  false,
		filepath.Join(cur, lib+".tmp2"):  true,
		filepath.Join(cur, "other.tmp3"): true,
		oldDir:                           false,
		newDir:                           true,
		filepath.Join(newDir, lib):       true,
		odd:                              true,
		filepath.Join(keep, "mine.txt"):  true,
	} {
		if exists(p) != want {
			t.Errorf("%s exists = %v, want %v", strings.TrimPrefix(p, root), !want, want)
		}
	}
}
