# Logs

Everything Gniza has done on this server, split by the kind of work.

| Tab | What is in it |
|---|---|
| **Backups** | account backup runs: what was stored, what was new, how long it took |
| **System backups** | the server's own settings — EasyApache, packages, tweak settings |
| **Restores** | restores and rehearsals, with the archive path or the error |
| **cPanel events** | what cPanel told Gniza as accounts were created, renamed, suspended or removed |
| **Service log** | what the service itself wrote, read back from the journal |

## While something is running

Every page carries a strip at the top naming what is happening now — the
account, whether it is being backed up or restored, and restic's own
percentage where there is one. It keeps itself up to date, and it is on every
page because somebody who asks for a restore and sees nothing asks again. A
customer's own page shows the same strip for their account and nothing else.

Notifications can say it too: **A backup or restore started** is one of the
events a channel can subscribe to under Settings. It is off unless asked for —
on a server with a nightly schedule it is one message per account per night —
and it exists for the operator who wants to know the moment a customer's
restore begins.

## Searching

The box above the tabs searches all of the history, not only the rows a tab
shows. Every word typed has to be on the row somewhere, in any case: its
account, its schedule, its date as the row writes it (`2026-09-14`), a
destination's name, a snapshot id, a database, a path, its result, or what
went wrong. `offsite failed` finds the runs that failed at the destination
called offsite; a snapshot id finds the run that made it. The tabs keep the
search as you move between them and count what matches; **Show everything**
drops it. The service log has its own **Containing** box, which keeps the
lines holding every word.

## Reading a backup row

**What** says what the run backed up — files, settings, how many databases —
and the account's line carries the schedule it ran under. **Took** is from the
run's start to its finish. **Details** opens the whole of it: when it was
queued, started and finished; the paths restic read and the databases by
name; what the schedule left out; and for each copy the destination and where
it is, the snapshot id in full, how much was read and how much of it was new,
the files restic counted — new, changed, as they were — how long the upload
took, and anything restic said. A run from before Gniza recorded what a run
backed up says so; what it holds is in its snapshot, under Restore.

The size column reads like `6.2 MiB new of 152.5 MiB`: what actually left the
server, out of what the account holds. With split mode an unchanged account is
almost entirely the first number being small.

A **partial** run is one where some accounts succeeded and others did not. The
detail says which.

**not backed up** on a row means the run stored the account but could not take
one particular thing — almost always a database that would not dump. The line
beside it names what and says why. The rest of the account was stored, and the
run is not a failure; but it is not a complete account either, so
[termination safety](accounts.md#termination-safety) will not accept it as one.
See [a database that will not dump](troubleshooting.md#a-database-that-will-not-dump).

## Clearing the logs

**Clear logs…** removes history from this server: backups, system backups,
restores and account events, each ticked or not. It cannot be undone. The
backups themselves are not touched — every snapshot stays at its destination
and restores as before.

A few rows stay, because they are more than a log. The overview says an
account is protected, [termination safety](accounts.md#termination-safety)
lets a panel remove one, and a schedule says how its last run went, all out of
these same rows; emptying them would turn every account unprotected and block
every removal until the next night. So a clearing keeps the last run of each
account under each schedule, whatever it came to, and the last good copy of
each account at each destination; the last rehearsal; a rebuilt archive still
waiting to be downloaded; and anything still running. A snapshot marked as
having unreadable files keeps that mark on the Restore page after the run that
made it has gone. The message afterwards says how many went and how many
stayed.

Customers see their own history on their own Logs tab, and it is this same
history: what is cleared here is gone there. The service log is the system
journal's, kept and rotated by journald, and is not cleared from here.

## cPanel events

A removal marked **Blocked** is Gniza refusing to let cPanel delete an
account it has no complete copy of — [termination safety](accounts.md#termination-safety)
doing its job. **Allowed** with a note means the copies were there.

If the service was down when cPanel called, the event says so and the removal
was allowed: cPanel administration is never wedged by a stopped backup service.

## The service log

The first four tabs are Gniza's own record of work it did. **Service log** is
the other thing: the lines the service wrote to the journal, the same ones
`journalctl -u gniza` would show, read back without a shell.

Five controls narrow it.

| Control | What it does |
|---|---|
| **Show** | a level and everything more severe — `warn` shows warnings and errors |
| **From** | Last hour, Today, Last 7 days, or everything the journal still keeps |
| **At most** | 200, 1,000, 5,000 lines, or all of them |
| **Containing** | keeps only the lines holding every word typed, in any case |
| **Account** | keeps only the lines naming that account, so one backup's story is one filter away |

The account filter matches the whole name: filtering on `studio` does not
also show `studio2`.

**Keep up with it** re-reads every few seconds while you watch, and the box
stays at the newest line. Following is capped at 1,000 lines however **At
most** is set, because re-reading a whole journal every three seconds is not
watching, it is hammering.

**Download all of it** hands over the whole of what the journal keeps for the
span chosen. The box on the page is the tail of that.

Nothing on this page is filtered for secrets. It is this server's own log,
behind the root-only socket, read by somebody who could run `journalctl`
anyway — and a log read to debug a credential is no use with the credential
taken out. A bug report is the other way round, and is redacted, because it
leaves the server. See
[Troubleshooting](troubleshooting.md#reporting-a-problem).

### How much it says

**Settings → Log level** decides. It takes effect at once, with no restart,
which is the point: the reason to turn `debug` on is usually something going
wrong now, and a restart would end it.

At `debug` the service writes down what it actually ran — every `pkgacct`,
`mysqldump` and `restic` invocation with its arguments and how long it took,
which schedule was due and which was not, which accounts it resolved to by
name, each destination probe, and the staging and upload of each account.
Enough to reproduce a failure by hand.

`pkgacct` and `mysqldump` lines carry the account, so the account filter
keeps them. `restic` lines do not: that runner works on repositories and is
told nothing about whose account it is copying. Filter by account for the
story, then clear the filter to see the `restic` command beside it.

What is never written down, at any level: restic's environment, the value of
any `-o` backend option, and any login embedded in a repository address. See
[Troubleshooting](troubleshooting.md#log-level).

## Sorting and paging

Every column with an order worth having is sortable — click the header, click
again to reverse. Newest first by default. Rows per page: 20, 50 or 100, and
the choice is remembered in this browser.

Raw restic output is kept for the number of days set in
[Settings](settings.md).
