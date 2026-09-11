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

## Setting it up from the terminal

There is no page to open on a server with no panel. The service listens
on a unix socket, and the terminal interface reads the same pages the
plugins draw from it:

```bash
gniza-agent -tui
```

Run it as root; the socket is root's. The screens are the plugins'
screens, numbered along the top: 1 Overview, 2 Destinations, 3
Schedules, 4 Accounts, 5 Logs, 6 Restore, 7 Settings. The foot of every
screen says what the keys do there. The first hour is:

1. On Destinations, press `a`. A local disk or mounted share needs a
   name and a directory; another Linux server needs its address, a
   user there and that user's password once, which installs Gniza's
   key and is then discarded. The server's host key is shown before
   anything is sent to it.
2. The recovery key is shown when the destination is ready. Write it
   down somewhere that survives this server, then press `n`. The
   destinations screen warns until that is done, because the disaster
   the backups exist for is also the one that destroys the only copy.
3. On Schedules, press `a`. The defaults are nightly at two, split
   shape, the server's own configuration included, seven daily, four
   weekly and six monthly backups kept, written to every destination.
4. On Accounts, `b` backs up the account under the cursor now, and
   `B` runs the schedule over every account. Logs shows what happened.

The same forms are posted with `curl` from a script. Every request
carries the CSRF token from any page:

```bash
S=/var/run/gniza/admin/ui.sock
CSRF=$(curl -s --unix-socket $S http://x/settings | grep -o 'name="csrf" value="[^"]*"' | head -1 | sed 's/.*value="//; s/"//')
curl -s --unix-socket $S -X POST http://x/destinations/add \
  -d "csrf=$CSRF" -d "name=usb" -d "type=local" -d "root=/mnt/backups/gniza"
curl -s --unix-socket $S -X POST http://x/schedule/save \
  -d "csrf=$CSRF" -d "name=Nightly" -d "cron=0 2 * * *" -d "mode=split" \
  -d "enabled=1" -d "include_system=1" \
  -d "keep_daily=7" -d "keep_weekly=4" -d "keep_monthly=6"
curl -s --unix-socket $S -X POST http://x/accounts/backup -d "csrf=$CSRF" -d "account=shop"
```

Any page reads as data with `-H 'Accept: application/json'`: `http://x/`,
`http://x/accounts`, `http://x/destinations`, `http://x/schedule`,
`http://x/logs?tab=backups`, `http://x/settings`.

## Restore

There is no native restore to hand an archive to. What a plain server
restores is files and databases: the website files go back over the
account's directory (a file added since the backup stays), and a
database dump is loaded into a database the account owns by name,
created first if it is gone. A whole-account restore is refused as
unverified, on purpose. The terminal's Restore screen shows what has
been restored; asking for one from the terminal is not written yet
(bead `cprest-91g.2`), so the restore form is posted with `curl`
until then. None of this has been proved on a live plain server yet;
the drill is bead `cprest-44s`.

## Remove

```bash
sh /usr/local/share/gniza/uninstall.sh
```

What was installed is moved under `/var/lib/gniza/removed/`, and the
backups, the state database and `/etc/gniza` stay.
