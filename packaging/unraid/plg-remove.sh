#!/bin/bash
# Run by the Unraid plugin manager when the plugin is removed. The flash drive
# copy in /boot/config/plugins/lookout is kept so a reinstall keeps the identity.
DESTDIR="${LOOKOUT_DESTDIR:-}"

[ -x "$DESTDIR/etc/rc.d/rc.lookout" ] && "$DESTDIR/etc/rc.d/rc.lookout" stop
rm -f "$DESTDIR/usr/local/bin/lookout" "$DESTDIR/etc/rc.d/rc.lookout"
rm -rf "$DESTDIR/usr/local/emhttp/plugins/lookout"
echo "lookout removed. /boot/config/plugins/lookout (config, credentials and binary) is kept; delete it to forget this server."
