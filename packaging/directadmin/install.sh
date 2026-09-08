#!/bin/sh
# Install Gniza on a DirectAdmin server.
#
# What this installs is the backup service, the account lifecycle hooks,
# and the administrator's page, which reaches the service over a socket
# of its own and is served only to a DirectAdmin administrator's session.
# See ADR 0020. What it does not install is the customer's own page: that
# needs an identity DirectAdmin's account pages cannot yet give, and the
# page shipped in user/ says so where a customer will read it.
#
# Everything here is idempotent: running it again upgrades in place.
set -eu

# This script runs as root. Do not let the invoking shell choose binaries
# from a writable directory.
PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
export PATH

PREFIX=/usr/local/bin
CONFIG_DIR=/etc/gniza
STATE_DIR=/var/lib/gniza
STAGING_DIR=/var/lib/gniza/staging
HOOK_SPOOL_DIR=/var/lib/gniza/hooks
CACHE_DIR=/var/cache/gniza/restic
RUN_DIR=/var/run/gniza
PLUGIN_DIR=/usr/local/directadmin/plugins/gniza
SERVICE=/etc/systemd/system/gniza.service
# The account DirectAdmin runs the administrator's page as, named in
# plugin.conf and matched by gniza-agent's -directadmin-plugin-user. It
# owns the socket that page connects to, and nothing else. See ADR 0020.
PLUGIN_USER=gniza-plugin

say() { printf '%s\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

[ "$(id -u)" = 0 ] || die "run this as root"
[ -d /usr/local/directadmin ] || die "this does not look like a DirectAdmin server (/usr/local/directadmin is missing)"
umask 077

SOURCE_DIR=$(cd "$(dirname "$0")" && pwd)
[ -f "$SOURCE_DIR/gniza-agent" ] || die "gniza-agent is not next to this script"
[ -d "$SOURCE_DIR/directadmin" ] || die "the DirectAdmin plugin files are missing from the package"

for dir in "$CONFIG_DIR" "$STATE_DIR" "$STAGING_DIR" "$HOOK_SPOOL_DIR" "$CACHE_DIR" "$RUN_DIR"; do
	mkdir -p "$dir"
	chmod 0700 "$dir"
done

install -m 0755 "$SOURCE_DIR/gniza-agent" "$PREFIX/gniza-agent"
# The hook is the same program under the name the hook scripts call. It
# is a copy rather than a symlink for the same reason it is on cPanel:
# the two must never drift, and a copy that did is visible as a different
# checksum.
install -m 0755 "$SOURCE_DIR/gniza-agent" "$PREFIX/gniza-hook"

# DirectAdmin will not run a plugin as root, so the administrator's page
# runs as this account instead. It has no home, no shell and no password:
# nobody logs into it, and what it can reach is one socket.
if ! id -u "$PLUGIN_USER" >/dev/null 2>&1; then
	nologin=/sbin/nologin
	[ -x "$nologin" ] || nologin=/usr/sbin/nologin
	[ -x "$nologin" ] || nologin=/bin/false
	useradd --system --no-create-home --home-dir /nonexistent \
		--shell "$nologin" "$PLUGIN_USER" ||
		die "could not create the $PLUGIN_USER account, which DirectAdmin runs Gniza's page as"
fi
# An account that resolves to root would make that page root's, which is
# the arrangement this exists to avoid -- and DirectAdmin refuses it in
# plugin.conf anyway.
[ "$(id -u "$PLUGIN_USER")" != 0 ] ||
	die "$PLUGIN_USER is uid 0; DirectAdmin will not run a plugin as root and neither will Gniza"

mkdir -p "$PLUGIN_DIR/hooks" "$PLUGIN_DIR/admin" "$PLUGIN_DIR/user"
install -m 0644 "$SOURCE_DIR/directadmin/plugin.conf" "$PLUGIN_DIR/plugin.conf"
for hook in "$SOURCE_DIR"/directadmin/hooks/*.sh; do
	install -m 0755 "$hook" "$PLUGIN_DIR/hooks/$(basename "$hook")"
done
# The menu entry. A plugin with no *_txt.html has no way in: the
# directory is installed and the page is never linked to.
for text in "$SOURCE_DIR"/directadmin/hooks/*_txt.html; do
	[ -f "$text" ] || continue
	install -m 0644 "$text" "$PLUGIN_DIR/hooks/$(basename "$text")"
done
install -m 0755 "$SOURCE_DIR/directadmin/admin/index.html" "$PLUGIN_DIR/admin/index.html"
install -m 0755 "$SOURCE_DIR/directadmin/user/index.html" "$PLUGIN_DIR/user/index.html"

if [ ! -f "$SERVICE" ]; then
	cat > "$SERVICE" <<'UNIT'
[Unit]
Description=Gniza backup service
After=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/gniza-agent -standalone -panel=directadmin
Restart=always
RestartSec=5
UMask=0077

[Install]
WantedBy=multi-user.target
UNIT
	chmod 0644 "$SERVICE"
fi

if command -v systemctl >/dev/null 2>&1; then
	systemctl daemon-reload
	systemctl enable gniza >/dev/null 2>&1 || true
	systemctl restart gniza
fi

say "Gniza is installed."
say ""
say "The administrator's page is under Admin Tools, and runs as"
say "$PLUGIN_USER. Gniza asks DirectAdmin whose session opened it and"
say "serves an administrator's session only."
say ""
say "The DirectAdmin provider is unfinished: it backs up a whole account"
say "as one archive and refuses the things that need an answer only a"
say "running DirectAdmin can give. Read ADR 0019 before relying on it."
