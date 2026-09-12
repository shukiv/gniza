#!/bin/sh
# Take Gniza off a server that has no control panel. What was installed
# is moved aside rather than deleted, so a removal that turns out to be a
# mistake is undone by moving it back. Repositories, the state database
# and the configuration are not touched: they are the backups.
#
# With --everything, the state, the configuration -- the master key with
# it -- restic's cache, restic and the attic go too, and nothing of Gniza
# is left on the server. That is confirmed on the terminal first, or with
# --yes where there is none, because the master key is what opens the
# stored credentials: after it, the recovery key written down is the only
# way into the backups. The backups at the destinations are never touched.
set -eu

PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
export PATH

SHARE_DIR=/usr/local/share/gniza
SERVICE=/etc/systemd/system/gniza.service
ATTIC=/var/lib/gniza/removed/$(date -u +%Y%m%dT%H%M%SZ)

say() { printf '%s\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

[ "$(id -u)" = 0 ] || die "run this as root"

EVERYTHING=no
YES=no
for arg in "$@"; do
	case "$arg" in
		--everything) EVERYTHING=yes ;;
		--yes) YES=yes ;;
		*) die "unknown option $arg; the options are --everything and --yes" ;;
	esac
done

# --everything is confirmed before anything is stopped or moved, so a
# "no" leaves the server as it was. On a terminal the phrase is typed;
# where there is none, --yes stands for it. The Remove button runs this
# script with no options and is not affected.
tty_usable() { ( : < /dev/tty ) 2>/dev/null && [ -w /dev/tty ]; }
if [ "$EVERYTHING" = yes ] && [ "$YES" != yes ]; then
	tty_usable || die "--everything needs a terminal to confirm on, or --yes with it"
	cat > /dev/tty <<'WARN'

--everything deletes, beyond what an uninstall moves aside:
  /etc/gniza             the master key, the browser password, the SSH keys
  /var/lib/gniza         the state database: destinations, schedules, history
  /var/cache/gniza       restic's cache
  /usr/local/bin/restic
Without the master key the credentials Gniza stored cannot be read again:
the recovery key you wrote down becomes the only way into the backups.
The backups themselves, at the destinations, are not touched.

WARN
	printf '%s' "Type  delete everything  to go on, anything else to stop: " > /dev/tty
	IFS= read -r reply < /dev/tty || reply=""
	[ "$reply" = "delete everything" ] || { say "Nothing was done."; exit 1; }
fi

# delete_everything is the one place this script deletes what a reinstall
# would come back to, and it runs only under --everything, after the
# confirmation above. The attic goes with the state directory, and last
# the directory this script is read from: the shell holds it open, so
# that is safe. restic is deleted from where the installer puts it, since
# the installer only puts one there when the server had none.
delete_everything() {
	rm -rf -- /var/lib/gniza /etc/gniza /var/cache/gniza /var/run/gniza
	rm -f -- /usr/local/bin/restic
	rm -rf -- /usr/local/share/gniza
}

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

if [ "$EVERYTHING" = yes ]; then
	delete_everything
	say "Gniza is gone from this server: the service, the programs, the"
	say "configuration with its master key, the state and restic's cache."
	say "The backups at the destinations are untouched; the recovery key"
	say "written down is the way to open them. The public key an SFTP"
	say "destination was given stays in its authorized_keys until removed there."
	exit 0
fi

say "Gniza is removed. Its repositories, its state database in"
say "/var/lib/gniza and its configuration in /etc/gniza are untouched."
if [ -d "$ATTIC" ]; then
	say ""
	say "What was taken off the server is in $ATTIC."
	say "Move it back to undo this, or delete it when you are sure."
else
	say "There was nothing installed here to take off."
fi
