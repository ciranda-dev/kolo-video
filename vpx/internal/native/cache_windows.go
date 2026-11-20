package native

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Rights that let a principal replace or delete what a directory holds, or give
// itself more rights. Adding entries (FILE_ADD_FILE, FILE_ADD_SUBDIRECTORY) is
// not in the set: like the sticky bit, it cannot touch an entry that exists.
// fileRights adds every right that changes a file's bytes or attributes.
const (
	fileDeleteChild = 0x40
	dangerousRights = windows.DELETE | windows.WRITE_DAC | windows.WRITE_OWNER | fileDeleteChild |
		windows.GENERIC_ALL | windows.GENERIC_WRITE
	fileRights = dangerousRights | windows.FILE_WRITE_DATA | windows.FILE_APPEND_DATA |
		windows.FILE_WRITE_EA | windows.FILE_WRITE_ATTRIBUTES
	anyRight       = ^uint32(0)
	inheritOnlyACE = 0x08
)

// Access control entry types (winnt.h) that grant rights; the deny types are
// skipped and any other type in a DACL is counted as granting everything.
const (
	aceAllowed               = 0x0
	aceDenied                = 0x1
	aceAllowedObject         = 0x5
	aceDeniedObject          = 0x6
	aceAllowedCallback       = 0x9
	aceDeniedCallback        = 0xa
	aceAllowedCallbackObject = 0xb
	aceDeniedCallbackObject  = 0xc
)

// trustedSIDs are the principals that may own and change the cache: the current
// user, SYSTEM, Administrators, TrustedInstaller (the owner of system folders)
// and CREATOR OWNER (an entry that applies to the creator of an object).
func trustedSIDs() ([]*windows.SID, error) {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	sids := []*windows.SID{u.User.Sid}
	for _, s := range []string{"S-1-5-18", "S-1-5-32-544", "S-1-3-0", "S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464"} {
		sid, err := windows.StringToSid(s)
		if err != nil {
			return nil, err
		}
		sids = append(sids, sid)
	}
	return sids, nil
}

func isTrusted(sid *windows.SID, trusted []*windows.SID) bool {
	for _, t := range trusted {
		if sid.Equals(t) {
			return true
		}
	}
	return false
}

// aclEntries calls fn for each entry that grants rights to the object itself
// (not inherit-only) with its principal and rights. Plain and callback
// (conditional) entries carry the SID after the mask, object entries after
// their flags and GUIDs; for any other granting type sid is nil and mask
// holds every right.
func aclEntries(acl *windows.ACL, fn func(sid *windows.SID, mask uint32)) error {
	if acl == nil {
		return nil
	}
	for i := uint32(0); i < uint32(acl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(acl, i, &ace); err != nil {
			return err
		}
		if ace.Header.AceFlags&inheritOnlyACE != 0 {
			continue
		}
		switch ace.Header.AceType {
		case aceDenied, aceDeniedObject, aceDeniedCallback, aceDeniedCallbackObject:
		case aceAllowed, aceAllowedCallback:
			fn((*windows.SID)(unsafe.Pointer(&ace.SidStart)), uint32(ace.Mask))
		case aceAllowedObject, aceAllowedCallbackObject:
			// Mask, Flags, then ObjectType and InheritedObjectType when their flag is set.
			flags := ace.SidStart
			off := uintptr(4)
			if flags&1 != 0 {
				off += 16
			}
			if flags&2 != 0 {
				off += 16
			}
			if uintptr(ace.Header.AceSize) < unsafe.Sizeof(ace.Header)+4+off+8 {
				fn(nil, anyRight)
				continue
			}
			fn((*windows.SID)(unsafe.Add(unsafe.Pointer(&ace.SidStart), off)), uint32(ace.Mask))
		default:
			fn(nil, anyRight)
		}
	}
	return nil
}

// inspect reads the owner and the access list of path. It reports whether a
// trusted principal owns it, whether the access list is protected from
// inheritance, and whether it is clean: no untrusted principal holds one of
// the rights in mask, and an access list exists.
func inspect(path string, mask uint32) (ownerOK, protected, clean bool, err error) {
	trusted, err := trustedSIDs()
	if err != nil {
		return false, false, false, err
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return false, false, false, err
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return false, false, false, err
	}
	ctl, _, err := sd.Control()
	if err != nil {
		return false, false, false, err
	}
	acl, _, err := sd.DACL()
	if err != nil {
		return false, false, false, err
	}
	clean = true
	err = aclEntries(acl, func(sid *windows.SID, rights uint32) {
		if (sid == nil || !isTrusted(sid, trusted)) && rights&mask != 0 {
			clean = false
		}
	})
	if acl == nil {
		clean = false // no access list grants everyone full access
	}
	return owner != nil && isTrusted(owner, trusted), ctl&windows.SE_DACL_PROTECTED != 0, clean, err
}

// checkDir accepts the cache directory when a trusted principal owns it and its
// access list is protected and grants only trusted principals; a directory of
// the right owner with another access list gets the protected one.
func checkDir(dir string, _ os.FileInfo) error {
	ownerOK, protected, clean, err := inspect(dir, anyRight)
	if err != nil {
		return err
	}
	if !ownerOK {
		return fmt.Errorf("%s is owned by an untrusted account", dir)
	}
	if protected && clean {
		return nil
	}
	return protectDir(dir)
}

// checkAncestor accepts a directory above the cache directory when a trusted
// principal owns it and no untrusted principal may delete or replace its
// entries or change its permissions.
func checkAncestor(dir string, _ os.FileInfo) error {
	ownerOK, _, clean, err := inspect(dir, dangerousRights)
	if err != nil {
		return err
	}
	if !ownerOK {
		return fmt.Errorf("%s is owned by an untrusted account", dir)
	}
	if !clean {
		return fmt.Errorf("%s can be changed by an untrusted account", dir)
	}
	return nil
}

// checkFile accepts a cached library that a trusted principal owns and that no
// untrusted principal can write to, delete or change the permissions of.
func checkFile(p string, _ os.FileInfo) bool {
	ownerOK, _, clean, err := inspect(p, fileRights)
	return err == nil && ownerOK && clean
}

// protectedSD is a security descriptor whose protected access list grants only
// the current user, SYSTEM and Administrators, inherited by what it holds.
func protectedSD() (*windows.SECURITY_DESCRIPTOR, error) {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	return windows.SecurityDescriptorFromString(fmt.Sprintf("D:PAI(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;%s)", u.User.Sid))
}

// protectDir gives an existing directory the protected access list.
func protectDir(dir string) error {
	sd, err := protectedSD()
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
}

// makeDir creates a directory with the protected access list from the start,
// so it never holds what its parent hands down to others.
func makeDir(dir string) error {
	sd, err := protectedSD()
	if err != nil {
		return err
	}
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return err
	}
	sa := &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	if err := windows.CreateDirectory(p, sa); err != nil {
		return &os.PathError{Op: "mkdir", Path: dir, Err: err}
	}
	return nil
}
