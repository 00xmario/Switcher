//go:build !linux

package fileperm

// Elsewhere (a Mac's staff group) group write access is never private.
const privateGroupsApply = false

func hasACL(int) bool { return true }
