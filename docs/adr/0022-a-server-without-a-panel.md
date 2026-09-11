# 0022 — A server without a panel

Status: accepted, 2026-09-11. The provider and the package are in; the
terminal interface is not yet (bead `cprest-u5y`).

## Context

`get.sh` refused any machine that had neither `/usr/local/cpanel` nor
`/usr/local/directadmin`. The operator wants Gniza on those machines too:
a LAMP server with sites under `/var/www`, a docker or podman host with
stacks under `/opt` and data in named volumes. Nothing in the scheduler,
the repositories or the restore machinery cares which panel is there
(ADR 0019); what stopped a plain server was that nobody had said what an
*account* is when no panel is around to say so, and that the only
interface is a panel plugin.

## Decision

**A plain server is a panel.** `internal/plain` implements
`panel.Provider` and `panel.Layout` like the other two, is chosen with
`-panel plain`, and nothing downstream asks again. DESIGN §1's rule
stands: one server, one panel.

**An account is a directory directly under a declared root.** The roots
are given once, in `/etc/gniza/plain.env` as `GNIZA_PLAIN_ROOTS`, and
default to `/var/www,/srv,/opt`. Every directory under one of them whose
name is usable as an account name is an account of that name; dot
directories and files are not. A root that is not there is skipped, so
one default serves a LAMP server and a container host alike. A name that
appears under two roots is taken from the first and the second is said
in the log. This is the cPanel shape, `/home/<user>`, with the operator
choosing the parent.

**Databases belong to an account by name.** A MySQL database called
`<account>` or `<account>_<anything>` is that account's, which is the
convention every panel uses and most people follow without one. A
server with no MySQL client, or one that cannot connect, has accounts
with files only, said once at warn. PostgreSQL, container volumes and
compose projects are not discovered yet (bead `cprest-arj`).

**The files are read where they lie.** A backup is the split shape:
the account's directory is a part restic reads in place, and beside it a
metadata part holds one dump per database and `gniza/account.json`,
which records the account's name, its path, its databases and the host,
so a snapshot read on another machine knows what it holds. There is no
monolithic shape, because there is no native archive to make.

**The system backup takes what a replacement machine needs.** The web,
PHP, database, container, cron and SSH configuration, the certificates,
the unit files, and a manifest with the installed packages and the
containers and volumes present, from a list of paths where the ones that
are not there are skipped.

**Restore is files and databases, and nothing is imitated.** `Apply` is
refused as unverified: there is no panel to hand an archive to.
`PutHomeDir` writes the restored tree over the account's directory,
keeping modes and owners, and leaves a file added since the backup
where it is. `CreateDatabase` and `LoadDatabase` act only on a database
the account owns by name. Crontabs and database users are refused: the
backup does not carry them as the account's.

**Socket only, as ADR 0007 says.** A LAMP box is not multi-tenant the
way a cPanel server is, but a loopback port holding every destination
credential is still the wrong shape, and a server that later gains a
panel must keep working the same way. The answer to "there is no
browser" is a terminal interface over the same socket, not a TCP port.

**The terminal interface reads JSON from the same handlers.** bolt is a
single-writer store, so the interface cannot open `state.db` beside the
service. The write side already works over the form routes. The read
side gets JSON from the existing view structs when asked with
`Accept: application/json` (bead `cprest-udv`), and the interface itself
is a subcommand of the agent (bead `cprest-u5y`). Until it lands, the
service is configured with `curl --unix-socket`, and the installer says
so where it cannot be missed.

**Its own package.** `gniza-plain-amd64.tar.gz` carries the binary and
two scripts, is signed in the same `SHA256SUMS` as the other two, and is
what `get.sh` installs when it finds neither panel directory. The
installer refuses a server that has one: the panel's package is the
right one there.

## Consequences

- A server with no panel installs with the same one-liner, updates from
  the same release and dist channels, and is removed by the same button
  path (`/usr/local/share/gniza/uninstall.sh`).
- Until the terminal interface ships, setting up a plain server is
  three `curl` commands from `docs/guide/plain-server.md`. That is a
  worse first hour than the plugins give, and the installer's closing
  warning says so rather than letting it be discovered at two in the
  morning.
- Nothing about restore has been proved on a live plain server. The
  provider carries the same kind of `Provisional` sentence DirectAdmin
  carries, said at start and by the installer, and the drill is a bead
  of its own (`cprest-44s`).
- The account-name convention for databases is a convention. A site
  whose database is called something else is backed up without it, and
  the record beside the dumps says which databases were taken, so the
  gap is visible rather than silent.
