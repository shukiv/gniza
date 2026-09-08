# DirectAdmin

Gniza runs on cPanel/WHM. DirectAdmin support exists and is unfinished,
and this page says exactly how far it goes so that nobody finds out from
a restore.

## Validation status — 2026-09-08

A disposable account passed a **native DirectAdmin** backup/restore round
trip. That did not run through Gniza. The test found staging-permission
requirements, failure results with exit code zero, and zstd/nested-home
archive layouts that still need to be handled in this integration. See the
[live validation report](../directadmin-validation-2026-09-08.md).

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
  worker. Its root-private staging assumptions need correction before
  the native command can work through the standard Gniza staging path.
  A Gniza-to-destination round trip has not been validated on DirectAdmin.

## What is refused

Gniza refuses these rather than guessing, and says so by name:

- backing up an account **in parts** — the payload shape that lets restic
  deduplicate, and the reason a nightly backup costs a fraction of the
  account's size;
- backing up **less than the whole account** — a schedule that excludes
  the databases or the mail;
- **restoring**, in any form: a whole account, one database, one mailbox,
  a file;
- backing up **the server's own configuration**.

Native backups on the tested host were compressed archives. Split-mode
deduplication has not been validated. Native DirectAdmin can restore the
tested archive, but Gniza's restore implementation remains unfinished.

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

`uninstall.sh` deletes nothing. The plugin directory, both binaries and
the unit file are moved into a dated directory under
`/var/lib/gniza/removed`, which the script names as it finishes, so an
uninstall can be undone by moving them back. Repositories, the state
database and `/etc/gniza` are left alone either way.

The service runs as `gniza-agent -standalone -panel=directadmin`, and
says once at startup that its layout came out of documentation rather
than from a server.
