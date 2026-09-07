#!/bin/sh
# Remove Gniza from a DirectAdmin server.
#
# Backups are not touched: they are in the repositories, not here, and a
# removal that deleted them would be the one mistake this program must
# never make. The state database is left as well, so a reinstall knows
# what it was doing.
set -eu

PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
export PATH

PLUGIN_DIR=/usr/local/directadmin/plugins/gniza
SERVICE=/etc/systemd/system/gniza.service

say() { printf '%s\n' "$*"; }
[ "$(id -u)" = 0 ] || { printf 'error: run this as root\n' >&2; exit 1; }

if command -v systemctl >/dev/null 2>&1; then
	systemctl stop gniza >/dev/null 2>&1 || true
	systemctl disable gniza >/dev/null 2>&1 || true
fi
rm -f "$SERVICE"
if command -v systemctl >/dev/null 2>&1; then
	systemctl daemon-reload || true
fi

rm -rf "$PLUGIN_DIR"
rm -f /usr/local/bin/gniza-agent /usr/local/bin/gniza-hook

say "Gniza is removed. Its repositories, its state database in"
say "/var/lib/gniza and its configuration in /etc/gniza are untouched."
