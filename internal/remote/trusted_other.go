//go:build !unix

package remote

// resolveTrusted trusts no program where file ownership cannot be checked.
func resolveTrusted(string) (string, bool) { return "", false }
