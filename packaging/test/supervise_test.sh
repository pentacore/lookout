#!/bin/bash
# Tests for packaging/unraid/supervise.
# Run with: bash packaging/test/supervise_test.sh
set -u

here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
supervise="$root/packaging/unraid/supervise"
work=$(mktemp -d)
sup=''

cleanup() {
	[ -n "$sup" ] && kill -KILL "$sup" 2>/dev/null
	[ -f "$work/fake.pid" ] && kill -KILL "$(cat "$work/fake.pid")" 2>/dev/null
	rm -rf "$work"
}
trap cleanup EXIT

fail() {
	printf 'FAIL: %s\n' "$*" >&2
	exit 1
}

# wait_gone PID SECONDS succeeds once PID no longer exists.
wait_gone() {
	local i
	for ((i = 0; i < $2 * 10; i++)); do
		kill -0 "$1" 2>/dev/null || return 0
		sleep 0.1
	done
	return 1
}

# wait_file PATH SECONDS succeeds once PATH exists and is not empty.
wait_file() {
	local i
	for ((i = 0; i < $2 * 10; i++)); do
		[ -s "$1" ] && return 0
		sleep 0.1
	done
	return 1
}

# start_supervise FAKE_BIN starts the supervisor detached from this shell (so
# init reaps it and kill -0 is a reliable liveness check) and sets $sup.
start_supervise() {
	rm -f "$work/sup.pid"
	(
		LOOKOUT_BIN="$1" LOOKOUT_CONFIG="$work/lookout.yaml" LOOKOUT_RESTART_DELAY=1 \
			bash "$supervise" >/dev/null 2>&1 &
		echo $! >"$work/sup.pid"
	)
	sup=$(cat "$work/sup.pid")
}

# A child that exits is restarted after the delay, with the run arguments.
cat >"$work/exits" <<FAKE
#!/bin/sh
echo "\$*" >>"$work/runs"
exit 0
FAKE
chmod +x "$work/exits"
start_supervise "$work/exits"
sleep 3.5
runs=$(wc -l <"$work/runs" | tr -d ' ')
[ "$runs" -ge 3 ] || fail "restarts: want >= 3 runs in 3.5s, got $runs"
[ "$(head -n 1 "$work/runs")" = "run --config $work/lookout.yaml" ] || fail "run args: got [$(head -n 1 "$work/runs")]"
kill -TERM "$sup"
wait_gone "$sup" 2 || fail "supervisor still running 2s after TERM"
sup=''

# TERM to the supervisor stops a running child too: no orphan is left behind.
cat >"$work/sleeps" <<FAKE
#!/bin/sh
echo \$\$ >"$work/fake.pid"
exec sleep 100
FAKE
chmod +x "$work/sleeps"
start_supervise "$work/sleeps"
wait_file "$work/fake.pid" 5 || fail "fake child never started"
fake=$(cat "$work/fake.pid")
kill -0 "$fake" 2>/dev/null || fail "fake child not running"
kill -TERM "$sup"
wait_gone "$sup" 2 || fail "supervisor with a running child still running 2s after TERM"
sup=''
wait_gone "$fake" 2 || fail "child $fake orphaned after the supervisor stopped"

echo "supervise_test: ok"
