//go:build !unix

package remote

// trustedBinary trusts no program where file ownership cannot be checked.
func trustedBinary(string) bool { return false }
