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
set -eu

PATH=/usr/local/cpanel/3rdparty/bin:/usr/local/cpanel/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
export PATH

[ "$(id -u)" = 0 ] || { echo "run this as root" >&2; exit 1; }

SOURCE_DIR=$(cd "$(dirname "$0")" && pwd)
ATTIC=/var/lib/gniza/removed/$(date -u +%Y%m%dT%H%M%SZ)

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

cat <<'DONE'
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

if [ -d "$ATTIC" ]; then
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
