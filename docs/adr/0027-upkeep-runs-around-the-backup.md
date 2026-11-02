# 0027 — Upkeep runs around the backup

Status: accepted, 2026-11-01.

## Context

Each server had its own timers for the work beside a backup: a destination check
and a look for stale locks every hour, a count of what a repository holds every
six hours, retention every twenty. Each of those is one or more logins on the
destination. On one server the backup host recorded 68 sessions by 19:10 on
2026-10-30, of which the backup was a handful, and the operator had a login alert
in their mail every hour for a backup that takes seconds.

The timers bought little. A destination check found out within the hour that a
destination had gone, and nobody was waiting on that between backups: the next
backup reports the same failure. The operator asked for this work to move to
before and after the backup. Excluding the two servers from the alert was
proposed and refused: the alert was right, and the logins were what was wrong.

## Decision

The upkeep belongs to the schedule.

- **Pre-task.** Just before a schedule queues its backups, each repository is
  looked at for stale locks. It has to come before the queueing: a queued backup
  counts as work in flight, and locks are not touched while there is any. It is
  bounded at two minutes a repository, and the backups go ahead whatever it finds.
  A look that did not reach every repository does not count as done, and is tried
  again when the next schedule fires.
- **Post-tasks.** Once nothing is running, in order: the destination check,
  retention one repository per scheduler tick so that anything queued meanwhile
  runs between them, the count, and the integrity check if one is due. Retention
  comes before the count so that the count is what is left, and the integrity
  check comes last because it wants the repository to itself.
- **Twenty hours.** Each step decides from what is stored whether it is due, and
  goes to a destination at most once in twenty hours. So a schedule firing hourly
  does not bring the hourly logins back, and a tick or a restart repeats nothing.
  Twenty rather than twenty-four so that a night which finishes earlier than the
  last still has its upkeep.
- **Thirty hours.** Following a backup is when the upkeep normally happens, and it
  is not what decides whether it happens. A destination or repository reading more
  than thirty hours old starts the upkeep by itself. That covers a server with no
  schedule, a schedule that fires and queues nothing, and a restart during the
  run. The first version of this asked whether a schedule had fired recently, and
  a review found that a schedule firing every night with nothing to queue would
  then stop the upkeep altogether.
- **A job that never finishes** holds back everything that touches the repository,
  and not an overdue destination check, which does not.

## Consequences

- About eight logins a day rather than about eighty. That figure is an estimate
  from the design and has not been measured on a server.
- A destination that goes away is noticed at the next backup, not within the hour.
- Free space and counts on the page can be a day old, so the page shows a
  reading's date after thirty-six hours rather than eighteen.
- Whether a backup has fired since the upkeep last ran is held in memory, so a
  restart forgets it. The thirty-hour rule is what catches that.
- A server does not reach this rhythm at once. Readings left by the old timers are
  hours old at the install, so the first night's post-tasks can skip at the twenty
  hour gate, and a server whose backup falls late relative to those readings can
  run its upkeep once or twice at another hour before it settles after the backup.
- Rehearsals are unchanged: they keep their own monthly schedule.
- The destination check still takes two sessions, a login and a `df`; merging them
  is `cprest-ax9`.
