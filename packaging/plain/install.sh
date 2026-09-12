#!/bin/sh
# Install Gniza on a server that has no control panel: a LAMP server, a
# docker or podman host. There is no plugin to install, because there is
# nothing to show one. What goes in is the service, its unit, and the
# scripts that put them in place and take them off again. The service is
# worked from the terminal interface over its socket (ADR 0022), or from a
# browser on the address this script asks about, behind a password
# (ADR 0024).
#
# Run as root from the unpacked release directory. Safe to run again: it
# replaces the binary and restarts the service.
set -eu

PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
export PATH

PREFIX=/usr/local/bin
SHARE_DIR=/usr/local/share/gniza
CONFIG_DIR=/etc/gniza
STATE_DIR=/var/lib/gniza
STAGING_DIR=/var/lib/gniza/staging
CACHE_DIR=/var/cache/gniza/restic
RUN_DIR=/var/run/gniza
SERVICE=/etc/systemd/system/gniza.service
ENV_FILE=/etc/gniza/plain.env

say() { printf '%s\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

# The warning at the end stands out from the lines above it, because it is
# the one thing here that somebody setting a server up has to read.
if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
	WARN_ON=$(printf '\033[1;33m')
	WARN_OFF=$(printf '\033[0m')
else
	WARN_ON=""
	WARN_OFF=""
fi
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

# An executable arrives by rename inside its own directory, so whoever
# reads it sees the old file or the new one and never half of either. A
# live server once restarted into a restic that existed and was not yet
# complete, and crash-looped until the download finished.
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
if [ -d /usr/local/cpanel ] || [ -d /usr/local/directadmin ]; then
	die "this server has a control panel; install the package for it instead (curl -fsSL https://github.com/shukiv/gniza/releases/latest/download/get.sh | sh)"
fi
umask 077

SOURCE_DIR=$(cd "$(dirname "$0")" && pwd)
[ -f "$SOURCE_DIR/gniza-agent" ] || die "gniza-agent is not next to this script"

for dir in "$CONFIG_DIR" "$STATE_DIR" "$STAGING_DIR" "$CACHE_DIR" "$RUN_DIR"; do
	mkdir -p "$dir"
	chmod 0700 "$dir"
done
install -d -m 0755 "$SHARE_DIR"

# restic, from its own release, checked against its own checksums. A
# server that already has one keeps it.
RESTIC_VERSION=0.19.1
case "$(uname -m)" in
	x86_64)  RESTIC_ARCH=amd64 ;;
	aarch64) RESTIC_ARCH=arm64 ;;
	*)       RESTIC_ARCH="" ;;
esac
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
# The scripts stay on the server, where the Remove button and a later
# install by hand look for them.
install_binary 0755 "$SOURCE_DIR/install.sh" "$SHARE_DIR/install.sh"
install_binary 0755 "$SOURCE_DIR/uninstall.sh" "$SHARE_DIR/uninstall.sh"

# Which directories hold the accounts. Written once, so an operator who
# keeps sites somewhere else edits one file and restarts the service,
# and an upgrade does not put the default back.
if [ ! -f "$ENV_FILE" ]; then
	cat > "$ENV_FILE" <<ENV
# The directories whose subdirectories Gniza backs up as accounts, comma
# separated. A root that is not there is skipped. Change it and run
# "systemctl restart gniza".
GNIZA_PLAIN_ROOTS=${GNIZA_PLAIN_ROOTS:-/var/www,/srv,/opt}
ENV
	chmod 0600 "$ENV_FILE"
fi

# The browser interface. Where it listens is asked once, on the terminal,
# and written to the same file as the roots, so an upgrade keeps the
# answer and the unit never changes: the service reads GNIZA_WEB_LISTEN
# from the environment. GNIZA_WEB_LISTEN set before this script -- even
# empty, for none -- answers without asking. See ADR 0024.
WEB_DIR=$CONFIG_DIR/web
tty_usable() { ( : < /dev/tty ) 2>/dev/null && [ -w /dev/tty ]; }
ask_tty() {
	printf '%s' "$1" > /dev/tty
	IFS= read -r ask_reply < /dev/tty || ask_reply=""
	printf '%s' "$ask_reply"
}
if [ -n "${GNIZA_WEB_LISTEN+set}" ]; then
	WEB_LISTEN=$GNIZA_WEB_LISTEN
elif grep -q '^GNIZA_WEB_LISTEN=' "$ENV_FILE"; then
	WEB_LISTEN=$(sed -n 's/^GNIZA_WEB_LISTEN=//p' "$ENV_FILE" | tail -1)
elif tty_usable; then
	cat > /dev/tty <<'ASK'

Where should the browser interface listen?
  1) this machine only, 127.0.0.1:8443 -- from your own machine run
     ssh -L 8443:127.0.0.1:8443 root@<this server>  and open http://127.0.0.1:8443/
  2) every address, 0.0.0.0:8443 -- reachable from the internet, over TLS,
     behind the password set next; open port 8443 in the firewall
  3) nowhere -- the terminal interface only (gniza-agent -tui)
ASK
	case "$(ask_tty 'Choice [1]: ')" in
		2) WEB_LISTEN=0.0.0.0:8443 ;;
		3) WEB_LISTEN="" ;;
		*) WEB_LISTEN=127.0.0.1:8443 ;;
	esac
else
	WEB_LISTEN=127.0.0.1:8443
	say "no terminal to ask on: the browser interface is set to 127.0.0.1:8443 (GNIZA_WEB_LISTEN in $ENV_FILE changes it)"
fi
if grep -q '^GNIZA_WEB_LISTEN=' "$ENV_FILE"; then
	sed -i "s|^GNIZA_WEB_LISTEN=.*|GNIZA_WEB_LISTEN=$WEB_LISTEN|" "$ENV_FILE"
else
	cat >> "$ENV_FILE" <<ENV
# Where the browser interface listens, host:port, behind the password in
# $WEB_DIR/password. 127.0.0.1:8443 is this machine only, reached over
# ssh -L; 0.0.0.0:8443 is every address, over TLS; empty is none.
GNIZA_WEB_LISTEN=$WEB_LISTEN
ENV
fi

# Its password, set once and kept across upgrades. Typed on the terminal
# without echo, or taken from GNIZA_WEB_PASSWORD. With neither -- an
# unattended install, or the service upgrading itself, which runs this
# script with no terminal -- none is set: the service refuses to open the
# door without one and says so in its log, and the closing block says
# what to run. A password made up here would have to be printed, and what
# this script prints is captured by the updater and kept by journald.
# The hash goes through the agent so there is one way to write it.
WEB_PASSWORD_NOTE=""
if [ -n "$WEB_LISTEN" ] && [ ! -f "$WEB_DIR/password" ]; then
	if [ -n "${GNIZA_WEB_PASSWORD:-}" ]; then
		WEB_PASSWORD=$GNIZA_WEB_PASSWORD
	elif ! tty_usable; then
		WEB_PASSWORD=""
		WEB_PASSWORD_NOTE="The browser interface has no password yet, so it is not listening: run  gniza-agent -web-set-password  as root, then  systemctl restart gniza."
	else
		trap 'stty echo < /dev/tty 2>/dev/null || true' 1 2 15
		while :; do
			stty -echo < /dev/tty 2>/dev/null || true
			WEB_PASSWORD=$(ask_tty 'Password for the browser interface (10 characters or more): ')
			printf '\n' > /dev/tty
			WEB_AGAIN=$(ask_tty 'The same again: ')
			printf '\n' > /dev/tty
			stty echo < /dev/tty 2>/dev/null || true
			if [ "$WEB_PASSWORD" != "$WEB_AGAIN" ]; then
				printf '%s\n' "The two differ; again." > /dev/tty
			elif [ ${#WEB_PASSWORD} -lt 10 ]; then
				printf '%s\n' "Too short; again." > /dev/tty
			else
				break
			fi
		done
		trap - 1 2 15
	fi
	if [ -n "$WEB_PASSWORD" ]; then
		GNIZA_WEB_PASSWORD=$WEB_PASSWORD "$PREFIX/gniza-agent" -web-set-password -web-password-file "$WEB_DIR/password" \
			|| die "could not set the browser interface's password"
	fi
	unset WEB_PASSWORD WEB_AGAIN
fi

if [ ! -f "$SERVICE" ]; then
	cat > "$SERVICE" <<'UNIT'
[Unit]
Description=Gniza backup service
After=network-online.target

[Service]
Type=simple
EnvironmentFile=/etc/gniza/plain.env
ExecStart=/usr/local/bin/gniza-agent -standalone -panel=plain -plain-roots=${GNIZA_PLAIN_ROOTS}
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
say "  accounts: every directory under $(sed -n 's/^GNIZA_PLAIN_ROOTS=//p' "$ENV_FILE") (edit $ENV_FILE to change)"
say "  socket:   $RUN_DIR/admin/ui.sock"
case "$WEB_LISTEN" in
	"") say "  browser:  none (GNIZA_WEB_LISTEN in $ENV_FILE turns it on)" ;;
	127.0.0.1:*|localhost:*)
		say "  browser:  http://$WEB_LISTEN/ from this machine, or from yours:"
		say "            ssh -L ${WEB_LISTEN#*:}:$WEB_LISTEN root@$(hostname -f 2>/dev/null || hostname)" ;;
	*)
		say "  browser:  https://$(hostname -f 2>/dev/null || hostname):${WEB_LISTEN#*:}/"
		say "            its certificate is $WEB_DIR/cert.pem, made on first start; compare the"
		say "            fingerprint the browser shows with:  openssl x509 -in $WEB_DIR/cert.pem -noout -fingerprint -sha256" ;;
esac
[ -z "$WEB_LISTEN" ] || say "  password: gniza-agent -web-set-password  changes it"
say "  remove:   sh $SHARE_DIR/uninstall.sh"

set -- "Run  gniza-agent -tui  as root, or sign in to the browser interface," \
	"to add a destination and a schedule." \
	"Every directory under the roots in /etc/gniza/plain.env is an account," \
	"and a MySQL database named after one is backed up with it." \
	"Write the recovery key down somewhere off this server when it is shown." \
	"A restore of files and databases has not yet been proved on a live" \
	"server (ADR 0022); see docs/guide/plain-server.md."
if [ -n "$WEB_PASSWORD_NOTE" ]; then
	set -- "$@" "$WEB_PASSWORD_NOTE"
fi
case "$WEB_LISTEN" in
	""|127.0.0.1:*|localhost:*) ;;
	*) set -- "$@" "The browser interface answers on every address: open port ${WEB_LISTEN#*:} in" \
		"the firewall, and remember that its password is the only door." ;;
esac
warn_block "This server has no panel: Gniza is set up from the terminal or a browser" "$@"
