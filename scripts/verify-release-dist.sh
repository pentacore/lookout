#!/usr/bin/env bash
# Verify a goreleaser dist directory before the release is published: the
# checks install.sh and the agent's self-update rely on.
# Usage: MINISIGN_PUBLIC_KEY=RW... scripts/verify-release-dist.sh DIST VERSION
# Needs go and minisign on PATH. Run from anywhere; DIST may be relative.
set -euo pipefail

if [ "$#" -ne 2 ]; then
	echo "usage: $0 DIST VERSION" >&2
	exit 2
fi
: "${MINISIGN_PUBLIC_KEY:?MINISIGN_PUBLIC_KEY must be set}"
dist="$(cd "$1" && pwd)"
version="$2"
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

fail() {
	printf 'verify-release-dist: %s\n' "$*" >&2
	exit 1
}

sha256_of() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | cut -d' ' -f1
	else
		shasum -a 256 "$1" | cut -d' ' -f1
	fi
}

shopt -s nullglob
archives=("$dist"/*.tar.gz)
[ "${#archives[@]}" -eq 2 ] || fail "want 2 archives in $dist, got ${#archives[@]}"

for arch in amd64 arm64; do
	name="lookout_linux_$arch.tar.gz"
	archive="$dist/$name"
	[ -f "$archive" ] || fail "missing $name"
	[ -f "$archive.minisig" ] || fail "missing $name.minisig"

	entries="$(tar -tzf "$archive")"
	[ "$entries" = lookout ] || fail "$name entries: want [lookout], got [$entries]"

	out="$(minisign -V -P "$MINISIGN_PUBLIC_KEY" -m "$archive" -x "$archive.minisig")" ||
		fail "minisign verification failed for $name"
	trusted="$(printf '%s\n' "$out" | sed -n 's/^Trusted comment: //p')"
	[ "$trusted" = "lookout $version $name" ] ||
		fail "$name trusted comment: want [lookout $version $name], got [$trusted]"

	grep -qxF "$(sha256_of "$archive")  $name" "$dist/checksums.txt" ||
		fail "checksums.txt has no matching line for $name"
done

# With -trimpath, go version -m omits -ldflags, so look for the key itself.
binaries=("$dist"/lookout_linux_*/lookout)
[ "${#binaries[@]}" -eq 2 ] || fail "want 2 built binaries in $dist, got ${#binaries[@]}"
for bin in "${binaries[@]}"; do
	go version -m "$bin" | grep -q 'CGO_ENABLED=0' || fail "$bin was not built with CGO_ENABLED=0"
	count="$(grep -c -a -F "$MINISIGN_PUBLIC_KEY" "$bin" || true)"
	[ "$count" -ge 1 ] || fail "$bin does not embed MINISIGN_PUBLIC_KEY, so it would never self-update"
done

# The agent's own verification code, on the archives exactly as uploaded.
(cd "$root" && LOOKOUT_DIST="$dist" LOOKOUT_RELEASE_VERSION="$version" \
	go test -tags release -count=1 -run '^TestReleaseArtifacts$' ./internal/selfupdate) ||
	fail "selfupdate release check failed"

echo "verify-release-dist: ok ($version)"
