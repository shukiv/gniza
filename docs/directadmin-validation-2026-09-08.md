# DirectAdmin native backup/restore validation — 2026-09-08

## Result and scope

**PASS: a native DirectAdmin backup restored the disposable account's test
files, database rows, mail message and cron entry to their recorded baseline.**

This is a native-platform validation, **not** a successful Gniza end-to-end
restore or certification of its DirectAdmin provider. Gniza was not installed,
started, upgraded or deployed. Its Go source was not changed and its Go test
suites were not rerun for this exercise. The checkout was `0e811f8`.

The user authorized creating a disposable account and backing up/restoring
that account on `root@182.54.236.10`, and explicitly identified the host as
production. No existing customer account was selected for backup, restore,
deletion or modification. No installers, manual service restarts, account
deletions, lifecycle-hook installations or global task-queue processing were
performed. DirectAdmin's normal account creation/restore operations were used.

The earlier hosts could not accommodate a new account: `.143` reported its
2-account license full, and `.22` reported its 1-account license full. Neither
attempt left a partial fixture account.

## Host and fixture

- Server: `182.54.236.10` (`uscp`).
- DirectAdmin: `1.709`, build `65ac6839c8e47738f160be5f8cc6c883941185e8`.
- Account: `gzv0908a`; UID observed: `1168`.
- Domain: `gzv0908a.gniza-test.invalid`; no public DNS registration or welcome
  email was requested.
- Database and database user: `gzv0908a_shop`.
- Mailbox: `sales@gzv0908a.gniza-test.invalid`; fixture message saved locally
  through Dovecot, not sent through SMTP.
- Cron: `17 3 * * * /bin/true # gniza-disposable-validation`.
- Account quota: 256 MB; initial home-directory usage was about 60 KB.

Data was synthetic but actually stored in the hosting services: a website,
private CSV file, three products, three orders with a foreign-key relationship,
one binary database value, one mailbox message and a cron entry. Text included
Hebrew, accented Latin characters and Japanese. Account, database and mailbox
passwords were generated and kept in a root-private validation directory; no
passwords or API credentials are included here.

Before the restore, the fixture's website and private CSV were moved aside,
its message was moved aside, database values were changed and an order row was
deleted, and the fixture cron entry was removed. Original files, message,
cron text and a verified SQL dump were retained privately as rollback aids.

## Checks performed

| Check | Outcome |
|---|---|
| Account creation through the local DirectAdmin API | Passed; account count changed from 141 to 142 |
| Database, mailbox and cron creation through the account-scoped API | Passed |
| Website HTTP request to the account's shared IP with its Host header | HTTP 200 and expected fixture content, before and after restore |
| Backup identity | `backup/user.conf` named the expected username and domain |
| Archive contents before mutation | Verified website/private-file hashes, message hash, cron entry, and expected SQL tables/fixture rows |
| Simulated fixture loss/change | Confirmed files/message absent, database hash different, cron entry absent |
| Native restore into the same disposable account | Completed in approximately 4.74 seconds |
| Restored website and private file | Exact baseline hashes, Unix owner and permission modes matched |
| Restored database | All canonical row bytes matched, including Unicode and binary data; fixture database login worked |
| Restored mail | Exact baseline message-content hash matched |
| Restored cron | Fixture entry matched the baseline |
| Final host checks | DirectAdmin active, fixture website working, no test backup/restore process visible, approximately 31 GB free |

The successful backup command completed in approximately 0.86 seconds. Its
archive was 8,084 bytes, expanding to a 112,640-byte tar stream:

```text
user.admin.gzv0908a.tar.zst
SHA256 39c2cff53e94e85d3a951ae7dede02b003e7e5eddefc5399c20d99a107b6f292
```

## Findings that change the integration plan

### 1. Native staging uses both admin and account permissions

A backup into the root-private validation directory failed because the
command tried to create its output as `admin`. A second attempt under a
private `admin:admin` directory failed because the selected account could not
traverse the parent. Its generated work directory was owned by `gzv0908a`.

The successful test gave **only the disposable account's group** traversal
permission on the newly created test parent. No pre-existing directory's
permissions were relaxed. After validation the retained backup directory was
sealed back to `admin:admin` mode 0700 and the archive to `admin:admin` mode 0600.

Implication: the root-owned mode-0700 staging setup in
`internal/directadmin/directadmin.go` cannot simply be handed to the native
backup tool. Design an isolated, temporary per-account native work area and a
protected handoff into Gniza storage. Do not make the service's state,
credentials or administrative socket accessible to hosting accounts.

### 2. Exit code zero does not establish backup or restore success

Both failed backup attempts returned process exit code **0**, while their task
logs contained `error running backup task` and an explicit error code 1.
Neither produced an archive.

A restore probe selected the fixture's canonical archive filename in a new
empty test directory. It also returned **0**, while the log reported that the
archive could not be found. The fixture baseline was unchanged by that probe.
The error message still used the wording `running backup task` even though
the task action was `restore`.

Successful process execution therefore needs additional completion/error
validation and output verification. A missing archive must not count as a
successful restore; merely finding a leftover archive must not establish that
a new backup succeeded. The precise error wording is version-specific evidence,
not a complete proposed parser contract.

### 3. Single-task execution was sufficient for this native restore

The installed CLI advertises `directadmin taskq --run=<task>` and `--file`.
This exercise used **one inline restore task**, with exactly one `select0`
entry naming the verified fixture archive. It did not append to or drain the
server's shared `task.queue`.

That invocation returned after the observed restore and the subsequent
content checks passed. This provides a candidate synchronous integration path;
it does not prove cancellation, timeout, crash recovery, concurrent-operation
locking or every delayed follow-up task's behavior.

### 4. Real archive layout differs from several provisional constants

This server generated zstd, not gzip. The current `dabackup.ValidateArchive`
implementation only adds a decompression reader for `.gz`, so this observed
format needs explicit support. This was a source inspection, not a separate
execution of the Gniza validator against the archive.

The observed member layout was:

| Data | Native archive location |
|---|---|
| Account identity | `backup/user.conf` |
| Cron | `backup/crontab.conf` |
| Database dump | `backup/gzv0908a_shop.sql` |
| Database settings/users metadata | `backup/gzv0908a_shop.conf` |
| Domain configuration | `backup/<domain>/domain.conf` |
| Domain address list | `backup/<domain>/domain.ip_list` |
| Domain zone | `backup/<domain>/<domain>.db` |
| FTP records | `backup/<domain>/ftp.conf`, `backup/<domain>/ftp.passwd` |
| Mail account/configuration records | `backup/<domain>/email/…` |
| Website | `domains/<domain>/public_html/…` |
| Mail messages | `imap/<domain>/sales/Maildir/{new,cur}/…` |
| Other home files | **Nested** `backup/home.tar.zst` |

The private CSV lived inside that nested home archive, not below the outer
`domains/` tree. Consequently, treating `domains/` as the entire home-directory
layout is insufficient. The provisional mail, FTP, domain and DNS member
selectors also need reconciliation with these real per-domain paths.

This fixture did not have SSL certificates, so certificate paths were not
validated. The database `.conf` contents were not published. The inspected
per-user SQLite database listed `git`, `cpanel_import`, `cpanel_import_log` and
`resource_metrics` tables; this does not settle every database-ownership rule.

## What the nested home archive holds — read 2026-09-08

Read from the retained fixture archive on `.10`, streamed rather than
extracted, with no new backup run:

```
tar --use-compress-program=unzstd -xOf <archive> backup/home.tar.zst \
  | tar --use-compress-program=unzstd -tvf -
```

- The outer archive has exactly three roots: `backup/`, `domains/`,
  `imap/`.
- `backup/home.tar.zst` holds the **complement** of `domains/` and
  `imap/`, not a second copy of them: dotfiles (`.bashrc`,
  `.bash_profile`), the account's own `Maildir/`, `.php/`, and whatever
  else is in the home directory. Nothing is stored twice.
- Its entries are relative to the home directory with no leading `./`
  and no wrapping directory.
- Ownership is recorded as **names, not numbers** — `gzv0908a/gzv0908a`,
  and also `gzv0908a/apache`, `gzv0908a/mail`, `gzv0908a/access`. The
  group is not always the account's own, and the modes that go with
  those groups are meaningful (`drwxrwx--- gzv0908a/apache` for
  `.php/`). Anything that rebuilds this archive has to carry the group
  and mode per entry, not assume the account owns everything.

This is the groundwork for staging a DirectAdmin account in parts.
Gniza does not need an undocumented `admin-backup` option to do it: it
can take the whole archive DirectAdmin already produces, unpack the
outer tree and the nested home archive, and store the parts, then
rebuild both on the way back. What is not yet established is whether a
rebuilt archive restores byte-for-identically enough for DirectAdmin's
own restore to accept it, which is a round trip on the fixture and not a
source question.

## What remains unverified

- Split/no-compression backup behavior. `admin-backup --help` on this host only
  listed `--destination` and repeatable `--user`; no global compression setting
  was changed and no partial-backup parameters were guessed.
- Gniza's worker, restic destinations, retention and download/granular-restore
  paths using this real layout.
- DirectAdmin plugin process identities, login-as behavior and a secure session
  bridge. No placeholder plugin or permission bypass was installed.
- Lifecycle deletion/suspension/rename hooks, new-account disaster recovery,
  cross-host restores, mixed-panel fleet behavior, and multi-user privilege
  edge cases. No customer account was used for these tests.

These results narrow [ADR 0019](adr/0019-the-panel-behind-an-interface.md)'s
unknowns; they do not make DirectAdmin support release-ready.

## Retained artifacts and handoff

The disposable account remains restored and available for subsequent explicit
testing. It has not been removed or suspended.

On `.10`:

```text
/root/gniza-da-validation.WtwDiT/
  fixture-private.json        # credentials; root-private, do not publish
  baseline.json
  archive.json
  archive-members.json
  archive-verified.json
  restore-verification.json
  backup*.log
  restore*.log
  held-fixture-data/
  verified-database.sql
  validate.py, roundtrip.py, seed-files.sh

/home/admin/gniza-validation-gzv0908a-20260908/
  native-backup-account-access/user.admin.gzv0908a.tar.zst
```

The scripts are guarded, one-off validation helpers, not production packaging
or a reusable certified test suite. They intentionally refuse to overwrite
several existing evidence files. Do not rerun mutation steps blindly against
the retained fixture.

Reference for the native command/task format:
[DirectAdmin backup and restore documentation](https://docs.directadmin.com/directadmin/backup-restore-migration/#how-to-create-a-full-backup-via-the-command-line).

## The archive taken apart and put back together — 2026-09-08

Split mode stores files rather than one compressed archive, so restic can
deduplicate them. DirectAdmin has no documented way to be asked for the parts
separately, so Gniza takes them out of the archive it does write. What has to
be true for that to be safe is that the archive comes back the same.

The retained fixture archive was copied off `.10` read-only — 8,084 bytes,
`sha256 39c2cff53e94e85d3a951ae7dede02b003e7e5eddefc5399c20d99a107b6f292` — and
unpacked and repacked by `dabackup.Layout.UnpackArchive` and `PackArchive`.
Nothing was restored and nothing on the panel was touched.

All 74 members across both archives came back with the same name, type, mode,
uid, gid, owner name, group name, mtime, size, link target and contents.
`tar -tv` of the original and of the rebuilt archive differ in exactly one
line, which is the nested archive's own length:

```text
< -rw-r----- gzv0908a/gzv0908a   864 2026-09-08 05:41 backup/home.tar.zst
> -rw-r----- gzv0908a/gzv0908a   861 2026-09-08 05:41 backup/home.tar.zst
```

Compressing the same bytes twice does not produce the same file, and does not
need to: the members inside it are identical, and GNU tar reads both.

The reason the tar headers are kept in a manifest rather than rebuilt from the
files on disk is visible in that listing. Three different groups own parts of
one account's home directory:

```text
-rw-r--r-- gzv0908a/gzv0908a 376 2025-08-26 11:44 .bashrc
-rw-r--r-- gzv0908a/access   102 2026-09-08 05:40 .myimunify_id
drwxrwx--- gzv0908a/apache     0 2026-09-08 05:32 .php/
drwxrwx--- gzv0908a/mail       0 2026-09-08 05:31 Maildir/
```

A rebuild that walked the unpacked tree would write whatever the staging
server's own passwd and group files said, and restore an account whose mail
directory Dovecot cannot write.

The unpacked form is a manifest beside a tree, and the tree is the account's
home directory put back together from the three places the archive keeps it
in:

```text
manifest.json
tree/backup/…            # DirectAdmin's records of the account
tree/home/domains/…      # from the outer archive
tree/home/imap/…         # from the outer archive
tree/home/.bashrc …      # from backup/home.tar.zst
```

Reproduce with:

```sh
GNIZA_DA_LIVE_ARCHIVE=/path/to/user.admin.<account>.tar.zst \
  go test -tags directadmin_live ./internal/layout/dabackup/ \
  -run TestLiveARealArchiveComesBackTheSame -v
```

Still not done here: the split payload is not yet wired into `Stage`, which
still refuses `ModeSplit`, and no account has been restored from a repacked
archive.
