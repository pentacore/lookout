#!/bin/bash
# Run by the Unraid plugin manager on install, on plugin update and on every
# boot (/usr/local/bin lives in RAM, so the binary is restored from the flash
# drive each time). Downloads the agent only when the flash copy is missing or
# older than the version this plugin was released with.
set -u

AGENT_VERSION='@VERSION@'
# LOOKOUT_DESTDIR prefixes every installed path (used by the tests only).
DESTDIR="${LOOKOUT_DESTDIR:-}"
PLGDIR="$DESTDIR/boot/config/plugins/lookout"
BIN="$DESTDIR/usr/local/bin/lookout"
RC="$DESTDIR/etc/rc.d/rc.lookout"

case "$AGENT_VERSION" in
@*@)
	echo "lookout: this plugin file was not rendered for a release"
	exit 1
	;;
esac

detect_arch() {
	local machine="${LOOKOUT_UNAME_M:-$(uname -m)}"
	case "$machine" in
	x86_64) echo amd64 ;;
	aarch64 | arm64) echo arm64 ;;
	*)
		echo "lookout: unsupported architecture: $machine"
		return 1
		;;
	esac
}

sha256_of() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | awk '{print $1}'
	else
		shasum -a 256 "$1" | awk '{print $1}'
	fi
}

# verify_sha256 FILE CHECKSUMS NAME succeeds when CHECKSUMS lists NAME with the
# sha256 of FILE. Unraid has no minisign, so this is the check (as in install.sh).
verify_sha256() {
	local want
	want=$(awk -v n="$3" '$2 == n || $2 == "*" n { print $1; exit }' "$2")
	[ -n "$want" ] && [ "$(sha256_of "$1")" = "$want" ]
}

# install_bin SRC installs SRC as /usr/local/bin/lookout without writing over a
# running binary in place.
install_bin() {
	mkdir -p "$(dirname "$BIN")" &&
		install -m 0755 "$1" "$BIN.new" &&
		mv -f "$BIN.new" "$BIN"
}

# fetch DIR downloads and verifies the release archive into DIR and extracts
# the binary to DIR/lookout.
fetch() {
	local dir="$1" arch name base file
	arch=$(detect_arch) || return 1
	name="lookout_linux_$arch.tar.gz"
	base="https://github.com/pentacore/lookout/releases/download/v$AGENT_VERSION"
	for file in "$name" checksums.txt; do
		if ! curl --proto '=https' --tlsv1.2 -fsSL -o "$dir/$file" "$base/$file"; then
			echo "lookout: download failed: $base/$file"
			return 1
		fi
	done
	if ! verify_sha256 "$dir/$name" "$dir/checksums.txt" "$name"; then
		echo "lookout: sha256 verification failed for $name"
		return 1
	fi
	tar -xzf "$dir/$name" -C "$dir" lookout
}

# download installs AGENT_VERSION on the flash drive and in /usr/local/bin.
download() {
	local tmp status
	tmp=$(mktemp -d) || return 1
	fetch "$tmp" &&
		cp "$tmp/lookout" "$PLGDIR/lookout.new" &&
		mv -f "$PLGDIR/lookout.new" "$PLGDIR/lookout" &&
		install_bin "$PLGDIR/lookout"
	status=$?
	rm -rf "$tmp"
	return "$status"
}

mkdir -p "$PLGDIR/state" || exit 1

installed=''
if [ -f "$PLGDIR/lookout" ] && install_bin "$PLGDIR/lookout"; then
	installed=$("$BIN" version 2>/dev/null)
	installed="${installed#lookout }"
fi

if [ -z "$installed" ] ||
	{ [ "$installed" != "$AGENT_VERSION" ] &&
		[ "$(printf '%s\n' "$installed" "$AGENT_VERSION" | sort -V | head -n 1)" = "$installed" ]; }; then
	if download; then
		installed="$AGENT_VERSION"
	elif [ -n "$installed" ]; then
		echo "lookout: warning: could not install $AGENT_VERSION; keeping $installed"
	else
		echo "lookout: could not install the agent binary"
		exit 1
	fi
fi

if [ ! -f "$PLGDIR/lookout.yaml" ]; then
	printf 'platform_url: ""\nstate_dir: /boot/config/plugins/lookout/state\nruntime_dir: /run/lookout\nauto_update: true\n' >"$PLGDIR/lookout.yaml" || {
		echo "lookout: cannot write $PLGDIR/lookout.yaml"
		exit 1
	}
fi

"$RC" restart
echo "lookout $installed installed. Enroll this server under Settings > User Utilities > Lookout."
