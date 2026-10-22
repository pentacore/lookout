#!/bin/sh
# Install or upgrade the lookout host agent on Debian/Ubuntu (systemd).
#
#   sh install.sh --url https://media.example.com [--version X.Y.Z]   (as root,
#   with the enrollment token in LOOKOUT_TOKEN; or pass --token TOKEN)
#
# Safe to re-run: it upgrades the binary, keeps the existing config and, when
# a token is given, re-enrolls the server. The enrollment token is never
# printed and is handed to `lookout enroll` in its environment, not on its
# command line.
set -eu

VERSION='@VERSION@'
MINISIGN_PUBLIC_KEY='@MINISIGN_PUBLIC_KEY@'

# LOOKOUT_DESTDIR prefixes every installed path (used by the tests only).
DESTDIR="${LOOKOUT_DESTDIR:-}"
BIN_PATH="$DESTDIR/usr/local/bin/lookout"
CONFIG_PATH="$DESTDIR/etc/lookout/lookout.yaml"
UNIT_PATH="$DESTDIR/etc/systemd/system/lookout.service"

usage() {
	cat >&2 <<'USAGE'
usage: install.sh [--url PLATFORM_URL] [--token ENROLLMENT_TOKEN] [--version VERSION|latest]

  --url      platform base URL; required on first install
  --token    enrollment token; required until this server is enrolled. Prefer
             setting LOOKOUT_TOKEN instead: sudo logs every command line.
  --version  release to install (default: the version this script was built for)
USAGE
}

die() {
	printf 'install.sh: %s\n' "$*" >&2
	exit 1
}

# detect_arch prints the release architecture name for this machine.
detect_arch() {
	machine="${LOOKOUT_UNAME_M:-$(uname -m)}"
	case "$machine" in
	x86_64) echo amd64 ;;
	aarch64 | arm64) echo arm64 ;;
	*)
		echo "unsupported architecture: $machine" >&2
		return 1
		;;
	esac
}

# sha256_of FILE prints the hex sha256 of FILE.
sha256_of() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | awk '{print $1}'
	else
		shasum -a 256 "$1" | awk '{print $1}'
	fi
}

# verify_sha256 FILE CHECKSUMS NAME succeeds when CHECKSUMS lists NAME with the
# sha256 of FILE.
verify_sha256() {
	vs_file="$1"
	vs_sums="$2"
	vs_name="$3"
	vs_want=$(awk -v n="$vs_name" '$2 == n || $2 == "*" n { print $1; exit }' "$vs_sums")
	[ -n "$vs_want" ] || return 1
	vs_got=$(sha256_of "$vs_file")
	[ "$vs_got" = "$vs_want" ]
}

# valid_url URL accepts https:// URLs, and http:// only for loopback hosts.
valid_url() {
	case "$1" in
	*[[:space:]@]*) return 1 ;;
	https://?*) return 0 ;;
	http://localhost | http://localhost[:/]* | http://127.0.0.1 | http://127.0.0.1[:/]*) return 0 ;;
	'http://[::1]' | 'http://[::1]'[:/]*) return 0 ;;
	esac
	return 1
}

# config_url PATH prints the platform_url of an existing config, if any.
config_url() {
	sed -n 's/^platform_url:[[:space:]]*//p' "$1" | head -n 1 | sed -e 's/[[:space:]]*$//' -e "s/^[\"']//" -e "s/[\"']\$//"
}

# write_config PATH URL creates the config, or, when it exists, replaces only
# its platform_url line (appending one when missing) and keeps everything else.
write_config() {
	wc_path="$1"
	wc_url="$2"
	mkdir -p "$(dirname "$wc_path")"
	if [ ! -f "$wc_path" ]; then
		printf 'platform_url: %s\nstate_dir: /var/lib/lookout\nauto_update: true\n' "$wc_url" >"$wc_path"
		chmod 0644 "$wc_path"
		return 0
	fi
	wc_tmp="$wc_path.new.$$"
	WC_URL="$wc_url" awk '
		/^platform_url:/ { print "platform_url: " ENVIRON["WC_URL"]; seen = 1; next }
		{ print }
		END { if (!seen) print "platform_url: " ENVIRON["WC_URL"] }
	' "$wc_path" >"$wc_tmp"
	chmod 0644 "$wc_tmp"
	mv "$wc_tmp" "$wc_path"
}

write_unit() {
	mkdir -p "$(dirname "$UNIT_PATH")"
	cat >"$UNIT_PATH" <<'UNIT'
[Unit]
Description=Lookout host agent
Documentation=https://github.com/pentacore/lookout
Wants=network-online.target
After=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/lookout run --config /etc/lookout/lookout.yaml
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
UNIT
}

# enroll_server [--force] runs `lookout enroll` with the token in LOOKOUT_TOKEN,
# so it never shows in the process list. This script restarts the service
# itself, so the agent's "restart lookout" hint is dropped.
enroll_server() {
	es_out=$(LOOKOUT_TOKEN="$token" "$BIN_PATH" enroll --config "$CONFIG_PATH" --url "$platform_url" "$@") || exit 1
	printf '%s\n' "$es_out" | sed '/^restart lookout /d'
}

main() {
	platform_url=''
	token="${LOOKOUT_TOKEN:-}"
	version="$VERSION"
	while [ "$#" -gt 0 ]; do
		case "$1" in
		--url | --token | --version)
			[ "$#" -ge 2 ] || {
				usage
				exit 2
			}
			case "$1" in
			--url) platform_url="$2" ;;
			--token) token="$2" ;;
			--version) version="$2" ;;
			esac
			shift 2
			;;
		*)
			usage
			exit 2
			;;
		esac
	done

	[ "$(id -u)" = 0 ] || die "must run as root (try: sudo sh install.sh ...)"
	command -v systemctl >/dev/null 2>&1 || die "systemd is required (systemctl not found)"
	[ -n "$DESTDIR" ] || [ -d /run/systemd/system ] || die "systemd is required (this system is not running it)"

	# An unrendered script still carries its placeholder: treat it as latest.
	case "$version" in
	'' | latest | @*@) version=latest ;;
	*[!0-9A-Za-z.+-]*) die "invalid --version" ;;
	esac
	if [ -n "$platform_url" ]; then
		valid_url "$platform_url" || die "--url must be https:// (http:// is allowed only for localhost, 127.0.0.1 and [::1]) and contain no spaces or '@'"
	fi
	if [ ! -f "$CONFIG_PATH" ]; then
		[ -n "$platform_url" ] || die "--url is required on first install"
		[ -n "$token" ] || die "--token or LOOKOUT_TOKEN is required (this server is not enrolled yet)"
	fi

	arch=$(detect_arch) || exit 1
	name="lookout_linux_$arch.tar.gz"
	if [ "$version" = latest ]; then
		default_base='https://github.com/pentacore/lookout/releases/latest/download'
	else
		default_base="https://github.com/pentacore/lookout/releases/download/v${version#v}"
	fi
	base="${LOOKOUT_DOWNLOAD_BASE:-$default_base}"

	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT
	trap 'exit 1' INT TERM HUP
	for file in "$name" checksums.txt "$name.minisig"; do
		curl --proto '=https' --tlsv1.2 -fsSL -o "$tmp/$file" "$base/$file" || die "download failed: $file"
	done

	case "$MINISIGN_PUBLIC_KEY" in
	@*@) have_key=no ;;
	*) have_key=yes ;;
	esac
	if [ "$have_key" = yes ] && command -v minisign >/dev/null 2>&1; then
		minisign -Vm "$tmp/$name" -P "$MINISIGN_PUBLIC_KEY" >/dev/null || die "minisign verification failed for $name"
	else
		verify_sha256 "$tmp/$name" "$tmp/checksums.txt" "$name" || die "sha256 verification failed for $name"
		if [ "$have_key" = yes ]; then
			echo "warning: minisign not installed; verified sha256 only" >&2
		else
			echo "warning: no signing key in this script; verified sha256 only" >&2
		fi
	fi

	tar -xzf "$tmp/$name" -C "$tmp" lookout || die "could not extract $name"
	mkdir -p "$(dirname "$BIN_PATH")"
	install -m 0755 "$tmp/lookout" "$BIN_PATH.new"
	mv -f "$BIN_PATH.new" "$BIN_PATH"

	# Existing credentials must never be pointed at a different platform, so a
	# changed --url is only ever applied by re-enrolling (enroll --force).
	switching=no
	if [ ! -f "$CONFIG_PATH" ]; then
		write_config "$CONFIG_PATH" "$platform_url"
	else
		existing_url=$(config_url "$CONFIG_PATH")
		if [ -n "$platform_url" ] && [ "${platform_url%/}" != "${existing_url%/}" ]; then
			switching=yes
		else
			platform_url="$existing_url"
		fi
	fi

	if ! check_out=$("$BIN_PATH" check-config --config "$CONFIG_PATH" 2>"$tmp/check-config.err"); then
		die "check-config failed: $(cat "$tmp/check-config.err")"
	fi
	enrolled=no
	if printf '%s\n' "$check_out" | grep -q '^enrolled: *yes'; then
		enrolled=yes
	fi

	if [ -z "$token" ]; then
		[ "$enrolled" = no ] || [ "$switching" = no ] || die "--url changes the platform; pass --token or LOOKOUT_TOKEN to re-enroll"
		[ "$enrolled" = yes ] || die "--token or LOOKOUT_TOKEN is required (this server is not enrolled yet)"
	else
		[ -n "$platform_url" ] || die "no platform_url in $CONFIG_PATH; pass --url"
		if [ "$enrolled" = yes ] || [ "$switching" = yes ]; then
			enroll_server --force
		else
			enroll_server
		fi
	fi

	write_unit
	systemctl daemon-reload
	systemctl enable lookout
	systemctl restart lookout

	echo "$("$BIN_PATH" version) installed; status: systemctl status lookout"
}

[ "${LOOKOUT_INSTALL_LIB_ONLY:-}" = 1 ] || main "$@"
