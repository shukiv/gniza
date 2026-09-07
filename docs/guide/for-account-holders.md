# For account holders

Your customers get a tile in cPanel: their own restore points, and nothing
else.

## What they can do

Restore point → category → item, with a basket to gather items from several
categories into one restore. The categories are:

- a full cPanel account archive
- the home directory
- cron jobs
- databases and database users
- domains and DNS
- SSL material
- mail
- FTP settings

Each of them can be downloaded as a **private package**. Six of them can also
be **put back into the live account**: the home directory, the website, mail,
a database, the database users and the cron jobs. Those are the ones where the
backup holds everything needed to make the account whole again, and the
customer is told plainly, and has to tick a box, before the live copy is
replaced.

Cron is replaced whole, because an account's jobs are lines in a single file
and that file is what cron reads. A job added since the backup was taken goes
with the rest; a copy of the jobs being replaced is kept on the server, so a
host can put one back.

The rest stay download-only. Not because putting them back is impossible, but
because each needs a change the control panel makes rather than a file copied
over another — a DNS zone, an installed certificate, an FTP login, the
account's own settings — and that is not built yet. So does the full
account archive, whatever else is selected: that one goes to cPanel's
`restorepkg`, which runs as root over a home directory the customer controls,
and it remains an operator's decision.

Putting the home directory back is done as the account, never as root: the
service reads the staged copy, which the customer cannot, and the customer's
own process writes into their home directory. What lands there is owned by
them, and a link planted beforehand leads nowhere they could not already
write. A database is loaded only after the server, not the backup, confirms
the account owns it.

## What is in a restore point

Every category says what the backup actually holds in it, so somebody who
deleted a zone this morning can see whether it is there before restoring
anything. The home directory, the mailboxes and the databases are listed
straight out of the snapshot. The rest of an account is inside the cpmove
archive, or inside one SQL file beside the dumps, so the container is
streamed out of the repository and read — nothing is restored to answer the
question, and one reading serves the whole visit.

DNS zones, certificates, domains and database users can be ticked one at a
time; leaving every box empty means all of them. Cron jobs and FTP logins are
listed to be read rather than ticked: each is lines inside a single file, and
handing back part of one would hand back a file that is not the one in the
backup. The FTP list shows the login name and its directory and never the
password hash beside them.

## The Logs tab

Three tabs now: **Overview**, **Restore & download**, and **Logs**. The last
one answers the question customers actually open the tile to ask — *was my
site backed up last night* — without anybody having to ask their host.

It is their own account's record and nothing else. Two tables:

- **Backups of your account**: when each run happened, whether it worked,
  and what is **not in this backup**. A run that stored everything but one
  database reads as *Backed up, without everything* rather than as a
  success or a failure, because it is neither, and it does not claim to be
  a restore point for the whole account.
- **Recovery activity**: the same restores the overview shows, in full
  rather than only the ones with a package still waiting to be collected.

What a customer is *not* shown is the reason something was left out. That
line is command output — it names the server's paths, the destination's
address and the software that failed — so only the thing itself survives:
`database studio_wp`, never the `mysqldump` line under it. The same rule
already governs a failed restore, which reaches the customer as a sentence
they can act on rather than the operator's diagnostics.

The server's own log is not here and never will be. That is what
[Logs](logs.md) in WHM is for, behind the root-only socket.

## Watching one run

The customer's page carries the same strip as the operator's, showing only
their own account's work: waiting first, because an account runs one job at
a time, then the stage it is on with a bar under the stages that can be
counted. The page keeps itself up to date while anything is running.

## The recovery basket

The parts of an account depend on each other. A database restored without the
users that open it is a site that still cannot start, and an account may only
have one job running at a time, so asking for them separately meant one
restore waiting on another with a gap in between where the database was back
and nothing could open it.

**Add to my basket** on any category collects what was ticked. The basket is
shown above the categories with a Remove on every row, and one pair of buttons
starts the whole thing: download it as a single package, or put all of it back.
Everything in it is checked before any of it is written, so a basket whose
users would be refused does not leave the database replaced and the users
missing.

A basket goes back whole or not at all. One download-only part in it — a DNS
zone, a certificate — makes the whole basket a download, and the page names
the part responsible rather than leaving it to be guessed at.

The basket lives on the server, not in the browser, because the recovery
centre works with scripting switched off and choosing runs across several
plain page loads. It belongs to one account, one destination, one restore
point and the page it was made on: a basket assembled out of Tuesday's backup
is not a basket out of Monday's, and an operator's basket in WHM is not the
customer's, because WHM offers parts of an account this page does not. It is
forgotten after a day, and immediately if the account is removed.

## Database users

A database restored without the user that reads it is a site that still cannot
start, so the users come back with the passwords they had rather than as names.
Three things have to happen and no single interface does all of them:

1. **MySQL** is given the login and the stored password hash. cPanel's own API
   needs the password in the clear, and nobody has it — a backup holds the hash.
2. **`dbmaptool`** records that the account owns the user. Without it MySQL has
   a user the panel has never heard of: it does not appear under MySQL
   Databases, and the customer cannot change or delete it.
3. **cPanel, as the account**, gives back the privileges, so the panel's own
   record of who may read what is written too — and so cPanel refuses a
   database that is not the account's, whatever this program believed.

The account's own MySQL user is the exception: it owns that record rather than
appearing in it, so cPanel refuses both of the last two steps for it, and its
grants are written directly instead. Its **password is not put back**. cPanel
keeps that login in step with the cPanel account password, so restoring the
backup's password would set the two out of step — and would undo a password
change made because the old one leaked, which is what a restore must not
quietly do.

Two checks are made before anything runs, and either one fails the whole
request rather than skipping an entry: no other account may currently hold the
user name, and every database granted must be one the account has, or one this
same restore is about to put back. A restore that quietly left out the grant an
application connects with is a restore that reports success and leaves the site
down.

Restoring the users before the database they read is the order a customer will
try, and it is answered with "add that database to this restore, or create it
first" rather than with a failure they cannot act on.

## A database that is no longer there

Dropping a database and wanting it back is the case this was built for, so a
database the account no longer has is made again before the backup's copy goes
into it. It is created as the account, which means cPanel applies the plan's
limit on how many databases the account may have, applies the account's name
prefix, and records the new database as theirs — one made any other way would
be a database MySQL has and the panel does not, which the customer could
neither see nor delete. An account already at its limit is told so, and nothing
is written.

## Who is let in

The account owner, and cPanel Administrator Team users with the right
capability. The root service verifies the live cpsrvd session itself and hands
the proxy a 20-second, one-use token for that exact request — sharing the
account's Unix uid is not enough to use the public socket.

A WHM root administrator arriving through cPanel's **Log in to cPanel** flow is
accepted only when the root-owned session record carries cPanel's temporary-user
and cPHulk markers.

See [ADR 12](../adr/0012-the-account-facing-socket.md) and
[ADR 13](../adr/0013-cpanel-session-capabilities.md).
