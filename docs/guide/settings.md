# Settings

## How backups run

- **Jobs at once.** Staging rebuilds an account in full on local disk, so this
  is bounded by the staging volume, not by CPU. One at a time is the safe
  default on a busy server.
- **Path to restic.** For a restic that is not on the default path.
- **Certificate authority.** For a restic REST server with a private CA.
- **This server's name.** What it calls itself inside the repository, and the
  folder name a new destination defaults to. It is on the recovery card, and a
  disaster recovery needs it.
- **Keep raw output for *n* days.** restic's own output per run, for diagnosis.

## Safety switches

- **Block account removal without recent complete copies.** See
  [Accounts](accounts.md#termination-safety). Turning it on requires a new
  successful backup first.
- **Back up on suspension.** See
  [Accounts](accounts.md#suspension-preservation).

## Notifications

Reporting a bug is not configured here and has no settings: see
[Reporting a problem](troubleshooting.md#reporting-a-problem). Backup and
restore notifications are separate from it:

Channels, each of which can be enabled or disabled without being deleted:

| Kind | Needs |
|---|---|
| **Email (SMTP)** | server, port, username, password, from, to |
| **Telegram** | bot token, chat id |
| **ntfy** | server, topic |

What gets reported is set per schedule: a run that did not finish in time, an
account with no backup for too long. See [Schedules](schedules.md#alerting).

Each channel picks the events it wants. **A backup or restore started** is the
one to be deliberate about: it fires as work begins rather than when it ends,
so on a server with a nightly schedule it is one message per account per night.
It is off unless chosen, and it is there for the operator who wants to know the
moment a restore into a live account begins.

## How long a deleted account's backups are kept

Ninety days by default, then they are removed from every destination and the
account's history here goes with them. Nothing else ever removes them:
retention thins a series but never empties it, so without this a server holds
the backups of every customer who has ever left, forever.

The choices are never, 30, 90, 180 days, a year, or a number you type. An
account whose name is back on the server is left alone — whoever holds it now
is not the customer who left. One can always be removed sooner by hand, under
Restore → Deleted accounts.

## Staging

Where an account is rebuilt before upload, and how much room is left there. A
backup needs room for one full account; the Overview shows the same figure,
because it is the thing that stops a backup at 02:00.

## Restored files waiting to be collected

Rebuilt archives and restored files that nobody has collected. They are not
removed on their own — a restore you have not looked at yet is not garbage —
so this is where you clear them out when you are done.

## What a backup contains

The global payload: home directory, databases, email. A schedule can exclude
more on top of this, never less.

Changing it changes what future backups hold. It does not change what existing
backups hold, and — with termination safety on — enabling that safety again
requires a fresh backup, because an old job record does not prove which
exclusions were in force when it ran.
