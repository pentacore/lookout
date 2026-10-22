#!/usr/bin/env bash
# Release dry run: renders the release files and builds a goreleaser snapshot
# signed with a throwaway minisign key, then runs the release workflow's
# pre-publish check (scripts/verify-release-dist.sh) on it.
# GORELEASER is the goreleaser command (default: go run of v2.18.3, which
# needs Go >= 1.27.1; CI passes an installed goreleaser). It rewrites dist/
# and build/release-extra/, which are both gitignored.
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

read -r -a goreleaser <<<"${GORELEASER:-go run github.com/goreleaser/goreleaser/v2@v2.18.3}"

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
"${goreleaser[@]}" release --snapshot --clean >"$work/goreleaser.log" 2>&1 ||
	fail "goreleaser release --snapshot failed: $(tail -n 20 "$work/goreleaser.log")"

version="$(sed -n 's/.*"version":"\([^"]*\)".*/\1/p' dist/metadata.json)"
case "$version" in
*-SNAPSHOT-*) ;;
*) fail "snapshot version: want *-SNAPSHOT-*, got [$version]" ;;
esac

scripts/verify-release-dist.sh dist "$version" >"$work/verify.log" 2>&1 ||
	fail "pre-publish verification failed: $(cat "$work/verify.log")"

# The check must reject a signature that belongs to the other arch's archive.
cp -R dist "$work/swapped"
cp "$work/swapped/lookout_linux_arm64.tar.gz.minisig" "$work/swapped/lookout_linux_amd64.tar.gz.minisig"
if scripts/verify-release-dist.sh "$work/swapped" "$version" >"$work/swapped.log" 2>&1; then
	fail "pre-publish verification accepted a swapped signature"
fi

printf 'PASS: release dry run (%s)\n' "$version"
