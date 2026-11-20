//go:build darwin

package native

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTrimDlError(t *testing.T) {
	const p = "/a/b/libkolovpx.dylib"
	for _, c := range []struct{ in, want string }{
		{
			"dlopen(" + p + ", 0x0006): tried: '" + p + "' (code signature in <AEC6308E-E35B-3C06-B412-C4FD130B5324> '" + p + "' not valid for use in process: mapping process and mapped file (non-platform) have different Team IDs), '/System/Volumes/Preboot/Cryptexes/OS" + p + "' (no such file), '" + p + "' (code signature in <AEC6308E-E35B-3C06-B412-C4FD130B5324> '" + p + "' not valid for use in process: mapping process and mapped file (non-platform) have different Team IDs)",
			"code signature in '<lib>' not valid for use in process: mapping process and mapped file (non-platform) have different Team IDs",
		},
		{
			"dlopen(" + p + ", 0x0006): tried: '" + p + "' (no such file), '/System/Volumes/Preboot/Cryptexes/OS" + p + "' (no such file)",
			"no such file",
		},
		{"dlopen(" + p + ", 0x0006): something else", "something else"},
		{"plain", "plain"},
	} {
		if got := trimDlError(c.in, p); got != c.want {
			t.Errorf("trim(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestTrimDlErrorResolvedPath(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink("real", link); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(link, "lib.dylib")
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	in := "dlopen(" + r + ", 0x0006): tried: '" + r + "' (code signature in '" + r + "' not valid), '/System/Volumes/Preboot/Cryptexes/OS" + r + "' (no such file)"
	if got, want := trimDlError(in, p), "code signature in '<lib>' not valid"; got != want {
		t.Fatalf("trim = %q, want %q", got, want)
	}
}
