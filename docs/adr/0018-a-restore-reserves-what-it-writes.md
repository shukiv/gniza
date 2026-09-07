# 0018 — A restore reserves what it writes

Status: accepted, 2026-09-07. Revises the scratch-capacity decision in
[0016](0016-restore-trust-and-backup-completion.md).

## Context

ADR 0016 budgeted "three source-sized copies plus 1 GiB, with the staging
manager's configured safety margin on top". One number served every
restore, and it was the largest of them.

On a production server that number stopped the product working. An
account of 48.7 GiB asked for 3 × 48.7 + 1 = 147.1 GiB, and the 0.2
safety margin took it to **176.5 GiB**. The staging volume has 63.2 GiB
free. Nothing could be restored from that account at all:

- the nightly rehearsal was refused, so the backup was never proven;
- taking one 800 KiB database out of it was refused for the same 176.5
  GiB, because a granular restore was sized as a whole-account
  reassembly.

The account large enough to need a proven backup is exactly the account
that could not have one, and "restore one thing" — the most common real
request — was impossible on any account bigger than a third of the free
disk.

The three copies were assumed, never measured. Measuring them found that
only one restore in four makes three, and one makes only one.

## Decision

**The estimate follows what the restore actually does.**

| | on disk at the peak |
|---|---|
| apply to a live account | the tree, and cPanel's own copy of it — 2× |
| rebuild for download | the tree, and the tar built from it — 2× |
| rehearsal (drill) | the tree — 1× |
| named items or files | roughly what was asked for |

`StagingBytes` is replaced by `TreeBytes` (1× + 1 GiB) and `ArchiveBytes`
(2× + 1 GiB), so each caller states which of the two it means instead of
sharing one number that suited none of them. The staging manager's safety
margin still applies on top.

**An apply hands cPanel the extracted directory.** `restorepkg` accepts
one — its usage lists `/path/to/extracted-cpuser-file` beside the archive
forms — and `Whostmgr::Transfers::ArchiveManager` copies a directory with
`cp --archive` into a temporary directory it creates beside the path it
was given. Repacking the tree into a tar first cost a third full copy of
the account for nothing, since cPanel's next act is to copy what it was
handed anyway. The directory branch runs before the restricted /
unrestricted split, so this holds in both modes; Gniza asks for a
restricted restore by default and continues to.

That temporary directory lands inside our staging directory, so it counts
against the same allocation and is swept up when the allocation is
released.

**A rehearsal builds no archive.** It untars nothing and hands nothing
over: it reads the tree, counts the files in the home directory and
parses the dumps. The verifier no longer insists on an archive that was
never asked for. The 48.7 GiB account now rehearses in 59.6 GiB of the
63.2 GiB free — not comfortable, but possible, where before it was not.

**A granular restore is sized by what it takes.** The backup is asked
what the resolved paths come to, with `restic ls`.

**A listing containing a directory is refused, not summed.** `restic ls`
without `--recursive` lists a directory's direct children, so asked about
a 30 GiB `public_html` it answers with a few kilobytes of top-level
files. Summing that would reserve about a gigabyte, pass the space check,
and then fill the volume of a live cPanel server while restic wrote the
rest — worse than the over-refusal it replaces. Not knowing the size
means the whole-account figure stands, which is always enough. Databases,
DNS zones, certificates, cron and settings are files, so the case this
exists for is sized exactly.

## Consequences

Restoring part of an account is now possible on a large account, which it
was not. Rehearsals fit where they did not. An apply is one copy cheaper
and one step shorter.

The estimate is still conservative and still not a quota: concurrent
external use of the staging volume and inaccurate source metadata remain
limitations, and scratch belongs on a dedicated, monitored filesystem.

Directory hand-off is the one change here on a destructive path that
rests on reading cPanel's source rather than on a run against a live
account. `ValidateAccountArchive` still runs, on the metadata archive
during reassembly, so the archive's identity is still checked against the
request before anything is handed over. The first real apply after this
release should be watched.
