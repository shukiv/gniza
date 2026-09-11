#!/bin/sh
# Take Gniza off a server that has no control panel. What was installed
# is moved aside rather than deleted, so a removal that turns out to be a
# mistake is undone by moving it back. Repositories, the state database
# and the configuration are not touched: they are the backups.
set -eu

PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
export PATH

SHARE_DIR=/usr/local/share/gniza
SERVICE=/etc/systemd/system/gniza.service
ATTIC=/var/lib/gniza/removed/$(date -u +%Y%m%dT%H%M%SZ)

say() { printf '%s\n' "$*"; }

[ "$(id -u)" = 0 ] || { printf 'error: run this as root\n' >&2; exit 1; }

retire() {
	if [ ! -e "$1" ] && [ ! -L "$1" ]; then
		return 0
	fi
	mkdir -p "$ATTIC"
	chmod 0700 "$ATTIC"
	mv -- "$1" "$ATTIC/$2"
}

if command -v systemctl >/dev/null 2>&1; then
	systemctl stop gniza >/dev/null 2>&1 || true
	systemctl disable gniza >/dev/null 2>&1 || true
fi
retire "$SERVICE" gniza.service
if command -v systemctl >/dev/null 2>&1; then
	systemctl daemon-reload || true
fi
retire "$SHARE_DIR" share
retire /usr/local/bin/gniza-agent gniza-agent

say "Gniza is removed. Its repositories, its state database in"
say "/var/lib/gniza and its configuration in /etc/gniza are untouched."
if [ -d "$ATTIC" ]; then
	say ""
	say "What was taken off the server is in $ATTIC."
	say "Move it back to undo this, or delete it when you are sure."
else
	say "There was nothing installed here to take off."
fi
