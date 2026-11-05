//go:build linux

package fileperm

import "golang.org/x/sys/unix"

// Private groups are a Linux convention.
const privateGroupsApply = true

// hasACL reports whether the open file carries a POSIX access ACL, or
// whether that cannot be ruled out. With an ACL the group bits are the ACL
// mask, so named users or groups may be able to write.
func hasACL(fd int) bool {
	_, err := unix.Fgetxattr(fd, "system.posix_acl_access", nil)
	return err != unix.ENODATA && err != unix.ENOTSUP && err != unix.EOPNOTSUPP
}
