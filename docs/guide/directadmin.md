# DirectAdmin

Gniza runs on cPanel/WHM. DirectAdmin support exists and is unfinished,
and this page says exactly how far it goes so that nobody finds out from
a restore.

## What works

- **Account lifecycle.** DirectAdmin's own hooks tell Gniza when an
  account is created, suspended, reactivated or removed. This is what
  keeps one customer's backups from being handed to the next holder of
  the same username, and it is fully in place.
- **Listing accounts.** Every account DirectAdmin knows about, its home
  directory, its primary domain, and whether that home directory is
  actually there.
- **Backing up a whole account.** `directadmin admin-backup` writes one
  archive per account and Gniza stores it in every destination the
  schedule names, with the same retention, the same append-only
  protection and the same nightly check as on cPanel.

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

A DirectAdmin backup taken today is therefore a full copy every night,
and getting anything back out of it is a manual job with `tar` and
`mysql`.

## Why

Each of those needs an answer that only a running DirectAdmin server can
give — whether its backup tool can be told to leave the home directory
out, how a queued restore reports that it failed, what the archive's
members are actually called. They are listed as six questions in
`docs/adr/0019-the-panel-behind-an-interface.md`, with a seventh about
who is allowed to read an account's backups through the panel.

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

The service runs as `gniza-agent -standalone -panel=directadmin`, and
says once at startup that its layout came out of documentation rather
than from a server.
