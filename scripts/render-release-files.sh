#!/usr/bin/env bash
# Render the release files that carry build-time placeholders: install.sh and
# the Unraid plugin lookout.plg.
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

# lookout.plg: include each script inside its CDATA block first, then replace
# the version placeholders (plg-install.sh carries @VERSION@), then check for
# leftovers. Unraid compares plugin versions as strings, so the plg version is
# a UTC timestamp rather than the semver.
unraid="$root/packaging/unraid"
for file in rc.lookout supervise enroll Lookout.page enroll.php plg-install.sh plg-remove.sh; do
	if grep -qF ']]>' "$unraid/$file"; then
		echo "$unraid/$file contains ]]>, which would end its CDATA block in lookout.plg" >&2
		exit 1
	fi
done
plg_version="$(date -u +%Y.%m.%d.%H%M)"
awk -v dir="$unraid" '
	function slurp(path,   line, text, status) {
		text = ""
		while ((status = (getline line < path)) > 0) {
			text = text line "\n"
		}
		if (status < 0) {
			print "cannot read " path > "/dev/stderr"
			exit 1
		}
		close(path)
		return text
	}
	BEGIN {
		include["@RC_LOOKOUT@"] = "rc.lookout"
		include["@SUPERVISE@"] = "supervise"
		include["@ENROLL@"] = "enroll"
		include["@PAGE@"] = "Lookout.page"
		include["@ENROLL_PHP@"] = "enroll.php"
		include["@INSTALL@"] = "plg-install.sh"
		include["@REMOVE@"] = "plg-remove.sh"
	}
	{
		out = ""
		rest = $0
		while (match(rest, /@[A-Z_]+@/)) {
			key = substr(rest, RSTART, RLENGTH)
			out = out substr(rest, 1, RSTART - 1) ((key in include) ? slurp(dir "/" include[key]) : key)
			rest = substr(rest, RSTART + RLENGTH)
		}
		print out rest
	}
' "$unraid/lookout.plg" >"$outdir/lookout.plg.new"
sed -i.bak -e "s|@PLG_VERSION@|$plg_version|g" -e "s|@VERSION@|$version|g" "$outdir/lookout.plg.new"
rm -f "$outdir/lookout.plg.new.bak"
if grep -q '@[A-Z_]*@' "$outdir/lookout.plg.new"; then
	echo "unreplaced placeholder in $outdir/lookout.plg" >&2
	rm -f "$outdir/lookout.plg.new"
	exit 1
fi
mv -f "$outdir/lookout.plg.new" "$outdir/lookout.plg"
