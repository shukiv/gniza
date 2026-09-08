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
   as it stands. (Confirmed, and DirectAdmin will not run a plugin as
   root even when asked. See "What the installed plugins answered"
   below.)
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
therefore conclude from it. The plugins already installed on the
DirectAdmin host, read alongside DirectAdmin's own documentation and the
1.709 binary, answer that -- and show that the unix user was only half
of what was needed, because DirectAdmin hands a plugin the caller's
session as well.

### What the other plugins do

Two third-party plugins on that host need to run work as root, and
neither of them is run as root. Installatron's `admin/index.raw`,
`reseller/index.raw` and `user/index.raw` are one setuid-root C binary
(`-r-sr-xr-x root root`), a suexec wrapper that records the uid it was
invoked as and re-executes the real program. JetBackup's `index.raw` is
an ordinary script owned by `diradmin`, and the binary it calls,
`/usr/local/jetapps/usr/bin/jetbackup5/jetbackup_admin`, is setuid root
(`-rwsr-xr-x root root`).

DirectAdmin's own documentation says why, and it is stronger than the
inference from the two plugins. A plugin runs as whoever is logged in --
"DA will run as the User that is logged in to DA, just like plugins
already do" -- and `plugin.conf` can name a different one per level with
`admin_run_as`, `reseller_run_as` and `user_run_as`. What it cannot name
is root: "you can set 'user' to any value you wish, as long as it's not
uid=0". The installed 1.709 binary carries all three option names and
the refusal that enforces it, `User %s is not a valid apache user.  Must
not be uid=0 and must exist.`

So question 3 is answered, and answered more firmly than it was asked:
an admin plugin runs as `admin`, and **no plugin can be configured to
run as root**. A setuid helper is not one way to reach root from a
plugin, it is the only one.

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

**3 — a plugin cannot be root, so something has to carry it there.**
The root-only socket cannot be opened from a plugin script. There are
two ways to bridge that, and they are not equally good.

The first is what both plugins here do: a small setuid-root helper that
allowlists the environment and execs the real program. It leaves ADR
0012 untouched -- `SO_PEERCRED` sees root. It also puts a setuid-root
binary on every DirectAdmin server Gniza is installed on, which is a
thing to add to a server only when nothing else will do.

The second uses `admin_run_as`. DirectAdmin will run the admin plugin as
a named account, so Gniza can install one of its own -- an account
nothing else runs as, that no customer can log in to, and that only root
can become. The administrative socket is then owned by that account
rather than by root, and `SO_PEERCRED` attributes a connection to it as
tightly as it attributes one to root: on a machine where only root can
setuid to that account, a connection from it is a connection from
DirectAdmin's plugin runner. No setuid binary is installed at all.

The second looks better, and rests on something nobody has tested. Every
piece of evidence that a plugin is handed `SESSION_ID` and `SESSION_KEY`
comes from a plugin running under DirectAdmin's default execution, as
whoever is logged in. Whether DirectAdmin still sets them once
`admin_run_as` has changed the user is not written down anywhere and
does not follow from anything above. If it does not, the second design
has no session to prove and only the first is left.

One experiment settles it, and it needs a plugin installed on a
DirectAdmin server, which is the user's to authorise: a plugin whose
`plugin.conf` names an existing unprivileged account in `admin_run_as`
and whose `admin/index.raw` prints `id -u` and the *names and lengths*
of the environment variables beginning `SESSION`, `IS_LOGIN` and
`LOGIN_` -- never their values, which are live credentials for that
session.

Until that has run, this records a preference and not a decision, and
ADR 0012 stays as it is. Nothing about socket ownership is written
before the answer.

**4 — `login-as` is asked about, not inferred.** `IS_LOGIN_AS` says the
session is an impersonation, `LOGIN_AS_MASTER` names who is behind it,
and `IS_LOGIN_KEY` and `LOGIN_KEY_NAME` say the same for a login key.

Whether all four reach a plugin's environment is not established: the
binary lists them beside `always_load_all_script_env_vars`, an option
that is off by default and that the changelog describes in terms of
DirectAdmin's own `all_pre.sh` and `all_post.sh` rather than plugins.
`SESSION_ID` and `SESSION_KEY` are not in doubt -- a plugin installed on
this host depends on them and works -- so the answer that does not
depend on the option is to ask DirectAdmin. `CMD_API_GET_SESSION`
answers `username` and `usertype` for the session those two identify.

That answer is enough to authorise and not enough to attribute. Under
login-as, `username` is the customer being impersonated, which is the
right answer to what the session may do and silent about who is at the
keyboard. `LOGIN_AS_MASTER` would say, and whether it reaches a plugin
is the same open question. So a restore run through a login-as session
would be recorded against the customer rather than against the reseller
who ran it: an audit gap, not an authorisation one, and one to close
before the account page does anything an operator would later want to
attribute.

**7 — this is the equivalent cPanel has, and the account page is a
separate decision from the admin page.** cPanel's plugin proves the
request came from that customer's own logged-in session; a DirectAdmin
plugin proves the same thing by replaying `SESSION_ID` and `SESSION_KEY`
and letting DirectAdmin say whose they are. A process merely running as
the account has neither value, so the bar is the same one, not a lower
one.

What the account page should not do is copy the admin page's answer.
`user_run_as` would run every customer's page as one account, and the
account socket's `SO_PEERCRED` -- which is the whole of what attributes
a request to a customer today -- would stop distinguishing them, leaving
DirectAdmin's answer as the only thing that does. Leaving the `user/`
page at default execution keeps both: DirectAdmin runs it as the account
the docs say it does, `SO_PEERCRED` reads that, and the session proof is
checked on top of it -- the uid the socket sees has to be the account
DirectAdmin names for the session. Two independent things agreeing,
which is the shape cPanel's has.

**Still to build.** The proof has to be made on Gniza's side of the
socket rather than the plugin's: a page that asks DirectAdmin who is
calling and then tells the daemon is only as trustworthy as the path
between them. The helper carries the two session values; the daemon
replays them to DirectAdmin and believes only DirectAdmin's answer.

Two things that decides nothing about yet. `CMD_API_GET_SESSION` returns
the session's `password`, base64 encoded, alongside the `username` and
`usertype` that are wanted -- so whatever reads that response has to
take the two fields it needs and never keep, log or forward the third.
And the daemon calling DirectAdmin back on loopback meets DirectAdmin's
own certificate, which is a decision about what to pin rather than a
reason to stop checking.

What the two pages ship today still says the interface is unavailable,
because saying so is honest until that path exists.

## What a plugin on the host answered — 2026-09-08

The section above recorded a preference and named the one experiment
that would turn it into a decision. That experiment has now run, on the
same DirectAdmin 1.709 host: a diagnostic plugin, installed into its own
directory and removed again, printing the unix user it ran as and the
*names and lengths* of the session variables it was handed. No session
value was ever printed; each one is a live credential.

### A plugin keeps its session after run_as changes the user

With `admin_run_as=nobody` in `plugin.conf`, the admin page reported:

```text
level=admin
uid=65534 user=nobody
SESSION_KEY=<39 characters>
SESSION_ID=<39 characters>
```

DirectAdmin changed the user *and still handed over the session*. That
was the assumption the second design rested on and the only thing that
could have sunk it. No setuid-root binary needs to be installed
anywhere.

What does not follow, and what an earlier draft of this section said
anyway, is that the administrative socket should become owned by that
account. It should not. Today that socket is root-only, and a connection
to it means an operator with a root shell. Give it to `gniza`'s uid and
a connection means *DirectAdmin's plugin runner* -- a service account,
acting for whoever DirectAdmin let through to the page. The uid stops
being a person. That is ADR 0013's lesson arriving on the administrative
side: a unix uid is not a login.

So the socket is not widened, it is joined. `/var/run/gniza/admin/ui.sock`
stays exactly as it is, root-only, for the command line and for
standalone mode. DirectAdmin gets a second one of its own, owned by the
dedicated account, whose handler does nothing at all until the session
carried with the request has been put to DirectAdmin and come back
naming an administrator. On that socket the verified session is the
whole authorization, in the way ADR 0013 made it the whole authorization
on the account side; the uid only says which door was used.

ADR 0020 records that, because it is a decision about who may read every
customer's backups rather than a note on this one.

The account page reported `uid=1000 user=admin` with no `user_run_as`
set, which is DirectAdmin running it as whoever is logged in, as its
documentation says. Leaving it at that keeps `SO_PEERCRED` attributing
the customer on the account socket, with the session proof checked on
top -- the shape question 7 settled on.

### What a plugin is actually handed

Observed, at both levels:

```text
USERNAME
SESSION_ID
SESSION_KEY
IS_LOGIN_AS
IS_LOGIN_KEY
SESSION_SELECTED_DOMAIN
```

`IS_LOGIN_AS` does reach a plugin, which question 4 could not assume.
`LOGIN_AS_MASTER` and `LOGIN_KEY_NAME` did not appear -- consistent with
being set only during an actual impersonation, which this session was
not, and still unconfirmed. `USERTYPE` is not handed over at all.

None of these is believed on its own. `USERNAME` is an environment
variable, and an environment variable is what the process was started
with rather than proof of anything; it is DirectAdmin's own answer that
authorises.

### Which endpoint answers, and which does not

Asked with the session replayed as `session=<id>; key=<key>`:

```text
/CMD_API_GET_SESSION      error=1  Cannot Execute Your Request
                          details=The requested command requires POST but GET was used
/CMD_API_LOGIN_TEST       error=0  Login OK
/CMD_API_SHOW_USER_CONFIG (the session's own record: username, usertype, and ~50 more)
```

`CMD_API_GET_SESSION` is the endpoint whose name says it answers this,
and it is the wrong one twice over: it refuses a GET, and its documented
answer carries the session's password. `CMD_API_SHOW_USER_CONFIG`
answers a GET, names the session's own account when asked about nobody
in particular, and carries no password at all. It is what the verifier
asks.

`CMD_API_LOGIN_TEST` confirms a session is live without saying whose, so
it adds nothing to a check that has to know the account.

### Two things a login-as session would still settle

What `usertype` reads for a real customer -- `user` is what the values
of `SHOW_USER_CONFIG` imply, and the session observed was an
administrator's. And whether `LOGIN_AS_MASTER` appears during an actual
impersonation, which is what closes the audit gap in question 4. Both
need one more session on a host and neither blocks the design.

### Packaging, learnt the hard way

A plugin directory on disk is not a plugin. `plugin.conf` needs `id=`
and `installed=yes` as well as `active=yes`; the entry point DirectAdmin
serves is `index.html` in each level directory, not `index.raw`; and the
menu entry comes from `hooks/admin_txt.html` and `hooks/user_txt.html`.
Without those the page is a 404 with the plugin sitting right there.

## Split mode does not need an option DirectAdmin never documented — 2026-09-08

`Stage` refuses split mode because backing an account up in parts was
taken to need `admin-backup` options that are not documented, and
guessing at them would produce the kind of backup that looks fine until
it is needed.

Reading the retained fixture archive settles that it does not need them.
The outer archive has three roots — `backup/`, `domains/`, `imap/` — and
`backup/home.tar.zst` carries the rest of the home directory, with no
overlap between them. So Gniza can ask DirectAdmin for exactly the
archive it already knows how to produce, then unpack that archive itself
into the parts restic wants to see, and rebuild it on the way back.
DirectAdmin is never asked to do anything it has not documented.

What that costs is a repack that DirectAdmin's own restore will accept.
The fixture's entries carry owners and groups as names rather than
numbers, and not always the account's own group — `gzv0908a/apache` on
`.php/`, `gzv0908a/mail` on `Maildir/` — with modes that go with those
groups. A rebuild that flattens ownership would restore an account whose
mail directory the mail server cannot write. That is a round trip on the
fixture to answer, not a source question, and it is what stands between
this and split mode on DirectAdmin.

See docs/directadmin-validation-2026-09-08.md for the listing.

## The round trip that stood between this and split mode — 2026-09-08

Answered, and split mode is no longer refused.

The archive is taken apart into two directories — the account's own
records and its home directory, the latter put back together from the
three places the archive keeps it in — and the tar headers of both
archives are written down beside them in a manifest. The bodies are what
restic deduplicates; the manifest is what makes the repack faithful. A
rebuild that walked the staged tree instead would write whatever the
rebuilding server's own group file said, which is the failure the section
above named.

All 74 members of the retained 1.709 fixture come back with the same
name, type, mode, uid, gid, owner and group names, mtime, size, link
target and contents, and GNU tar's listing of the original and the
rebuilt archive differ in one line: the nested archive's own length,
because compressing the same bytes twice does not produce the same file.
The three groups that motivated the manifest are visible in that listing
— `gzv0908a/apache`, `gzv0908a/mail`, `gzv0908a/access`.

Three things follow from a panel that will not produce its own parts, and
all three are decided by asking the layout rather than by naming
DirectAdmin:

- The restore does not rebuild the tree into an archive by walking it. It
  restores the parts and asks the panel's own layout to repack them.
- A rehearsal builds the archive even when it was told to stop at the
  tree. That option exists because cPanel's restore takes a directory as
  readily as an archive, so the tar answers nothing the tree does not.
  DirectAdmin's restore reads an archive and nothing else, so here the
  archive is the thing being rehearsed.
- The staging estimate is the whole account twice rather than a fifth of
  it. On cPanel a split payload stages metadata and dumps and backs the
  home directory up where it lies; here the archive is written into
  staging and taken apart there, so at the peak the account is on that
  disk twice.

Still open: no account has been restored from an archive Gniza rebuilt.
That is a restore on a production host, and it is the last question this
record is waiting on.
