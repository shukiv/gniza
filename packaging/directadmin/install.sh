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

# set_colours decides whether this run may use terminal escapes.
#
# Only when the output is a terminal: piped into a file or a log, escapes
# are noise an operator has to read around. NO_COLOR is honoured because
# it is the convention an operator sets once and expects obeyed.
set_colours() {
	if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
		WARN_ON=$(printf '\033[1;33m')
		WARN_OFF=$(printf '\033[0m')
	else
		WARN_ON=
		WARN_OFF=
	fi
}

# warn_block prints the one thing an operator must not scroll past.
#
# It is the last thing said after a wall of installer output, and it was
# said in the same voice as everything above it -- so it read as another
# line of progress. A rule and a colour are what make it the thing the
# eye stops on.
warn_block() {
	_wb_heading=$1
	shift
	_wb_rule="------------------------------------------------------------------------"
	printf '\n%s%s%s\n' "$WARN_ON" "$_wb_rule" "$WARN_OFF"
	printf '%s  %s%s\n' "$WARN_ON" "$_wb_heading" "$WARN_OFF"
	printf '%s%s%s\n' "$WARN_ON" "$_wb_rule" "$WARN_OFF"
	for _wb_line in "$@"; do
		printf '  %s\n' "$_wb_line"
	done
	printf '%s%s%s\n' "$WARN_ON" "$_wb_rule" "$WARN_OFF"
}

# install_binary puts an executable in place atomically.
#
# install(1) writes over the destination where it stands, so for the
# length of the copy whoever reads it sees a file that is there and not
# yet complete. On 2026-09-09 a live server restarted into a
# /usr/local/bin/restic that existed and was not yet executable, and
# crash-looped every five seconds for three and a half minutes until the
# download finished. A rename inside the same directory is atomic: a
# reader sees the old file or the new one and never half of either, and
# a binary that is running is replaced rather than refused with ETXTBSY.
install_binary() {
	_ib_mode=$1
	_ib_source=$2
	_ib_target=$3
	_ib_temp="$(dirname "$_ib_target")/.gniza-incoming.$$.$(basename "$_ib_target")"
	install -m "$_ib_mode" "$_ib_source" "$_ib_temp" \
		|| die "could not write $_ib_temp"
	mv -f "$_ib_temp" "$_ib_target" || {
		rm -f "$_ib_temp"
		die "could not put $_ib_target in place"
	}
}

[ "$(id -u)" = 0 ] || die "run this as root"
[ -d /usr/local/directadmin ] || die "this does not look like a DirectAdmin server (/usr/local/directadmin is missing)"
umask 077

SOURCE_DIR=$(cd "$(dirname "$0")" && pwd)
[ -f "$SOURCE_DIR/gniza-agent" ] || die "gniza-agent is not next to this script"

# Two ways in, and they arrive in different shapes.
#
# The release tarball unpacks anywhere and holds the agent beside a
# directadmin/ tree to install from. DirectAdmin's own plugin manager
# unpacks the same files straight into the plugin directory and runs this
# from inside them, so there is nothing to copy: the files to install are
# the ones underfoot, and copying them over themselves is an error.
if [ -f "$SOURCE_DIR/plugin.conf" ]; then
	IN_PLACE=1
	PLUGIN_FILES=$SOURCE_DIR
	[ "$SOURCE_DIR" = "$PLUGIN_DIR" ] ||
		die "this looks like a plugin directory but is not $PLUGIN_DIR; move it there or run the installer from the release tarball"
else
	IN_PLACE=0
	PLUGIN_FILES=$SOURCE_DIR/directadmin
	[ -d "$PLUGIN_FILES" ] || die "the DirectAdmin plugin files are missing from the package"
fi

for dir in "$CONFIG_DIR" "$STATE_DIR" "$STAGING_DIR" "$HOOK_SPOOL_DIR" "$CACHE_DIR" "$RUN_DIR"; do
	mkdir -p "$dir"
	chmod 0700 "$dir"
done

# Gniza drives restic; it does not carry a copy of it. Telling an
# administrator to go and fetch one is a step they have to get right on a
# machine that is not yet doing backups, so this installs the version
# Gniza is built against -- checked against restic's own published
# checksum before anything becomes executable, because a backup program
# that installs an unverified binary as root is not a backup program
# worth having.
RESTIC_VERSION=0.19.1
case "$(uname -m)" in
	x86_64)  RESTIC_ARCH=amd64 ;;
	aarch64) RESTIC_ARCH=arm64 ;;
	*)       RESTIC_ARCH="" ;;
esac

# install_hint names the package a missing tool comes in, and the command
# this machine installs it with. An operator told "bunzip2 is needed" has
# to work back from a command to a package; this says the package.
install_hint() {
	if command -v dnf >/dev/null 2>&1; then
		echo "dnf install -y $1"
	elif command -v yum >/dev/null 2>&1; then
		echo "yum install -y $1"
	elif command -v apt-get >/dev/null 2>&1; then
		echo "apt-get install -y $1"
	else
		echo "install the $1 package"
	fi
}

# unpack_bz2 reads restic's download, which is published as a .bz2 and
# nothing else.
#
# bunzip2 is the usual way and is not always installed: a DirectAdmin
# server stopped here on 2026-09-10 with everything else in place.
# python3 is on every panel server -- DirectAdmin and cPanel both need one
# -- and its standard library reads bz2, so it is asked before the
# installer gives up.
unpack_bz2() {
	if command -v bunzip2 >/dev/null 2>&1; then
		bunzip2 -c "$1" > "$2"
	elif command -v bzip2 >/dev/null 2>&1; then
		bzip2 -dc "$1" > "$2"
	elif command -v python3 >/dev/null 2>&1; then
		python3 -c 'import bz2, shutil, sys
with bz2.BZ2File(sys.argv[1]) as packed, open(sys.argv[2], "wb") as plain:
    shutil.copyfileobj(packed, plain)' "$1" "$2"
	else
		die "nothing here can read a .bz2, which is the only way restic is published: $(install_hint bzip2), or install restic yourself, and run this again"
	fi
}

install_restic() {
	[ -n "$RESTIC_ARCH" ] || die "there is no restic build for $(uname -m); install restic yourself and run this again"
	for tool in curl sha256sum; do
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

	# Only the one line for the file actually downloaded: the rest of
	# that file names builds for platforms this machine does not have.
	grep " $restic_file\$" "$RESTIC_TMP/SHA256SUMS" > "$RESTIC_TMP/expected" \
		|| die "restic's checksum file does not mention $restic_file"
	( cd "$RESTIC_TMP" && sha256sum -c expected ) >/dev/null \
		|| die "the restic download does not match its published checksum; nothing was installed"

	unpack_bz2 "$RESTIC_TMP/$restic_file" "$RESTIC_TMP/restic" || die "could not unpack restic"
	install_binary 0755 "$RESTIC_TMP/restic" "$PREFIX/restic"
	rm -rf -- "$RESTIC_TMP"
	RESTIC_TMP=""
	trap - 0 1 2 15
	say "installed $PREFIX/restic"
}

if ! command -v restic >/dev/null 2>&1 && [ ! -x /usr/local/bin/restic ]; then
	install_restic
fi
say "restic: $(restic version 2>/dev/null | head -1)"

install_binary 0755 "$SOURCE_DIR/gniza-agent" "$PREFIX/gniza-agent"
# The hook is the same program under the name the hook scripts call. It
# is a copy rather than a symlink for the same reason it is on cPanel:
# the two must never drift, and a copy that did is visible as a different
# checksum.
install_binary 0755 "$SOURCE_DIR/gniza-agent" "$PREFIX/gniza-hook"

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

# Explicit modes because this script runs under umask 077 and nothing
# under here is secret. A directory left at 0700 root:root is a plugin
# DirectAdmin installs without complaining and then answers 404 for: the
# page is there, and neither DirectAdmin nor the account it runs the page
# as can traverse to it.
install -d -m 0755 "$PLUGIN_DIR" "$PLUGIN_DIR/hooks" "$PLUGIN_DIR/admin" \
	"$PLUGIN_DIR/user" "$PLUGIN_DIR/images"

if [ "$IN_PLACE" = 1 ]; then
	# Already unpacked here by the plugin manager. Nothing to copy, only
	# the modes to settle: an archive carries whatever modes it was made
	# with, and a directory DirectAdmin cannot traverse is a plugin it
	# installs without complaining and then answers 404 for.
	#
	# Five digits, because coreutils 8.30 -- which is what these servers
	# have -- keeps a directory's set-group-ID bit through any octal mode
	# of four digits or fewer, "chmod 0755" included; only a mode that
	# names the bit clears it. The archive is made on somebody's
	# checkout, and a checkout on a group-shared directory has that bit
	# on every directory in it, so "chmod 0755" would carry it onto the
	# server and every file written under those directories would take
	# the directory's group rather than the writer's.
	find "$PLUGIN_DIR" -type d -exec chmod 00755 {} +
	find "$PLUGIN_DIR" -type f -exec chmod 00644 {} +
	for script in "$PLUGIN_DIR"/hooks/*.sh "$PLUGIN_DIR"/scripts/*.sh \
		"$PLUGIN_DIR/install.sh" "$PLUGIN_DIR/uninstall.sh" \
		"$PLUGIN_DIR/admin/index.html" "$PLUGIN_DIR/user/index.html"; do
		[ -f "$script" ] || continue
		chmod 0755 "$script"
	done
	say "the plugin files were already in place"
else
	install -m 0644 "$PLUGIN_FILES/plugin.conf" "$PLUGIN_DIR/plugin.conf"
	for hook in "$PLUGIN_FILES"/hooks/*.sh; do
		install_binary 0755 "$hook" "$PLUGIN_DIR/hooks/$(basename "$hook")"
	done
	# The menu entries. A plugin with no *_txt.html has no way in: the
	# directory is installed and the page is never linked to. The *_img.html
	# beside it is the tile Evolution actually draws, with the icon on it, so
	# both are installed -- a glob that took only the first left the plugin
	# listed as bare words and reachable only by typing its address.
	for entry in "$PLUGIN_FILES"/hooks/*.html; do
		[ -f "$entry" ] || continue
		install -m 0644 "$entry" "$PLUGIN_DIR/hooks/$(basename "$entry")"
	done
	# The icon the menu tile shows. DirectAdmin serves it from the plugin's
	# own images directory, which is the address admin_img.html points at.
	for image in "$PLUGIN_FILES"/images/*; do
		[ -f "$image" ] || continue
		install -m 0644 "$image" "$PLUGIN_DIR/images/$(basename "$image")"
	done
	# The typefaces, as files. Everything the plugin script prints is wrapped
	# in DirectAdmin's skin, so a font fetched through it is HTML with a woff2
	# inside; the images directory is served as files, which is where every
	# other plugin on a DirectAdmin server keeps its own web fonts.
	if [ -d "$PLUGIN_FILES/images/fonts" ]; then
		install -d -m 0755 "$PLUGIN_DIR/images/fonts"
		for font in "$PLUGIN_FILES"/images/fonts/*; do
			[ -f "$font" ] || continue
			install -m 0644 "$font" "$PLUGIN_DIR/images/fonts/$(basename "$font")"
		done
	fi
	install_binary 0755 "$PLUGIN_FILES/admin/index.html" "$PLUGIN_DIR/admin/index.html"
	install_binary 0755 "$PLUGIN_FILES/user/index.html" "$PLUGIN_DIR/user/index.html"
	# What DirectAdmin's plugin manager runs to update or remove the
	# plugin. Installed by hand as well as by the manager, so that a
	# server set up from the release tarball can still be updated and
	# removed from the page an administrator expects to do it from.
	if [ -d "$PLUGIN_FILES/scripts" ]; then
		install -d -m 0755 "$PLUGIN_DIR/scripts"
		for script in "$PLUGIN_FILES"/scripts/*.sh; do
			[ -f "$script" ] || continue
			install_binary 0755 "$script" "$PLUGIN_DIR/scripts/$(basename "$script")"
		done
		install_binary 0755 "$SOURCE_DIR/install.sh" "$PLUGIN_DIR/install.sh"
		install_binary 0755 "$SOURCE_DIR/uninstall.sh" "$PLUGIN_DIR/uninstall.sh"
		install_binary 0755 "$SOURCE_DIR/gniza-agent" "$PLUGIN_DIR/gniza-agent"
	fi
fi

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

# The wording is dabackup.Provisional, which the agent prints too. Say it
# differently here and the two drift: this installer told operators the
# provider "backs up a whole account as one archive" for every release
# after split mode landed. A test compares the two.
set_colours
warn_block "The DirectAdmin provider is unfinished" \
	"DirectAdmin archives were validated on 1.709 and split mode" \
	"rebuilds one header for header, but no account has yet been" \
	"restored from an archive Gniza rebuilt, and granular restore and" \
	"the session bridge remain experimental: see ADR 0019"
