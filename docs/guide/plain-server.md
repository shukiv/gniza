# A server without a panel

Gniza runs on a server that has no cPanel and no DirectAdmin: a LAMP box,
a docker or podman host. The same one-liner installs it; `get.sh` finds
neither panel directory and takes the plain package (ADR 0022).

```bash
curl -fsSL https://github.com/shukiv/gniza/releases/latest/download/get.sh | sh
```

## What to back up

Nothing on a server without a panel says what an account is, so you do.
On the page called **What to back up** (screen 4 in the terminal) a
*source* is chosen, and each source is backed up on its own, under its
name, the way a panel's account would be:

- **A folder**, backed up from where it lies, with the **MySQL** and
  **PostgreSQL** databases you tick beside it dumped into the same
  backup. The folders under `/var/www`, `/srv` and `/opt` are offered;
  any folder on the server can be named. A source may have databases
  and no folder.
- **A container**, docker or podman. Each of its volumes and bind mounts
  becomes a source, read where it lies on this machine, with the
  container's own description and its compose file kept beside every
  backup. A database that lives inside a running container is not
  consistent when read this way: tick its database as a folder source's
  database instead, or stop the container for the backup. The pages
  say so where the container is ticked.

Nothing is backed up until it is chosen; the page opens on the form
until something is. A source can be removed at any time; the backups
already taken of it stay at the destinations, and choosing it again
under the same name carries its history on.

The database lists come from the `mysql` and `psql` clients on the
server; a client that is not there, or cannot connect, is said on the
form and that kind is not offered. `psql` and `pg_dump` run as the
`postgres` unix account. The container list comes from `docker ps -a`
and `podman ps -a`.

To change which folders are offered, edit `/etc/gniza/plain.env` and
restart the service:

```
GNIZA_PLAIN_ROOTS=/var/www,/home/deploy/sites
```

```bash
systemctl restart gniza
```

The files are read where they lie; only the dumps and a record of what
the source is are written to staging. The system backup, when a schedule
includes it, takes the web, PHP, database, container, cron and SSH
configuration, the certificates and the unit files, with a list of the
installed packages and of the containers and volumes present.

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
4. On What to back up, `a` chooses a folder and the databases beside
   it, and `c` a container. `b` backs up the source under the cursor
   now, and `B` runs the schedule over every source. Logs shows what
   happened.

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
curl -s --unix-socket $S -X POST http://x/accounts/add \
  -d "csrf=$CSRF" -d "path=/var/www/shop" -d "mysql=shop" -d "mysql=shop_wp"
curl -s --unix-socket $S -X POST http://x/accounts/add -d "csrf=$CSRF" -d "container=docker/web"
curl -s --unix-socket $S -X POST http://x/accounts/backup -d "csrf=$CSRF" -d "account=shop"
```

Any page reads as data with `-H 'Accept: application/json'`: `http://x/`,
`http://x/accounts`, `http://x/destinations`, `http://x/schedule`,
`http://x/logs?tab=backups`, `http://x/settings`.

## From a browser

The same pages the panels show, on a TCP address, behind a password
([ADR 0024](../adr/0024-a-browser-on-a-server-without-a-panel.md)). The
installer asks where it should listen:

| Choice | Address | How to reach it |
|---|---|---|
| this machine only (the default) | `127.0.0.1:8443` | from your own machine, `ssh -L 8443:127.0.0.1:8443 root@<server>`, then open `http://127.0.0.1:8443/` |
| every address | `0.0.0.0:8443` | `https://<server>:8443/` from anywhere; open port 8443 in the firewall |
| nowhere | | the terminal interface only |

The first choice is the safe one: the port never leaves the machine and
ssh is the door. The second puts a root service on the internet with its
password as the only door, so choose it knowingly, and with a password
you would not use anywhere else.

The installer asks for the password on the terminal, without echo, and
keeps its hash in `/etc/gniza/web/password`. To change it later:

```bash
gniza-agent -web-set-password
```

It applies to the next sign-in; nothing restarts. A script sets it with
`GNIZA_WEB_PASSWORD=... gniza-agent -web-set-password`, or pipes it on
stdin. Five wrong passwords from one address start a wait of thirty
seconds that doubles with each wrong one after it.

On every address the interface is TLS. A self-signed certificate is made
on the service's first start, at `/etc/gniza/web/cert.pem` with its key
beside it, and the browser will say so the first time. Compare the
fingerprint the browser shows with the server's before accepting it:

```bash
openssl x509 -in /etc/gniza/web/cert.pem -noout -fingerprint -sha256
```

The service log says the same fingerprint when it starts. To use a
certificate of your own, put the pair at those two paths and restart the
service.

To change the address, or turn the interface off, edit
`GNIZA_WEB_LISTEN` in `/etc/gniza/plain.env` and restart the service. An
unattended install answers with the same variable set before the
installer runs (`GNIZA_WEB_LISTEN=` empty means none) and
`GNIZA_WEB_PASSWORD` for the password. With no terminal and no password
given, the address is set to `127.0.0.1:8443` and no password is: the
service does not open the door until one has been set with
`gniza-agent -web-set-password`, and says so in its log. That is also
what happens on a plain server that upgrades itself to this release.

A sign-out is in the rail's foot. A session ends twelve hours after it
began, an hour after it was last used, or when the service restarts,
which it does to upgrade itself.

## Restore

There is no native restore to hand an archive to. What a plain server
restores is files and databases: the files go back over the source's
folder (a file added since the backup stays), and a dump is loaded into
a database the source was chosen with, created first if it is gone; a
PostgreSQL dump is the one whose name ends in `.pg`. A whole-account restore is refused as
unverified, on purpose. The terminal's Restore screen shows what has
been restored; asking for one from the terminal is not written yet
(bead `cprest-91g.2`), so a restore is asked for from the browser, or
the form is posted with `curl`. None of this has been proved on a live plain server yet;
the drill is bead `cprest-44s`.

## Remove

```bash
sh /usr/local/share/gniza/uninstall.sh
```

What was installed is moved under `/var/lib/gniza/removed/`, and the
backups, the state database and `/etc/gniza` stay.
