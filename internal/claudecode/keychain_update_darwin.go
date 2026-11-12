//go:build cgo

package claudecode

/*
#cgo LDFLAGS: -framework Security -framework CoreFoundation
#cgo CFLAGS: -Wno-deprecated-declarations
#include <Security/Security.h>
#include <stdlib.h>

// Match security add-generic-password -U: update the first file-based item in
// the default search list in place, without replacing its access controls.
static OSStatus updateExistingPassword(const char *account, UInt32 accountLength,
	const char *service, UInt32 serviceLength, const void *data, UInt32 length) {
	Boolean interactionAllowed;
	OSStatus status = SecKeychainGetUserInteractionAllowed(&interactionAllowed);
	if (status != errSecSuccess) {
		return status;
	}
	status = SecKeychainSetUserInteractionAllowed(false);
	if (status != errSecSuccess) {
		return status;
	}
	SecKeychainItemRef item = NULL;
	status = SecKeychainFindGenericPassword(NULL, serviceLength, service,
		accountLength, account, NULL, NULL, &item);
	if (status == errSecSuccess) {
		status = SecKeychainItemModifyContent(item, NULL, length, data);
		CFRelease(item);
	}
	OSStatus restore = SecKeychainSetUserInteractionAllowed(interactionAllowed);
	return status == errSecSuccess ? restore : status;
}
*/
import "C"

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"unsafe"
)

const nativeKeychainUpdate = true

// Keychain interaction is a process-wide setting. Serialize native updates
// while interaction is disabled so a dialog cannot hold Claude's locks.
var keychainUpdateMu sync.Mutex

func updateExistingKeychainItem(ctx context.Context, account, service string, value []byte) error {
	keychainUpdateMu.Lock()
	defer keychainUpdateMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	accountName := C.CString(account)
	defer C.free(unsafe.Pointer(accountName))
	serviceName := C.CString(service)
	defer C.free(unsafe.Pointer(serviceName))
	status := C.updateExistingPassword(accountName, C.UInt32(len(account)),
		serviceName, C.UInt32(len(service)), unsafe.Pointer(unsafe.SliceData(value)), C.UInt32(len(value)))
	switch status {
	case C.errSecSuccess:
		return nil
	case C.errSecItemNotFound:
		return errKeychainItemMissing
	case C.errSecInteractionNotAllowed, C.errSecAuthFailed, C.errSecNoAccessForItem, C.errSecInvalidOwnerEdit:
		return errors.New("Claude Code Keychain item is locked or denied; unlock Keychain or sign in with Claude Code")
	default:
		return fmt.Errorf("could not update the existing Claude Code Keychain item (macOS status %d)", int(status))
	}
}
