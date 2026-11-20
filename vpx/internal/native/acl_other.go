//go:build !darwin && !windows

package native

// aclAllowsWrite has nothing to read: a POSIX access list shows in the group
// bits of the mode, which are checked.
func aclAllowsWrite(string) (bool, error) { return false, nil }
