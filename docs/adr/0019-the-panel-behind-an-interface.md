# 0019 — The panel behind an interface, and what DirectAdmin costs

Status: accepted, 2026-09-08.

## Context

DESIGN §1 has always said the panel sits behind one interface, and that
DirectAdmin and Plesk are meant to be further implementations of it rather
than forks of the program. Two things stopped that being true.

The first was where the interface lived. `Provider` was declared in
`internal/cpanel`, so a second panel would have had to import the first
one to say what it implements.

The second is the one that matters. A provider stages an account and hands
a rebuilt one back, but *where the parts live inside that archive* was not
behind the interface at all. `internal/reassemble` knew that the home
directory goes in `homedir/` and one dump per database in `mysql/`, that
the grants file is `mysql.sql` at the top of the tree, and that an
account's identity is `cp/<user>` and `meta/user`. `internal/granular`
knew that the account's settings are `cp/`, `meta/`, `quota`, `shell`,
that its mail configuration is `va/`, `vad/`, `vf/`, and that a mailbox
lives under `mail/` in the home directory. Those are cPanel's names, and
they were spread across the restore machinery as literals.

A second panel that only implemented `Provider` would have produced
archives the restore machinery then rebuilt into cPanel's shape.

## Decision

Two interfaces, both in `internal/panel`, which imports nothing else of
Gniza's:

- **`Provider`** — what the panel can be asked to do: list accounts, stage
  one, apply an archive, create a database, put back a home directory.
- **`Layout`** — where the panel keeps things. `ArchiveLayout` is the
  archive side (`HomedirDir`, `DatabaseDir`, `AccountRoot`,
  `PlaceDatabaseUsers`, `ValidateArchive`); `ItemLayout` is the
  single-item side (which members of the metadata archive hold the
  settings, the cron jobs, the FTP logins, the mail configuration, the
  zones, the certificates, the domains; and which paths under the home
  directory hold the website and a mailbox).

A `Provider` returns its own `Layout`. `reassemble.Request` carries an
`ArchiveLayout` and refuses to run without one; `granular.Build` takes an
`ItemLayout`. Neither has a default: a restore built to the wrong panel's
layout is an archive that panel's own restore will not read, and a silent
fallback to cPanel is exactly how that would happen.

cPanel's answers move to `internal/layout/cpmove`, a package that imports
nothing but `internal/panel` — a format cannot depend on the machinery
that reads it. DirectAdmin's will sit beside it.

A server runs one panel, as DESIGN §1 already said, so the choice is made
once where the agent starts and nothing downstream asks again. The
maintenance runner is the exception: it runs where the repositories are
reachable rather than where the accounts are, so it is told which layout
to expect. A fleet running two panels will have to record the panel
against the server a snapshot came from.

## Consequences

The cPanel path is unchanged — the move is behaviour-free and the existing
suite is what proves it.

What a second panel now has to provide is a list rather than a hunt: a
`Provider`, a `Layout`, its own packaging, and its own answer to how an
account is attributed on a socket.

Three names stay frozen whatever else happens, because deployed servers
resolve them: the release asset `cprest-plugin-amd64.tar.gz`, the
`cprest` word in `SHA256SUMS`, and the `cprest-plugin/` top directory.
DirectAdmin gets its own asset beside them, never a rename of them.

## What DirectAdmin is, as far as its own documentation says

Verified against docs.directadmin.com, September 2026:

- An account backup is `user.admin.<user>.tar.gz`, written by
  `/usr/local/directadmin/directadmin admin-backup --destination=<dir>
  --user=<user>`. Extracted, it holds `backup/` and the domains.
- Per-account configuration is `/usr/local/directadmin/data/users/<user>/`:
  `user.conf`, `domains.list`, `crontab.conf`, `user_ip.list`,
  `httpd.conf`, `nginx.conf`, and a `domains/` subdirectory.
- Home directories are under `/home/`. DirectAdmin's own backups skip
  `backups`, `user_backups`, `admin_backups`, `usr`, `bin`, `etc`, `lib`,
  `lib64`, `tmp`, `var`, `sbin`, `dev` inside one.
- MySQL credentials for the panel are in
  `/usr/local/directadmin/conf/my.cnf`.
- A restore is queued, not run: a line beginning `action=restore&` is
  appended to `/usr/local/directadmin/data/task.queue` and executed by
  `dataskq`. It is asynchronous, which `Provider.Apply` — synchronous,
  returning the panel's transcript — is not.
- Lifecycle hooks are `user_create_pre.sh`, `user_create_post.sh`,
  `user_create_post_confirmed.sh`, `user_destroy_pre.sh`,
  `user_destroy_post.sh`, `user_suspend_pre.sh`, `user_suspend_post.sh`,
  `user_activate_pre.sh`, `user_activate_post.sh`, each receiving the
  contents of `user.conf`. They live in
  `/usr/local/directadmin/plugins/<name>/hooks/`.
- A plugin is a directory under `/usr/local/directadmin/plugins/<name>/`
  with a `plugin.conf` (`name`, `author`, `version`, `active`), and
  `admin/`, `reseller/` and `user/` directories whose scripts are reached
  at `/CMD_PLUGINS_ADMIN/<name>/…`, `/CMD_PLUGINS_RESELLER/<name>/…` and
  `/CMD_PLUGINS/<name>/…`.

## What is not settled, and needs a DirectAdmin host to settle

None of these can be answered from documentation, and each changes code:

1. Whether `admin-backup` can be told to leave out the home directory or
   the databases, and to skip compression. Split mode — the payload shape
   that lets restic deduplicate at all — depends on it.
2. How a queued restore reports that it finished, and that it failed.
   `Apply` returns a transcript today because cPanel's restore exits zero
   on a module that failed; DirectAdmin's answer is somewhere in
   `dataskq`, the message system or the logs.
3. **Plugin scripts run as the requesting unix user, and an admin plugin
   therefore runs as `admin`, not as root.** Gniza's admin interface is a
   root-only unix socket (ADR 0012). That model does not survive the move
   as it stands.
4. How `login-as` affects the unix user a `user/` script runs as, which is
   what the account socket attributes a request by (`SO_PEERCRED`).
5. Whether DirectAdmin's record of an account's databases is a file or a
   query by name prefix, which decides how `CreateDatabase` puts one back
   where the customer can see it.
6. The exact member names inside the backup archive, beyond `backup/` and
   the domains, which is what `ItemLayout` has to answer for a
   single-item restore.
7. What stands in for cPanel's session bridge on the account-facing
   socket. DirectAdmin runs a `user/` plugin script as the account, so
   `SO_PEERCRED` attributes the request correctly — but cPanel's plugin
   also proves the request came from that customer's own logged-in
   session, and without an equivalent every process running as the
   account could read its backups. That is a decision about who may read
   a customer's data, not a detail to arrange quietly in the packaging,
   so the DirectAdmin plugin ships two pages that say what is and is not
   available rather than an interface that quietly lowers the bar.

Until 1–6 are answered on a real host, the DirectAdmin provider is not
something to run against a customer's server.
