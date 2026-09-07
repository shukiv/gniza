# Accounts

The Accounts page is coverage, not a list of usernames: which accounts are
promised a backup, which of those promises were kept, and which were not.

## What the states mean

| State | Meaning |
|---|---|
| **Backed up** | every destination a schedule promises has a recent complete copy |
| **Needs attention** | overdue, partial, or a destination whose copy is missing or stale |
| **Never** | no successful backup under this name |
| **Unscheduled** | no schedule covers this account at all — the quiet one that bites |

Coverage is measured against what the policies promise, per destination, on
that destination's own schedule. An old backup sitting in one place is not
coverage if a schedule says it should also be in another.

## Backing one up now

**Back up now** on the row, or from the account page. It queues the covering
policy; if several cover the account you choose which. Jobs run one at a time —
staging rebuilds an account in full, and a volume with room for one is not a
volume with room for nineteen at once.

**Repair copies** on the Overview runs the policy that fixes the most gaps,
after the service checks that the policy still covers the account.

## The account page

Everything known about one account: current state, its snapshots per
destination, and its activity — runs, restores and the cPanel events that
touched it.

## Termination safety

Optional, in [Settings](settings.md). With it on, cPanel account termination is
blocked while the account lacks recent complete copies at every destination a
full-account schedule promises.

- Enabling it requires a **new** successful backup: older job records do not
  prove which payload exclusions were in force when they ran.
- The check is local and synchronous, inside cPanel's hook. It never starts a
  long backup while WHM waits.
- **Prepare for termination** queues the smallest set of schedules that
  refreshes every missing destination. It never deletes the account.
- If the service is down, the hook logs it and *allows* removal, so cPanel
  administration cannot be wedged by a stopped backup service.
- A run that stored the account but could not take one of its databases is
  not a complete copy and does not count here, however successful it looks.
  The row says [not backed up](logs.md#reading-a-backup-row) and names what
  is missing.

The Accounts list and the account page preview the same decision, so you learn
the answer before you open cPanel's termination flow rather than during it.
See [ADR 14](../adr/0014-account-termination-safety.md).

## Suspension preservation

Also optional. When cPanel suspends an account, Gniza queues the smallest
set of enabled full-account schedules needed to reach every destination those
schedules promise, then returns without waiting for `pkgacct` or the network.
Repeated suspension events add no work while that account already has a job
queued or running. Unsuspension is recorded but queues nothing.
See [ADR 15](../adr/0015-suspension-preservation.md).

## Renames and recycled names

cPanel hooks record who an account was: its uid, when it appeared, when it was
retired. So a renamed account keeps its history, and a username handed to a new
customer does not inherit the previous customer's backups.
