# Schedules

A schedule answers four questions: when, which accounts, what goes in, and how
long it is kept.

## When

A cron expression — `0 2 * * *` for 02:00 nightly. The page spells it back to
you in words, so a wrong field is visible before it is saved rather than at
04:00 on a Sunday.

## Which accounts

- **Every account on this server.** Accounts created later are included
  automatically. This is what makes the Overview's coverage figure mean
  something.
- **Only the ones I choose.** A named list. An account renamed by cPanel keeps
  its membership; a terminated name cannot quietly turn a one-account policy
  into an all-account one.

## What goes in

**Split (recommended)** stores the home directory and each database separately,
so an unchanged account costs almost nothing to back up again. **Single
archive** keeps one archive per run — simpler to reason about, far more
expensive to repeat.

You can leave things out: the home directory, databases, or email. What a
backup contains is also set globally in [Settings](settings.md); a schedule's
exclusions are on top of that.

**Include the server's own settings** adds a system backup — EasyApache,
packages, tweak settings, the things that turn a bare machine back into this
one. Restore those first in a disaster; an account restored onto a server that
was never set up lands somewhere that cannot serve it.

## How long it is kept

Keep *n* daily, *n* weekly, *n* monthly. Retention runs once the last backup has
finished, at most once in twenty hours for each repository, and applies to that
schedule's own group, so two runs of one account land in the same group and the
older one can actually be removed.

In standalone mode retention runs on the cPanel server, which means the
credential able to delete backups lives on the machine an attacker would
compromise. That is the deliberate trade standalone makes — see
[ADR 7](../adr/0007-standalone-mode.md).

## Alerting

Two thresholds per schedule: a run that has not finished within *n* hours, and
an account with no backup for *n* days. Both report through the channels in
[Settings](settings.md).

**Retry failed accounts** re-queues the ones that failed, rather than waiting a
day for the next run to try again.

## Disabling

A disabled schedule stays, with its retention settings and its history. It just
does not run. Deleting one does not delete its backups.
