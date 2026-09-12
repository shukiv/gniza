# 0025 — What to back up on a server without a panel

Status: accepted, 2026-09-12; amended the same day, three times (the choosing
is by kind, and it sits in the schedule form, below). Amends
[ADR 0022](0022-a-server-without-a-panel.md).

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

## Amended: the choosing is by kind

The first form was one folder with the databases ticked beside it, and
the operator's answer to it was: "There should be in tabs: Files/Folder,
MySQL, Postgresql, Docker/Podman ... in the mysql it should be in a
table, with the ability to select all. and also list the db users.
docker/podman should be offering to backup containers/stacks and
configurations." That is a different unit of choice, and it is the
right one: what an operator of a LAMP box wants is *all the databases*,
not a database beside a folder.

**One tab per kind, one source per row.** The form is four tabs --
Folders, MySQL, PostgreSQL, Docker / Podman -- each a table of what
there is with a box per row and a box in the header that ticks the
column. Every ticked row becomes a source of its own. Databases and
containers are ticked by default, since backing them up is what the
operator came for; folders are not, since the roots hold things like
`/opt/containerd` too. A source of one database is named
`mysql-<name>` or `pg-<name>` so it does not fight a folder of the same
name for the one name space the snapshot tags are. The old shape, a
folder with databases beside it, is still legal in `panel.Source` and
still staged; the form no longer makes it.

**The users are listed and their grants are kept, not restored.** The
MySQL tab shows the grantees `information_schema.schema_privileges`
names for each database, the PostgreSQL tab the owner
`pg_database.datdba` names. A backup of a MySQL database keeps `SHOW
GRANTS` for each of those accounts beside the record as
`metadata/gniza/mysql-grants-<db>.sql`; a backup of a PostgreSQL
database keeps `pg_dumpall --roles-only` there as `pg-roles.sql`. They
are beside the record and not under `databases/`, because the rehearsal
expects every `.sql` there to create something, and a grants file
creates nothing. Nothing runs them on restore: `LoadDatabase` loads a
dump into a database that exists, and creating accounts on a machine
that may already have them is a decision for whoever restores. MySQL 8
does not put the password in `SHOW GRANTS` anyway. The pages said
"kept beside the dump for reference; not created again on restore";
the third amendment below keeps the accounts the operator chooses.

**A stack is its containers and the folder its compose file lives in.**
Containers are grouped under the compose project their labels name
(`com.docker.compose.project`, which podman-compose writes too), with a
box on the group that ticks its rows. The stack's `working_dir` -- the
compose file and usually the `.env` beside it -- is offered as a folder
under the group, ticked by default: that is the stack's configuration.
The engine's own configuration, `/etc/docker` or `/etc/containers`, is
offered the same way. Both are ordinary folder sources; nothing new is
restored.

**What there is to choose from is read when asked.** The candidate
lists cost a size query on each database client, a `docker ps` and one
`docker inspect` of every container. They are read when the form is
asked for (`?add=1`), shown again after a refusal, or opened by a person
on an empty page -- not on the live refresh every three seconds while a
backup runs, and not on the terminal's five-second read; the terminal
asks with `add=1` when `a`, `m`, `p` or `c` is pressed. The Add button
fetches the form into the sheet the way Edit does on the destinations
page.

## Amended: the choosing sits in the schedule form too

The operator's next answer was that the choosing "should be
incorporated in the schedule where the Which accounts is". On a panel
server the schedule form asks *every account* or *only the ones I
choose* and lists the accounts; on a server without a panel the list is
what has been chosen, so the question and the choosing are the same
question, and the form asks it once.

**The same tables, in the schedule form, and nothing else.** The four
tabs are one template (`choosetabs` in `partials.html`) drawn by the
What to back up page inside one form with a "Back these up" button,
and by the schedule form in place of the account picker. The first
version kept the two radios -- *everything on the list* and *only what
I tick* -- and the operator's answer was "only the tabs". So the
schedule form has no scope of its own: what is ticked is what it
covers, and every source on the list ticked means everything, now and
later (`Policy.AllAccounts`), which is what "Back up all" runs under
and what a source chosen afterwards comes under; leave one out and the
schedule covers only what is ticked. A form with no tables, the
terminal's or a script's, still says `scope=all`. What differs between
the two pages is what a row posts. On the What to back up page a row
already chosen is disabled, since it cannot be chosen twice. In the
schedule form it is a box that posts `source=<name>`, ticked when the
schedule covers it; a fresh row posts `folder`, `mysql`, `postgresql`
or `container` as before and is added when the schedule is saved, then
covered. One source can stand behind several rows -- a folder chosen
with its databases, in the form's first shape, is five rows on two tabs
-- so rows with the same name tick together (`data-same`) rather than
one of them silently bringing the rest.

**The folders tab is a browser.** "Folders should be showing me a file
browser to select from, and add paths to backup." The tab is the
server's directories as a tree, from `/`, each level read as data from
`/accounts/browse?dir=` when it is opened (`panel.Browser`, which the
plain provider implements over `os.ReadDir`, leaving the pseudo
filesystems out). A level lists its folders and then its files, the
files dimmed and capped at two hundred, because a folder is recognised
by what it holds; a symbolic link is shown as what it points at and
marked. Every folder has a box. A box ticked puts the folder in the
table above the tree as a row that posts `folder`, so the pick outlives
whatever the tree does next, and the table also holds every folder
source already chosen; the two are kept in step both ways. A folder
already backed up, or inside one that is (the listing's `Within`, and
the picks in the table), has its box disabled and says so, so the same
files are not backed up twice one tick apart; picking a folder above one
already chosen is allowed and both are marked. A path bar opens the tree
down to a typed path, and the roots are shortcuts to it. The first
shape, one directory at a time in a table, was "awful": two tables that
looked the same, no hierarchy, no height, "not yet" on every row. The
list of folders under the roots (`Candidates.Folders`) stays for the
terminal, whose form is toggles. A typed path stays too, folded away,
for the folder one knows the name of and the name one wants for it.

**Amended: the accounts are chosen too, and kept with their
databases.** "I should be able to select from all the available users
and databases, and their relationships and permissions." The MySQL tab
lists every account the server has (`mysql.user`), what authenticates
it, its privileges on every database (`information_schema.user_privileges`)
and on single ones (`schema_privileges`), and each database says who
has what on it. An account ticked is not a source of its own: the
point of keeping it is that restoring the database brings back the
login that opens it, the way a panel account restores home, databases
and users together. So `Chooser.AttachMySQLUser` keeps the account with
every source that dumps a database it has schema-level rights on, and
refuses one with rights on no such database, naming the databases to
tick with it; the server's own accounts are shown and refused. The
backup writes, beside the dumps, the two files a cPanel backup carries
for the same thing -- the hashes as data (`_users.sql-auth.json`) and
the statements a person can run (`_users-runnable.sql`: `CREATE USER
IF NOT EXISTS` in the server's own dialect, MariaDB's `VIA ... USING`
or MySQL's `WITH ... AS 0x...`, then the grants on the source's
databases) -- so the agent's restore reads them as it reads a panel's,
checks the databases are there, and `PutDatabaseUsers` makes the
accounts again with the hashes they had. Grants on other databases,
and the grant option, are not put back; the file says which, and so
does the backup's report. The hash is never shown on a page. The
PostgreSQL roles are listed as before and not chosen yet.

**Saving adds first, then covers.** The save handler reads the names
ticked, adds the fresh rows, and the schedule covers the union. What
could not be added is said and the schedule is still saved, as the
What to back up page does; a schedule that would cover nothing is
refused rather than saved empty, since a nightly run over nothing would
look like protection. The candidates are read when a person opens the
form, not for the terminal's read of the page as data.

The What to back up page stays: it is where a source is removed, where
its last backup shows, and the only form the terminal has, whose
schedule form has no picker at all.

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
- A database chosen on the first form as part of a folder source keeps
  that shape; one chosen on the MySQL or PostgreSQL tab is a source of
  its own, `mysql-<name>` or `pg-<name>`. Both restore the same way.
- The grants and roles files are reference, not restore. Making them
  restore is a decision for a later ADR, with the password question it
  carries.
