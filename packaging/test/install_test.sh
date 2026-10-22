#!/bin/sh
# Tests for packaging/install.sh and scripts/render-release-files.sh.
# Run with: sh packaging/test/install_test.sh
set -u

here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

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

LOOKOUT_INSTALL_LIB_ONLY=1
export LOOKOUT_INSTALL_LIB_ONLY
# shellcheck source=packaging/install.sh
. "$root/packaging/install.sh"
set +e

# detect_arch
assert_eq "detect_arch x86_64" amd64 "$(LOOKOUT_UNAME_M=x86_64 detect_arch)"
assert_eq "detect_arch aarch64" arm64 "$(LOOKOUT_UNAME_M=aarch64 detect_arch)"
assert_eq "detect_arch arm64" arm64 "$(LOOKOUT_UNAME_M=arm64 detect_arch)"
out=$(LOOKOUT_UNAME_M=armv7l detect_arch 2>&1)
rc=$?
assert_eq "detect_arch armv7l status" 1 "$rc"
assert_eq "detect_arch armv7l message" "unsupported architecture: armv7l" "$out"

# verify_sha256
mkdir "$work/sha"
printf 'archive-bytes' >"$work/sha/lookout_linux_amd64.tar.gz"
printf '%s  lookout_linux_amd64.tar.gz\n' "$(sha256_of "$work/sha/lookout_linux_amd64.tar.gz")" >"$work/sha/checksums.txt"
verify_sha256 "$work/sha/lookout_linux_amd64.tar.gz" "$work/sha/checksums.txt" lookout_linux_amd64.tar.gz
assert_eq "verify_sha256 match" 0 $?
verify_sha256 "$work/sha/lookout_linux_amd64.tar.gz" "$work/sha/checksums.txt" lookout_linux_arm64.tar.gz
assert_eq "verify_sha256 name absent" 1 $?
printf 'tampered' >"$work/sha/lookout_linux_amd64.tar.gz"
verify_sha256 "$work/sha/lookout_linux_amd64.tar.gz" "$work/sha/checksums.txt" lookout_linux_amd64.tar.gz
assert_eq "verify_sha256 altered file" 1 $?

# write_config
write_config "$work/cfg/new.yaml" https://media.example.com
assert_eq "write_config new" "platform_url: https://media.example.com
state_dir: /var/lib/lookout
auto_update: true" "$(cat "$work/cfg/new.yaml")"
printf 'platform_url: https://old\nstate_dir: /x\n' >"$work/cfg/old.yaml"
write_config "$work/cfg/old.yaml" 'https://new.example.com/a&b|c'
assert_eq "write_config existing" "platform_url: https://new.example.com/a&b|c
state_dir: /x" "$(cat "$work/cfg/old.yaml")"
printf 'state_dir: /x\n' >"$work/cfg/nourl.yaml"
write_config "$work/cfg/nourl.yaml" https://new.example.com
assert_eq "write_config existing without url" "state_dir: /x
platform_url: https://new.example.com" "$(cat "$work/cfg/nourl.yaml")"

# render
mkdir "$work/out"
MINISIGN_PUBLIC_KEY=RWQtest "$root/scripts/render-release-files.sh" 0.1.0 "$work/out" || fail "render failed"
rendered=$(cat "$work/out/install.sh")
assert_contains "render key" RWQtest "$rendered"
assert_contains "render version" "VERSION='0.1.0'" "$rendered"
if grep -q '@[A-Z_]*@' "$work/out/install.sh"; then
	fail "render left a placeholder"
fi
(unset MINISIGN_PUBLIC_KEY; "$root/scripts/render-release-files.sh" 0.1.0 "$work/out2" 2>/dev/null)
rc=$?
[ "$rc" -ne 0 ] || fail "render without key must fail"

for badver in 1.2 v1.2.3.4 '1.2.3;rm' '../x'; do
	if MINISIGN_PUBLIC_KEY=RWQtest "$root/scripts/render-release-files.sh" "$badver" "$work/out3" 2>/dev/null; then
		fail "render must reject version [$badver]"
	fi
done
if MINISIGN_PUBLIC_KEY='RW|Q&test' "$root/scripts/render-release-files.sh" 0.1.0 "$work/out3" 2>/dev/null; then
	fail "render must reject a malformed key"
fi
MINISIGN_PUBLIC_KEY=RWQt+e/s=t "$root/scripts/render-release-files.sh" v0.2.0-rc.1 "$work/out3" || fail "render must accept v-prefixed prerelease and base64 key"

# Full flow of the rendered script against fakes (no root, no network, no systemd).
fx="$work/fx"
mkdir -p "$fx/release/pkg" "$fx/bin"
cat >"$fx/release/pkg/lookout" <<'FAKE'
#!/bin/sh
case "$1" in
version) echo "lookout 0.1.0" ;;
check-config)
	if [ -n "${FAKE_CHECK_CONFIG_RC:-}" ]; then echo "config exploded" >&2; exit "$FAKE_CHECK_CONFIG_RC"; fi
	if [ -f "$LOOKOUT_DESTDIR/enrolled" ]; then echo "enrolled:     yes (server abc)"; else echo "enrolled:     no"; fi
	;;
enroll)
	echo "enroll $*" >>"$LOOKOUT_DESTDIR/calls"
	: >"$LOOKOUT_DESTDIR/enrolled"
	;;
esac
FAKE
chmod +x "$fx/release/pkg/lookout"
tar -czf "$fx/release/lookout_linux_amd64.tar.gz" -C "$fx/release/pkg" lookout
printf 'sig' >"$fx/release/lookout_linux_amd64.tar.gz.minisig"
printf '%s  lookout_linux_amd64.tar.gz\n' "$(sha256_of "$fx/release/lookout_linux_amd64.tar.gz")" >"$fx/release/checksums.txt"

cat >"$fx/bin/id" <<'FAKE'
#!/bin/sh
echo 0
FAKE
cat >"$fx/bin/curl" <<'FAKE'
#!/bin/sh
out=''
for a in "$@"; do
	[ "$prev" = -o ] && out="$a"
	prev="$a"
	url="$a"
done
echo "$url" >>"$FAKE_LOG/curl"
echo "$*" >>"$FAKE_LOG/curl-args"
cp "$FAKE_RELEASE/$(basename "$url")" "$out"
FAKE
cat >"$fx/bin/systemctl" <<'FAKE'
#!/bin/sh
echo "$*" >>"$FAKE_LOG/systemctl"
FAKE
cat >"$fx/bin/minisign" <<'FAKE'
#!/bin/sh
echo "$*" >>"$FAKE_LOG/minisign"
exit "${FAKE_MINISIGN_RC:-0}"
FAKE
chmod +x "$fx/bin/id" "$fx/bin/curl" "$fx/bin/systemctl" "$fx/bin/minisign"

# run_install DIR PATH_WITH_MINISIGN ARGS... runs the rendered script with fakes.
run_install() {
	ri_dir="$1"
	ri_path="$2"
	shift 2
	mkdir -p "$ri_dir"
	: >"$ri_dir/curl"
	: >"$ri_dir/systemctl"
	(
		unset LOOKOUT_INSTALL_LIB_ONLY
		PATH="$ri_path" LOOKOUT_DESTDIR="$ri_dir/root" LOOKOUT_UNAME_M=x86_64 \
			FAKE_LOG="$ri_dir" FAKE_RELEASE="${FAKE_RELEASE_OVERRIDE:-$fx/release}" \
			sh "$work/out/install.sh" "$@"
	) >"$ri_dir/stdout" 2>"$ri_dir/stderr"
	return $?
}
# run_install_root DIR ROOT ARGS... is run_install against an existing install root.
run_install_root() {
	rr_dir="$1"
	rr_root="$2"
	shift 2
	mkdir -p "$rr_dir"
	: >"$rr_dir/curl"
	: >"$rr_dir/systemctl"
	(
		unset LOOKOUT_INSTALL_LIB_ONLY
		PATH="$withminisign" LOOKOUT_DESTDIR="$rr_root" LOOKOUT_UNAME_M=x86_64 \
			FAKE_LOG="$rr_dir" FAKE_RELEASE="$fx/release" \
			sh "$work/out/install.sh" "$@"
	) >"$rr_dir/stdout" 2>"$rr_dir/stderr"
}
nominisign="$work/nominisign"
mkdir -p "$nominisign"
for t in id curl systemctl; do cp "$fx/bin/$t" "$nominisign/$t"; done
base_path=$(printf '%s' "$PATH")
withminisign="$fx/bin:$base_path"
withoutminisign="$nominisign:$base_path"

# Rendered with a key and a pinned version: pins v0.1.0 and runs minisign.
run_install "$work/run1" "$withminisign" --url https://media.example.com --token SECRET-TOKEN
assert_eq "run1 status" 0 $?
assert_contains "run1 pinned url" "/releases/download/v0.1.0/lookout_linux_amd64.tar.gz" "$(cat "$work/run1/curl")"
assert_contains "run1 minisign invoked" "-P RWQtest" "$(cat "$work/run1/minisign")"
assert_not_contains "run1 no sha256 warning" "minisign not installed" "$(cat "$work/run1/stderr")"
assert_eq "run1 config" "platform_url: https://media.example.com
state_dir: /var/lib/lookout
auto_update: true" "$(cat "$work/run1/root/etc/lookout/lookout.yaml")"
assert_contains "run1 enroll args" "--url https://media.example.com --token SECRET-TOKEN" "$(cat "$work/run1/root/calls")"
assert_not_contains "run1 enroll without force" "--force" "$(cat "$work/run1/root/calls")"
assert_not_contains "run1 token not echoed (stdout)" SECRET-TOKEN "$(cat "$work/run1/stdout")"
assert_not_contains "run1 token not echoed (stderr)" SECRET-TOKEN "$(cat "$work/run1/stderr")"
assert_eq "run1 systemctl" "daemon-reload
enable lookout
restart lookout" "$(cat "$work/run1/systemctl")"
assert_contains "run1 unit" "ExecStart=/usr/local/bin/lookout run --config /etc/lookout/lookout.yaml" "$(cat "$work/run1/root/etc/systemd/system/lookout.service")"
assert_contains "run1 summary" "lookout 0.1.0 installed; status: systemctl status lookout" "$(cat "$work/run1/stdout")"
[ -x "$work/run1/root/usr/local/bin/lookout" ] || fail "run1 binary not installed"

# No minisign on PATH: sha256 only, with a warning.
run_install "$work/run2" "$withoutminisign" --url https://media.example.com --token T
assert_eq "run2 status" 0 $?
assert_contains "run2 warning" "warning: minisign not installed; verified sha256 only" "$(cat "$work/run2/stderr")"

# minisign failure aborts before anything is installed.
if (FAKE_MINISIGN_RC=1 && export FAKE_MINISIGN_RC && run_install "$work/run3" "$withminisign" --url https://media.example.com --token T); then
	fail "run3 must fail when minisign fails"
fi
[ ! -e "$work/run3/root/usr/local/bin/lookout" ] || fail "run3 installed despite failed verification"

# Tampered checksum aborts.
cp -R "$fx/release" "$fx/release-bad"
printf '0000  lookout_linux_amd64.tar.gz\n' >"$fx/release-bad/checksums.txt"
if (FAKE_RELEASE_OVERRIDE="$fx/release-bad" && export FAKE_RELEASE_OVERRIDE && run_install "$work/run4" "$withoutminisign" --url https://media.example.com --token T); then
	fail "run4 must fail on checksum mismatch"
fi
[ ! -e "$work/run4/root/usr/local/bin/lookout" ] || fail "run4 installed despite bad checksum"

# Re-run without --url or --token: keeps the config, does not enroll, restarts the service.
cp -R "$work/run1/root" "$work/rerun-root"
rm -f "$work/rerun-root/calls"
mkdir -p "$work/run5"
: >"$work/run5/systemctl"
(
	unset LOOKOUT_INSTALL_LIB_ONLY
	PATH="$withminisign" LOOKOUT_DESTDIR="$work/rerun-root" LOOKOUT_UNAME_M=x86_64 \
		FAKE_LOG="$work/run5" FAKE_RELEASE="$fx/release" \
		sh "$work/out/install.sh"
) >"$work/run5/stdout" 2>"$work/run5/stderr"
assert_eq "run5 status" 0 $?
[ ! -e "$work/rerun-root/calls" ] || fail "run5 must not call enroll"
assert_eq "run5 config unchanged" "$(cat "$work/run1/root/etc/lookout/lookout.yaml")" "$(cat "$work/rerun-root/etc/lookout/lookout.yaml")"
assert_eq "run5 systemctl" "daemon-reload
enable lookout
restart lookout" "$(cat "$work/run5/systemctl")"

# Re-run with a token on an enrolled host re-enrolls with --force and the config URL.
(
	unset LOOKOUT_INSTALL_LIB_ONLY
	PATH="$withminisign" LOOKOUT_DESTDIR="$work/rerun-root" LOOKOUT_UNAME_M=x86_64 \
		FAKE_LOG="$work/run5" FAKE_RELEASE="$fx/release" \
		sh "$work/out/install.sh" --token T2
) >/dev/null 2>&1
assert_eq "run6 status" 0 $?
assert_contains "run6 force and config url" "--url https://media.example.com --token T2 --force" "$(cat "$work/rerun-root/calls")"

# A different --url on an enrolled host without --token is refused: the old
# credentials must not be sent to another platform. Nothing changes.
cp -R "$work/run1/root" "$work/switch-root"
rm -f "$work/switch-root/calls"
if run_install_root "$work/run11" "$work/switch-root" --url https://other.example.com; then
	fail "run11 must refuse to switch platform without --token"
fi
assert_contains "run11 message" "--url changes the platform; pass --token to re-enroll" "$(cat "$work/run11/stderr")"
assert_eq "run11 config unchanged" "$(cat "$work/run1/root/etc/lookout/lookout.yaml")" "$(cat "$work/switch-root/etc/lookout/lookout.yaml")"
[ ! -e "$work/switch-root/calls" ] || fail "run11 must not call enroll"
assert_eq "run11 no systemctl" "" "$(cat "$work/run11/systemctl")"

# With --token the switch re-enrolls with --force; install.sh leaves the config to enroll.
run_install_root "$work/run12" "$work/switch-root" --url https://other.example.com --token T3
assert_eq "run12 status" 0 $?
assert_contains "run12 enroll" "--url https://other.example.com --token T3 --force" "$(cat "$work/switch-root/calls")"
assert_eq "run12 config left to enroll" "$(cat "$work/run1/root/etc/lookout/lookout.yaml")" "$(cat "$work/switch-root/etc/lookout/lookout.yaml")"

# A trailing slash is the same platform, not a switch.
cp -R "$work/run1/root" "$work/slash-root"
run_install_root "$work/run13" "$work/slash-root" --url https://media.example.com/
assert_eq "run13 status" 0 $?

# Invalid URLs are rejected before anything is downloaded or written.
for bad in http://evil.example.com ftp://x.example.com https://user@evil.example.com http://localhost@evil.example.com 'https://a b'; do
	if run_install "$work/run14" "$withminisign" --url "$bad" --token T; then
		fail "run14 must reject url [$bad]"
	fi
	assert_contains "run14 message" "--url must be https://" "$(cat "$work/run14/stderr")"
	assert_eq "run14 no download" "" "$(cat "$work/run14/curl")"
	[ ! -e "$work/run14/root/etc/lookout/lookout.yaml" ] || fail "run14 wrote a config for [$bad]"
done
for ok in http://localhost:8080 http://127.0.0.1 'http://[::1]:9000/x'; do
	run_install "$work/run15" "$withminisign" --url "$ok" --token T || fail "run15 must accept url [$ok]"
done

# Downloads are pinned to HTTPS.
assert_contains "curl pinned to https" "--proto =https --tlsv1.2 -fsSL" "$(cat "$work/run1/curl-args")"

# A failing check-config is reported, not mistaken for "not enrolled".
cp -R "$work/run1/root" "$work/broken-root"
if (FAKE_CHECK_CONFIG_RC=1 && export FAKE_CHECK_CONFIG_RC && run_install_root "$work/run16" "$work/broken-root" --token T); then
	fail "run16 must fail when check-config fails"
fi
assert_contains "run16 message" "check-config failed: config exploded" "$(cat "$work/run16/stderr")"
assert_eq "run16 no systemctl" "" "$(cat "$work/run16/systemctl")"

# First install without --url or without --token fails with a clear message.
if run_install "$work/run7" "$withminisign" --token T; then
	fail "run7 must fail without --url"
fi
assert_contains "run7 message" "--url is required on first install" "$(cat "$work/run7/stderr")"
if run_install "$work/run8" "$withminisign" --url https://media.example.com; then
	fail "run8 must fail without --token"
fi
assert_contains "run8 message" "--token is required (this server is not enrolled yet)" "$(cat "$work/run8/stderr")"

# Unknown flags print usage and exit 2 without echoing the argument.
run_install "$work/run9" "$withminisign" --bogus SECRET-TOKEN
assert_eq "run9 status" 2 $?
assert_contains "run9 usage" "usage: install.sh" "$(cat "$work/run9/stderr")"
assert_not_contains "run9 no echo" SECRET-TOKEN "$(cat "$work/run9/stderr")"

# Unrendered script: placeholder version means latest; placeholder key means sha256 only.
mkdir -p "$work/run10" && : >"$work/run10/curl"
(
	unset LOOKOUT_INSTALL_LIB_ONLY
	PATH="$withminisign" LOOKOUT_DESTDIR="$work/run10/root" LOOKOUT_UNAME_M=x86_64 \
		FAKE_LOG="$work/run10" FAKE_RELEASE="$fx/release" \
		sh "$root/packaging/install.sh" --url https://media.example.com --token T
) >/dev/null 2>"$work/run10/stderr"
assert_eq "run10 status" 0 $?
assert_contains "run10 latest" "/releases/latest/download/lookout_linux_amd64.tar.gz" "$(cat "$work/run10/curl")"
assert_contains "run10 no key warning" "no signing key in this script; verified sha256 only" "$(cat "$work/run10/stderr")"
[ ! -s "$work/run10/minisign" ] || fail "run10 must not run minisign without a key"

echo "install_test: ok"
