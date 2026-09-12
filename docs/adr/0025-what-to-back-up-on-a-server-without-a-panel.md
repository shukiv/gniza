# 0025 — What to back up on a server without a panel

Status: accepted, 2026-09-12. Amends [ADR 0022](0022-a-server-without-a-panel.md).

## Context

ADR 0022 said what an account is on a server with no panel: a directory
directly under one of the declared roots, with the MySQL databases named
after it. That was the cPanel shape with the operator choosing the
parent, and it was wrong twice over on the first server it met. The
sites were not under `/var/www`, `/srv` or `/opt`, so the page listed
nothing and said the accounts were read from `/var/cpanel/users`. And
the operator did not want accounts at all: "there is no need for
accounts, but what to backup. it should let me backup files/folders, it
should ask me to backup mysql/postgresql and it should ask me to backup
docker/podman containers."

Everything downstream of the provider — the scheduler, the snapshot
tags, retention, the rehearsals, the restore — is built around a unit
called an account with one home directory and some dumps beside it.
`internal/reassemble` reads a snapshot back through exactly one home
directory part and one metadata part, and refuses a second candidate.
Changing that unit would touch every one of those places for a shape
that is, underneath, the same: some files, some dumps, a name.

## Decision

**The operator chooses; each choice is an account.** A *source*
(`panel.Source`) is one folder, or none, with the MySQL and PostgreSQL
databases ticked beside it, or one mount of a container. It is backed
up under its name as an account is, so nothing in the engine, the tags
or the restore changes. The roots in `/etc/gniza/plain.env` are what the
form offers, not what is backed up: nothing is backed up until chosen.
The choices live in the node's store (`sources` bucket) beside
everything else, so the pages can change them, and the provider reads
them on every call.

**One folder per source.** A snapshot has one home directory part, so
a second folder is a second source, and a container with three mounts
is three sources that remember the container they came from. A source
with databases and no folder has a files part holding one note, because
a payload with an empty part is refused as incomplete. This is said
where it costs the operator something — a container's volumes appear
as separate rows — rather than threaded through the restore machinery
as a new shape.

**Databases are the operator's choice, not a naming rule.** The
`<account>_...` convention is gone; the form lists what `mysql` and
`psql` can see and the operator ticks. `CreateDatabase` and
`LoadDatabase` act only on a database the source was chosen with, and
say to add it to the source first otherwise. A PostgreSQL dump is named
`<database>.pg.sql`: everything that reads a dump knows it by its file
name and nothing else, and the suffix keeps a MySQL and a PostgreSQL
database of the same name apart through a restore. `psql` and `pg_dump`
run as the `postgres` unix account when the service is root, because
that is who a fresh PostgreSQL trusts over its socket. The
`HasPostgreSQL` flag is not set: it exists to force the monolithic
shape, which a plain server refuses.

**A container is chosen whole and backed up as its mounts.** `docker
inspect` or `podman inspect` says where its volumes and bind mounts are
on the host, and each that is a directory becomes a source, read in
place. The description itself and the compose files its labels name are
copied beside the record, so a replacement machine knows how the
container was run. A container with no mounts is a source of its
description alone. A volume read while its container runs is not
consistent for a database kept inside it; the backup carries a warning
saying so, and the form says to tick the database instead or stop the
container. Nothing is paused or stopped by Gniza.

**The same page, the same handlers, one interface more.** The accounts
page carries the choosing when the provider implements `panel.Chooser`,
the way `Identifier` and `Certifier` are optional today, and is the
panel's list where it does not. The rail calls the page *What to back
up* there. The terminal reads the candidates from the page's data and
builds its forms from them.

## Consequences

- A plain server upgraded to this release backs up nothing until
  something is chosen. Its accounts were directories under the roots; a
  directory chosen again under the same name has the same snapshot tag
  and carries on. The page opens on the form until a choice is made,
  and the installer's closing lines say so.
- `plain.Provider.Roots` is a list of suggestions now. The unit and the
  environment file are unchanged.
- The restore of a plain server is still files and databases, and still
  unproved on a live server (`cprest-44s`). What it restores to is the
  source's folder and the source's databases.
- Bead `cprest-arj` (volumes discovered as accounts) is closed by this:
  they are chosen, not discovered.
