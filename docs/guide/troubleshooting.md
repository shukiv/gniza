# Troubleshooting

## Reporting a problem

The bug icon at the foot of the rail — beside the documentation and GitHub
links — opens a form: a subject, and what happened in your own words.

Pressing **Show me the report** gathers the rest and shows you the whole
thing: versions and environment, the recent failures this server has
recorded, its settings without any credential, and the last 200 lines the
service logged. Passwords, keys, tokens and private keys are removed from
those lines first, and each section is capped so a noisy week does not become
a report nobody reads.

Nothing is transmitted from the server. **File it** opens
<https://github.com/shukiv/gniza/issues> in a new tab with the whole report
already in the form — what you typed and every section above it — and you
press GitHub's own button to send it. You see the issue before you create
it, which is where to check it one last time.

A link cannot carry every line of a log: browsers and servers each stop
reading a URL somewhere. So the report is cut down until it fits, longest
section first and oldest lines first, because what matters is nearest the
failure. **Download it** hands over the uncut report as
`gniza-report-<when>.md`; attach that to the issue when the log matters.

There is nothing to configure and no credential to install. A plugin that is
published to every cPanel server cannot hold a key to an authenticated
endpoint without publishing the key too, so it does not have one: reporting
works the same on every server, out of the box.

The reviewed report is signed and expires after twenty minutes or a service
restart, so a download hands over exactly the diagnostics you read rather
than freshly gathered logs that may say something else. If the subject or
description changes, show the report again first.

Redaction is best-effort, and you are the last check before the report
becomes public: read it for secrets in unusual formats or sensitive customer
details before you file it. Legacy `bug_email` and `sendmail_path` settings
remain readable for compatibility but are not used.

Customers with no access to WHM can open the same tracker directly; it needs
nothing installed.

## Reading the log

**Logs → Service log** is what the service itself wrote, read back from the
journal without a shell: a level, a span, a number of lines and an account
to filter by. [Logs](logs.md#the-service-log) describes the controls. What
follows is how much there is to read, and what is kept out of it.

Nothing on that page is filtered for secrets. It is this server's own log,
behind the root-only socket, read by somebody who could run `journalctl`
anyway — and a log read to debug a credential is no use with the credential
taken out. A bug report is the other way round, and is redacted, because it
leaves the server.

### Log level

**Settings → Log level** is how much the service writes: `error`, `warn`,
`info` or `debug`, quietest first. It takes effect at once, with no restart,
which is the point — the reason to turn `debug` on is usually something
going wrong now, and a restart would end it. It is stored, so the service
comes back at the level you chose.

At `debug` the service writes down what it actually ran: every `pkgacct`,
`mysqldump` and `restic` invocation with its arguments and how long it
took, which schedule was due and which was not and why, which accounts a
schedule resolved to by name, each destination probe, and the staging and
upload of each account. That is enough to reproduce a failure by hand —
copy the command out of the log and run it.

The account filter reaches `pkgacct` and `mysqldump`, which carry the
account on the line. The `restic` lines do not: that runner works on
repositories and does not know whose account it is copying. Filter by
account for the story, then clear the filter to see the `restic` command
beside it — the "uploading to a destination" line just before it names both.

Not written down at any level, because nothing is instrumented there yet:
the `mysql` calls that read an account's database users and grants, and
whatever `StageSystem` runs for a system backup. The three that fail in
practice — `pkgacct`, `mysqldump`, `restic` — are.

What is never written down, at any level: restic's environment, which is
where the backend credentials and the repository password file's path are;
the value of any `-o` backend option, whose key is kept and whose value is
not; and any login embedded in a repository address, which becomes
`[credentials]`. There is a test that fails if any of those reach a log
line.

`debug` is loud. Turn it back down when you are done.

The `-log-level` flag in the unit file is the level the service starts at
before it reads its settings. The stored level wins, because it is the one
that can be changed without editing a unit file.

## First three commands

```bash
systemctl status gniza
journalctl -u gniza -n 100 --no-pager
ls -l /var/run/gniza/admin/ui.sock
```

## The plugin page is blank, or WHM 500s

The CGI proxies to the unix socket. If the service is down, there is nothing to
proxy to. Check `systemctl status gniza` first, then that the socket exists.

If the plugin is missing from the WHM sidebar entirely, look under
**Development → Apps Managed by AppConfig** — not **Manage Plugins**, which
lists cPanel's own RPM addons.

## A backup fails at staging

Almost always room. Staging rebuilds one account in full before upload, so the
staging volume needs room for your largest account, not your average one.
Settings → Staging shows what is left; the Overview shows the same number.

Second most common: an account whose home directory is being written to
heavily. The run reports partial and names the account.

## A database that will not dump

The backup carries on. The row says **not backed up** and names the database,
the notification says the same, and the run is recorded as short of a
complete account — so termination safety will not let that account be deleted
on the strength of it.

Everything else was stored: the home directory, and every other database. One
corrupt table is not a reason to store nothing, which is what used to happen —
an account with forty-seven databases and one bad table in one of them had no
backup at all.

The dump that failed is deleted rather than left behind. An empty `.sql` would
restore as a database with no tables and say nothing about it.

`Lost connection to MySQL server during query (2013)` is usually a corrupt
table rather than a network problem: reading it takes the server down, and the
server comes back on its own a second or two later. The service log at `debug`
names the table. `SELECT` it by hand to confirm — on a copy of the data, or
out of hours, because it will take the server down again.

Fixing it is MySQL's business, not Gniza's: repair the table, or drop and
regenerate it if it is a cache. The backup starts including that database
again the next night, with nothing to change here.

Reading a corrupt table can stop the server for a moment, and a dump attempted
in that moment fails for a reason that has nothing to do with the database
being dumped. Gniza waits for the server to answer again and tries that
database once more, rather than recording the rest of the account's databases
as missing. cPanel's own `pkgacct` does not: measured on such an account, it
dumped 16 of 47 databases, lost the other 31 to `Can'''t connect`, and exited
zero saying `pkgacct completed`.

## A destination stops answering

The destination row says when it was last reachable. Test it from the row menu
— that runs the real connection, not a cached verdict.

For SFTP, the usual causes are a rotated host key or a key that was removed on
the far side. For S3, an expired access key. For a local path, a mount that is
no longer mounted — a destination pointing at an unmounted mountpoint will
happily write to the underlying directory instead, which is why free space is
shown per destination.

## Restore says the account has no backups here

Check the destination select. An account is often in one destination and not
another, and *Restore account(s)* lists what the chosen destination actually
holds — read from the backups, not from cPanel.

If the account was deleted and recycled, the current holder of the name does
not inherit the previous one's backups. That is deliberate.

## cPanel will not let me delete an account

[Termination safety](accounts.md#termination-safety) is on and the account is
missing a promised copy. Either **Prepare for termination**, which queues the
smallest set of backups that fixes it, or turn the setting off.

## Everything is fine but the pages are slow

The Restore page reads the destination when it loads — that is a real `restic
snapshots` against remote storage. A cold read against SFTP takes seconds; a
warm one about a second. Other pages read local state and are instant.

## Restic output for a specific run

Logs → the run → its detail. Raw output is kept for the number of days set in
Settings.

## Rebuilding this server from nothing

You need three things: `/etc/gniza/master.key` (or the destination credentials
typed again by hand), the **recovery key** for the destination, and the folder
name the old server used inside it. Then
[disaster recovery](restoring.md#disaster-recovery): system settings first,
accounts after.

If the master key is gone, the stored credentials are unreadable and must be
re-entered. If the recovery key is gone, the backups themselves are unreadable
by anyone, including you. Keep both somewhere that is not this server.
