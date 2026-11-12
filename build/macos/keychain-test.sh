#!/bin/sh
# Tests the native Keychain writer against a real keychain: an item made by
# /usr/bin/security, as Claude Code makes its own, updated by a freshly built
# binary that macOS has never seen. It makes a throwaway keychain the default
# one, so it runs only on a CI runner, never on a Mac someone uses.
set -eu
if [ "${CI:-}" != "true" ]; then
  echo "keychain-test.sh changes the default keychain; it runs only in CI" >&2
  exit 1
fi
tmp=$(mktemp -d)
kc="$tmp/switcher-test.keychain-db"
trap 'security delete-keychain "$kc" 2>/dev/null || true; rm -rf "$tmp"' EXIT
security create-keychain -p switcher-test "$kc"
security set-keychain-settings "$kc"
security unlock-keychain -p switcher-test "$kc"
security list-keychains -d user -s "$kc"
security default-keychain -d user -s "$kc"

# Signed ad hoc, as the released app is.
go test -c -o "$tmp/keychain.test" ./internal/claudecode/
codesign --force -s - "$tmp/keychain.test"
SWITCHER_REAL_KEYCHAIN_TEST=1 "$tmp/keychain.test" -test.v -test.count=3 \
  -test.run 'TestRealKeychain(OversizedUpdate|ManagerSwitchAndRefresh)'

# The item's access control list is the same after a native update, so
# /usr/bin/security, and with it Claude Code, still owns the item.
item=Switcher-test-acl
printf 'add-generic-password -U -a "%s" -s "%s" -X %s\n' "$USER" "$item" "$(printf small | xxd -p)" | security -i
dump() { perl -e 'alarm 30; exec @ARGV' security dump-keychain -a "$kc" 2>&1 | grep -v '"mdat"' > "$1" || true; }
dump "$tmp/acl-before.txt"
SWITCHER_REAL_KEYCHAIN_UPDATE="$item" "$tmp/keychain.test" -test.v -test.run TestRealKeychainUpdateNamed
dump "$tmp/acl-after.txt"
grep -q 'authorizations' "$tmp/acl-before.txt"
diff "$tmp/acl-before.txt" "$tmp/acl-after.txt"
test "$(security find-generic-password -a "$USER" -s "$item" -w | wc -c | tr -d ' ')" = 9001

# Locked, with nobody to answer a dialog: an error at once, not a wait.
security lock-keychain "$kc"
SWITCHER_REAL_KEYCHAIN_LOCKED="$item" "$tmp/keychain.test" -test.v -test.run TestRealKeychainLockedFailsFast
echo "native Keychain writer verified on $(sw_vers -productVersion) $(uname -m)"
