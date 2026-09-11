# A server without a panel

Gniza runs on a server that has no cPanel and no DirectAdmin: a LAMP box,
a docker or podman host. The same one-liner installs it; `get.sh` finds
neither panel directory and takes the plain package (ADR 0022).

```bash
curl -fsSL https://github.com/shukiv/gniza/releases/latest/download/get.sh | sh
```

## What an account is here

Every directory directly under one of the roots is an account, named
after the directory. The roots default to `/var/www`, `/srv` and `/opt`;
a root that is not there is skipped. To change them, edit
`/etc/gniza/plain.env` and restart the service:

```
GNIZA_PLAIN_ROOTS=/var/www,/home/deploy/sites
```

```bash
systemctl restart gniza
```

A MySQL database named after the account, alone or with an underscore
and a suffix (`shop`, `shop_wp`), is backed up with it. A database named
differently is not, and the record beside the dumps in every backup
says which databases were taken.

The files are read where they lie; only the dumps and that record are
written to staging. The system backup, when a schedule includes it,
takes the web, PHP, database, container, cron and SSH configuration, the
certificates and the unit files, with a list of the installed packages
and of the containers and volumes present.

## Until the terminal interface ships

There is no page to open on a server with no panel, and the terminal
interface is not written yet. The service listens on a unix socket, and
the same forms the plugins post are posted with `curl`. Every request
carries the CSRF token from any page:

```bash
S=/var/run/gniza/admin/ui.sock
CSRF=$(curl -s --unix-socket $S http://x/settings | grep -o 'name="csrf" value="[^"]*"' | head -1 | sed 's/.*value="//; s/"//')
```

A destination on a mounted disk:

```bash
curl -s --unix-socket $S -X POST http://x/destinations/add \
  -d "csrf=$CSRF" -d "name=usb" -d "type=local" -d "root=/mnt/backups/gniza"
```

The destination's recovery key exists only on this server until it is
shown and kept somewhere else. Show it, and keep it:

```bash
curl -s --unix-socket $S http://x/destinations | sed 's/<[^>]*>/ /g' | grep -A3 -i "recovery"
```

A nightly schedule of every account, seven daily, four weekly and six
monthly backups kept:

```bash
curl -s --unix-socket $S -X POST http://x/schedule/save \
  -d "csrf=$CSRF" -d "name=Nightly" -d "cron=0 2 * * *" -d "mode=split" \
  -d "enabled=1" -d "include_system=1" \
  -d "keep_daily=7" -d "keep_weekly=4" -d "keep_monthly=6"
```

A backup of one account now, and the history afterwards:

```bash
curl -s --unix-socket $S -X POST http://x/accounts/backup -d "csrf=$CSRF" -d "account=shop"
curl -s --unix-socket $S "http://x/logs?tab=backups" | sed 's/<[^>]*>/ /g' | tr -s ' \n' ' ' | cut -c1-2000
```

Any page reads the same way: `http://x/`, `http://x/accounts`,
`http://x/destinations`, `http://x/schedule`, `http://x/settings`.

## Restore

There is no native restore to hand an archive to. What a plain server
restores is files and databases: from the Restore page's item restores,
the website files go back over the account's directory (a file added
since the backup stays), and a database dump is loaded into a database
the account owns by name, created first if it is gone. A whole-account
restore is refused as unverified, on purpose. None of this has been
proved on a live plain server yet; the drill is bead `cprest-44s`.

## Remove

```bash
sh /usr/local/share/gniza/uninstall.sh
```

What was installed is moved under `/var/lib/gniza/removed/`, and the
backups, the state database and `/etc/gniza` stay.
