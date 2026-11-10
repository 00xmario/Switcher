//go:build !darwin || !cgo

package claudecode

import "context"

const nativeKeychainUpdate = false

func updateExistingKeychainItem(context.Context, string, string, []byte) error {
	return errKeychainLineTooLong
}
