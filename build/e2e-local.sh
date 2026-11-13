#!/bin/sh
# Starts the real server in a throwaway home and reaches it the way real
# clients do: from other processes, curl and the switcher command. This
# user's own programs must be served on the main port and the Desktop
# relay's, never dropped by the same-user check. Runs in `make verify` and
# `make verify-linux`, on a Mac and on Linux.
set -eu

work=$(mktemp -d)
pid=
cleanup() {
	[ -z "$pid" ] || kill "$pid" 2>/dev/null || true
	[ -z "$pid" ] || wait "$pid" 2>/dev/null || true
	rm -rf "$work"
}
trap cleanup EXIT
fail() {
	echo "e2e-local: $*" >&2
	[ ! -f "$work/home/server.log" ] || sed 's/^/  server: /' "$work/home/server.log" >&2
	exit 1
}

go build -o "$work/switcher" .
mkdir -p "$work/home"
port=$((20000 + $$ % 20000))
relay=$((port + 1))
HOME="$work/home" XDG_CONFIG_HOME="$work/home/.config" "$work/switcher" -port "$port" -desktop-relay-port "$relay" serve \
	>"$work/home/server.log" 2>&1 &
pid=$!

up=
for _ in $(seq 1 100); do
	if curl -s -o /dev/null "http://127.0.0.1:$port/api/auth/status"; then
		up=1
		break
	fi
	kill -0 "$pid" 2>/dev/null || fail "the server stopped"
	sleep 0.1
done
[ -n "$up" ] || fail "the server never answered on $port"

# curl, another process of this user.
for i in 1 2 3 4 5; do
	code=$(curl -s -o /dev/null -w '%{http_code}' -H "Origin: http://127.0.0.1:$port" "http://127.0.0.1:$port/api/state") ||
		fail "curl's request $i to the main port was dropped"
	[ "$code" = 200 ] || fail "curl's request $i to the main port: HTTP $code"
done
# The Desktop relay's port, where it listens, answers with HTTP whatever it
# says: no answer at all (curl 52 or 56) is a dropped connection.
status=0
curl -s -o /dev/null "http://127.0.0.1:$relay/" || status=$?
case $status in
0 | 7) ;; # answered, or not listening in a home without Claude Desktop set up
*) fail "curl's request to the Desktop relay was dropped (curl $status)" ;;
esac
# The switcher command, another process again, with its own check of the
# port's owner.
HOME="$work/home" "$work/switcher" -port "$port" status >/dev/null 2>"$work/cli.err" ||
	fail "the switcher command: $(cat "$work/cli.err")"
if grep -q "refused a connection" "$work/home/server.log"; then
	fail "the server refused this user's own programs"
fi
echo "e2e-local: this user's programs are served (main port, Desktop relay, switcher command)"
