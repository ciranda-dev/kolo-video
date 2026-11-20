package native

import (
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// addACL adds an extended access list entry to p with chmod +a and removes
// every entry again before the temporary directory is deleted.
func addACL(t *testing.T, p, entry string) {
	t.Helper()
	if out, err := exec.Command("/bin/chmod", "+a", entry, p).CombinedOutput(); err != nil {
		t.Fatalf("chmod +a: %v %s", err, out)
	}
	t.Cleanup(func() { exec.Command("/bin/chmod", "-N", p).Run() })
}

func TestCacheAncestorACLRefused(t *testing.T) {
	for name, tc := range map[string]struct {
		entry string
		ok    bool
	}{
		"everyone may add and delete entries": {"group:everyone allow add_file,add_subdirectory,delete_child", false},
		"everyone may change the permissions": {"group:everyone allow writesecurity", false},
		"everyone may take ownership":         {"group:everyone allow chown", false},
		"everyone may not delete":             {"group:everyone deny delete", true},
		"everyone may list and search":        {"group:everyone allow list,search,readattr", true},
	} {
		t.Run(name, func(t *testing.T) {
			s := stubSource(t)
			parent := filepath.Join(realPath(t, t.TempDir()), "shared")
			if err := os.Mkdir(parent, 0o755); err != nil {
				t.Fatal(err)
			}
			addACL(t, parent, tc.entry)
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

func TestCacheLeafACLRefused(t *testing.T) {
	_, dir, s := cacheTree(t)
	addACL(t, dir, "group:everyone allow add_file,delete_child")
	if _, err := ensureFile(dir, libName(), s.data, s.sum); !errors.Is(err, ErrCache) {
		t.Fatalf("err = %v, want ErrCache", err)
	}
}

func TestCacheFileACLReplaced(t *testing.T) {
	_, dir, s := cacheTree(t)
	p := filepath.Join(dir, libName())
	before, _ := os.Lstat(p)
	addACL(t, p, "group:everyone allow write,append")
	if fileHasSum(p, s.sum, len(s.data)) {
		t.Fatal("a file that everyone may write passes the check")
	}
	if _, err := ensureFile(dir, libName(), s.data, s.sum); err != nil {
		t.Fatal(err)
	}
	after, err := os.Lstat(p)
	if err != nil || os.SameFile(before, after) || !fileHasSum(p, s.sum, len(s.data)) {
		t.Fatalf("the file was not replaced by a clean one: %v", err)
	}
}

func TestACLReply(t *testing.T) {
	le := binary.NativeEndian
	reply := func(entries ...[2]uint32) []byte {
		fs := make([]byte, 44+24*len(entries))
		le.PutUint32(fs, kauthFilesecMagic)
		le.PutUint32(fs[36:], uint32(len(entries)))
		for i, e := range entries {
			le.PutUint32(fs[44+24*i+16:], e[0])
			le.PutUint32(fs[44+24*i+20:], e[1])
		}
		b := make([]byte, 12, 12+len(fs))
		le.PutUint32(b, uint32(12+len(fs)))
		le.PutUint32(b[4:], 8)
		le.PutUint32(b[8:], uint32(len(fs)))
		return append(b, fs...)
	}
	for name, tc := range map[string]struct {
		b      []byte
		writes bool
		err    bool
	}{
		"no list":              {[]byte{12, 0, 0, 0, 8, 0, 0, 0, 0, 0, 0, 0}, false, false},
		"allow add_file":       {reply([2]uint32{kauthACEPermit, 1 << 2}), true, false},
		"allow generic write":  {reply([2]uint32{kauthACEPermit, 1 << 23}), true, false},
		"deny delete":          {reply([2]uint32{2, 1 << 4}), false, false},
		"allow read":           {reply([2]uint32{kauthACEPermit, 1 << 1}), false, false},
		"inherit only":         {reply([2]uint32{kauthACEPermit | kauthACEOnlyInherit, 1 << 2}), false, false},
		"second entry":         {reply([2]uint32{2, 1 << 4}, [2]uint32{kauthACEPermit, 1 << 6}), true, false},
		"no ACL marker":        {func() []byte { b := reply(); le.PutUint32(b[12+36:], kauthNoACL); return b }(), false, false},
		"short":                {[]byte{12, 0, 0}, false, true},
		"length past buffer":   {func() []byte { b := reply(); le.PutUint32(b, 1000); return b }(), false, true},
		"offset past length":   {func() []byte { b := reply(); le.PutUint32(b[4:], 100); return b }(), false, true},
		"negative offset":      {func() []byte { b := reply(); le.PutUint32(b[4:], 0xfffffff0); return b }(), false, true},
		"bad magic":            {func() []byte { b := reply(); b[12] ^= 1; return b }(), false, true},
		"count past the reply": {func() []byte { b := reply(); le.PutUint32(b[12+36:], 2); return b }(), false, true},
		"huge count":           {func() []byte { b := reply(); le.PutUint32(b[12+36:], 0xfffffffe); return b }(), false, true},
	} {
		w, err := aclWrites(tc.b)
		if w != tc.writes || (err != nil) != tc.err {
			t.Errorf("%s: %v %v", name, w, err)
		}
	}
}
