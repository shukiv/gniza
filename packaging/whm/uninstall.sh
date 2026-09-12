#!/bin/sh
# Remove the Gniza WHM plugin. Backups already stored remotely are not
# touched, and neither is the local state file, so a reinstall picks up
# where this left off.
#
# Almost nothing here is deleted. What this script takes off the server is
# moved into one dated directory under /var/lib/gniza/removed, which it
# names on its way out, so an uninstall can be undone by moving those
# files back and deleting them stays a decision somebody makes on purpose.
# The two exceptions are named where they happen: restic's cache, which is
# rebuilt from the repository and is the one thing here that reaches
# gigabytes, and the scratch directory this script makes for cPanel's own
# plugin remover.
#
# With --everything, the state, the configuration -- the master key with
# it -- restic's cache, restic and the attic go too, and nothing of Gniza
# is left on the server. That is confirmed on the terminal first, or with
# --yes where there is none, because the master key is what opens the
# stored credentials: after it, the recovery key written down is the only
# way into the backups. The backups at the destinations are never touched.
set -eu

PATH=/usr/local/cpanel/3rdparty/bin:/usr/local/cpanel/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
export PATH

say() { printf '%s\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }
[ "$(id -u)" = 0 ] || die "run this as root"

SOURCE_DIR=$(cd "$(dirname "$0")" && pwd)
ATTIC=/var/lib/gniza/removed/$(date -u +%Y%m%dT%H%M%SZ)

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

# retire moves one path into the attic under a name that says what it was.
# A path that is not there is not an error: an uninstall run twice, or run
# after an install that stopped half way, still finishes.
#
# The attic can be on another filesystem, in which case mv copies and then
# unlinks. That is still a move: the files are at the destination before
# the originals go.
retire() {
	if [ ! -e "$1" ] && [ ! -L "$1" ]; then
		return 0
	fi
	mkdir -p "$ATTIC"
	chmod 0700 "$ATTIC"
	mv -- "$1" "$ATTIC/$2"
}

systemctl stop gniza 2>/dev/null || true
systemctl disable gniza 2>/dev/null || true
retire /etc/systemd/system/gniza.service gniza.service
systemctl daemon-reload 2>/dev/null || true

if [ -x /usr/local/cpanel/bin/unregister_appconfig ]; then
    /usr/local/cpanel/bin/unregister_appconfig Gniza || true
fi
retire /var/cpanel/apps/gniza.conf gniza-appconfig.conf
retire /usr/local/cpanel/whostmgr/docroot/cgi/gniza.cgi whostmgr-gniza.cgi
retire /usr/local/cpanel/cgi/gniza.cgi cgi-gniza.cgi

if [ -x /usr/local/cpanel/bin/manage_hooks ]; then
    # Current releases describe every registration, including the blocking
    # pre-remove hook, from the installed executable.
    if [ -x /usr/local/cpanel/3rdparty/bin/gniza-hook ]; then
        /usr/local/cpanel/bin/manage_hooks delete script /usr/local/cpanel/3rdparty/bin/gniza-hook \
            >/dev/null 2>&1 || true
    fi
    # Also clean up post hooks from releases that registered manually.
    for hook in "Accounts::Create:create" "Accounts::Modify:modify" "Accounts::Remove:remove"; do
        event=${hook%:*}
        action=${hook#*:}
        /usr/local/cpanel/bin/manage_hooks delete script /usr/local/cpanel/3rdparty/bin/gniza-hook --manual \
            --category Whostmgr --event "$event" --stage post \
            --action="--cpanel-hook=$action" >/dev/null 2>&1 || true
    done
fi
retire /usr/local/cpanel/3rdparty/bin/gniza-hook gniza-hook
retire /usr/local/bin/gniza-agent gniza-agent
retire /usr/local/cpanel/Cpanel/API/Gniza.pm Gniza.pm
# The whole directory, so the session module goes with the one thing that
# was ever in it and no empty directory is left behind to tidy up.
retire /var/cpanel/perl/Cpanel/Admin/Modules/Gniza admin-module

# Remove the account-facing registration with the same supported mechanism
# used at install time. Older releases wrote DynamicUI directly, so that
# exact legacy file is removed as well.
FRONTEND=/usr/local/cpanel/base/frontend/jupiter
if [ -x /usr/local/cpanel/scripts/uninstall_plugin ] &&
   [ -f "$SOURCE_DIR/cpanel/install.json" ] &&
   [ -f "$SOURCE_DIR/branding/badge.svg" ]; then
    PLUGIN_META=$(mktemp -d /var/tmp/gniza-cpanel.XXXXXX)
    trap 'if [ -n "${PLUGIN_META:-}" ]; then rm -rf -- "$PLUGIN_META"; fi' 0 1 2 15
    install -m 0644 "$SOURCE_DIR/cpanel/install.json" "$PLUGIN_META/install.json"
    install -m 0644 "$SOURCE_DIR/branding/badge.svg" "$PLUGIN_META/gniza.svg"
    /usr/local/cpanel/scripts/uninstall_plugin "$PLUGIN_META" --theme=jupiter || true
    rm -rf -- "$PLUGIN_META"
    PLUGIN_META=
    trap - 0 1 2 15
fi
retire "$FRONTEND/dynamicui/dynamicui_gniza.conf" dynamicui_gniza.conf
retire "$FRONTEND/gniza" jupiter-plugin
retire "$FRONTEND/assets/application_icons/gniza.png" gniza.png
retire /usr/local/cpanel/whostmgr/docroot/addon_plugins/gniza.svg gniza.svg

# restic's cache is the one thing here that is deleted rather than moved.
# It is rebuilt from the repository on the next backup, and it is the one
# thing here that reaches gigabytes: moving it into the attic would free
# no disk at all on a server somebody is uninstalling to make room.
rm -rf -- /var/cache/gniza

# Account events the hooks left for a service that is now going away. The
# hooks are unregistered above, so nothing will add more and nothing will
# ever read these; reinstalling later must not replay account changes from
# whenever Gniza was last installed. Moved rather than deleted, because an
# account created or removed while Gniza was installed is exactly what
# somebody investigating a gap in the history will want to read.
retire /var/lib/gniza/hooks hooks

[ "$EVERYTHING" = yes ] || cat <<'DONE'
Gniza removed.

Left in place on purpose:
  /etc/gniza/master.key   the key that decrypts your stored credentials
  /var/lib/gniza/state.db your destinations, schedules and history

Delete those only when you are certain you will not reinstall, and never
before you have another way to read your backups: without the key file the
stored destination credentials cannot be recovered.

Reinstalling picks up from there: the same destinations, schedules and
history come back with it.
DONE

if [ "$EVERYTHING" != yes ] && [ -d "$ATTIC" ]; then
	cat <<DONE

What was taken off the server is in $ATTIC.
Move it back to undo this, or delete it when you are sure.
DONE
fi

# Last, because this script is reading itself out of that directory: the copy
# of the uninstaller the installer left behind, and the two cPanel files it
# needs to remove the account-facing tile. Moving a running script's own
# directory is safe -- the shell reads it through an open descriptor -- and
# it means the uninstaller that was run is still there to be read.
retire /usr/local/share/gniza share

if [ "$EVERYTHING" = yes ]; then
	delete_everything
	say "Gniza is gone from this server: the service, the programs, the"
	say "configuration with its master key, the state and restic's cache."
	say "The backups at the destinations are untouched; the recovery key"
	say "written down is the way to open them. The public key an SFTP"
	say "destination was given stays in its authorized_keys until removed there."
	exit 0
fi
