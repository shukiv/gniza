# 0021 — The account is read where it lies

Status: accepted
Date: 2026-09-09

## Context

On cPanel, `split` mode hands restic the account's home directory as a
path: `pkgacct` writes a small metadata archive with the home directory
skipped, the databases are dumped one file each, and restic reads
`/home/<user>` in place. Restic then does what restic does — it compares
each file against the parent snapshot and reads only the ones whose size,
mtime or inode moved.

On DirectAdmin the same mode reads nothing in place. The only way in was
`directadmin admin-backup`, which writes one archive of the whole
account, so `split` meant: DirectAdmin reads the account and compresses
it into an archive, Gniza copies that archive into staging, unpacks it
into a tree, deletes the archive, and points restic at the tree. Every
night, for every account, whether or not one byte changed.

The cost, per account per night, is about three and a half times the
account read, twice the account written, two full compression passes and
one full hash pass. On the validation host that is roughly 435 GiB read
and 250 GiB written to store a few gigabytes of change.

Restic cannot even skip the unchanged files. `UnpackArchive` writes every
member with `O_CREATE|O_TRUNC` and never restores its mtime — the modes
and times live in the manifest, for repacking, not on the files — so
every file in the tree has a new inode, a new ctime and an mtime of
"now". Restic compares exactly those, and reads and re-chunks the whole
account. Deduplication saves the upload. It saves nothing else.

## What DirectAdmin can actually be asked for

The administrator's backup page has a fourth step, "What", which is
either all of an account or a chosen set. The values are the ones its
own form names:

    domain  subdomain  email  email_data  emailsettings  forwarder
    autoresponder  vacation  list  ftp  ftpsettings  database
    database_data  trash

and the page posts them as `what=select&option0=…&option1=…`. The same
task line runs through `directadmin taskq --run=`, which is how Gniza
already performs a native restore. So the selection is per run and per
account: nothing in `directadmin.conf` changes, and the server's own
backups are not affected.

Two runs against the disposable fixture on 2026-09-09 settled what the
values mean; both listings are checked in at
`internal/layout/dabackup/testdata/lean-account.tar.list`.

**Leaving out `domain` takes `domains/` and the nested
`backup/home.tar.zst` with it.** Both are the account's own files, and
both are in `/home/<user>` where restic can read them. The archive went
from the whole account to 153,600 bytes.

**Leaving out `email` as well takes `imap/`, and too much besides.** It
also removes `backup/<domain>/email/passwd`, `email/quota`, the per-
mailbox `data/limit` and `data/imap` records and the webmail settings —
the mailboxes' own passwords among them, which are nowhere in `/home`.
So `email` stays, `imap/` arrives with it, and Gniza drops `imap/` from
the metadata part after unpacking: the messages are read from
`/home/<user>/imap` in place like everything else.

What is left of the round trip is DirectAdmin reading and compressing
the mail once. Across the validation host's 142 accounts that is 31.7
GiB of the 248.5 GB under management — about an eighth. The global
`skip_imap_in_backups=1` would remove it, at the price of the server's
own backups losing their messages too. That is an administrator's
decision about their panel, not Gniza's, and Gniza does not set it. The
administrator of the validation host was asked on 2026-09-09 and left it
off, because nothing has established whether DirectAdmin's restore reads
the same setting: if it does, the `imap/` a rebuild hands it would be
ignored and the messages would not come back. An eighth of one night is
the cheaper side of that question until someone answers it.

## The global switches are not this

`directadmin.conf` has `skip_hometargz_in_backups`,
`skip_domains_in_backups`, `skip_imap_in_backups` and
`skip_databases_in_backups`, all `0` by default, and they leave the same
things out of an archive that the selection above leaves out. They are
not the same decision. They change every backup the server makes, its
own included, and an administrator who set one would find the panel's
own backups no longer carrying what they used to. The selection is per
run and per account, posted as `what=select` on the task line; nothing
in `directadmin.conf` changes and the server's own backups are
untouched. Gniza uses the selection and does not set the switches.

## Decision

1. A DirectAdmin backup asks for every option except `domain`, through
   `taskq --run`, and takes the account's own files from `/home/<user>`
   as a path handed to restic — the same shape cPanel's `split` mode has
   always had.
2. `imap/` is dropped from the metadata part when the archive is
   unpacked, because those messages are under the home path already and
   storing them twice is what this exists to avoid.
3. The capability is learned, not assumed. The first run on a server
   checks the produced archive: if `domains/` is in it, DirectAdmin
   ignored the selection, and Gniza says so and stays on the whole-
   archive path rather than silently backing up half an account.
4. A restore rebuilds what DirectAdmin will read. The tree restic
   restores is a copy of the home directory, so `PackArchive` has to
   synthesise the `domains/`, `imap/` and `backup/home.tar.*` members
   from that tree's own stat rather than from a manifest that never
   described them, and rewrite `backup/backup_options.list` to the full
   set so DirectAdmin's restore does not skip what it is being handed.
   A file reached under two names is carried as a link to the first,
   once per archive, because DirectAdmin's own backup does that and an
   archive that did not would restore a linked Maildir at twice its
   size.
5. Nothing ships until a restore drill on a real archive proves point 4.
   A backup that cannot be restored is worse than an expensive one. So
   the shape is asked for one server at a time, through the agent's
   `-directadmin-read-home-in-place`, and a server that was not asked
   keeps writing the whole account to disk as it did before. The flag
   goes away, and the shape becomes the only one, when the drill has
   been run on more than the one server it has been run on.

## The drill — 2026-09-09

Run on the validation host against the disposable account `gzv0908a`:
backup, rebuild, and DirectAdmin's own restore over the live account.
It found two things, both of which only exist because the home
directory is now read where it lies, and both of which would have made
every backup on the server unrestorable.

**The home part came back under the wrong name.** `restorePacked` put
each part back under `filepath.Base` of its snapshot path. That held
while both were directories Gniza staged -- `<staging>/metadata` and
`<staging>/home` -- and stopped the moment one became `/home/<account>`,
whose base is the account. The repack looked in `tree/home` and the part
was at `tree/gzv0908a`. The name now comes from the packer, which is the
only thing that knows where its own repack reads.

**DirectAdmin could not extract its own restore.**

    Error extracting /home/gzv0908a/backups/backup/home.tar.zst :
    /bin/tar: .jb-roundcube: Cannot utime: Operation not permitted

It extracts the home archive as the account. JetBackup keeps a directory
inside every account's home that belongs to root, so a faithful archive
of that home is one DirectAdmin refuses outright -- tar exits and takes
the run with it. It is left out now, like `.cagefs`: empty on all 142
accounts of that host, and JetBackup makes it again.

That directory is not the only one of its kind. A full walk of all 142
homes on the validation host, `find /home/<user> -xdev ! -uid <uid>`,
found root-owned files in six of them besides `.jb-roundcube`: a lone
`.htaccess`, an `info.php`, an uploaded `.zip`, and one account with
12,352 of them under a WordPress tree someone unpacked as root. Those
are the account's own site files, and leaving them out would be losing
data rather than skipping a cache, so they stay in.

The consequence is that those six accounts restore no further than the
first root-owned member: DirectAdmin extracts `backup/home.tar.zst` as
the account, and tar cannot chown to root. This change did not introduce
it -- the whole-archive shape carries the real owner of every file too,
from DirectAdmin's own tar or from the manifest a repack reads, so both
shapes meet the same member the same way. It is a property of
DirectAdmin's restore meeting a home root has written into, and it is
worth a check of its own: a backup that cannot be restored should say so
on the night it is taken, not on the day it is needed.

After both, the restore succeeded: `Account gzv0908a has been restored
from user.admin.gzv0908a.tar.zst under admin`, with its databases, its
messages, its domains and its dotfiles.

What the restore does not put back is a group the account is not in.
`.php/` came back `gzv0908a:gzv0908a` where the archive said
`gzv0908a:apache`, and `Maildir/` the same. The archive carried the
right names, and what the outer archive holds -- `imap/`,
`backup/.shadow` -- kept them, because that one is extracted as root.
The reading that fits all three, and the one the `utime` failure above
points to, is that DirectAdmin extracts the nested archive as the
account and the account cannot name a group it is not in. If that is
right the whole-archive shape loses them too, which has not been
measured; either way a DirectAdmin restore wants a group pass after it.

## Consequences

- The nightly cost of a DirectAdmin server falls to what cPanel's has
  always been: read the files that changed.
- The first night after the switch is a full read, because the paths in
  the snapshot change and there is no parent to compare against.
- Two shapes of snapshot now exist for the same account. Restore has to
  recognise which it has, and retention has to keep grouping them
  together rather than ageing the old shape out on its own. Retention
  groups on `host,tags` and never on paths, so the home part moving from
  staging to `/home/<user>` does not split a group.
- `stagingEstimate` can no longer key on the layout being an
  `ArchivePacker`, because the same layout now packs or does not
  depending on what the server was found to support. It asks the
  provider what the next backup will do, which is not the same question
  as what the last one did: a server asked for this shape is taken to
  honour the selection until a run finds it does not. Asking the
  narrower question reserved the whole account on every agent restart,
  and the first account of the night is as likely to be the largest as
  the smallest -- a large one would be refused before the run that would
  have settled the question.
- The staging estimate is what the panel writes in this shape -- the
  messages and the database dumps -- rather than the whole account's
  size. It was the account's size until the measurement below, which is
  what that paragraph was waiting for. DirectAdmin's own accounting does
  not answer it, because `user.usage` records `email_quota` as what the
  mailboxes were allotted rather than what they hold -- 157,260,176
  against 14 MiB of messages on the validation host. So `Account`
  measures the mail in the same walk that measures the home, and adds
  the database sizes it already asks `information_schema` for.
- Gniza reads a home directory DirectAdmin's own archive would have
  filtered. It skips what DirectAdmin skips — `backups/`,
  `user_backups/`, `admin_backups/` — and, unlike DirectAdmin, it also
  skips the caches that are regenerated on their own: CloudLinux's
  `.cagefs` was 3.2 GiB of one 10.8 GiB account on the validation host.

## The measurement — 2026-09-10

Run on `server-182-54-236-143.da.direct` against the live account
`pager`, which is 19.7 GiB with 12 GiB of application backups and 6.1
GiB of `domains/` in its home. One lean backup through
`taskq --run=`, 26 seconds:

| | bytes |
| --- | --- |
| the account, from `user.usage` | 19,707.5 MiB |
| the lean archive | 310,915,651 (296.5 MiB) |
| its members, uncompressed | 1,281,932,189 |
| of those, `imap/` | 732,924,695 |
| of those, `backup/*.sql` | 546,542,870 |

Nothing else was in it: 6,253 members under `imap/` and 318 under
`backup/`, and no `domains/`, no `home.tar.*`, no `trash`. The `trash`
option is in the selection and contributed nothing, because the file
manager's `.trash` is in the home directory and the home directory is
what the selection leaves out. That account's `.trash` is 1.3 GiB.

Two numbers the estimate can be built from, both already known before a
backup runs:

- The mail on disk, `/home/pager/imap`, is 733,507,893 bytes against the
  732,924,695 the archive carried -- 0.08% apart.
- `information_schema` reports 756,011,257 for the account's tables
  against 546,542,870 bytes of dumps: the dumps are 0.72 of it, because
  a dump carries no indexes.

So `LeanBytes` is the mail measured where it lies plus the database
sizes, which is 1,489,519,150 for this account: 16% over what the
archive actually held uncompressed, and 4.8 times what it held
compressed. Reserving the account instead demanded 39 GiB on a disk with
22 GiB free, and refused the backup.
