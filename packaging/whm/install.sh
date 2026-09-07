#!/bin/sh
# Install Gniza as a WHM plugin on this cPanel server.
#
# Everything here is idempotent: running it again upgrades in place.
set -eu

# This script runs as root. Do not let the invoking shell choose binaries
# such as install, systemctl, sed or restic from a writable directory.
PATH=/usr/local/cpanel/3rdparty/bin:/usr/local/cpanel/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
export PATH

PREFIX=/usr/local/bin
CONFIG_DIR=/etc/gniza
STATE_DIR=/var/lib/gniza
STAGING_DIR=/var/lib/gniza/staging
# Where a cPanel lifecycle hook leaves an account event the service was
# not running to hear, for it to replay when it comes back.
HOOK_SPOOL_DIR=/var/lib/gniza/hooks
CACHE_DIR=/var/cache/gniza/restic
RUN_DIR=/var/run/gniza
APPCONFIG_DIR=/var/cpanel/apps
SHARE_DIR=/usr/local/share/gniza
SERVICE=/etc/systemd/system/gniza.service

say() { printf '%s\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

[ "$(id -u)" = 0 ] || die "run this as root"
[ -d /usr/local/cpanel ] || die "this does not look like a cPanel server (/usr/local/cpanel is missing)"
umask 077

SOURCE_DIR=$(cd "$(dirname "$0")" && pwd)
[ -f "$SOURCE_DIR/gniza-agent" ] || die "gniza-agent is not next to this script"
[ -f "$SOURCE_DIR/gniza.cgi" ] || die "gniza.cgi is not next to this script"
[ -f "$SOURCE_DIR/uninstall.sh" ] || die "uninstall.sh is not next to this script"
[ -f "$SOURCE_DIR/cpanel/install.json" ] || die "cpanel/install.json is missing from the package"
[ -f "$SOURCE_DIR/cpanel/uapi/Gniza.pm" ] || die "the cPanel UAPI bridge is missing from the package"
[ -f "$SOURCE_DIR/cpanel/admin/Gniza/Session.pm" ] || die "the cPanel AdminBin bridge is missing from the package"
[ -f "$SOURCE_DIR/branding/badge.svg" ] || die "the cPanel plugin icon is missing from the package"
[ -d /var/cpanel/sessions/raw ] || die "cPanel's session store is missing (/var/cpanel/sessions/raw)"
[ -x /usr/local/cpanel/bin/uapi ] || die "cPanel's uapi command is missing"

# --- the installation this one replaces ------------------------------------
# Until v0.1.7 this program was called cprest, and everything it owned was
# named after it: a service, two binaries, a WHM plugin, an account-facing
# plugin, a cPanel feature, and two directories holding the master key and
# the state file. The names all change here at once, so an upgrade has to
# take the old installation apart before putting this one together.
#
# The order matters. Everything that could refuse is checked first, while
# the old installation is still whole and still running: a server that
# fails half way through this has no service at all.
LEGACY_CONFIG_DIR=/etc/cprest
LEGACY_STATE_DIR=/var/lib/cprest
LEGACY_SHARE_DIR=/usr/local/share/cprest
LEGACY_AGENT=/usr/local/bin/cprest-agent
LEGACY_HOOK_BIN=/usr/local/cpanel/3rdparty/bin/cprest-hook

legacy_present() {
    [ -d "$LEGACY_CONFIG_DIR" ] || [ -d "$LEGACY_STATE_DIR" ] ||
        [ -x "$LEGACY_AGENT" ] || [ -f /etc/systemd/system/cprest.service ]
}

if legacy_present; then
    say "found the earlier cprest installation; taking it apart first"

    # Refuse before anything is touched rather than half way through. Two
    # state files, one under each name, is not a thing this script can
    # resolve: whichever it kept could be the one with this server's
    # destinations and history in it.
    for pair in "$LEGACY_CONFIG_DIR:$CONFIG_DIR" "$LEGACY_STATE_DIR:$STATE_DIR"; do
        old=${pair%:*}
        new=${pair#*:}
        if [ -d "$old" ] && [ -e "$new" ]; then
            die "both $old and $new exist. Only one of them holds this server's key and state, and this script cannot tell which. Move or remove the one you do not want to keep, then run this again."
        fi
    done

    # Unregister the account lifecycle hooks while the executable that
    # describes them is still there: manage_hooks asks the script itself
    # what it registered.
    if [ -x /usr/local/cpanel/bin/manage_hooks ] && [ -x "$LEGACY_HOOK_BIN" ]; then
        /usr/local/cpanel/bin/manage_hooks delete script "$LEGACY_HOOK_BIN" >/dev/null 2>&1 || true
        for hook in "Accounts::Create:create" "Accounts::Modify:modify" "Accounts::Remove:remove"; do
            event=${hook%:*}
            action=${hook#*:}
            /usr/local/cpanel/bin/manage_hooks delete script "$LEGACY_HOOK_BIN" --manual \
                --category Whostmgr --event "$event" --stage post \
                --action="--cpanel-hook=$action" >/dev/null 2>&1 || true
        done
        say "unregistered the old account lifecycle hooks"
    fi

    # The uninstaller the old release left on the server. It knows the
    # names that release used -- including the cPanel plugin descriptor it
    # keeps beside itself, which this package no longer carries -- and it
    # deliberately leaves the master key and the state file behind.
    if [ -f "$LEGACY_SHARE_DIR/uninstall.sh" ]; then
        sh "$LEGACY_SHARE_DIR/uninstall.sh" >/dev/null 2>&1 ||
            say "warning: the old uninstaller reported a problem; carrying on"
    fi

    # What the old uninstaller does not cover, either because it was not
    # there to run or because it came from a release that did less.
    systemctl stop cprest >/dev/null 2>&1 || true
    systemctl disable cprest >/dev/null 2>&1 || true
    rm -f /etc/systemd/system/cprest.service
    systemctl daemon-reload >/dev/null 2>&1 || true
    if [ -x /usr/local/cpanel/bin/unregister_appconfig ]; then
        /usr/local/cpanel/bin/unregister_appconfig cprest >/dev/null 2>&1 || true
    fi
    rm -f /var/cpanel/apps/cprest.conf
    rm -f /usr/local/cpanel/whostmgr/docroot/cgi/cprest.cgi /usr/local/cpanel/cgi/cprest.cgi
    rm -f /usr/local/cpanel/whostmgr/docroot/addon_plugins/cprest.svg
    rm -f "$LEGACY_HOOK_BIN" "$LEGACY_AGENT"
    rm -f /usr/local/cpanel/Cpanel/API/Cprest.pm
    rm -f /var/cpanel/perl/Cpanel/Admin/Modules/Cprest/Session.pm
    rmdir /var/cpanel/perl/Cpanel/Admin/Modules/Cprest 2>/dev/null || true
    rm -rf -- /usr/local/cpanel/base/frontend/jupiter/cprest
    rm -f /usr/local/cpanel/base/frontend/jupiter/dynamicui/dynamicui_cprest.conf
    rm -f /usr/local/cpanel/base/frontend/jupiter/assets/application_icons/cprest.png \
        /usr/local/cpanel/base/frontend/jupiter/assets/application_icons/cprest.svg
    rm -rf -- /var/cache/cprest
    rm -rf -- "$LEGACY_SHARE_DIR"

    # The two directories worth keeping. The master key decrypts the stored
    # destination credentials and the state file holds every destination,
    # schedule and backup this server has: losing either is losing the
    # backups, whatever is still sitting on the backup server.
    #
    # What the state file says about these directories is stale after this
    # -- it records absolute paths, including the private key each SFTP
    # destination connects with -- and the service rewrites those on its
    # next start.
    for pair in "$LEGACY_CONFIG_DIR:$CONFIG_DIR" "$LEGACY_STATE_DIR:$STATE_DIR"; do
        old=${pair%:*}
        new=${pair#*:}
        if [ -d "$old" ]; then
            mv -- "$old" "$new" || die "could not move $old to $new; nothing else has been installed"
            say "moved $old to $new"
            # This script is inside one of the directories being moved: an
            # upgrade unpacks the release under the state directory and runs
            # it from there. SOURCE_DIR was worked out before the move, so
            # without this every path built from it afterwards names
            # somewhere that no longer exists, and the old installation has
            # already been taken apart by then.
            case $SOURCE_DIR in
                "$old"|"$old"/*) SOURCE_DIR=$new${SOURCE_DIR#"$old"} ;;
            esac
        fi
    done
    MIGRATED_FROM_CPREST=1
    # Say it here rather than let the first missing file say it: from this
    # point the old installation is gone, so a package that cannot be found
    # is a server with no backups on it and nothing left to explain why.
    [ -f "$SOURCE_DIR/gniza-agent" ] \
        || die "the package is no longer at $SOURCE_DIR after moving the directories; the earlier installation has been removed and nothing has replaced it"
    say "the earlier installation is gone; installing Gniza"
fi

# --- where WHM keeps plugin CGIs -------------------------------------------
# Sources disagree about this path and it has moved between versions, so it
# is probed rather than assumed.
CGI_DIR=""
for candidate in \
    /usr/local/cpanel/whostmgr/docroot/cgi \
    /usr/local/cpanel/cgi
do
    if [ -d "$candidate" ]; then CGI_DIR=$candidate; break; fi
done
[ -n "$CGI_DIR" ] || die "could not find WHM's plugin cgi directory; is this WHM?"
say "WHM plugin directory: $CGI_DIR"

# --- restic ----------------------------------------------------------------
# Gniza drives restic; it does not carry a copy of it. Telling an
# administrator to go and fetch one is a step they have to get right on a
# machine that is not yet doing backups, so this installs the version Gniza
# is built against -- checked against restic's own published checksum before
# anything becomes executable, because a backup program that installs an
# unverified binary as root is not a backup program worth having.
RESTIC_VERSION=0.19.1
case "$(uname -m)" in
    x86_64)  RESTIC_ARCH=amd64 ;;
    aarch64) RESTIC_ARCH=arm64 ;;
    *)       RESTIC_ARCH="" ;;
esac

install_restic() {
    [ -n "$RESTIC_ARCH" ] || die "there is no restic build for $(uname -m); install restic yourself and run this again"
    for tool in curl bunzip2 sha256sum; do
        command -v "$tool" >/dev/null 2>&1 || die "$tool is needed to install restic; install it and run this again"
    done

    restic_base=https://github.com/restic/restic/releases/download/v$RESTIC_VERSION
    restic_file=restic_${RESTIC_VERSION}_linux_${RESTIC_ARCH}.bz2
    RESTIC_TMP=$(mktemp -d /var/tmp/gniza-restic.XXXXXX)
    trap 'if [ -n "${RESTIC_TMP:-}" ]; then rm -rf -- "$RESTIC_TMP"; fi' 0 1 2 15

    say "downloading restic $RESTIC_VERSION ($RESTIC_ARCH)"
    curl -fsSL -o "$RESTIC_TMP/$restic_file" "$restic_base/$restic_file" \
        || die "could not download $restic_base/$restic_file; install restic yourself and run this again"
    curl -fsSL -o "$RESTIC_TMP/SHA256SUMS" "$restic_base/SHA256SUMS" \
        || die "could not download restic's checksums from $restic_base; install restic yourself and run this again"

    # Only the one line for the file actually downloaded: the rest of that
    # file names builds for platforms this machine does not have.
    grep " $restic_file\$" "$RESTIC_TMP/SHA256SUMS" > "$RESTIC_TMP/expected" \
        || die "restic's checksum file does not mention $restic_file"
    ( cd "$RESTIC_TMP" && sha256sum -c expected ) >/dev/null \
        || die "the restic download does not match its published checksum; nothing was installed"

    bunzip2 -c "$RESTIC_TMP/$restic_file" > "$RESTIC_TMP/restic" || die "could not unpack restic"
    install -m 0755 "$RESTIC_TMP/restic" "$PREFIX/restic"
    rm -rf -- "$RESTIC_TMP"
    RESTIC_TMP=""
    trap - 0 1 2 15
    say "installed $PREFIX/restic"
}

if ! command -v restic >/dev/null 2>&1 && [ ! -x /usr/local/bin/restic ]; then
    install_restic
fi
say "restic: $(restic version 2>/dev/null | head -1)"

# --- files -----------------------------------------------------------------
install -d -m 0700 "$CONFIG_DIR" "$STATE_DIR" "$STAGING_DIR" "$CACHE_DIR" "$RUN_DIR" \
	"$HOOK_SPOOL_DIR"
install -d -m 0755 "$APPCONFIG_DIR"
install -m 0755 "$SOURCE_DIR/gniza-agent" "$PREFIX/gniza-agent"
HOOK_BIN=/usr/local/cpanel/3rdparty/bin/gniza-hook
install -m 0755 "$SOURCE_DIR/gniza-agent" "$HOOK_BIN"
install -m 0755 "$SOURCE_DIR/gniza.cgi" "$CGI_DIR/gniza.cgi"
say "installed $PREFIX/gniza-agent and $CGI_DIR/gniza.cgi"

# Keep the way out on the server. Installing from a release unpacks the
# package into a temporary directory and removes it again, so the uninstaller
# would otherwise exist only inside a tarball somebody has to find and
# download a second time. It needs cPanel's plugin descriptor and the icon
# beside it -- without those it removes everything except the account-facing
# tile, and does so without complaining. Modes are explicit because this
# script runs under umask 077 and these are not secret.
install -d -m 0755 "$SHARE_DIR" "$SHARE_DIR/cpanel" "$SHARE_DIR/branding"
install -m 0755 "$SOURCE_DIR/uninstall.sh" "$SHARE_DIR/uninstall.sh"
install -m 0644 "$SOURCE_DIR/cpanel/install.json" "$SHARE_DIR/cpanel/install.json"
install -m 0644 "$SOURCE_DIR/branding/badge.svg" "$SHARE_DIR/branding/badge.svg"
say "installed $SHARE_DIR/uninstall.sh"

# LivePHP intentionally does not expose the browser's cpsession cookie. The
# UAPI module enters cPanel's authenticated engine; its root-owned AdminBin
# counterpart restores the matching server-side session and exchanges that
# opaque proof with Gniza over the root-only admin socket. An account process
# calling the module outside a live cPanel session has no session to restore.
CPANEL_UAPI_DIR=/usr/local/cpanel/Cpanel/API
CPANEL_ADMIN_DIR=/var/cpanel/perl/Cpanel/Admin/Modules/Gniza
install -d -o root -g root -m 0755 "$CPANEL_ADMIN_DIR"
install -o root -g root -m 0644 "$SOURCE_DIR/cpanel/uapi/Gniza.pm" \
    "$CPANEL_UAPI_DIR/Gniza.pm"
install -o root -g root -m 0700 "$SOURCE_DIR/cpanel/admin/Gniza/Session.pm" \
    "$CPANEL_ADMIN_DIR/Session.pm"
say "installed cPanel UAPI/AdminBin session bridge"

# cPanel account lifecycle integration. Standardized Hooks are the
# supported registry; do not edit /var/cpanel/hooks/data directly. Remove
# the old manual post-hook registrations first when upgrading from a release
# before --describe support, then let the executable register all current
# descriptors. cPanel requires descriptor registration for a blocking hook.
MANAGE_HOOKS=/usr/local/cpanel/bin/manage_hooks
[ -x "$MANAGE_HOOKS" ] || die "cPanel's manage_hooks utility is missing"
for hook in "Accounts::Create:create" "Accounts::Modify:modify" "Accounts::Remove:remove"; do
    event=${hook%:*}
    action=${hook#*:}
    "$MANAGE_HOOKS" delete script "$HOOK_BIN" --manual \
        --category Whostmgr --event "$event" --stage post \
        --action="--cpanel-hook=$action" >/dev/null 2>&1 || true
done
"$MANAGE_HOOKS" delete script "$HOOK_BIN" >/dev/null 2>&1 || true
"$MANAGE_HOOKS" add script "$HOOK_BIN"
say "registered cPanel account lifecycle hooks"

if [ -f "$SOURCE_DIR/gniza.png" ]; then
    install -m 0644 "$SOURCE_DIR/gniza.png" "$CGI_DIR/gniza.png" || true
fi

# --- service ---------------------------------------------------------------
cat > "$SERVICE" <<'UNIT'
[Unit]
Description=Gniza backups
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
# pkgacct and reading every account's home directory both need root.
#
# HOME matters more than it looks: cPanel keeps root's MySQL credentials in
# /root/.my.cnf, and systemd does not set HOME by default. Without it every
# "SHOW DATABASES" fails with "Access denied", and an account's databases
# would be missing from its backup.
Environment=HOME=/root
UMask=0077
ExecStart=/usr/local/bin/gniza-agent -standalone
Restart=always
RestartSec=15
RuntimeDirectory=Gniza
RuntimeDirectoryMode=0700
NoNewPrivileges=true
ProtectKernelTunables=true
ProtectControlGroups=true

[Install]
WantedBy=multi-user.target
UNIT
chown root:root "$SERVICE"
chmod 0644 "$SERVICE"

systemctl daemon-reload
systemctl enable gniza >/dev/null 2>&1 || true
systemctl restart gniza
say "service Gniza started"

# --- WHM registration ------------------------------------------------------
# Both "acls" and "entryurl" matter here, and getting either wrong fails
# silently: WHM accepts the registration and simply never shows a menu item.
#
#   acls     is required, and must name a real ACL. cPanel documents "all"
#            as its root-level ACL. The CGI independently calls hasroot on
#            every request, so menu registration is not the trust boundary.
#   entryurl is what WHM actually links to from the Plugins section, and is
#            relative to /usr/local/cpanel/whostmgr/docroot/cgi/.
cat > "$APPCONFIG_DIR/gniza.conf" <<'APPCONFIG'
name=Gniza
service=whostmgr
url=/cgi/gniza.cgi
entryurl=gniza.cgi
user=root
acls=all
displayname=Gniza Backups
icon=gniza.svg
searchtext=backup restore restic
target=_self
APPCONFIG
chown root:root "$APPCONFIG_DIR/gniza.conf"
chmod 0644 "$APPCONFIG_DIR/gniza.conf"

# The icon has to be in place before the registration that names it.
ICON_DIR=/usr/local/cpanel/whostmgr/docroot/addon_plugins
if [ -d "$ICON_DIR" ] && [ -f "$SOURCE_DIR/branding/badge.svg" ]; then
    install -m 644 "$SOURCE_DIR/branding/badge.svg" "$ICON_DIR/gniza.svg"
fi

/usr/local/cpanel/bin/register_appconfig "$APPCONFIG_DIR/gniza.conf"

# Confirm WHM kept what we sent. It accepts a registration with an invalid
# ACL by discarding the ACL, which leaves a plugin that exists and is
# invisible — so check rather than trust.
CPANEL_PERL=/usr/local/cpanel/3rdparty/bin/perl
[ -x "$CPANEL_PERL" ] || die "cPanel's bundled perl is missing"
if ! /usr/local/cpanel/bin/whmapi1 --output=json get_appconfig_application_list 2>/dev/null |
    "$CPANEL_PERL" -MJSON::PP -e '
        local $/;
        my $response = decode_json(<STDIN>);
        for my $app (@{$response->{data}{whostmgr} || []}) {
            next unless (($app->{name} || "") eq "gniza");
            my %acls = map { $_ => 1 } @{$app->{acls} || []};
            exit((($app->{entryurl} || "") eq "gniza.cgi" && $acls{all}) ? 0 : 2);
        }
        exit 3;
    '
then
    cat >&2 <<PROBLEM

warning: WHM did not retain Gniza's entry URL and root-level ACL.

The plugin will not appear in the WHM menu. Check the output of:

  /usr/local/cpanel/bin/whmapi1 get_appconfig_application_list

PROBLEM
fi

# ---------------------------------------------------------------------------
# The account-facing plugin.
#
# cPanel runs these as the account they belong to, which is the whole
# point: the service reads who is asking from the socket rather than from
# anything the page says. Installed by copying, because a plugin that is
# a handful of PHP files and one menu entry does not need a tarball and a
# script that unpacks it.
FRONTEND=/usr/local/cpanel/base/frontend/jupiter
if [ -d "$FRONTEND" ]; then
    install -d -m 755 "$FRONTEND/gniza"
    for page in index.live.php browse.live.php logs.live.php restore.live.php download.live.php proxy.php; do
        if [ -f "$SOURCE_DIR/cpanel/$page" ]; then
            install -m 644 "$SOURCE_DIR/cpanel/$page" "$FRONTEND/gniza/$page"
        fi
    done

    # cPanel draws a plugin's tile from assets/application_icons/<file>.png,
    # where <file> is what the menu entry below calls itself.
    if [ -d "$FRONTEND/assets/application_icons" ] &&
       [ -f "$SOURCE_DIR/branding/badge-48.png" ]; then
        install -m 644 "$SOURCE_DIR/branding/badge-48.png" \
            "$FRONTEND/assets/application_icons/gniza.png"
        # Jupiter draws the tile from a sprite sheet, not from that file:
        # the page asks for .icon-gniza, a background-position into
        # icon_spritemap.png. Replacing the icon without rebuilding the
        # sheet changes nothing an account can see, and the sheet's URL
        # carries a content hash, so a stale one is cached hard.
        if [ -x /usr/local/cpanel/bin/sprite_generator ]; then
            /usr/local/cpanel/bin/sprite_generator \
                --application=cpanel --theme=jupiter >/dev/null 2>&1 || true
        fi
    fi

    # Register through cPanel's supported plugin installer. Besides keeping
    # the DynamicUI cache consistent, install.json registers "gniza" with
    # Feature Manager, so a host can withhold backup access from a package
    # instead of every account receiving it unconditionally.
    [ -x /usr/local/cpanel/scripts/install_plugin ] || \
        die "/usr/local/cpanel/scripts/install_plugin is missing"
    PLUGIN_META=$(mktemp -d /var/tmp/gniza-cpanel.XXXXXX)
    trap 'if [ -n "${PLUGIN_META:-}" ]; then rm -rf -- "$PLUGIN_META"; fi' 0 1 2 15
    install -m 0644 "$SOURCE_DIR/cpanel/install.json" "$PLUGIN_META/install.json"
    install -m 0644 "$SOURCE_DIR/branding/badge-48.png" "$PLUGIN_META/gniza.png"
    # Remove the entry written by older releases; if left behind it can
    # override the Feature Manager-aware entry generated below.
    DYNAMICUI="$FRONTEND/dynamicui/dynamicui_gniza.conf"
    rm -f "$DYNAMICUI"
    # Jupiter prefers an SVG over a PNG with the same application id. cPanel's
    # SVG installer rewrites geometry attributes in custom artwork, so remove
    # the generated SVG left by older releases and register the exact 48px PNG.
    rm -f "$FRONTEND/assets/application_icons/gniza.svg"
    # This command creates only public plugin metadata. Keep the restrictive
    # process-wide umask for service state and credentials, but do not pass it
    # into cPanel's registration writer.
    ( umask 022; /usr/local/cpanel/scripts/install_plugin "$PLUGIN_META" --theme=jupiter )
    [ -f "$DYNAMICUI" ] || die "cPanel did not generate $DYNAMICUI"
    # install_plugin inherits this script's security-first umask (077), but
    # Jupiter reads DynamicUI records while building an account's application
    # list. A root-only record is silently omitted even when Feature Manager
    # enables Gniza for the account. Plugin metadata is public, like every
    # other record in this directory, so make the generated file readable.
    chown root:root "$DYNAMICUI"
    chmod 0644 "$DYNAMICUI"
    # Permission changes update ctime, while cPanel invalidates its parsed
    # per-account application cache using this record's mtime.
    touch "$DYNAMICUI"
    rm -rf -- "$PLUGIN_META"
    PLUGIN_META=
    trap - 0 1 2 15
    echo "Installed the account-facing plugin in $FRONTEND/gniza."
else
    echo "warning: no jupiter theme at $FRONTEND; the account-facing plugin was not installed." >&2
fi

cat <<DONE

Gniza is installed.

  Open WHM and look for "Gniza Backups" in the plugins section.

Registered as:
$(sed 's/^/  /' "$APPCONFIG_DIR/gniza.conf")

It is registered against cPanel's root-level "all" ACL. The plugin also
checks that ACL itself on every request. Please still confirm in WHM that
only administrators with root-level privileges can see it: this plugin can
read and delete every backup on the server.

If it does not appear, look in WHM under Development > Apps Managed by
AppConfig. The Manage Plugins page lists cPanel RPM addons and will not
show it.

Next: add a backup destination in the plugin, then a schedule.

To remove it again: sh /usr/local/share/gniza/uninstall.sh
DONE

if [ "${MIGRATED_FROM_CPREST:-0}" = 1 ]; then
    cat <<'RENAMED'

This server was running cprest, and the rename changed names cPanel keeps
its own records of. Two of them need looking at:

  Feature Manager  the account-facing plugin is now the "gniza" feature,
                   not "cprest". A package or feature list that named the
                   old one has to be edited to name the new one, or those
                   accounts will not see the Backups tile.

  Destinations     the private key each SFTP destination connects with
                   moved from /etc/cprest to /etc/gniza. The service
                   rewrote that in its own records on this start; press
                   Test on each destination to confirm it.

Your destinations, schedules, history and master key were kept.
RENAMED
fi
