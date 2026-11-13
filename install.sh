#!/bin/sh
# Installs or updates Switcher on this Mac or Linux computer, also over SSH:
#
#   curl -fsSL https://raw.githubusercontent.com/00xmario/switcher/main/install.sh | sh
#
# It downloads the latest release over HTTPS only and installs nothing that
# does not match the release's checksums, signed with Switcher's release key
# (checked with ssh-keygen). On a Mac it copies
# Switcher.app to /Applications (or ~/Applications without write access) and
# puts the switcher command on your PATH. On Linux it installs the switcher
# binary as ~/.local/bin/switcher, starts it at login (a systemd user service,
# and the tray with your desktop) and opens the tray. Then it starts
# Switcher. A running Switcher is stopped first, which interrupts requests
# that are still in flight.
set -eu

repo=00xmario/switcher
# Where releases come from; tests point it at a local copy.
releases=${SWITCHER_RELEASES:-https://github.com/$repo/releases}

fail() {
	echo "$*" >&2
	exit 1
}

# fetch downloads over HTTPS only, redirects included, so nothing on the way
# can move a download to a plain connection, and no larger than $1 bytes. A
# test's local copy is exempt from HTTPS.
fetch() {
	max=$1
	shift
	case "$releases" in
	https://*) curl --proto '=https' --proto-redir '=https' --tlsv1.2 --max-filesize "$max" -fsSL "$@" ;;
	*) curl --max-filesize "$max" -fsSL "$@" ;;
	esac
}

# The largest download: release files are tens of megabytes.
max_download=268435456

# signing_tools makes sure the release's signature can be checked before
# anything is downloaded: ssh-keygen -Y arrived in OpenSSH 8.1.
signing_tools() {
	command -v ssh-keygen >/dev/null 2>&1 ||
		fail "Checking the release's signature needs ssh-keygen (OpenSSH 8.1 or newer); install it and run this again."
	# An ssh-keygen without -Y says the option is unknown, in its C library's
	# words: "unknown option -- Y", "illegal option -- Y", "invalid option --
	# 'Y'", "unrecognized option: Y".
	if ssh-keygen -Y verify </dev/null 2>&1 | grep -Eq "option[^A-Za-z]*'?Y'?([^A-Za-z]|\$)"; then
		fail "Checking the release's signature needs OpenSSH 8.1 or newer; this ssh-keygen is older. Update it and run this again."
	fi
}

# newer_than says whether version $1 is newer than version $2 (X.Y.Z).
newer_than() {
	awk -v a="$1" -v b="$2" 'BEGIN {
		n = split(a, x, "."); m = split(b, y, ".")
		for (i = 1; i <= (n > m ? n : m); i++) {
			if (x[i] + 0 > y[i] + 0) exit 0
			if (x[i] + 0 < y[i] + 0) exit 1
		}
		exit 1
	}'
}

# no_downgrade refuses to replace installed version $1 with an older one:
# an older release can have flaws a newer one fixed. SWITCHER_TAG, set by
# you, installs the release it names all the same.
no_downgrade() {
	[ -n "$1" ] && [ -z "${SWITCHER_TAG:-}" ] || return 0
	if newer_than "$1" "$version"; then
		fail "Switcher $1 is installed, newer than the latest release, $version; nothing was installed. To install $version all the same: SWITCHER_TAG=v$version"
	fi
}

# A release tag is vX.Y.Z and nothing else: it becomes part of file names
# and addresses.
check_tag() {
	case "$1" in
	v[0-9]*) ;;
	*) fail "Unexpected release name: $1" ;;
	esac
	case "${1#v}" in
	*[!0-9.]* | *..* | .* | *.) fail "Unexpected release name: $1" ;;
	esac
}

# The keys Switcher's releases are signed with, in ssh-keygen's
# allowed_signers form: the one releases are signed with, and one kept
# offline in case that one is lost. See build/release-sign.sh.
release_signers='switcher-release namespaces="switcher-release" ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIO2UJMe8yL/18YTufkesQEr9AjT32/Z8JzvCgtKjvya0 switcher-release-1
switcher-release namespaces="switcher-release" ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAINkxLFXWXQDLvgFD+nK2tMMikz25kdCf3rh1W9F4Cmzp switcher-release-2'

# signed_checksum downloads the release's list of checksums and its
# signature into $1, checks the signature, and leaves the one line for the
# file $2 in $1/$2.sha256. Without a good signature nothing is installed:
# whoever can change a release on GitHub cannot sign one.
signed_checksum() {
	fetch 65536 -o "$1/SHA256SUMS" "$releases/download/$tag/SHA256SUMS" 2>/dev/null ||
		fail "This release publishes no signed checksums; nothing was installed."
	fetch 65536 -o "$1/SHA256SUMS.sig" "$releases/download/$tag/SHA256SUMS.sig" 2>/dev/null ||
		fail "This release is not signed; nothing was installed."
	printf '%s\n' "$release_signers" >"$1/allowed_signers"
	ssh-keygen -Y verify -f "$1/allowed_signers" -I switcher-release -n switcher-release \
		-s "$1/SHA256SUMS.sig" <"$1/SHA256SUMS" >/dev/null 2>&1 ||
		fail "This release is not signed with Switcher's release key; nothing was installed."
	awk -v asset="$2" '{ name = $2; sub(/^\*/, "", name) } name == asset' "$1/SHA256SUMS" >"$1/$2.sha256"
}

# checksum_names says whether the checksum file $1 is one line for the file
# $2. A list that named another file would pass without checking the
# download.
checksum_names() {
	[ "$(awk 'END { print NR }' "$1")" = 1 ] || return 1
	[ "$(awk '{ name = $2; sub(/^\*/, "", name); print name }' "$1")" = "$2" ]
}

# stop_switcher quits the app in $1 and waits until it has exited.
stop_switcher() {
	pattern="$1/Contents/MacOS/"
	# This user's processes only: another account's cannot hold this up.
	me=$(id -u)
	pgrep -u "$me" -f "$pattern" >/dev/null 2>&1 || return 0
	osascript -e 'quit app "Switcher"' >/dev/null 2>&1 || true
	for _ in 1 2 3 4 5 6 7 8 9 10; do
		pgrep -u "$me" -f "$pattern" >/dev/null 2>&1 || return 0
		sleep 1
	done
	# Over SSH macOS often refuses the quit request; stop the processes.
	pkill -u "$me" -f "$pattern" >/dev/null 2>&1 || true
	for _ in 1 2 3 4 5 6 7 8 9 10; do
		pgrep -u "$me" -f "$pattern" >/dev/null 2>&1 || return 0
		sleep 1
	done
	fail "Switcher did not quit; quit it from its menu and run this again."
}

latest_tag() {
	if [ -n "${SWITCHER_TAG:-}" ]; then
		echo "$SWITCHER_TAG"
		return
	fi
	curl --proto '=https' --proto-redir '=https' --tlsv1.2 --max-filesize 1048576 -fsSL "https://api.github.com/repos/$repo/releases/latest" | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -n 1
}

# stop_linux stops a running Switcher: the login service, or a server and
# tray started by hand. It waits until they are gone.
stop_linux() {
	if command -v systemctl >/dev/null 2>&1 && systemctl --user is-active --quiet switcher.service 2>/dev/null; then
		systemctl --user stop switcher.service || true
	fi
	pkill -u "$(id -u)" -x switcher >/dev/null 2>&1 || true
	for _ in 1 2 3 4 5 6 7 8 9 10; do
		pgrep -u "$(id -u)" -x switcher >/dev/null 2>&1 || return 0
		sleep 1
	done
	fail "Switcher did not stop; stop it with: pkill -x switcher, and run this again."
}

# install_skill puts the skill that teaches AI agents (Claude Code, Codex and
# others) to use Switcher in ~/.agents/skills, on a first install only: an
# update leaves the skill to Switcher, which keeps its own copy current and
# leaves one you removed or edited alone. SWITCHER_SKILL=0 skips it; a
# release without the skill goes without.
install_skill() {
	[ "${SWITCHER_SKILL:-1}" = 0 ] && return 0
	[ "$2" = fresh ] || return 0
	grep -q "switcher skill install" "$1" 2>/dev/null || return 0
	if "$1" skill install >/dev/null 2>&1; then
		echo "Your AI agents know Switcher now: its skill is in ~/.agents/skills/switcher (SWITCHER_SKILL=0 skips this)."
	fi
}

install_linux() {
	case "$(uname -m)" in
	x86_64 | amd64) arch=amd64 ;;
	aarch64 | arm64) arch=arm64 ;;
	*) fail "Switcher is built for x86-64 and ARM64 Linux, not $(uname -m)." ;;
	esac
	signing_tools
	tag=$(latest_tag)
	[ -n "$tag" ] || fail "Could not find the latest Switcher release."
	check_tag "$tag"
	version=${tag#v}
	asset="switcher-server_${version}_linux_$arch"
	bin="$HOME/.local/bin"
	installed=
	if [ -x "$bin/switcher" ]; then
		installed=$("$bin/switcher" version 2>/dev/null | sed -n 's/^switcher \([0-9][0-9.]*\)$/\1/p' | head -n 1)
	fi
	no_downgrade "$installed"

	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT
	echo "Downloading Switcher $version..."
	fetch "$max_download" -o "$tmp/$asset" "$releases/download/$tag/$asset"
	signed_checksum "$tmp" "$asset"
	checksum_names "$tmp/$asset.sha256" "$asset" || fail "The checksum is not for this download; nothing was installed."
	(cd "$tmp" && sha256sum -c "$asset.sha256" >/dev/null) || fail "The download does not match its checksum; nothing was installed."
	chmod 0755 "$tmp/$asset"
	"$tmp/$asset" version | grep -qx "switcher $version" || fail "The download does not run on this computer; nothing was installed."

	mkdir -p "$bin"
	# Stopping Switcher also closes its tray; remember to bring it back.
	tray_running=$(pgrep -u "$(id -u)" -x -a switcher 2>/dev/null | grep -c ' tray' || true)
	stop_linux
	first=fresh
	[ -e "$bin/switcher" ] && first=update
	# Into place in one step, so a half-copied binary never runs.
	cp "$tmp/$asset" "$bin/.switcher.new"
	mv -f "$bin/.switcher.new" "$bin/switcher"

	"$bin/switcher" autostart on >/dev/null
	"$bin/switcher" start
	install_skill "$bin/switcher" "$first"
	if [ -n "${DISPLAY:-}${WAYLAND_DISPLAY:-}" ]; then
		nohup "$bin/switcher" tray >/dev/null 2>&1 &
	elif [ "${tray_running:-0}" != 0 ] && command -v systemd-run >/dev/null 2>&1 &&
		systemctl --user show-environment 2>/dev/null | grep -qE '^(WAYLAND_DISPLAY|DISPLAY)='; then
		# Over SSH the tray that was open on the desktop comes back there:
		# the login's service manager knows the desktop session.
		systemd-run --user --collect --quiet "$bin/switcher" tray >/dev/null 2>&1 || true
	fi
	echo
	echo "Switcher $version is installed as $bin/switcher and starts when you log in."
	case ":$PATH:" in
	*":$bin:"*) ;;
	*) echo "Add it to your PATH: echo 'export PATH=\"\$HOME/.local/bin:\$PATH\"' >> ~/.bashrc" ;;
	esac
	echo "The dashboard is at http://127.0.0.1:8787 (switcher open)."
	echo "Next: switcher login claude, or switcher connect <host> --code <code>"
}

main() {
	case "$(uname -s)" in
	Darwin) ;;
	Linux)
		install_linux
		return 0
		;;
	*) fail "Switcher runs on macOS and Linux." ;;
	esac

	signing_tools
	tag=$(latest_tag)
	[ -n "$tag" ] || fail "Could not find the latest Switcher release."
	check_tag "$tag"
	version=${tag#v}
	dmg="Switcher_$version.dmg"
	installed=
	for app in /Applications/Switcher.app "$HOME/Applications/Switcher.app"; do
		[ -n "$installed" ] || [ ! -f "$app/Contents/Info.plist" ] ||
			installed=$(/usr/libexec/PlistBuddy -c 'Print :CFBundleShortVersionString' "$app/Contents/Info.plist" 2>/dev/null | sed -n 's/^\([0-9][0-9.]*\)$/\1/p')
	done
	no_downgrade "$installed"

	tmp=$(mktemp -d)
	trap 'hdiutil detach -quiet "$tmp/mnt" 2>/dev/null || true; rm -rf "$tmp"' EXIT

	echo "Downloading Switcher $version..."
	fetch "$max_download" -o "$tmp/$dmg" "$releases/download/$tag/$dmg"
	signed_checksum "$tmp" "$dmg"
	checksum_names "$tmp/$dmg.sha256" "$dmg" || fail "The checksum is not for this download; nothing was installed."
	(cd "$tmp" && shasum -a 256 -c "$dmg.sha256" >/dev/null) || fail "The download does not match its checksum; nothing was installed."

	apps=/Applications
	if [ ! -w "$apps" ]; then
		apps="$HOME/Applications"
		mkdir -p "$apps"
	fi
	hdiutil attach -quiet -nobrowse -readonly -mountpoint "$tmp/mnt" "$tmp/$dmg"
	[ -d "$tmp/mnt/Switcher.app" ] || fail "The download contains no Switcher.app; nothing was installed."
	# Older releases were built for Apple silicon only.
	arch=$(uname -m)
	# file, not lipo: lipo needs the developer tools, which most Macs lack.
	/usr/bin/file -b "$tmp/mnt/Switcher.app/Contents/MacOS/Switcher" | grep -qw "$arch" ||
		fail "Switcher $version does not run on this Mac ($arch); nothing was installed."
	first=fresh
	if [ -d "$apps/Switcher.app" ]; then
		first=update
		stop_switcher "$apps/Switcher.app"
		rm -rf "$apps/Switcher.app"
	fi
	ditto "$tmp/mnt/Switcher.app" "$apps/Switcher.app"

	server="$apps/Switcher.app/Contents/MacOS/SwitcherServer"
	# Releases before the switcher command treat any argument as "serve".
	if ! grep -q "install-cli" "$server"; then
		open -g "$apps/Switcher.app" 2>/dev/null || true
		echo "Switcher $version is installed. The switcher command arrives with the next release."
		return 0
	fi
	"$server" install-cli
	"$server" start
	install_skill "$server" "$first"
	if "$server" status --json 2>/dev/null | grep -q "\"version\":\"$version\""; then
		echo
		echo "Switcher $version is installed and running."
	else
		echo
		echo "Switcher $version is installed, but another Switcher still answers on this Mac; quit it and run: switcher start"
	fi
	echo "Next: switcher login claude, or switcher connect <host> --code <code>"
}

main "$@"
