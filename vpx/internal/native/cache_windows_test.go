package native

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestCacheDirACL(t *testing.T) {
	s := stubSource(t)
	dir, _ := cacheDir(t.TempDir(), s.sum)
	p, err := ensureFile(dir, libName(), s.data, s.sum)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{dir, p} {
		sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			t.Fatal(err)
		}
		ctl, _, err := sd.Control()
		if err != nil {
			t.Fatal(err)
		}
		if ctl&windows.SE_DACL_PROTECTED == 0 && path == dir {
			t.Errorf("%s: DACL inherits from its parent", path)
		}
		acl, _, err := sd.DACL()
		if err != nil || acl == nil {
			t.Fatalf("%s: DACL %v %v", path, acl, err)
		}
		if acl.AceCount != 3 {
			t.Errorf("%s: %d access entries, want 3 (user, SYSTEM, Administrators)", path, acl.AceCount)
		}
	}
	if fi, err := os.Stat(p); err != nil || fi.Mode().Perm()&0o200 != 0 {
		t.Errorf("cached DLL is writable: %v %v", fi, err)
	}
}

// setDACL replaces the access list of path with the one SDDL describes; user is
// the current user's SID text, for entries that keep the owner in.
func setDACL(t *testing.T, path, sddl string) {
	t.Helper()
	if err := trySetDACL(path, sddl); err != nil {
		t.Fatal(err)
	}
}

func trySetDACL(path, sddl string) error {
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
}

// aceCount returns the number of entries of path's access list and whether it
// is protected from inheritance.
func aceCount(t *testing.T, path string) (int, bool) {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	ctl, _, err := sd.Control()
	if err != nil {
		t.Fatal(err)
	}
	acl, _, err := sd.DACL()
	if err != nil || acl == nil {
		t.Fatalf("%s: DACL %v %v", path, acl, err)
	}
	return int(acl.AceCount), ctl&windows.SE_DACL_PROTECTED != 0
}

func userSID(t *testing.T) string {
	t.Helper()
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	return u.User.Sid.String()
}

func TestCacheAncestorACL(t *testing.T) {
	user := userSID(t)
	for name, tc := range map[string]struct {
		extra string
		ok    bool
	}{
		"everyone full control":                   {"(A;OICI;FA;;;WD)", false},
		"users may delete children":               {"(A;OICI;0x40;;;BU)", false},
		"users may change permissions":            {"(A;OICI;WD;;;BU)", false},
		"everyone may add files and folders only": {"(A;;0x6;;;WD)", true},
		"everyone may read":                       {"(A;OICI;FR;;;WD)", true},
		// A conditional entry is counted as if its condition held.
		"a conditional entry for everyone": {"(XA;OICI;FA;;;WD;(Member_of {SID(BA)}))", false},
		// New directories below it are protected from what it hands down.
		"an inheritable entry for everyone, not for the directory itself": {"(A;OICIIO;FA;;;WD)", true},
	} {
		t.Run(name, func(t *testing.T) {
			s := stubSource(t)
			parent := filepath.Join(t.TempDir(), "shared")
			if err := os.Mkdir(parent, 0o700); err != nil {
				t.Fatal(err)
			}
			setDACL(t, parent, fmt.Sprintf("D:PAI(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;%s)%s", user, tc.extra))
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

func TestCacheLeafACLRepaired(t *testing.T) {
	s := stubSource(t)
	dir, _ := cacheDir(t.TempDir(), s.sum)
	if _, err := ensureFile(dir, libName(), s.data, s.sum); err != nil {
		t.Fatal(err)
	}
	// Anyone may read the directory and inherits it to the file: not a cache this package made.
	setDACL(t, dir, fmt.Sprintf("D:PAI(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;%s)(A;OICI;FR;;;WD)", userSID(t)))
	p, err := ensureFile(dir, libName(), s.data, s.sum)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{dir, p} {
		sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			t.Fatal(err)
		}
		acl, _, err := sd.DACL()
		if err != nil || acl == nil || acl.AceCount != 3 {
			t.Fatalf("%s: access list after the repair: %v %v", path, acl, err)
		}
	}
}

func TestCacheFileWritableByOthersReplaced(t *testing.T) {
	for name, tc := range map[string]struct {
		extra string
		ok    bool
	}{
		"everyone full control":                {"(A;;FA;;;WD)", false},
		"everyone generic file write":          {"(A;;FW;;;WD)", false},
		"users may write data":                 {"(A;;0x2;;;BU)", false},
		"users may append data":                {"(A;;0x4;;;BU)", false},
		"users may write extended attributes":  {"(A;;0x10;;;BU)", false},
		"users may write attributes":           {"(A;;0x100;;;BU)", false},
		"users may delete":                     {"(A;;SD;;;BU)", false},
		"users may change permissions":         {"(A;;WD;;;BU)", false},
		"a conditional write entry":            {"(XA;;FW;;;WD;(Member_of {SID(BA)}))", false},
		"everyone may read":                    {"(A;;FR;;;WD)", true},
		"everyone is denied writing (no harm)": {"(D;;FW;;;WD)", true},
	} {
		t.Run(name, func(t *testing.T) {
			s := stubSource(t)
			dir, _ := cacheDir(t.TempDir(), s.sum)
			p, err := ensureFile(dir, libName(), s.data, s.sum)
			if err != nil {
				t.Fatal(err)
			}
			before, _ := os.Lstat(p)
			os.SameFile(before, before) // Windows reads the file ID on first use: take it now
			setDACL(t, p, fmt.Sprintf("D:PAI%s(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;%s)", tc.extra, userSID(t)))
			if got := fileHasSum(p, s.sum, len(s.data)); got != tc.ok {
				t.Fatalf("check gives %v, want %v", got, tc.ok)
			}
			if _, err := ensureFile(dir, libName(), s.data, s.sum); err != nil {
				t.Fatal(err)
			}
			after, _ := os.Lstat(p)
			if os.SameFile(before, after) != tc.ok || !fileHasSum(p, s.sum, len(s.data)) {
				t.Fatal("the file was not replaced by a trusted one")
			}
		})
	}
}

// TestCacheFileGrantedByIcacls grants everyone write access the way an
// administrator would, with icacls, and expects the file to be refused.
func TestCacheFileGrantedByIcacls(t *testing.T) {
	s := stubSource(t)
	dir, _ := cacheDir(t.TempDir(), s.sum)
	p, err := ensureFile(dir, libName(), s.data, s.sum)
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("icacls", p, "/grant", "*S-1-1-0:(W)").CombinedOutput(); err != nil {
		t.Fatalf("icacls: %v %s", err, out)
	}
	if fileHasSum(p, s.sum, len(s.data)) {
		t.Fatal("a file that everyone can write passes the check")
	}
	if _, err := openCached(p, s); !errors.Is(err, ErrCache) {
		t.Fatalf("openCached: %v, want ErrCache", err)
	}
}

// TestCacheObjectACERefused: an object entry keeps its SID after GUIDs; one for
// everyone must be found there. Skipped where the file system refuses to store it.
func TestCacheObjectACERefused(t *testing.T) {
	s := stubSource(t)
	dir, _ := cacheDir(t.TempDir(), s.sum)
	p, err := ensureFile(dir, libName(), s.data, s.sum)
	if err != nil {
		t.Fatal(err)
	}
	sddl := fmt.Sprintf("D:PAI(OA;;FA;00000000-0000-0000-0000-000000000001;;WD)(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;%s)", userSID(t))
	if err := trySetDACL(p, sddl); err != nil {
		t.Skipf("an object entry cannot be set on a file here: %v", err)
	}
	if fileHasSum(p, s.sum, len(s.data)) {
		t.Fatal("an object entry for everyone passes the check")
	}
}

// TestCacheTrustedObjectACEAccepted: an object entry for the current user, after
// the other entries, is read at the SID behind its GUIDs and passes. A SID read
// from the wrong offset would look untrusted and replace a good file every time.
func TestCacheTrustedObjectACEAccepted(t *testing.T) {
	const g1, g2 = "00000000-0000-0000-0000-000000000001", "00000000-0000-0000-0000-000000000002"
	for name, guids := range map[string]string{
		"object type":           g1 + ";",
		"inherited object type": ";" + g2,
		"both":                  g1 + ";" + g2,
	} {
		t.Run(name, func(t *testing.T) {
			s := stubSource(t)
			dir, _ := cacheDir(t.TempDir(), s.sum)
			p, err := ensureFile(dir, libName(), s.data, s.sum)
			if err != nil {
				t.Fatal(err)
			}
			user := userSID(t)
			sddl := fmt.Sprintf("D:PAI(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;%s)(OA;;FA;%s;%s)", user, guids, user)
			if err := trySetDACL(p, sddl); err != nil {
				t.Skipf("an object entry cannot be set on a file here: %v", err)
			}
			if n, _ := aceCount(t, p); n != 4 {
				t.Fatalf("%d entries stored, want 4", n)
			}
			if !fileHasSum(p, s.sum, len(s.data)) {
				t.Fatal("an object entry for the current user is refused")
			}
		})
	}
}

// TestCacheMakeDirProtected checks the access list of a new directory at once:
// a directory created the plain way first takes what its parent hands down.
func TestCacheMakeDirProtected(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "shared")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	setDACL(t, parent, fmt.Sprintf("D:PAI(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;%s)(A;OICIIO;FA;;;WD)", userSID(t)))
	plain := filepath.Join(parent, "plain")
	if err := os.Mkdir(plain, 0o700); err != nil {
		t.Fatal(err)
	}
	if n, prot := aceCount(t, plain); n == 3 && prot {
		t.Fatal("control: a plain directory did not inherit the entry for everyone")
	}
	made := filepath.Join(parent, "made")
	if err := makeDir(made); err != nil {
		t.Fatal(err)
	}
	if n, prot := aceCount(t, made); n != 3 || !prot {
		t.Fatalf("new directory: %d entries, protected %v", n, prot)
	}
	if err := makeDir(made); !os.IsExist(err) {
		t.Fatalf("second makeDir: %v", err)
	}
	s := stubSource(t)
	dir, _ := cacheDir(filepath.Join(parent, "kv"), s.sum)
	if _, err := ensureFile(dir, libName(), s.data, s.sum); err != nil {
		t.Fatal(err)
	}
	for d := dir; d != parent; d = filepath.Dir(d) {
		if n, prot := aceCount(t, d); n != 3 || !prot {
			t.Errorf("%s: %d entries, protected %v", d, n, prot)
		}
	}
}
