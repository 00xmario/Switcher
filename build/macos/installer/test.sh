#!/bin/sh
# Checks Install Switcher.app end to end on a Mac, in scratch folders only:
# it never touches /Applications, the real home or a running Switcher.
#
#   build/macos/installer/test.sh "INSTALL_SWITCHER_APP"
#
# The installer copy carries macOS's download flag on every file, as a
# browser download does, so the test proves the installed app comes out
# without it.
set -eu
[ $# -ge 1 ] || { echo "usage: $0 INSTALL_SWITCHER_APP" >&2; exit 2; }
installer=$(cd "$1" && pwd -P)
work=$(mktemp -d "${TMPDIR:-/tmp}/installer-test.XXXXXX")
[ -n "$work" ] && [ -d "$work" ] || { echo "test.sh: no work folder" >&2; exit 1; }
# Physical paths: /var is a link to /private/var, and the switcher command
# links to the resolved path.
work=$(cd "$work" && pwd -P)
trap 'rm -rf "$work"' EXIT
fail() { echo "installer test: $*" >&2; exit 1; }
# bounded SECONDS NAME COMMAND...: runs the command, and if it is still
# running after SECONDS prints where it is stuck and stops it.
bounded() {
	b_secs=$1 b_name=$2
	shift 2
	"$@" &
	b_pid=$!
	b_ticks=0
	while kill -0 "$b_pid" 2>/dev/null; do
		b_ticks=$((b_ticks + 1))
		if [ "$b_ticks" -gt $((b_secs * 2)) ]; then
			echo "installer test: $b_name did not finish in ${b_secs}s; where it is:" >&2
			sample "$b_pid" 2 2>/dev/null | grep -v "^ *+.*mach_msg\|^ *+.*CFRunLoop" | head -60 >&2
			log show --last 5m --style compact --predicate 'process == "InstallSwitcher"' 2>/dev/null | tail -40 >&2
			kill -9 "$b_pid" 2>/dev/null
			return 124
		fi
		sleep 0.5
	done
	wait "$b_pid"
}

# A downloaded copy: every file flagged, as on a quarantined disk image.
download="$work/download/Install Switcher.app"
mkdir -p "$work/download"
ditto "$installer" "$download"
flag="0083;$(printf %x "$(date +%s)");Safari;$(uuidgen)"
find "$download" -exec xattr -w com.apple.quarantine "$flag" {} \;
xattr -p com.apple.quarantine "$download/Contents/Resources/Switcher.app/Contents/MacOS/SwitcherServer" >/dev/null ||
	fail "the test could not flag the download"

version=$(/usr/libexec/PlistBuddy -c 'Print :CFBundleShortVersionString' "$download/Contents/Resources/Switcher.app/Contents/Info.plist")
apps="$work/Applications" home="$work/home" bin="$work/bin"
mkdir -p "$apps" "$home" "$bin"
exe="$download/Contents/MacOS/InstallSwitcher"

check_install() {
	app="$apps/Switcher.app"
	[ -d "$app" ] || fail "no Switcher.app in $apps"
	[ "$(/usr/libexec/PlistBuddy -c 'Print :CFBundleShortVersionString' "$app/Contents/Info.plist")" = "$version" ] || fail "installed the wrong version"
	if xattr -r -l "$app" | grep -q com.apple.quarantine; then fail "the installed app still carries the download flag"; fi
	codesign --verify --deep --strict "$app" || fail "the installed app's signature does not check out"
	cmp -s "$app/Contents/MacOS/SwitcherServer" "$installer/Contents/Resources/Switcher.app/Contents/MacOS/SwitcherServer" || fail "the installed server differs from the one carried"
	[ "$("$app/Contents/MacOS/SwitcherServer" version)" = "switcher $version" ] || fail "the installed server does not run"
	[ "$(readlink "$bin/switcher")" = "$app/Contents/MacOS/SwitcherServer" ] || fail "the switcher command does not point at the installed app"
	agent="$home/Library/LaunchAgents/sh.switcher.app.plist"
	[ "$(/usr/libexec/PlistBuddy -c 'Print :ProgramArguments:0' "$agent")" = "$app/Contents/MacOS/Switcher" ] || fail "the login item does not open the installed app"
	[ "$(/usr/libexec/PlistBuddy -c 'Print :RunAtLoad' "$agent")" = true ] || fail "the login item does not run at login"
	ls -a "$apps" | grep -q '^\.switcher-install-' && fail "a staging folder was left behind"
	ls -a "$apps" | grep -q 'previous' && fail "the previous app was kept"
	return 0
}

echo "installer test: fresh install"
bounded 180 "the fresh install" "$exe" --install-test "$apps" "$home" "$bin" >"$work/fresh.log" 2>&1 || { cat "$work/fresh.log"; fail "the fresh install failed"; }
cat "$work/fresh.log"
grep -q '"relation":"fresh"' "$work/fresh.log" || fail "a first install was not seen as fresh"
check_install
[ -f "$home/.agents/skills/switcher/SKILL.md" ] || fail "the skill was not installed for the agents"

echo "installer test: the same version again"
bounded 180 "the reinstall" "$exe" --install-test "$apps" "$home" "$bin" >"$work/same.log" 2>&1 || { cat "$work/same.log"; fail "reinstalling failed"; }
grep -q '"relation":"same"' "$work/same.log" || fail "a reinstall was not seen as one"
grep -q 'step skill' "$work/same.log" && fail "a reinstall installed the skill again"
check_install

echo "installer test: an update over an older Switcher that is running"
/usr/libexec/PlistBuddy -c "Set :CFBundleShortVersionString 1.0.0" "$apps/Switcher.app/Contents/Info.plist"
# A stand-in for the running server: a process running the old app's
# SwitcherServer as a server runs (with flags; a switcher command, which is
# the same binary, is left alone), detached so that launchd, not this
# shell, collects it when it stops.
stand_in="$apps/Switcher.app/Contents/MacOS/SwitcherServer"
printf '#include <unistd.h>\nint main(void) { for (;;) pause(); }\n' >"$work/stand-in.c"
cc -o "$stand_in" "$work/stand-in.c"
("$stand_in" --port 18787 >/dev/null 2>&1 &)
# A switcher command from the same app, which the installer must not stop.
("$stand_in" status >/dev/null 2>&1 &)
sleep 1
stand_in_pid=$(pgrep -f "$stand_in --port 18787" | head -n 1)
command_pid=$(pgrep -f "$stand_in status" | head -n 1)
[ -n "$command_pid" ] || fail "the stand-in for a switcher command did not start"
[ -n "$stand_in_pid" ] || fail "the stand-in for a running Switcher did not start"
bounded 180 "the update" "$exe" --install-test "$apps" "$home" "$bin" >"$work/update.log" 2>&1 || { cat "$work/update.log"; kill "$stand_in_pid" 2>/dev/null; fail "the update failed"; }
cat "$work/update.log"
grep -q '"relation":"update"' "$work/update.log" || fail "an older install was not seen as an update"
grep -q '"running":true' "$work/update.log" || fail "the running Switcher was not seen"
# The new app goes in place first; the old one is stopped after.
grep -n 'step place done' "$work/update.log" | head -n 1 | cut -d: -f1 >"$work/place"
grep -n 'step stop done' "$work/update.log" | head -n 1 | cut -d: -f1 >"$work/stop"
[ -s "$work/stop" ] || fail "the running Switcher was not stopped"
[ "$(cat "$work/place")" -lt "$(cat "$work/stop")" ] || fail "the running Switcher was stopped before the new one was in place"
sleep 1
if kill -0 "$stand_in_pid" 2>/dev/null; then kill "$stand_in_pid"; fail "the running Switcher is still running"; fi
if pgrep -f "$stand_in --port 18787" >/dev/null; then pkill -f "$stand_in --port 18787"; fail "a Switcher server from the old app still runs"; fi
kill -0 "$command_pid" 2>/dev/null || fail "a switcher command someone was running was stopped"
kill "$command_pid"
check_install

echo "installer test: a newer Switcher is left alone"
/usr/libexec/PlistBuddy -c "Set :CFBundleShortVersionString 99.0.0" "$apps/Switcher.app/Contents/Info.plist"
status=0
bounded 60 "the refused downgrade" "$exe" --install-test "$apps" "$home" "$bin" >"$work/newer.log" 2>&1 || status=$?
cat "$work/newer.log"
[ "$status" = 1 ] || fail "a downgrade ended with $status, not a refusal"
grep -q '"relation":"newer"' "$work/newer.log" || { cat "$work/newer.log"; fail "a newer install was not seen as newer"; }
[ "$(/usr/libexec/PlistBuddy -c 'Print :CFBundleShortVersionString' "$apps/Switcher.app/Contents/Info.plist")" = 99.0.0 ] || fail "a newer Switcher was changed"

echo "installer test: another app called Switcher.app is left alone"
other="$work/Other"
mkdir -p "$other/Switcher.app/Contents"
printf '<?xml version="1.0" encoding="UTF-8"?>\n<plist version="1.0"><dict><key>CFBundleIdentifier</key><string>com.example.switcher</string><key>CFBundleShortVersionString</key><string>2.0</string></dict></plist>\n' >"$other/Switcher.app/Contents/Info.plist"
status=0
bounded 60 "the other app" "$exe" --install-test "$other" "$home" "$bin" >"$work/other.log" 2>&1 || status=$?
cat "$work/other.log"
[ "$status" = 1 ] || fail "another app called Switcher.app did not stop the install ($status)"
grep -q "com.example.switcher" "$other/Switcher.app/Contents/Info.plist" || fail "another app called Switcher.app was replaced"

echo "installer test: passed (fresh, reinstall, update over a running app, newer left alone, another app left alone)"
