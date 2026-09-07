#!/bin/sh
# Remove Gniza from a DirectAdmin server.
#
# Nothing here is deleted. Everything this script takes off the server --
# the plugin directory, the two binaries, the unit file -- is moved into
# one directory under /var/lib/gniza/removed named after the time, so a
# removal can be undone by moving those files back, and deleting them
# stays a decision somebody makes on purpose. The script says where that
# directory is on its way out.
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
ATTIC=/var/lib/gniza/removed/$(date -u +%Y%m%dT%H%M%SZ)

say() { printf '%s\n' "$*"; }
[ "$(id -u)" = 0 ] || { printf 'error: run this as root\n' >&2; exit 1; }

# retire moves one path into the attic under a name that says what it
# was. A path that is not there is not an error: an uninstall run twice,
# or run after an install that stopped half way, still finishes.
#
# The attic can be on another filesystem, in which case mv copies and
# then unlinks. That is still a move: the files are at the destination
# before the originals go.
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

retire "$PLUGIN_DIR" plugin
retire /usr/local/bin/gniza-agent gniza-agent
retire /usr/local/bin/gniza-hook gniza-hook

say "Gniza is removed. Its repositories, its state database in"
say "/var/lib/gniza and its configuration in /etc/gniza are untouched."
if [ -d "$ATTIC" ]; then
	say ""
	say "What was taken off the server is in $ATTIC."
	say "Move it back to undo this, or delete it when you are sure."
else
	say "There was nothing installed here to take off."
fi
