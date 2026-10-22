#!/usr/bin/env bash
# Render the release files that carry build-time placeholders.
# Usage: MINISIGN_PUBLIC_KEY=RW... scripts/render-release-files.sh VERSION OUTDIR
set -euo pipefail

if [ "$#" -ne 2 ]; then
	echo "usage: $0 VERSION OUTDIR" >&2
	exit 2
fi
version="${1#v}"
outdir="$2"
: "${MINISIGN_PUBLIC_KEY:?MINISIGN_PUBLIC_KEY must be set}"
if ! [[ $version =~ ^[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.+-]+)?$ ]]; then
	echo "invalid version: $version" >&2
	exit 2
fi
if ! [[ $MINISIGN_PUBLIC_KEY =~ ^[A-Za-z0-9+/=]+$ ]]; then
	echo "invalid MINISIGN_PUBLIC_KEY (expected base64)" >&2
	exit 2
fi

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
mkdir -p "$outdir"

# Placeholders only contain [A-Z_], so a '|' delimiter is safe for the key (base64 has no '|').
sed -e "s|@VERSION@|$version|g" -e "s|@MINISIGN_PUBLIC_KEY@|$MINISIGN_PUBLIC_KEY|g" \
	"$root/packaging/install.sh" >"$outdir/install.sh"
chmod 0755 "$outdir/install.sh"

if grep -q '@[A-Z_]*@' "$outdir/install.sh"; then
	echo "unreplaced placeholder in $outdir/install.sh" >&2
	exit 1
fi
