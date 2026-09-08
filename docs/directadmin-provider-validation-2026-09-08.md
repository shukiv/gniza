# DirectAdmin provider validation — 2026-09-08

## Result

**PASS: Gniza's DirectAdmin provider backed up the disposable account, stored
the archive in a real restic repository, retrieved identical bytes through
Gniza reassembly, and restored the removed fixture website through its provider.**

This is narrower than complete DirectAdmin support. It does not validate the
standalone scheduler, admin/user plugin pages, session authorization, split or
granular restores, account lifecycle isolation, or new-account recovery.

## Production scope

The authorized target was `182.54.236.10`, DirectAdmin 1.709. The only account
selected was `gzv0908a`, domain `gzv0908a.gniza-test.invalid`, previously created
as a disposable fixture. Before the test, its original baseline was verified.
The host had approximately 31 GB free and no observed competing native backup,
restore, dataskq, mysqldump or Gniza process. Existing JetBackup daemons were
left alone.

Temporary test binaries were copied into root-private validation storage.
No plugin, service, hook or scheduled job was installed. No panel configuration
or shared `task.queue` was modified. No manual service restart was performed.

## Changes exercised

- Native backup uses a fresh per-account workspace outside root-private state.
  Only the native admin owner can write its parent, and only the selected
  account group can traverse it. Completed archives are copied into private
  staging, checked for identity and compression integrity, and retained mode
  0600. Symlink/hardlink archive handoffs are refused.
- Backup/restore require a matching native task start/completion transcript
  and reject error records even with process exit zero. Unexpected task
  selection, missing completion and transcript overflow fail closed. This
  parser is grounded in the 1.709 format, not a guarantee for future versions.
- `.tar.zst` is classified as a monolithic snapshot archive. The DirectAdmin
  validator handles tar/gzip/zstd, checks the compressed trailer, and rejects
  conflicting identity records, traversal names and non-padding trailers.
- Existing ordinary-account overwrite uses one explicit `taskq --run` request.
  It requires `Overwrite=true` and `Unrestricted=true`; it does not silently
  claim cPanel's restricted-restore protections. Native metadata is trusted to
  DirectAdmin's restore. New/renamed/admin/reseller account restores and
  certification options remain refused.
- Per-account process locks and process-group cancellation guard native work;
  native/private staging-space estimates guard backups. Account sizing includes home
  files and database sizes. These are not cross-product locks or reservations;
  unrelated DirectAdmin/JetBackup operations must not overlap the same account.

The local subprocess fixture and real-restic regression test are in
`internal/directadmin/native_test.go` and `roundtrip_test.go`. The live probe is
behind `-tags directadmin_live`, requires an explicit confirmation string and
a new root-private evidence directory, and is deliberately pinned to this
disposable account. It must not be generalized to customer accounts.

## Live outcome

The successful provider test took 15.10 seconds. It created a native backup,
initialized a private test repository, wrote a completion receipt, checked the
repository, and used `reassemble.Run` to retrieve the archive. Original and
retrieved archive SHA256 values matched:

```text
d64c9838eaaa48256f19ffe6a782c5fcb8334f60718e0ef00e35d88ee5521172
```

Only the fixture's website index was moved aside in this round. The provider
restored its exact bytes, and the rollback copy was retained privately.
Independent checks then matched all original baseline categories: website and
private-file bytes/owners/modes, database rows including Unicode/binary values,
mail message hashes and cron. The fixture website returned HTTP 200.

DirectAdmin remained active with MainPID `2094187`; approximately 31 GB remained
free. No test worker remained visible. The account was left restored.

An initial attempt staged a valid archive but stopped before restore because
the temporary restic executable was still being transferred. Transfer hashes
were verified before the successful retry. No fixture data was changed by the
failed attempt.

Evidence on `.10` is retained under:

```text
/root/gniza-da-validation.WtwDiT/provider-r3/
  stage/user.admin.gzv0908a.tar.zst
  repository/
  repository.key             # root-private; do not publish
  restore/archive/user.admin.gzv0908a.tar.zst
  restore.log
  held-index.html
```

Native per-job directories were cleaned after execution. The dedicated
temporary roots (`/var/tmp/gniza-da-fixture-3619070126` for the stopped attempt,
`/var/tmp/gniza-da-fixture-1438167584` for the passing attempt) contain only the
root-owned account lock file. Test binaries and failed-attempt evidence remain
under the same root-private validation directory for inspection.

## Remaining release gates

1. **Authenticated DirectAdmin bridge and real plugin pages.** Preserve the
   root-only administrative socket and require verified current panel sessions;
   a Unix uid or client-supplied admin flag is not a login. Observe plugin
   execution identity/authentication-field availability in admin, ordinary-user
   and login-as sessions. Installing that diagnostic plugin on production needs
   the user's approval; none was installed during this round.
2. **Split and granular data layout.** Handle nested home archives and actual
   per-domain metadata, plus database users/grants, mail, FTP, DNS and SSL.
   Unsupported selectors must not be presented as working self-service restore.
3. **Lifecycle and recovery.** Validate delete/recreate/rename isolation on an
   isolated host or specifically authorized additional disposable operations.
   Validate new-account recovery, ownership/domain conflicts and interrupted
   native operations before production restore support.
4. **Operational packaging.** Validate installer rollback/busy-job checks,
   restart/crash workspace handling, panel-specific update artifacts, and a
   complete installed UI/scheduler/destination round trip before signed release.

No source push, release publication or service deployment was performed by
this implementation round. The package remains experimental.

## Local verification

- `go test ./... -count=1`: passed.
- `go vet ./...` and the `directadmin_live` test build/vet: passed.
- Full existing `make e2e`: passed in 251.39 seconds. This covers the existing
  cPanel/fleet/standalone paths, not an installed DirectAdmin UI.
- Real-restic DirectAdmin archive round trip: passed with distribution restic
  0.18.0 and the project's pinned 0.19.1.
- Race tests for DirectAdmin, its archive layout and reassembly: passed.
- `govulncheck`: no reachable vulnerabilities. It reports one module-level
  advisory for unused `golang.org/x/crypto/openpgp`; this project does not
  import that package. The added zstd dependency is pinned in go.mod/go.sum.

After the live probe, native child-process completion handling and diagnostic
redaction were tightened locally. Those follow-up changes were regression
tested locally; the live result above identifies the path exercised before
those final hardening changes, not a deployment of the final worktree.
