# 0016 — Restore trust, source-read completion, and scratch capacity

Status: accepted; in production on cPanel 136 since 2026-09-06
Date: 2026-09-06

## Context

The follow-up review found five unsafe assumptions: SQL dumps still reached
an administrative database session; an unmapped database login was treated as
available; a snapshot tag and archive filename stood in for the embedded
account identity; exit-code-3 backups counted as successful complete copies;
and restore space could be estimated from a much smaller current account.

## Decisions

### A SQL dump never receives an administrative database session

Granular imports create a random temporary database login at the exact client
host, grant only table/data privileges on the selected owned schema, and
import through a private option file. Client command execution and LOCAL
INFILE remain disabled. Root credentials and login-path files are excluded
from the import process. Setup and cleanup SQL contain no statements from the
dump. Cleanup has its own timeout so cancellation still attempts DROP USER;
cleanup failure makes the restore fail and names the temporary login.

There is no root fallback. Views, routines, triggers, events, and privileged
DEFINER objects are not supported by this granular importer: they fail rather
than leave objects tied to a deleted temporary login or regain root access.
An SQL error is not a rollback of earlier DDL or writes to the selected
database. Operators must still treat an applied restore as destructive.

The active MySQL option files supply allowlisted connection and TLS settings.
Ambiguous whitespace-containing option output is refused. Remote profiles,
MariaDB variants, and unusual login-path/environment-only profiles still need
certification on a disposable cPanel host before release.

### Existing database logins need positive ownership

Restoring database users reads every cPanel ownership record and the server's
actual login list before mutation. Missing, malformed, or conflicting maps
fail closed. Reserved administrative names and existing unmapped logins
cannot be claimed. Existing account-owned logins may be updated; a missing
login uses CREATE USER without IF NOT EXISTS or ALTER so a concurrent name
collision fails instead of changing somebody else's password.

### Archive identity must agree with the request

Both monolithic archives and split metadata are scanned before whole-account
reassembly/application. Their root, cp/<account> record, optional USER field,
and meta/user record must agree with the requested account. Traversal,
conflicting identities, and nonregular identity records are refused. The
archive itself cannot be a symlink. This is an additional identity check,
not a replacement for cPanel Restricted Restore or proof that every archive
member is safe to apply.

### Source-read completion survives loss of local history

Agent payload snapshots carry the stable tag `cprest:read-status-v1` before
the read starts. Only a successful restic backup is followed by a separate,
small snapshot tagged `cprest:completion-v1` and `completed:<payload-id>`.
An interrupted or unreadable payload has no completion receipt. Failure to
write the receipt also fails the target while retaining its payload ID.
Exit-code-3 targets no longer count as successful copies in job rollups.

This works through append-only endpoints: no retag, delete, or replacement of
the original snapshot is required. Listings hide receipts and derive managed
payload completeness from them. Automatic full-account recovery and applied
whole-account restores reject managed snapshots without completion evidence.
Receipts describe the agent's read outcome, not independent attestation that
an already-compromised source supplied trustworthy content.

Retention first asks restic for a dry-run plan. If any snapshot in a group
has unverified reads, the whole group is protected from automatic expiry;
other groups can expire normally. Application deletes only the exact safe
IDs from that preview, rather than recomputing a policy while new backups
may be arriving. The UI explains the protection. Receipts are excluded from
retention and retained, so small metadata snapshots accumulate; they are not
counted as customer backups. Copying repositories externally must preserve
the receipts as well as the payload snapshots.

Legacy snapshots have no receipt requirement. Available local job history
marks known pre-upgrade incomplete snapshots for recovery selection and
retention protection. If that history has been lost, the old format cannot
prove that reads completed; this change does not retroactively certify old
backups. The new tag creates a separate retention group from legacy payloads.
Do not downgrade agents/maintenance tools against receipt-enabled repositories:
older tools do not understand these safeguards or the hidden snapshots.

### Restore estimates include historical size and simultaneous copies

The chosen snapshot's recorded logical size is used, falling back to
`restic stats --mode restore-size` when older snapshots lack it. Unknown size
is an error, not permission to use a tiny live-account size. Restore workers
repeat this check rather than trusting controller estimates. Reassembly and
drill preflights budget three source-sized copies plus 1 GiB, with the staging
manager's configured safety margin on top where applicable.

This is a conservative capacity estimate, not a filesystem quota or a bound
on hostile compressed-archive expansion. Concurrent external disk use and
maliciously inaccurate source metadata remain limitations. Keep scratch on
a dedicated, monitored filesystem.

## Verification and release boundary

Regression tests cover database ownership, isolated SQL invocation and cleanup,
archive identity mismatches, incomplete job rollups, repository-based read
status, exact-ID retention, and historical-size preflights. The database
isolation test was also run against a disposable local MySQL 8.4.11: normal
table restoration succeeded; cross-schema mutation, user creation, FILE,
global grants, and a root-definer view were rejected. Temporary logins were
removed after each case.

Verification passed: `go test ./... -count=1`,
`go test -tags=e2e ./internal/e2e -count=1`, `go vet ./...`, and race-detector
tests for agent, cpanel, resticrun, node, reassemble, and job. Test scratch
was placed on a disk-backed filesystem with a short temporary-path alias:
the default small tmpfs triggered the new capacity refusal, while long
temporary paths exceeded the operating system's Unix-socket limit.

The opt-in `TestIsolatedDatabaseImportAgainstMySQL` requires
`CPREST_DISPOSABLE_MYSQL_TEST=1` and creates/drops its own test databases.
Never enable it on a hosting server. End-to-end tests use real restic and
temporary repositories, with a fake cPanel provider; they do not prove real
restorepkg compatibility. No connection or deployment to `.144` was made
during this implementation.
