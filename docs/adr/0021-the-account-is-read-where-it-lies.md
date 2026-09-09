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
decision about their panel, not Gniza's, and Gniza does not set it.

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
   been run on a server that was restored from.

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
  depending on what the server was found to support.
- The staging estimate is the whole account's size, which is more than
  a backup in this shape writes. What is left in the archive is the
  messages and the database dumps, and nothing has measured either;
  DirectAdmin's own accounting does not answer it, because `user.usage`
  records `email_quota` as what the mailboxes were allotted rather than
  what they hold -- 157,260,176 against 14 MiB of messages on the
  validation host. Reserving too much refuses a backup on a full disk;
  reserving too little fills one. The drill measures a real archive
  against the account it came from, and the estimate follows.
- Gniza reads a home directory DirectAdmin's own archive would have
  filtered. It skips what DirectAdmin skips — `backups/`,
  `user_backups/`, `admin_backups/` — and, unlike DirectAdmin, it also
  skips the caches that are regenerated on their own: CloudLinux's
  `.cagefs` was 3.2 GiB of one 10.8 GiB account on the validation host.
