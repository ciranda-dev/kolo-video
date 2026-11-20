//go:build !windows

package native

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// foreign makes the paths look as if another user owned them, for the life of the test.
func foreign(t *testing.T, paths ...string) {
	t.Helper()
	var infos []os.FileInfo
	for _, p := range paths {
		fi, err := os.Lstat(p)
		if err != nil {
			t.Fatal(err)
		}
		infos = append(infos, fi)
	}
	orig := ownerOf
	t.Cleanup(func() { ownerOf = orig })
	ownerOf = func(fi os.FileInfo) (int, int, bool) {
		uid, nlink, ok := orig(fi)
		for _, o := range infos {
			if os.SameFile(o, fi) {
				return currentUID() + 4242, nlink, ok
			}
		}
		return uid, nlink, ok
	}
}

func cacheTree(t *testing.T) (root, dir string, s *source) {
	t.Helper()
	s = stubSource(t)
	root = realPath(t, t.TempDir())
	dir, err := cacheDir(root, s.sum)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ensureFile(dir, libName(), s.data, s.sum); err != nil {
		t.Fatal(err)
	}
	return root, dir, s
}

func TestCacheForeignOwnerRefused(t *testing.T) {
	for name, pick := range map[string]func(root, dir string) string{
		"cache root":     func(root, dir string) string { return root },
		"vpx directory":  func(root, dir string) string { return filepath.Dir(dir) },
		"leaf directory": func(root, dir string) string { return dir },
	} {
		t.Run(name, func(t *testing.T) {
			root, dir, s := cacheTree(t)
			foreign(t, pick(root, dir))
			if _, err := ensureFile(dir, libName(), s.data, s.sum); !errors.Is(err, ErrCache) {
				t.Fatalf("err = %v, want ErrCache", err)
			}
		})
	}
}

func TestCacheWritableAncestorRefused(t *testing.T) {
	for name, tc := range map[string]struct {
		mode os.FileMode
		ok   bool
	}{
		"0777":                    {0o777, false},
		"0770 group writable":     {0o770, false},
		"0707 others writable":    {0o707, false},
		"1777 sticky, like /tmp":  {0o777 | os.ModeSticky, true},
		"0755 readable by others": {0o755, true},
	} {
		t.Run(name, func(t *testing.T) {
			s := stubSource(t)
			parent := filepath.Join(realPath(t, t.TempDir()), "shared")
			if err := os.Mkdir(parent, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(parent, tc.mode); err != nil {
				t.Fatal(err)
			}
			dir, _ := cacheDir(filepath.Join(parent, "kv"), s.sum)
			_, err := ensureFile(dir, libName(), s.data, s.sum)
			if tc.ok && err != nil {
				t.Fatal(err)
			}
			if !tc.ok && !errors.Is(err, ErrCache) {
				t.Fatalf("err = %v, want ErrCache", err)
			}
			if !tc.ok && fileExists(filepath.Join(parent, "kv")) {
				t.Fatal("a directory was created under the writable parent")
			}
		})
	}
}

// TestCacheStickyForeignAncestorRefused: the sticky bit excuses the mode of a
// parent such as /tmp, not its owner.
func TestCacheStickyForeignAncestorRefused(t *testing.T) {
	s := stubSource(t)
	parent := filepath.Join(realPath(t, t.TempDir()), "shared")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0o777|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	foreign(t, parent)
	dir, _ := cacheDir(filepath.Join(parent, "kv"), s.sum)
	if _, err := ensureFile(dir, libName(), s.data, s.sum); !errors.Is(err, ErrCache) {
		t.Fatalf("err = %v, want ErrCache", err)
	}
	if fileExists(filepath.Join(parent, "kv")) {
		t.Fatal("a directory was created under the foreign parent")
	}
}

func TestCacheSymlinkedAncestorResolved(t *testing.T) {
	s := stubSource(t)
	base := realPath(t, t.TempDir())
	real := filepath.Join(base, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks: %v", err)
	}
	dir, _ := cacheDir(link, s.sum)
	p, err := ensureFile(dir, libName(), s.data, s.sum)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(real, "vpx"); filepath.Dir(filepath.Dir(p)) != want {
		t.Fatalf("path %q is not under the resolved directory %q", p, real)
	}
	foreign(t, real)
	if _, err := ensureFile(dir, libName(), s.data, s.sum); !errors.Is(err, ErrCache) {
		t.Fatalf("a link to a foreign directory: err = %v", err)
	}
}

func TestCacheLeafSymlinkRefused(t *testing.T) {
	s := stubSource(t)
	base := realPath(t, t.TempDir())
	other := filepath.Join(base, "other")
	if err := os.Mkdir(other, 0o700); err != nil {
		t.Fatal(err)
	}
	dir, _ := cacheDir(filepath.Join(base, "cache"), s.sum)
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, dir); err != nil {
		t.Skipf("symlinks: %v", err)
	}
	if _, err := ensureFile(dir, libName(), s.data, s.sum); !errors.Is(err, ErrCache) {
		t.Fatalf("err = %v, want ErrCache", err)
	}
}

func TestCacheUntrustedFileReplaced(t *testing.T) {
	for name, plant := range map[string]func(t *testing.T, p string){
		"owned by another user": func(t *testing.T, p string) { foreign(t, p) },
		"group writable": func(t *testing.T, p string) {
			if err := os.Chmod(p, 0o560); err != nil {
				t.Fatal(err)
			}
		},
		"another hard link": func(t *testing.T, p string) {
			if err := os.Link(p, p+".link"); err != nil {
				t.Skipf("hard links: %v", err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, dir, s := cacheTree(t)
			p := filepath.Join(dir, libName())
			before, _ := os.Lstat(p)
			plant(t, p)
			if fileHasSum(p, s.sum, len(s.data)) {
				t.Fatal("the untrusted file passes the check")
			}
			if _, err := ensureFile(dir, libName(), s.data, s.sum); err != nil {
				t.Fatal(err)
			}
			after, err := os.Lstat(p)
			if err != nil || os.SameFile(before, after) {
				t.Fatalf("the file was not replaced: %v", err)
			}
			if !fileHasSum(p, s.sum, len(s.data)) {
				t.Fatal("the replacement does not pass")
			}
		})
	}
}

// TestCacheNewDirectoryChecked makes the directory that the call creates look as
// if another user had created it first: it must be refused, and no library written.
func TestCacheNewDirectoryChecked(t *testing.T) {
	s := stubSource(t)
	root := realPath(t, t.TempDir())
	dir, _ := cacheDir(root, s.sum)
	orig := ownerOf
	t.Cleanup(func() { ownerOf = orig })
	ownerOf = func(fi os.FileInfo) (int, int, bool) {
		uid, nlink, ok := orig(fi)
		if fi.Name() == "vpx" {
			uid = currentUID() + 4242
		}
		return uid, nlink, ok
	}
	if _, err := ensureFile(dir, libName(), s.data, s.sum); !errors.Is(err, ErrCache) {
		t.Fatalf("err = %v, want ErrCache", err)
	}
	if f := countFiles(t, root); len(f) != 0 {
		t.Fatalf("files written: %v", f)
	}
}
