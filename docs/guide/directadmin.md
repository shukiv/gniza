# DirectAdmin

Gniza runs on cPanel/WHM. DirectAdmin support exists and is unfinished,
and this page says exactly how far it goes so that nobody finds out from
a restore.

## Validation status — 2026-09-08

A disposable account now passed the **Gniza provider → restic repository →
reassembly → Gniza provider** round trip on DirectAdmin 1.709. Its removed
website file was restored, and independent checks confirmed its files,
database rows, mailbox message and cron matched the original baseline.
See the [provider validation report](../directadmin-provider-validation-2026-09-08.md)
and the earlier [native-platform report](../directadmin-validation-2026-09-08.md).

The admin and account plugin pages are placeholders, not a working
configuration or restore interface. Treat this package as development work;
do not enable backup schedules for production accounts or rely on it for
recovery yet. Pushing the source does not publish a DirectAdmin release.

## Implemented paths, not yet certified

- **Account lifecycle.** DirectAdmin's own hooks tell Gniza when an
  account is created, suspended, reactivated or removed. This is what
  keeps one customer's backups from being handed to the next holder of
  the same username. The scripts exist, but lifecycle isolation has not
  been validated on the live host.
- **Listing accounts.** Every account DirectAdmin knows about, its home
  directory, its primary domain, and whether that home directory is
  actually there.
- **Whole-account backup adapter.** The provider calls
  `directadmin admin-backup` and returns its archive to the shared backup
  worker. It uses a fresh per-account native workspace, verifies the task
  transcript and archive identity, and copies into root-private staging.
  `.tar`, `.tar.gz` and `.tar.zst` identity validation is supported.
- **Explicit native overwrite.** The provider can restore a whole archive
  over an existing ordinary user account, with both `Overwrite` and
  `Unrestricted` explicitly enabled. It executes one `taskq --run` task,
  never the server's shared queue. This is not cPanel restricted restore;
  native DirectAdmin is trusted to apply the archive. Only restore archives
  you trust, under an administrator's explicit authorization.

  A restore that DirectAdmin reports as successful is checked afterwards:
  the account has to still be an ordinary user account of that name, and
  every database the archive names has to be on it. DirectAdmin restores
  an account in modules, and an account whose website answers and whose
  orders are gone is not the account back. Only the dumps DirectAdmin's
  own backup writes count, not a `.sql` file the customer left in their
  web root. Where the archive names no databases the check is quiet
  rather than wrong -- see question 8 in ADR 0019 for what that leaves
  open.

## What is refused

Gniza refuses these rather than guessing, and says so by name:

- backing up an account **in parts** — the payload shape that lets restic
  deduplicate, and the reason a nightly backup costs a fraction of the
  account's size;
- backing up **less than the whole account** — a schedule that excludes
  the databases or the mail. `directadmin admin-backup` takes a
  destination and a user and nothing else, so this is not a gap in Gniza
  to be filled in later: DirectAdmin has no such flag;
- **granular restoring**: one database, mailbox, file, or selected
  component. The paths these would use are now taken from a real archive
  rather than from documentation, and are checked against its listing, but
  a DirectAdmin backup is one archive rather than separate parts, so there
  is nothing for a granular restore to reach into yet. One domain's FTP
  logins and one domain's mail configuration cannot be asked for at all --
  see question 9 in ADR 0019;
- restricted restore, new-account disaster recovery, account renaming,
  certificate-isolated certification, or applying archives as admin/reseller accounts;
- backing up **the server's own configuration**.

Native backups on the tested host were compressed archives. Split-mode
deduplication has not been validated; every run may store close to a full
copy. Normal account-level self-service remains unavailable until the
session bridge and granular restore paths are implemented and validated.

Native temporary work is separate from service state, under
`/var/lib/gniza-directadmin-native`. This dedicated root is service-owned
mode 0711; each random job directory permits traversal only to its selected
account's group, and writing only to the native admin user. The account
cannot traverse Gniza's state/staging directories. Archive handoff is mode
0600. Per-account locks and disk-space estimates guard the native operation;
they do not coordinate with unrelated DirectAdmin/JetBackup jobs. Do not
run conflicting operations against the same account.

## Why

These paths need answers from a running DirectAdmin server — whether its
backup tool can be told to leave the home directory
out, how a queued restore reports that it failed, what the archive's
members are actually called. They are listed as six questions in
`docs/adr/0019-the-panel-behind-an-interface.md`, with a seventh about
who is allowed to read an account's backups through the panel. Some answers
are now recorded in the live validation report; implementing and testing
them in Gniza is still required.

A backup provider that guesses produces backups that look successful and
restore into nothing. That is the one failure this program exists to
prevent, so the guesses are refusals instead.

## Installing it anyway

For whoever is doing that verification:

```sh
make directadmin-package
# copy bin/gniza-directadmin-amd64.tar.gz to the server, then
tar xzf gniza-directadmin-amd64.tar.gz
cd gniza-directadmin && sh install.sh
```

It is not on the releases page, and that is deliberate: a package one
`curl` away is a package that ends up on a customer's server.

`uninstall.sh` deletes nothing, and neither does the cPanel one. The plugin directory, both binaries and
the unit file are moved into a dated directory under
`/var/lib/gniza/removed`, which the script names as it finishes, so an
uninstall can be undone by moving them back. Repositories, the state
database and `/etc/gniza` are left alone either way.

The service runs as `gniza-agent -standalone -panel=directadmin`, and
prints the remaining experimental limitations at startup. No production
deployment or signed DirectAdmin release is implied by the validation.
