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
   as it stands. (The half of this that a real host confirmed is that a
   plugin is not root; which user it is instead turned out not to be the
   question. See "What the installed plugins answered" below.)
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

## Live evidence and implementation update — 2026-09-08

The [native validation](../directadmin-validation-2026-09-08.md) and subsequent
[Gniza provider round trip](../directadmin-provider-validation-2026-09-08.md)
settle part of questions 2 and 6 on DirectAdmin 1.709. One `taskq --run` task
can synchronously restore the existing disposable account, but failures can
exit zero and require transcript validation. Native backup requires a fresh
dual-identity workspace, not Gniza's root-private staging directory. The
archive is zstd on this host and carries additional home files in a nested
`backup/home.tar.zst`.

Whole-account provider backup/repository/reassembly/native overwrite now
passes the disposable test. Explicit unrestricted overwrite is required; there
is no implied equivalent to cPanel restricted restore. The earlier statements
that every restore is asynchronous/refused and every archive path is merely
documented are superseded by this evidence. Split/granular layout, secure
plugin-session authentication, lifecycle isolation and new-account recovery
remain open. These results do not authorize a production deployment or make
the experimental package release-ready.

### 8. Where the archive keeps an account's database dumps

A finished restore is held to the databases the archive names, the way the
cPanel path already holds one to the dumps beside a rebuilt tree. On
DirectAdmin a dump counts only if it is named `<account>_<something>`,
which is DirectAdmin's own convention and what the provider's listing
already goes by, and only if it sits directly in the archive's `backup/`
directory, which is where the 1.709 fixture had it.

Both halves matter. Without the directory, a `.sql` file in the customer's
own web root -- what phpMyAdmin writes on every export, and what the
WordPress migration plugins leave behind -- would be held against the
restore, and a restore reported as failed is a restore somebody runs
again. Without the naming, a customer's copy of somebody else's dump would
count as one of theirs.

The consequence is that the check is quiet when it finds nothing. An
account whose dumps are nested inside `backup/home.tar.zst`, or in some
other directory on a host that is not 1.709, restores with nothing holding
its databases to account. That is better than failing every such restore
and better than the silence there was before, but it is not the check the
cPanel side has, and it stays this way until a host says where the dumps
are on it. What settles this is a listing of a real archive from more than
one DirectAdmin version -- question 6 above -- which nothing in the repo
records yet.

## What a real archive answered — 2026-09-08

The whole-account archive from the disposable 1.709 fixture was listed
member by member, and the listing is checked in at
`internal/layout/dabackup/testdata/account.tar.list` with the nested home
archive beside it. It settles questions 1, 5 and 6, narrows 8, and opens
a ninth.

**1 — no.** `directadmin admin-backup` takes `--destination` and `--user`
and nothing else. There is no flag that leaves the home directory or the
databases out, so a schedule that backs up less than the whole account
cannot be expressed through it and stays refused. The archive records what
it did include, in `backup/backup_options.list`.

**5 — there is no file.** `/usr/local/directadmin/data/users/<user>/`
holds no list of the account's databases; the only per-account database
record is inside DirectAdmin's own `user.db`. The naming convention plus a
MySQL query, which is what the provider already does, is the record.

**6 — settled for 1.709, and the documentation was wrong.** An account's
per-domain records live with the domain, under `backup/<domain>/`: that
domain's mail configuration, its FTP logins, its zone file as
`<domain>.db`, and its own settings. Only what belongs to the account as
a whole sits directly in `backup/` -- `user.conf`, `crontab.conf`,
`.shadow`, `user.db`, and the database dumps. The websites are under
`domains/<domain>/public_html`, the messages under
`imap/<domain>/<mailbox>/Maildir`, and the rest of the home directory is a
second compressed archive at `backup/home.tar.zst`.

Every member table in `dabackup` has been rewritten from that listing.
Twelve of the paths taken from documentation named nothing a real archive
carries; a fixture test now holds the tables to the listing.

**8 — the rule holds.** `backup/gzv0908a_shop.sql` sits directly in
`backup/`, which is what the restore check already required, with a
`.conf` beside it carrying that database's grants and password hash.

### 9. A layout cannot be asked for one domain's FTP or mail

`FTPMembers` and `MailMembers` take no arguments, because on cPanel each
is one file for the whole account. DirectAdmin keeps both per domain, and
the directory that contains them contains that domain's zone and mail as
well, so there is no path that means "this account's FTP logins" and
nothing else.

Both now return nothing, which makes a restore of either refuse rather
than hand over the containing directory -- `backup/` holds the account's
password hash and every database dump. Restoring a mailbox still brings
back its messages; what it does not bring back is that domain's
forwarders and autoresponders.

Settling this means giving those two methods the domain names the other
three already take, which changes the interface for both panels. It is
worth doing when granular restore on DirectAdmin is worth having.

**Still open.** The plugin session bridge (questions 3, 4 and 7 --
answered below, but not yet built), split mode -- which needs the nested
`home.tar.zst` reassembled into one tree and back -- new-account
recovery, lifecycle isolation, and where DirectAdmin puts a certificate,
which this fixture had none of.

## What the installed plugins answered — 2026-09-08

Questions 3, 4 and 7 were written as a problem about unix users: which
account a plugin script runs as, and what a `SO_PEERCRED` socket can
therefore conclude from it. Reading the plugins already installed on the
DirectAdmin host shows that the unix user is the wrong thing to ask
about, and that DirectAdmin hands a plugin something better.

### What the other plugins do

Two third-party plugins on that host need to run work as root, and
neither of them is run as root. Installatron's `admin/index.raw`,
`reseller/index.raw` and `user/index.raw` are one setuid-root C binary
(`-r-sr-xr-x root root`), a suexec wrapper that records the uid it was
invoked as and re-executes the real program. JetBackup's `index.raw` is
an ordinary script owned by `diradmin`, and the binary it calls,
`/usr/local/jetapps/usr/bin/jetbackup5/jetbackup_admin`, is setuid root
(`-rwsr-xr-x root root`).

A setuid helper is only needed by a process that is not already root. So
the part of question 3 that matters is answered: **a plugin script does
not run as root**, at any of the three levels. Which non-root user it
runs as was not established, and no longer needs to be, for the reason
below.

### What DirectAdmin passes a plugin

DirectAdmin puts the caller's panel session into the script's
environment. Its own binary names the variables together:

```text
SESSION_ID
IS_LOGIN_AS
LOGIN_AS_MASTER
IS_LOGIN_KEY
LOGIN_KEY_NAME
```

with `SESSION_KEY` alongside them, and the Installatron wrapper passes
exactly `SESSION_ID` and `SESSION_KEY` through its environment
allowlist.

The `hosts_click` plugin shows what they are for. Its
`exec/httpsocket.inc.php` calls DirectAdmin's own API back over HTTPS
with no credentials of its own:

```php
curl_setopt($ch, CURLOPT_COOKIE, "session={$_SERVER['SESSION_ID']}; key={$_SERVER['SESSION_KEY']}");
```

That is the session bridge, and it is a better one than the unix user.
A plugin does not have to believe anything the request tells it: it
replays the two values to DirectAdmin and DirectAdmin answers as
whoever that session belongs to, or refuses. `CMD_API_LOGIN_TEST`
answers `Login OK`, and `CMD_API_GET_SESSION` describes the session.

### What this settles

**3 — the unix user is not the question.** A plugin script is not root,
so a root-only socket cannot be opened from one. What replaces the check
is not a widened socket but a proof: the plugin asks DirectAdmin who the
session belongs to, and only an answer naming an administrator gets
administrative work done.

**4 — `login-as` is declared, not inferred.** `IS_LOGIN_AS` says the
session is an impersonation and `LOGIN_AS_MASTER` names who is behind
it, so a `user/` page does not have to work out from a uid whether the
customer or their host is at the keyboard. `IS_LOGIN_KEY` and
`LOGIN_KEY_NAME` say the same for a login key.

**7 — this is the equivalent cPanel has.** cPanel's plugin proves the
request came from that customer's own logged-in session; a DirectAdmin
plugin proves the same thing by replaying `SESSION_ID` and
`SESSION_KEY` and letting DirectAdmin say whose they are. A process
merely running as the account has neither value, so the bar is the same
one, not a lower one.

**Still to build.** The proof has to be made on Gniza's side of the
socket rather than the plugin's: a page that asks DirectAdmin who is
calling and then tells the daemon is only as trustworthy as the path
between them. What the two pages ship today still says the interface is
unavailable, because saying so is honest until that path exists.
