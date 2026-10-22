#!/bin/bash
# Tests for the Unraid plugin: packaging/unraid/* and the rendered lookout.plg.
# Needs bash, php (CLI) and xmllint. Run with: bash packaging/test/unraid_test.sh
set -u

here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
unraid="$root/packaging/unraid"
work=$(mktemp -d)

cleanup() {
	[ -f "$work/rc.pid" ] && kill -KILL "$(cat "$work/rc.pid")" 2>/dev/null
	rm -rf "$work"
}
trap cleanup EXIT

fail() {
	printf 'FAIL: %s\n' "$*" >&2
	exit 1
}

assert_eq() {
	[ "$2" = "$3" ] || fail "$1: want [$2], got [$3]"
}

assert_contains() {
	case "$3" in
	*"$2"*) ;;
	*) fail "$1: want substring [$2] in [$3]" ;;
	esac
}

assert_not_contains() {
	case "$3" in
	*"$2"*) fail "$1: unexpected substring [$2] in [$3]" ;;
	esac
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

sha256_of() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | awk '{print $1}'
	else
		shasum -a 256 "$1" | awk '{print $1}'
	fi
}

command -v php >/dev/null 2>&1 || fail "php (CLI) is required"
command -v xmllint >/dev/null 2>&1 || fail "xmllint is required"

fx="$work/fx"
mkdir -p "$fx/log"
tok=AbCdEfGhIjKlMnOpQrStUvWxYz012345

# --- enroll ---------------------------------------------------------------

cat >"$fx/lookout" <<'FAKE'
#!/bin/sh
printf '%s\n' "$@" >"$FAKE_LOG/args"
echo "enrolled as srv-1 [$7]"
exit "${FAKE_RC:-0}"
FAKE
cat >"$fx/rc" <<'FAKE'
#!/bin/sh
echo "$*" >>"$FAKE_LOG/rc"
FAKE
chmod +x "$fx/lookout" "$fx/rc"

# run_enroll ARGS... runs the enroll wrapper against fakes; sets $out and $rc.
run_enroll() {
	rm -f "$fx/log/args" "$fx/log/rc"
	out=$(FAKE_LOG="$fx/log" LOOKOUT_BIN="$fx/lookout" LOOKOUT_CONFIG="$work/cfg.yaml" LOOKOUT_RC="$fx/rc" \
		bash "$unraid/enroll" "$@" 2>&1)
	rc=$?
}

for bad_url in http://x '' 'https://user@evil.example/' 'https://media.example/a b' 'ftp://media.example'; do
	run_enroll "$bad_url" "$tok"
	assert_eq "enroll url [$bad_url] status" 1 "$rc"
	assert_eq "enroll url [$bad_url] message" "invalid platform URL" "$out"
	[ ! -e "$fx/log/args" ] || fail "enroll url [$bad_url] ran lookout"
done
for bad_token in 'abc def' "${tok:1}" "${tok}x" ''; do
	run_enroll https://media.example "$bad_token"
	assert_eq "enroll token [$bad_token] status" 1 "$rc"
	assert_eq "enroll token [$bad_token] message" "invalid enrollment token" "$out"
	[ ! -e "$fx/log/args" ] || fail "enroll token [$bad_token] ran lookout"
done

run_enroll https://media.example "$tok"
assert_eq "enroll status" 0 "$rc"
assert_eq "enroll args" "enroll
--config
$work/cfg.yaml
--url
https://media.example
--token
$tok
--force" "$(cat "$fx/log/args")"
assert_eq "enroll restarts the agent" restart "$(cat "$fx/log/rc")"
assert_contains "enroll output" "enrolled as srv-1" "$out"
assert_not_contains "enroll output hides the token" "$tok" "$out"

run_enroll http://localhost:8000 "$tok"
assert_eq "enroll loopback http status" 0 "$rc"

rm -f "$fx/log/rc"
out=$(FAKE_RC=1 FAKE_LOG="$fx/log" LOOKOUT_BIN="$fx/lookout" LOOKOUT_CONFIG="$work/cfg.yaml" LOOKOUT_RC="$fx/rc" \
	bash "$unraid/enroll" https://media.example "$tok" 2>&1)
assert_eq "enroll failure status" 1 "$?"
[ ! -e "$fx/log/rc" ] || fail "failed enroll restarted the agent"

# --- rc.lookout -----------------------------------------------------------

cat >"$fx/sleeps" <<'FAKE'
#!/bin/sh
exec sleep 100
FAKE
chmod +x "$fx/sleeps"
pidfile="$work/lookout.pid"

# run_rc ACTION runs rc.lookout with the real supervisor and a fake agent.
run_rc() {
	out=$(LOOKOUT_PIDFILE="$pidfile" LOOKOUT_SUPERVISE="$unraid/supervise" LOOKOUT_BIN="$fx/sleeps" \
		LOOKOUT_CONFIG="$work/cfg.yaml" LOOKOUT_RESTART_DELAY=1 bash "$unraid/rc.lookout" "$1" 2>&1)
	rc=$?
}

run_rc status
assert_eq "status without pidfile" 3 "$rc"
assert_eq "status without pidfile message" "lookout is stopped" "$out"

echo 999999 >"$pidfile"
run_rc status
assert_eq "status with stale pidfile" 3 "$rc"

run_rc start
assert_eq "start status" 0 "$rc"
pid=$(cat "$pidfile")
cp "$pidfile" "$work/rc.pid"
run_rc status
assert_eq "status running" 0 "$rc"
assert_eq "status running message" "lookout is running (pid $pid)" "$out"

run_rc start
assert_eq "second start status" 0 "$rc"
assert_eq "second start keeps the supervisor" "$pid" "$(cat "$pidfile")"

run_rc restart
assert_eq "restart status" 0 "$rc"
wait_gone "$pid" 2 || fail "restart left the old supervisor running"
newpid=$(cat "$pidfile")
[ "$newpid" != "$pid" ] || fail "restart kept pid $pid"
kill -0 "$newpid" 2>/dev/null || fail "restart: new supervisor not running"
cp "$pidfile" "$work/rc.pid"

run_rc stop
assert_eq "stop status" 0 "$rc"
[ ! -e "$pidfile" ] || fail "stop left the pidfile"
wait_gone "$newpid" 2 || fail "stop left the supervisor running"
run_rc status
assert_eq "status after stop" 3 "$rc"

# A supervisor that ignores TERM is killed after LOOKOUT_STOP_TIMEOUT.
cat >"$fx/stubborn" <<'FAKE'
#!/bin/sh
trap '' TERM
exec sleep 100
FAKE
chmod +x "$fx/stubborn"
LOOKOUT_PIDFILE="$pidfile" LOOKOUT_SUPERVISE="$fx/stubborn" bash "$unraid/rc.lookout" start >/dev/null 2>&1
pid=$(cat "$pidfile")
cp "$pidfile" "$work/rc.pid"
LOOKOUT_PIDFILE="$pidfile" LOOKOUT_STOP_TIMEOUT=1 bash "$unraid/rc.lookout" stop >/dev/null 2>&1
wait_gone "$pid" 2 || fail "stop did not kill a supervisor that ignores TERM"
[ ! -e "$pidfile" ] || fail "forced stop left the pidfile"

# --- render ---------------------------------------------------------------

MINISIGN_PUBLIC_KEY=RWQtest "$root/scripts/render-release-files.sh" 0.2.0 "$work/out" || fail "render failed"
plg="$work/out/lookout.plg"
xmllint --noout "$plg" || fail "rendered plg is not well-formed XML"
if grep -q '@[A-Z_]*@' "$plg"; then
	fail "rendered plg left a placeholder: $(grep -o '@[A-Z_]*@' "$plg" | head -n 1)"
fi
grep -q 'lookout is running' "$plg" || fail "rendered plg is missing rc.lookout"

xpath() {
	xmllint --xpath "$1" "$plg"
}
plg_version=$(xpath 'string(/PLUGIN/@version)')
[[ $plg_version =~ ^[0-9]{4}\.[0-9]{2}\.[0-9]{2}\.[0-9]{4}$ ]] || fail "plg version [$plg_version] is not YYYY.MM.DD.HHMM"
assert_eq "plg name" lookout "$(xpath 'string(/PLUGIN/@name)')"
assert_eq "plg pluginURL" https://github.com/pentacore/lookout/releases/latest/download/lookout.plg "$(xpath 'string(/PLUGIN/@pluginURL)')"
assert_contains "plg changes" "$plg_version - lookout 0.2.0" "$(xpath 'string(/PLUGIN/CHANGES)')"

# inline FILE_XPATH prints the INLINE content of one FILE element.
inline() {
	xpath "string(/PLUGIN/FILE[$1]/INLINE)"
}
assert_eq "plg rc.lookout" "$(cat "$unraid/rc.lookout")" "$(inline '@Name="/etc/rc.d/rc.lookout"')"
assert_eq "plg rc.lookout mode" 0755 "$(xpath 'string(/PLUGIN/FILE[@Name="/etc/rc.d/rc.lookout"]/@Mode)')"
assert_eq "plg supervise" "$(cat "$unraid/supervise")" "$(inline '@Name="/usr/local/emhttp/plugins/lookout/scripts/supervise"')"
assert_eq "plg enroll" "$(cat "$unraid/enroll")" "$(inline '@Name="/usr/local/emhttp/plugins/lookout/scripts/enroll"')"
assert_eq "plg page" "$(cat "$unraid/Lookout.page")" "$(inline '@Name="/usr/local/emhttp/plugins/lookout/Lookout.page"')"
assert_eq "plg enroll.php" "$(cat "$unraid/enroll.php")" "$(inline '@Name="/usr/local/emhttp/plugins/lookout/include/enroll.php"')"
assert_eq "plg install script" "$(sed 's/@VERSION@/0.2.0/g' "$unraid/plg-install.sh")" "$(inline '@Run="/bin/bash" and not(@Method)')"
assert_eq "plg remove script" "$(cat "$unraid/plg-remove.sh")" "$(inline '@Run="/bin/bash" and @Method="remove"')"
inline '@Run="/bin/bash" and not(@Method)' >"$work/plg-install.sh"
inline '@Run="/bin/bash" and @Method="remove"' >"$work/plg-remove.sh"

# A source that would end its CDATA block early fails the render.
mkdir -p "$work/tree"
cp -R "$root/scripts" "$root/packaging" "$work/tree/"
printf '# ]]>\n' >>"$work/tree/packaging/unraid/supervise"
if MINISIGN_PUBLIC_KEY=RWQtest "$work/tree/scripts/render-release-files.sh" 0.2.0 "$work/out-cdata" 2>/dev/null; then
	fail "render must reject a source containing ]]>"
fi
[ ! -e "$work/out-cdata/lookout.plg" ] || fail "render wrote a plg despite ]]>"

# --- plg install / remove -------------------------------------------------

rel="$work/release"
mkdir -p "$rel/pkg" "$work/badrel" "$fx/pbin"
cat >"$rel/pkg/lookout" <<'FAKE'
#!/bin/sh
echo "lookout 0.2.0"
FAKE
chmod +x "$rel/pkg/lookout"
tar -czf "$rel/lookout_linux_amd64.tar.gz" -C "$rel/pkg" lookout
printf '%s  lookout_linux_amd64.tar.gz\n' "$(sha256_of "$rel/lookout_linux_amd64.tar.gz")" >"$rel/checksums.txt"
cp "$rel/lookout_linux_amd64.tar.gz" "$work/badrel/"
printf '%s  lookout_linux_amd64.tar.gz\n' 0000000000000000000000000000000000000000000000000000000000000000 >"$work/badrel/checksums.txt"
cat >"$fx/pbin/curl" <<'FAKE'
#!/bin/sh
echo "$*" >>"$FAKE_LOG/curl"
[ -z "${FAKE_CURL_FAIL:-}" ] || exit 22
for a in "$@"; do
	[ "$prev" = -o ] && out="$a"
	prev="$a"
	url="$a"
done
cp "$FAKE_RELEASE/$(basename "$url")" "$out"
FAKE
chmod +x "$fx/pbin/curl"

# new_root DIR creates an Unraid-like root with a fake rc.lookout.
new_root() {
	mkdir -p "$1/etc/rc.d" "$1/log"
	cat >"$1/etc/rc.d/rc.lookout" <<'FAKE'
#!/bin/sh
echo "$*" >>"$(dirname "$0")/../../log/rc"
FAKE
	chmod +x "$1/etc/rc.d/rc.lookout"
}

# persisted DIR VERSION puts a persisted binary of VERSION on the fake flash drive.
persisted() {
	mkdir -p "$1/boot/config/plugins/lookout"
	printf '#!/bin/sh\necho "lookout %s"\n' "$2" >"$1/boot/config/plugins/lookout/lookout"
	chmod +x "$1/boot/config/plugins/lookout/lookout"
}

# run_install DIR [ENV=VALUE...] runs the rendered install script; sets $out and $rc.
run_install() {
	local dir="$1"
	shift
	out=$(env PATH="$fx/pbin:$PATH" FAKE_RELEASE="$rel" FAKE_LOG="$dir/log" LOOKOUT_DESTDIR="$dir" \
		LOOKOUT_UNAME_M=x86_64 "$@" bash "$work/plg-install.sh" 2>&1)
	rc=$?
}

d="$work/fresh"
new_root "$d"
run_install "$d"
assert_eq "install fresh status ($out)" 0 "$rc"
assert_eq "install fresh downloads" "--proto =https --tlsv1.2 -fsSL -o" "$(head -n 1 "$d/log/curl" | awk '{print $1, $2, $3, $4, $5}')"
assert_contains "install fresh archive url" "https://github.com/pentacore/lookout/releases/download/v0.2.0/lookout_linux_amd64.tar.gz" "$(cat "$d/log/curl")"
assert_contains "install fresh checksums url" "https://github.com/pentacore/lookout/releases/download/v0.2.0/checksums.txt" "$(cat "$d/log/curl")"
assert_eq "install fresh persisted binary" "lookout 0.2.0" "$("$d/boot/config/plugins/lookout/lookout")"
assert_eq "install fresh binary" "lookout 0.2.0" "$("$d/usr/local/bin/lookout")"
[ -d "$d/boot/config/plugins/lookout/state" ] || fail "install fresh: no state dir"
assert_eq "install fresh config" 'platform_url: ""
state_dir: /boot/config/plugins/lookout/state
runtime_dir: /run/lookout
auto_update: true' "$(cat "$d/boot/config/plugins/lookout/lookout.yaml")"
assert_eq "install fresh restarts" restart "$(cat "$d/log/rc")"

# Boot: /usr/local/bin is empty again and the persisted binary is current.
rm -f "$d/usr/local/bin/lookout" "$d/log/curl" "$d/log/rc"
printf 'platform_url: https://media.example\n' >"$d/boot/config/plugins/lookout/lookout.yaml"
run_install "$d"
assert_eq "install boot status ($out)" 0 "$rc"
[ ! -e "$d/log/curl" ] || fail "install boot downloaded although the persisted binary is current"
assert_eq "install boot binary" "lookout 0.2.0" "$("$d/usr/local/bin/lookout")"
assert_eq "install boot keeps config" "platform_url: https://media.example" "$(cat "$d/boot/config/plugins/lookout/lookout.yaml")"
assert_eq "install boot restarts" restart "$(cat "$d/log/rc")"

d="$work/older"
new_root "$d"
persisted "$d" 0.1.0
run_install "$d"
assert_eq "install upgrade status ($out)" 0 "$rc"
assert_eq "install upgrade persisted binary" "lookout 0.2.0" "$("$d/boot/config/plugins/lookout/lookout")"
assert_eq "install upgrade binary" "lookout 0.2.0" "$("$d/usr/local/bin/lookout")"

d="$work/newer"
new_root "$d"
persisted "$d" 0.10.0
run_install "$d"
assert_eq "install self-updated status ($out)" 0 "$rc"
[ ! -e "$d/log/curl" ] || fail "install downloaded over a newer self-updated binary"
assert_eq "install self-updated binary" "lookout 0.10.0" "$("$d/usr/local/bin/lookout")"

d="$work/badsum"
new_root "$d"
run_install "$d" FAKE_RELEASE="$work/badrel"
assert_eq "install bad checksum status" 1 "$rc"
assert_contains "install bad checksum message" "sha256" "$out"
[ ! -e "$d/boot/config/plugins/lookout/lookout" ] || fail "install bad checksum persisted a binary"
[ ! -e "$d/usr/local/bin/lookout" ] || fail "install bad checksum installed a binary"
[ ! -e "$d/log/rc" ] || fail "install bad checksum started the agent"

d="$work/offline-old"
new_root "$d"
persisted "$d" 0.1.0
run_install "$d" FAKE_CURL_FAIL=1
assert_eq "install offline with binary status ($out)" 0 "$rc"
assert_contains "install offline with binary warns" "warning" "$out"
assert_eq "install offline keeps binary" "lookout 0.1.0" "$("$d/usr/local/bin/lookout")"
assert_eq "install offline restarts" restart "$(cat "$d/log/rc")"

d="$work/offline-new"
new_root "$d"
run_install "$d" FAKE_CURL_FAIL=1
assert_eq "install offline without binary status" 1 "$rc"
[ ! -e "$d/log/rc" ] || fail "install offline without binary started the agent"

d="$work/remove"
new_root "$d"
mkdir -p "$d/usr/local/bin" "$d/usr/local/emhttp/plugins/lookout/scripts" "$d/boot/config/plugins/lookout"
: >"$d/usr/local/bin/lookout"
: >"$d/usr/local/emhttp/plugins/lookout/scripts/supervise"
: >"$d/boot/config/plugins/lookout/lookout.yaml"
out=$(LOOKOUT_DESTDIR="$d" bash "$work/plg-remove.sh" 2>&1)
assert_eq "remove status" 0 "$?"
assert_eq "remove stops" stop "$(cat "$d/log/rc")"
[ ! -e "$d/usr/local/bin/lookout" ] || fail "remove kept the binary"
[ ! -e "$d/etc/rc.d/rc.lookout" ] || fail "remove kept rc.lookout"
[ ! -e "$d/usr/local/emhttp/plugins/lookout" ] || fail "remove kept the webGUI files"
[ -f "$d/boot/config/plugins/lookout/lookout.yaml" ] || fail "remove deleted the config"
assert_contains "remove message" "/boot/config/plugins/lookout" "$out"

# --- settings page ----------------------------------------------------------

assert_eq "page header" 'Menu="Utilities"
Title="Lookout"
Icon="eye"
Markdown="false"
---' "$(sed -n '1,/^---$/p' "$unraid/Lookout.page")"

# render_page ROOT renders the page body with php against a fake root; sets
# $out (stdout) and $err (PHP notices and warnings).
render_page() {
	{
		cat <<'PHP'
<?php define('LOOKOUT_TEST_ROOT', getenv('PAGE_ROOT')); $var = ['csrf_token' => 'c"<sr>f']; ?>
PHP
		sed '1,/^---$/d' "$unraid/Lookout.page"
	} >"$work/page.php"
	out=$(PAGE_ROOT="$1" php -d display_errors=stderr -d error_reporting=-1 -d short_open_tag=1 "$work/page.php" 2>"$work/page.err")
	err=$(cat "$work/page.err")
}

pr="$work/pageroot"
mkdir -p "$pr/etc/rc.d" "$pr/usr/local/bin" "$pr/boot/config/plugins/lookout/state" "$pr/run/lookout"
printf '#!/bin/sh\necho "lookout is running (pid 42)"\n' >"$pr/etc/rc.d/rc.lookout"
printf '#!/bin/sh\necho "lookout 0.3.0"\n' >"$pr/usr/local/bin/lookout"
chmod +x "$pr/etc/rc.d/rc.lookout" "$pr/usr/local/bin/lookout"
printf '{"server_uuid":"srv-<i>1</i>","agent_token":"srv-<i>1</i>.TOPSECRETAGENTTOKEN"}\n' >"$pr/boot/config/plugins/lookout/state/credentials.json"
printf 'state_dir: /boot/config/plugins/lookout/state\nplatform_url: "https://media.example/?a=1&b=<2>"\n' >"$pr/boot/config/plugins/lookout/lookout.yaml"
printf '{"last_success_at":"2026-10-22T10:00:00Z","last_error":"<script>alert(1)</script>","extra":1}\n' >"$pr/run/lookout/status.json"
render_page "$pr"
assert_eq "page warnings" "" "$err"
assert_contains "page status" "lookout is running (pid 42)" "$out"
assert_contains "page version" "lookout 0.3.0" "$out"
assert_contains "page server uuid" "srv-&lt;i&gt;1&lt;/i&gt;" "$out"
assert_contains "page last success" "2026-10-22T10:00:00Z" "$out"
assert_contains "page last error" "&lt;script&gt;alert(1)&lt;/script&gt;" "$out"
assert_contains "page url prefilled" 'value="https://media.example/?a=1&amp;b=&lt;2&gt;"' "$out"
assert_contains "page csrf" 'name="csrf_token" value="c&quot;&lt;sr&gt;f"' "$out"
assert_contains "page form" 'action="/plugins/lookout/include/enroll.php"' "$out"
assert_contains "page token field" '<input type="password" name="token"' "$out"
assert_contains "page submit" 'value="Enroll"' "$out"
assert_not_contains "page raw last error" "<script>alert" "$out"
assert_not_contains "page raw uuid" "<i>1</i>" "$out"
assert_not_contains "page agent token" "TOPSECRETAGENTTOKEN" "$out"

render_page "$work/emptyroot"
assert_eq "page warnings without files" "" "$err"
assert_contains "page not enrolled" "not enrolled" "$out"
assert_contains "page never reported" "never" "$out"

# --- enroll endpoint ----------------------------------------------------------

cat >"$fx/enroll-script" <<'FAKE'
#!/bin/sh
printf '%s\n' "$@" >"$FAKE_LOG/eargs"
echo "enrolled as srv-1 using $2"
exit "${FAKE_RC:-0}"
FAKE
chmod +x "$fx/enroll-script"
cat >"$work/endpoint.php" <<'PHP'
<?php
define('LOOKOUT_ENROLL_SCRIPT', getenv('FAKE_ENROLL'));
$_SERVER['REQUEST_METHOD'] = getenv('REQ_METHOD');
$_POST = json_decode(getenv('REQ_POST'), true);
include getenv('ENDPOINT');
PHP

# post METHOD JSON_BODY [ENV=VALUE...] calls the endpoint; sets $out and $err.
post() {
	local method="$1" body="$2"
	shift 2
	rm -f "$fx/log/eargs"
	out=$(cd "$work" && env FAKE_ENROLL="$fx/enroll-script" FAKE_LOG="$fx/log" REQ_METHOD="$method" REQ_POST="$body" \
		ENDPOINT="$unraid/enroll.php" "$@" php -d display_errors=stderr -d error_reporting=-1 "$work/endpoint.php" 2>"$work/endpoint.err")
	err=$(cat "$work/endpoint.err")
}

post GET '{}'
assert_eq "endpoint GET warnings" "" "$err"
assert_contains "endpoint GET" '"ok":false' "$out"
[ ! -e "$fx/log/eargs" ] || fail "endpoint GET ran enroll"

for body in '{"url":"http://x","token":"'"$tok"'"}' '{"url":["https://media.example"],"token":"'"$tok"'"}' '{"token":"'"$tok"'"}'; do
	post POST "$body"
	assert_eq "endpoint [$body] warnings" "" "$err"
	assert_contains "endpoint [$body]" "invalid platform URL" "$out"
	[ ! -e "$fx/log/eargs" ] || fail "endpoint [$body] ran enroll"
done
post POST '{"url":"https://media.example","token":"short"}'
assert_contains "endpoint short token" "invalid enrollment token" "$out"
assert_not_contains "endpoint short token echo" "short" "$out"
[ ! -e "$fx/log/eargs" ] || fail "endpoint short token ran enroll"

post POST '{"url":"https://media.example","token":"'"$tok"'"}'
assert_eq "endpoint warnings" "" "$err"
assert_eq "endpoint args" "https://media.example
$tok" "$(cat "$fx/log/eargs")"
assert_contains "endpoint ok" '"ok":true' "$out"
assert_contains "endpoint output" "enrolled as srv-1" "$out"
assert_not_contains "endpoint hides the token" "$tok" "$out"

# The URL is a literal shell-injection attempt, so nothing in it may expand.
# shellcheck disable=SC2016
post POST '{"url":"https://media.example/$(touch${IFS}PWNED)`touch${IFS}PWNED`;touch${IFS}PWNED","token":"'"$tok"'"}'
[ ! -e "$work/PWNED" ] || fail "endpoint ran a shell on the URL"
# shellcheck disable=SC2016
assert_eq "endpoint passes the URL verbatim" 'https://media.example/$(touch${IFS}PWNED)`touch${IFS}PWNED`;touch${IFS}PWNED' "$(head -n 1 "$fx/log/eargs")"

post POST '{"url":"https://media.example","token":"'"$tok"'"}' FAKE_RC=1
assert_contains "endpoint failure" '"ok":false' "$out"
assert_contains "endpoint failure output" "enrolled as srv-1" "$out"

echo "unraid_test: ok"
