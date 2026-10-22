#!/usr/bin/env bash
# Release dry run: renders the release files and builds a goreleaser snapshot
# signed with a throwaway minisign key, then checks the archives the way
# install.sh and the agent's self-update do.
# Needs Go >= 1.27.1 on PATH (goreleaser v2.18.3 runs with go run). It
# rewrites dist/ and build/release-extra/, which are both gitignored.
# Run with: bash packaging/test/release_test.sh
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "$here/../.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

fail() {
	printf 'FAIL: %s\n' "$*" >&2
	exit 1
}

sha256_of() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | cut -d' ' -f1
	else
		shasum -a 256 "$1" | cut -d' ' -f1
	fi
}

cd "$root"

GOBIN="$work/bin" go install aead.dev/minisign/cmd/minisign@v0.3.0
PATH="$work/bin:$PATH"
# A password-protected key, like the real one, so the password reaches
# minisign through goreleaser's signs.stdin. The '$' and spaces prove the
# password is passed verbatim.
MINISIGN_PASSWORD="dry run \$HOME pa{{ss}}"
printf '%s\n' "$MINISIGN_PASSWORD" | minisign -G -p "$work/lk.pub" -s "$work/lk.key" >/dev/null
MINISIGN_KEY_FILE="$work/lk.key"
MINISIGN_PUBLIC_KEY="$(tail -n 1 "$work/lk.pub")"
export MINISIGN_KEY_FILE MINISIGN_PASSWORD MINISIGN_PUBLIC_KEY

scripts/render-release-files.sh 0.0.0 build/release-extra
go run github.com/goreleaser/goreleaser/v2@v2.18.3 release --snapshot --clean >"$work/goreleaser.log" 2>&1 ||
	fail "goreleaser release --snapshot failed: $(tail -n 20 "$work/goreleaser.log")"

version="$(sed -n 's/.*"version":"\([^"]*\)".*/\1/p' dist/metadata.json)"
case "$version" in
0.0.0-SNAPSHOT-*) ;;
*) fail "snapshot version: want 0.0.0-SNAPSHOT-*, got [$version]" ;;
esac

for arch in amd64 arm64; do
	name="lookout_linux_$arch.tar.gz"
	[ -f "dist/$name" ] || fail "missing dist/$name"
	[ -f "dist/$name.minisig" ] || fail "missing dist/$name.minisig"

	entries="$(tar -tzf "dist/$name")"
	[ "$entries" = lookout ] || fail "$name entries: want [lookout], got [$entries]"

	out="$(minisign -V -P "$MINISIGN_PUBLIC_KEY" -m "dist/$name" -x "dist/$name.minisig")" ||
		fail "minisign -V failed for $name"
	trusted="$(printf '%s\n' "$out" | sed -n 's/^Trusted comment: //p')"
	[ "$trusted" = "lookout $version $name" ] ||
		fail "$name trusted comment: want [lookout $version $name], got [$trusted]"

	grep -qxF "$(sha256_of "dist/$name")  $name" dist/checksums.txt ||
		fail "checksums.txt has no matching line for $name"
done

bin=dist/lookout_linux_amd64_v1/lookout
go version -m "$bin" | grep -q 'CGO_ENABLED=0' || fail "$bin was not built with CGO_ENABLED=0"
count="$(grep -c -a -F "$MINISIGN_PUBLIC_KEY" "$bin" || true)"
[ "$count" -ge 1 ] || fail "$bin does not embed MINISIGN_PUBLIC_KEY"

LOOKOUT_DIST="$root/dist" go test -tags release -count=1 -run '^TestReleaseArtifacts$' ./internal/selfupdate >"$work/gotest.log" 2>&1 ||
	fail "selfupdate release check failed: $(cat "$work/gotest.log")"

printf 'PASS: release dry run (%s)\n' "$version"
