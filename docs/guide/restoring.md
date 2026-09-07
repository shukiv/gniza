# Restoring

Three tabs, because there are three different situations.

| Tab | The situation |
|---|---|
| **Restore account(s)** | the account is on this server, or was, and you want it back |
| **Deleted accounts** | cPanel no longer has it; the backups are still here |
| **Disaster recovery** | the machine is gone and this one is taking its place |

## Nothing is overwritten unless you ask

By default a restore **rebuilds** the archive and leaves it on this server for
you to inspect. Overwriting the live account, or handing the archive to
cPanel's own restore, is a separate tick.

Every one of those goes through a confirmation page first. It names the
account, the restore point, and what would be replaced — the databases you
ticked, the accounts in a bulk restore, everything in a basket — and says
plainly that there is no undo. Nothing runs until the box on that page is
ticked. The tick is a form field the server requires, not a browser dialog,
so it works with scripting switched off and a request that arrives another
way is refused too.

An account cPanel no longer has is the one case that reads differently: it
says the account is created again, because there is nothing on this server
to replace.

## How much room a restore needs

The staging volume is checked before anything is written, and a restore
that would not fit is refused rather than started. What is needed depends
on what the restore does:

| What you asked for | Room needed |
|---|---|
| **Restore one thing**, or picked files | about what those parts come to |
| Rebuild it for me to download | twice the account |
| Overwrite the live account | twice the account |
| The nightly rehearsal | once the account |

Twice, because cPanel copies whatever it is handed before it restores it,
and because a download has to be packed into a file as well as extracted.
Once, for a rehearsal, because it reads the account and hands nothing
over. The safety margin in Settings is added on top of all of these.

A restore of a folder is sized as a whole account, not as the folder: the
backup can say what a list of files comes to, but not what is under a
directory, and a guess that came out low would fill the volume halfway
through. Databases, DNS zones, certificates, cron jobs and FTP settings
are files, so those are sized exactly — which is what makes *Restore one
thing* possible on an account far larger than the free disk. A mailbox is
a folder of messages, so it falls back to the account's figure like any
other folder.

## Restore account(s)

Choose an account and a destination. The page then shows:

- **Accounts in this destination** — read from the backups themselves rather
  than from cPanel, so an account this server no longer has is in the list.
  Tick several and restore them together; they still run one after another.
- **Available backups** of the chosen account — pick one, or press *Pick files
  from it* to browse what is actually inside and tick the paths you want.
  Folders come back whole, and what you pick stays picked as you move between
  folders.
- **Restore one thing** — a single mailbox, database, DNS zone, cron job, SSL
  item or FTP setting, taken out of a backup without rebuilding the account.
  What comes back is left on this server to collect.

  Every part says what the backup actually holds in it. The home directory,
  the mailboxes and the databases are listed straight out of the snapshot; the
  rest of an account is inside the cpmove archive, or inside one SQL file
  beside the dumps, so the container is streamed out of the repository and
  read. Nothing is restored to answer the question, and one reading serves the
  whole visit. DNS zones, certificates, domains and database users can be
  ticked one at a time; cron jobs and FTP logins are listed to be read, since
  each is lines inside a single file.

- **Which backup** — the *Restore point* field beside the destination takes a
  date, and each account gets its newest backup taken on or before the end of
  that day. Leave it empty for the most recent there is. A date rather than a
  snapshot because several accounts are restored at once and their backups are
  not taken at the same minute; an account with nothing that old is named and
  the others still go.

- **Accounts that are gone** are in the same list as the ones that are here,
  shaded and labelled *deleted* — an account cPanel no longer has, whose backups
  are still on the destination. One belonging to a different server altogether
  reads *another server*. They used to be a tab of their own; which of these is
  gone is a question about a list, not a reason to navigate. The list is drawn
  from what this server remembers as well as what the destination says, so a
  deleted account stays visible when the destination cannot be reached.

- **Forgetting a deleted account** — a customer has gone and asked for their
  backups to go with them. **Forget…** on a shaded row removes
  every backup of that name from every destination, and this server's record
  that the account existed, its history with it. It asks first, on the page, and
  only for a name cPanel no longer has. Nothing here brings any of it back; the
  space comes back at the next prune.

  It also happens on its own: a deleted account's backups are kept for
  **90 days** by default and then forgotten, because nothing else ever removes
  them — retention thins a series but never empties it, so a server otherwise
  accumulates the backups of every customer who has ever left. The period is
  under Settings: never, 30, 90, 180 days, a year, or a number you choose. A
  name that is back on the server is left alone: whoever holds it now is not
  the customer who left.

- **Cron jobs** can go back into the account. The whole crontab is replaced,
  because that is what the backup holds and what cron reads, so a job added
  since the backup was taken goes with it — the crontab being replaced is
  written to `/var/lib/gniza/replaced/crontab-<account>-<when>` first, because
  the staging directory an applied restore used is removed when it finishes.
  It goes in through `crontab`, which checks the syntax: a file copied into
  place with a line cron cannot read is a crontab cron ignores in full.

- **A dropped database** comes back whole: the database is made again before
  the dump goes into it, created as the account so cPanel applies the plan's
  database limit and the account's name prefix and records it as theirs. An
  account already at its limit is told so, and nothing is written.

  Granular SQL import uses a temporary login restricted to that database.
  Views, routines, triggers, events and DEFINER objects are currently refused;
  the importer never retries them with root privileges. A failed import can
  already have changed tables in the selected database. Database-user restore
  also refuses an existing server login unless cPanel positively records it as
  this account's; unreadable ownership records must be repaired first.

- **The basket** — *Add to basket* on any part collects what was ticked, and
  the basket at the top runs all of it as one restore. The parts of an account
  depend on each other: a database restored without the users that open it is
  a site that still cannot start, and one account may only have one job
  running at a time, so two restores meant a gap in between. Everything is
  checked before any of it is written. A basket carrying a part that cannot be
  written back is a copy to collect, whole — the page names the part
  responsible. It belongs to one account and one backup, and is forgotten
  after a day. This basket is WHM's own: the customer's recovery centre keeps
  a separate one, so a part only WHM offers never lands in a basket they
  cannot start.

Restores run as restricted restores by default, because the archive holds the
account's own home directory and cPanel's restore runs as root. The
**unrestricted** tick exists for the case where a restricted restore refuses
something the account legitimately had.

## Deleted accounts

Accounts removed from this server that still have backups here. Gniza knows
they existed because it recorded the cPanel removal event, and it only offers
names that actually have a successful backup.

Restoring one hands the archive to cPanel's own restore, which **creates the
account again** — files, databases, mail and settings as they were. Nothing is
overwritten, because the account is not there to overwrite.

Tick several and restore them in one go; the destination they come from is the
select in the bulk bar.

## Disaster recovery

For a server that is gone. Attach the destination the old server wrote to, plus
that server's **recovery key** — without the key nothing can read those backups,
which is the point of the encryption.

Once attached, its accounts and its system backup show up. Restore **the
server's own settings first**: an account restored onto a machine that has not
been set up lands somewhere that cannot serve it. Then restore the accounts
from the *Restore account(s)* tab.

## Watching one run

Every page carries a strip at the top saying what is happening now. A
restore appears there the moment it is asked for — as **waiting**, because
an account runs one job at a time — and then as it runs, naming the stage
it is on: reading the account settings, unpacking them, reading the home
directory, reading the databases, building the archive, handing that
archive to cPanel's restore.

A bar sits under the stages restic can count. It does not appear on the
others, because a bar that cannot move is read as a restore that is stuck.
The percentage belongs to the stage, not to the whole restore: a split
backup is several reads, each counting from zero, and the stage beside the
number is what says so.

The page refreshes itself while anything is running and stops when nothing
is. With scripting switched off, reloading shows the same thing.

## Where restores show up

On the [Logs](logs.md) page, under **Restores**: what was asked for, what
happened, and either the error or where the rebuilt archive is.

## One known fidelity note

A sparse file comes back fully allocated. `pkgacct`/tar do not preserve
sparseness, so a 20 MB sparse file restores as 20 MB of real blocks. The
contents are identical.
