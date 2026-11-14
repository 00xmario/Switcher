#!/bin/sh
# Starts the real server in a throwaway home and reaches it the way real
# clients do: from other processes. curl, the switcher command and, on a
# Mac, Foundation's URL loading (as the menu bar app uses) and Python. The
# same-user check must be on, and must serve every one of them, on the main
# port, the Desktop relay's and the sign-in callbacks', without once failing
# to see whose a connection is. Runs in `make verify` and `make verify-linux`.
#
# It touches nothing of a running Switcher's: its own home, its own ports
# (a callback port another program holds is left alone), and the variables
# that would point it elsewhere are cleared.
set -eu

# Its own folder, by its real path: on a Mac the temporary folder is reached
# through the /var symlink, which the Desktop relay's store refuses, as it
# would any home reached through one. Only that folder is ever removed.
tmp=$(mktemp -d) || exit 1
[ -n "$tmp" ] && [ -d "$tmp" ] || exit 1
work=$(cd "$tmp" && pwd -P) || exit 1
pid=
cleanup() {
	trap '' INT TERM HUP # a second signal does not cut the cleanup short
	[ -z "$pid" ] || kill "$pid" 2>/dev/null || true
	[ -z "$pid" ] || wait "$pid" 2>/dev/null || true
	rm -rf "$tmp"
}
trap cleanup EXIT
# An interrupted run cleans up too (dash runs no EXIT trap on a signal).
trap 'exit 130' INT TERM HUP
fail() {
	echo "e2e-local: $*" >&2
	[ ! -f "$work/home/server.log" ] || sed 's/^/  server: /' "$work/home/server.log" >&2
	exit 1
}
# Nothing from the developer's own setup: not the check's switch, not
# another Switcher's address, not the real CLIs' folders (Claude Code's is
# this home's own, which on a Mac also names its own Keychain item), not a
# proxy.
unset_vars="-u SWITCHER_ALLOW_LOCAL_USERS -u SWITCHER_URL -u CODEX_HOME -u GROK_HOME
	-u CLAUDE_SECURESTORAGE_CONFIG_DIR -u SWITCHER_PEER_PROBE
	-u http_proxy -u https_proxy -u all_proxy -u HTTP_PROXY -u HTTPS_PROXY -u ALL_PROXY"
# shellcheck disable=SC2086 # unset_vars is a list of flags
clean() {
	env $unset_vars HOME="$work/home" XDG_CONFIG_HOME="$work/home/.config" CLAUDE_CONFIG_DIR="$work/home/.claude" "$@"
}
# serve_bg is clean for a program run in the background: it becomes that
# background process, so stopping it stops the program.
# shellcheck disable=SC2086
serve_bg() {
	exec env $unset_vars HOME="$work/home" XDG_CONFIG_HOME="$work/home/.config" CLAUDE_CONFIG_DIR="$work/home/.claude" "$@"
}
get() { curl -s --noproxy '*' --max-time 10 "$@"; }

go build -o "$work/switcher" .
mkdir -p "$work/home"

# A free pair of ports, the server answering and the Desktop relay connected
# (in this home's Claude Desktop settings): tried a few times.
mkdir -p "$work/home/.claude"
up=
for _ in 1 2 3 4 5; do
	port=$((20000 + $(od -An -N2 -tu2 /dev/urandom | tr -d ' ') % 20000))
	relay=$((port + 1))
	: >"$work/home/server.log"
	serve_bg "$work/switcher" -port "$port" -desktop-relay-port "$relay" serve >"$work/home/server.log" 2>&1 &
	pid=$!
	for _ in $(seq 1 100); do
		if get -o /dev/null "http://127.0.0.1:$port/api/auth/status"; then
			up=1
			break
		fi
		kill -0 "$pid" 2>/dev/null || break
		sleep 0.1
	done
	if [ -n "$up" ]; then
		clean "$work/switcher" -port "$port" desktop connect >/dev/null 2>"$work/cli.err" && break
		up=
	fi
	kill "$pid" 2>/dev/null || true
	wait "$pid" 2>/dev/null || true
	pid=
done
[ -n "$up" ] || fail "the server never answered, or Claude Desktop would not connect: $(cat "$work/cli.err" 2>/dev/null)"
grep -q "serving this user's programs only" "$work/home/server.log" ||
	fail "the same-user check is not on here; it must be, to be tested"

# curl, another process of this user.
checked="main port"
for i in 1 2 3 4 5; do
	code=$(get -o /dev/null -w '%{http_code}' -H "Origin: http://127.0.0.1:$port" "http://127.0.0.1:$port/api/state") ||
		fail "curl's request $i to the main port was dropped"
	[ "$code" = 200 ] || fail "curl's request $i to the main port: HTTP $code"
done

# The Desktop relay answers with HTTP whatever it says.
get -o /dev/null "http://127.0.0.1:$relay/" || fail "curl's request to the Desktop relay was dropped"
checked="$checked, Desktop relay"

# A sign-in callback port this server holds (one another program holds is
# not this test's to touch).
if ! grep -q "another program is using port 1455" "$work/home/server.log"; then
	get -o /dev/null "http://127.0.0.1:1455/auth/callback" || fail "curl's request to a sign-in callback was dropped"
	checked="$checked, sign-in callback"
fi

# The switcher command, another process again, with its own check of the
# port's owner.
clean "$work/switcher" -port "$port" status >/dev/null 2>"$work/cli.err" ||
	fail "the switcher command: $(cat "$work/cli.err")"
checked="$checked, switcher command"

# On a Mac: Foundation's URL loading, as the menu bar app's, and Python.
if [ "$(uname -s)" = Darwin ]; then
	answer=$(osascript -l JavaScript -e "ObjC.import('Foundation'); var d = \$.NSData.dataWithContentsOfURL(\$.NSURL.URLWithString('http://127.0.0.1:$port/api/auth/status')); d.isNil() ? 'nothing' : 'answered'" 2>&1) ||
		fail "Foundation's request could not be made: $answer"
	[ "$answer" = answered ] || fail "Foundation's request to the main port was dropped ($answer)"
	checked="$checked, Foundation"
	if command -v python3 >/dev/null 2>&1; then
		python3 -I -c "import urllib.request as u; u.build_opener(u.ProxyHandler({})).open('http://127.0.0.1:$port/api/auth/status', timeout=10).read()" ||
			fail "Python's request to the main port was dropped"
		checked="$checked, Python"
	fi
fi

if grep -q "refused a connection" "$work/home/server.log"; then
	fail "the server refused this user's own programs"
fi
if grep -q "did not show" "$work/home/server.log"; then
	fail "the server could not see whose this user's own programs' connections were"
fi
echo "e2e-local: the check is on and serves this user's programs ($checked)"
