# 0017 — A backup carries on past a database it cannot dump

Status: accepted
Date: 2026-09-07

## Context

A live account had 47 databases and one corrupt InnoDB table in one of
them. Reading that table aborted `mysqld`; `mysqldump` returned exit
status 2 with `Lost connection to MySQL server during query (2013)`.

Staging returned on the first failed dump, so the account had **no backup
at all for a week** — not its home directory, not the other 46 databases.
Every night the same table, the same abort, the same nothing stored.

The rule being followed was deliberate and is written down in
`pkgacct.Payload.Verify`: a payload missing a part is not a backup. It
came from a real incident where `pkgacct` wrote nothing, restic warned
about a path it could not read and carried on, and a snapshot holding no
account configuration was recorded as a success. That rule is right for a
missing part. Applied to one database out of 47, it destroys far more
value than it protects.

The obvious alternative was to hand the whole job to cPanel's own
`pkgacct`, which does not abort on a bad database. It was measured on the
affected account before deciding:

| | |
|---|---|
| databases on the server | 47 |
| dumps in the archive | 16 |
| missing entirely | 31 |
| `2002 Can't connect` errors | 33 |
| exit code | **0**, `pkgacct completed` |

`pkgacct` survived the crash, lost every database that came after it —
each failing with `Can't connect to local MySQL server` while the server
restarted — and reported success, having itself printed `mysqlsize is:
4915611017` beside a 1.47 GB archive.

That is the failure this decision has to avoid. An archive that looks
complete and is missing two-thirds of the data is worse than no archive:
nobody investigates a success.

## Decision

A backup carries on past a database it cannot dump, and says so.

**Per-database failures are collected, not returned.** Each one becomes an
`Omission` on the payload — what, and why, with mysqldump's own words. The
remaining databases and the home directory are staged and uploaded as
normal.

**The failed dump is deleted.** A zero-byte `.sql` left in the staging
directory restores as a database with no tables and says nothing about
it. Absence is recoverable; a convincing empty database is not.

**A failure that is really the server going away is not charged to one
database.** After a failed dump the server is pinged; if it has gone, the
run waits up to 90 seconds for it to answer and retries that database
once. This is precisely what `pkgacct` does not do, and it is the
difference between one missing database and thirty-one.

**The run is announced as short.** The omissions reach the job record, the
backup row, and the notification — whose subject changes, so a run with a
hole does not read as an ordinary success.

**The run is not a complete account.** `CompleteAccount` is false whenever
anything was left out, so
[termination safety](0014-account-termination-safety.md) refuses it as
authority to delete the account, exactly as it refuses a run made with
payload exclusions in force.

## Consequences

A backup can now succeed while being incomplete, which is a state the job
model did not previously have: target rollup says where copies went, and
says nothing about what went into them. Two questions, two answers, both
on the row.

Anything that treats `status = success` as "this account is safe" is
wrong from here on. The one place that mattered — termination safety —
reads `CompleteAccount` and was already correct.

An operator who ignores the notification gets a backup missing a database
and no further prompting. That is accepted: the alternative was a backup
missing everything.

The retry-and-wait costs up to 90 seconds per crash on an account whose
tables abort the server. On a healthy server it costs one `SELECT 1`
after a failure that did not happen.

Not adopted: dumping databases through `pkgacct` instead of directly.
Beyond the measurement above, its dumps land inside the metadata archive,
which would cost per-database granular restore — restoring one database
would mean fetching the whole archive — and the schema/grants handling
Gniza does per database.
