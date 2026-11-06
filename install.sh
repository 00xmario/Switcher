#!/bin/sh
# Installs or updates Switcher on this Mac or Linux computer, also over SSH:
#
#   curl -fsSL https://raw.githubusercontent.com/00xmario/switcher/main/install.sh | sh
#
# It downloads the latest release and checks its checksum. On a Mac it copies
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

# stop_switcher quits the app in $1 and waits until it has exited.
stop_switcher() {
	pattern="$1/Contents/MacOS/"
	pgrep -f "$pattern" >/dev/null 2>&1 || return 0
	osascript -e 'quit app "Switcher"' >/dev/null 2>&1 || true
	for _ in 1 2 3 4 5 6 7 8 9 10; do
		pgrep -f "$pattern" >/dev/null 2>&1 || return 0
		sleep 1
	done
	# Over SSH macOS often refuses the quit request; stop the processes.
	pkill -f "$pattern" >/dev/null 2>&1 || true
	for _ in 1 2 3 4 5 6 7 8 9 10; do
		pgrep -f "$pattern" >/dev/null 2>&1 || return 0
		sleep 1
	done
	fail "Switcher did not quit; quit it from its menu and run this again."
}

latest_tag() {
	if [ -n "${SWITCHER_TAG:-}" ]; then
		echo "$SWITCHER_TAG"
		return
	fi
	curl -fsSL "https://api.github.com/repos/$repo/releases/latest" | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -n 1
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

install_linux() {
	case "$(uname -m)" in
	x86_64 | amd64) arch=amd64 ;;
	aarch64 | arm64) arch=arm64 ;;
	*) fail "Switcher is built for x86-64 and ARM64 Linux, not $(uname -m)." ;;
	esac
	tag=$(latest_tag)
	[ -n "$tag" ] || fail "Could not find the latest Switcher release."
	version=${tag#v}
	asset="switcher-server_${version}_linux_$arch"

	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT
	echo "Downloading Switcher $version..."
	curl -fsSL -o "$tmp/$asset" "$releases/download/$tag/$asset"
	curl -fsSL -o "$tmp/$asset.sha256" "$releases/download/$tag/$asset.sha256" ||
		fail "This release publishes no checksum for Linux; nothing was installed."
	(cd "$tmp" && sha256sum -c "$asset.sha256" >/dev/null) || fail "The download does not match its checksum; nothing was installed."
	chmod 0755 "$tmp/$asset"
	"$tmp/$asset" version | grep -qx "switcher $version" || fail "The download does not run on this computer; nothing was installed."

	bin="$HOME/.local/bin"
	mkdir -p "$bin"
	# Stopping Switcher also closes its tray; remember to bring it back.
	tray_running=$(pgrep -u "$(id -u)" -x -a switcher 2>/dev/null | grep -c ' tray' || true)
	stop_linux
	# Into place in one step, so a half-copied binary never runs.
	cp "$tmp/$asset" "$bin/.switcher.new"
	mv -f "$bin/.switcher.new" "$bin/switcher"

	"$bin/switcher" autostart on >/dev/null
	"$bin/switcher" start
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

	tag=$(latest_tag)
	[ -n "$tag" ] || fail "Could not find the latest Switcher release."
	version=${tag#v}
	dmg="Switcher_$version.dmg"

	tmp=$(mktemp -d)
	trap 'hdiutil detach -quiet "$tmp/mnt" 2>/dev/null || true; rm -rf "$tmp"' EXIT

	echo "Downloading Switcher $version..."
	curl -fsSL -o "$tmp/$dmg" "$releases/download/$tag/$dmg"
	if curl -fsSL -o "$tmp/$dmg.sha256" "$releases/download/$tag/$dmg.sha256" 2>/dev/null; then
		(cd "$tmp" && shasum -a 256 -c "$dmg.sha256" >/dev/null) || fail "The download does not match its checksum; nothing was installed."
	else
		echo "This release publishes no checksum for the app; installing it without that check."
	fi

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
	if [ -d "$apps/Switcher.app" ]; then
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
