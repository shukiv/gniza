# Gniza — Design

Hosting-panel fleet backup orchestration built on
[restic](https://restic.net/). cPanel/WHM is the only panel implemented
today; DirectAdmin and Plesk are planned, and §1 says what that costs.

Status: design accepted; everything described here is implemented and
covered end to end. The real cPanel provider runs in production on cPanel
136. What is not built is listed in the status table in the README —
notably the controller's own web interface, and the Azure, GCS and rclone
destination types.
Last updated: 2026-09-07.

---

## 1. Goals and non-goals

### Goals

- Back up hosting accounts from many servers to one or more remote destinations.
- Keep the panel behind one interface. Everything that knows what an account
  is, how to package one and how to put one back sits behind the provider in
  `internal/cpanel`; the scheduler, the repositories, the restore machinery
  and the interface do not. cPanel/WHM is the implementation that exists;
  DirectAdmin and Plesk are meant to be further implementations of the same
  interface rather than forks of the program.
- Keep backup *data* off the control plane. The controller orchestrates and records state; it never proxies backup bytes.
- Support several destination types (local, SFTP, restic REST server, S3-compatible) behind one abstraction.
- Survive a single destination being unavailable without invalidating the copies that succeeded.
- Resist ransomware on a compromised cPanel server: an attacker with root on a source server must not be able to destroy backup history.
- Make restore a first-class, regularly exercised path — not an afterthought.

### Non-goals (for v1)

- Bare-metal / whole-server imaging. We back up hosting accounts and server-level configuration, not block devices.
- Replacing the panel's own transport for restore. We reconstruct an archive and hand it to the panel — `restorepkg` on cPanel.
- Running two panels on one server, or moving an account between panels. A server has one panel and Gniza asks it what is there.
- Cross-account deduplication. See §6 for why repositories are deliberately split.

---

## 2. Components

There are four components, not three. The maintenance runner is load-bearing — see §8.

```
                     ┌───────────────────────┐
                     │      Controller       │
                     │                       │
                     │ PostgreSQL            │
                     │ Scheduler             │
                     │ Policies              │
                     │ Destinations / Repos  │
                     │ Credential vault      │
                     │ Job + alert state     │
                     └───────────┬───────────┘
                                 │ HTTPS + mTLS
                                 │ agent-initiated, control only
                                 ▼
                    ┌────────────────────────┐
                    │  cPanel Backup Agent   │
                    │                        │
                    │  1. plan               │
                    │  2. stage (pkgacct)    │
                    │  3. restic backup × N  │
                    │  4. report             │
                    └──────────┬─────────────┘
                               │  backup data
               ┌───────────────┼────────────────┐
               ▼               ▼                ▼
         ┌──────────┐    ┌───────────┐   ┌────────────┐
         │ S3-compat│    │ SFTP      │   │ rest-server│
         └──────────┘    └───────────┘   └────────────┘
               ▲               ▲                ▲
               └───────────────┴────────────────┘
                               │ delete-capable credentials
                    ┌──────────┴─────────────┐
                    │  Maintenance Runner    │
                    │                        │
                    │  forget --prune        │
                    │  check --read-data-... │
                    │  restore drills        │
                    └────────────────────────┘
```

**Controller.** Postgres-backed. Owns servers, accounts, policies, destinations, repositories, schedules, credentials, job history, alerting. Serves the agent API. Never touches backup data.

**Agent.** Runs on each cPanel server as root (pkgacct requires it). Long-polls the controller for work, stages a payload, runs restic once per target repository, reports per-repository results.

**Maintenance runner.** Runs on trusted infrastructure — *not* on cPanel servers. Holds the only credentials that can delete from a repository. Executes retention, integrity checks, and restore drills.

**Destinations.** Dumb storage. No Gniza software required on the far end, except optionally the `rest-server` package.

---

## 3. Transport: agent pulls

The agent initiates all connections to the controller. The controller never dials the agent.

Rationale:

- cPanel servers routinely sit behind NAT, provider firewalls, or CSF rules that make inbound reachability a per-customer support ticket.
- Outbound HTTPS on 443 works everywhere.
- An agent that only makes outbound connections presents no new inbound attack surface on the source server.

Mechanics:

- Mutual TLS. Each agent gets a client certificate at enrolment; the controller pins the certificate to a server record.
- `GET /v1/jobs/next` long-polls (default 60s). Empty response on timeout, agent re-polls.
- `POST /v1/jobs/{id}/events` streams progress; `POST /v1/jobs/{id}/result` closes it out.
- Job leases carry a TTL. A crashed agent's job is reclaimed after the lease expires; the reclaiming logic must assume the previous attempt may have partially written to a repository (restic tolerates this — the incomplete pack data is orphaned and later pruned).

---

## 4. Payload strategy — the dedup problem

This is the single most consequential decision in the system, and it is easy to get wrong.

`pkgacct` produces a **compressed** archive by default (`cpmove-<user>.tar.gz`). Restic deduplicates using content-defined chunking over the byte stream. A gzip stream has no stable chunk boundaries across runs: change one byte early in the input and every subsequent compressed byte differs. Feeding restic a nightly `.tar.gz` therefore stores approximately the full archive size *every night*, in *every* destination repository.

For a 40 GB account backed up nightly to three destinations, that is ~120 GB/night of upload and ~3.6 TB/month of storage for data that barely changed.

### Two supported modes

**`split` (default, recommended).**

The payload is decomposed so restic sees real files:

| Part | Source | Notes |
|---|---|---|
| Account metadata | `pkgacct` with homedir excluded | Small: cPanel config, DNS zones, mail config, cron, SSL. |
| Home directory | backed up directly from `/home/<user>` | Native restic file-level dedup and change detection. |
| Databases | one uncompressed SQL dump per database | Per-database granularity; dumps dedup well between nights. |

Restore reassembles these into a `cpmove` structure before calling `restorepkg` (§10).

Cost: more moving parts, and reconstruction must track cPanel's archive layout across versions. Benefit: typically one to two orders of magnitude less stored and transferred data.

**`monolithic` (compatibility mode).**

One `pkgacct` archive per account, **with pkgacct compression disabled**, backed up as a single `.tar`. Restic's own compression (repository format v2, zstd) is applied instead.

> Implementation note: the exact flag to disable pkgacct compression varies by cPanel version. The agent must probe `pkgacct --help` on the target server at enrolment and record the supported flag in the server record, rather than assuming one. If no such flag exists on that version, the agent must report the server as `monolithic` mode being degraded and warn that dedup will be poor.

Even uncompressed, tar is imperfect: inserting or resizing a file shifts all subsequent bytes. Content-defined chunking absorbs shifts far better than gzip does, but `split` mode still wins decisively. `monolithic` exists for accounts where reconstruction fidelity matters more than storage cost.

### Compression

Repositories are created at format version 2, which supports zstd compression. Agents set `RESTIC_COMPRESSION=auto` by default, `max` for `monolithic` mode where the payload is an uncompressed tar.

---

## 5. Destination and repository are separate objects

The pasted-in earlier draft conflated "where credentials point" with "which restic repository we write". Splitting them matters because many repositories share one set of storage credentials.

**Destination** — a storage endpoint plus credentials.

```
Destination "Wasabi Miami"
  type:        s3
  endpoint:    s3.us-east-1.wasabisys.com
  bucket:      cp-backups
  credentials: <vault ref>
```

**Repository** — a restic repository living at a path inside a destination, with its own repository password.

```
Repository "cp01-primary"
  destination: Wasabi Miami
  path:        /cp01
  password:    <vault ref>
  chunker:     seeded from repository "cp01-rest" at init
```

A policy targets N repositories, not N destinations.

### Code shape

Destinations are **configuration**, not execution. They answer three questions and nothing else:

```go
type Destination interface {
    // URI returns the RESTIC_REPOSITORY value for a repository path.
    URI(repoPath string) (string, error)
    // Env returns backend credentials as environment variables.
    Env() (map[string]string, error)
    // Options returns restic extended options ("-o key=value"). These
    // configure the backend rather than authenticate to it, so unlike Env
    // they may appear in the argument list.
    Options() (map[string]string, error)
    // Preflight checks reachability and configuration without touching restic.
    Preflight(ctx context.Context) error
}
```

`Options` exists because SFTP cannot be configured any other way. Restic
builds `ssh <host> [-p <port>] [-l <user>] <sftp.args…> -s sftp`, so without
`-o sftp.args` the agent would inherit root's ssh configuration: the wrong
key, an unpinned host key, and an interactive prompt that hangs an
unattended backup. The SFTP destination therefore emits
`-i <identity> -o UserKnownHostsFile=… -o StrictHostKeyChecking=yes -o BatchMode=yes`.
File paths are not secrets, so argv is the right place for them.

One consequence: restic applies `-o` per process, not per repository. Two
repositories in one invocation — as `init --from-repo` needs — cannot
disagree about the same option key. The runner detects that and fails
rather than quietly using one repository's ssh identity for both.

There is exactly **one** restic runner (`internal/resticrun`). It builds arguments, injects environment, executes, parses `--json`, and returns a typed result. Per-destination `Backup()` / `Check()` / `Init()` methods would be four near-identical copies of the same `exec.Command` plumbing; we do not want them.

### Supported destination types

| Type | Backend | v1 |
|---|---|---|
| Local filesystem / mounted NAS | restic `local` | yes |
| SFTP / SSH | restic `sftp` | yes |
| restic REST server | restic `rest` | yes |
| S3, and S3-compatible (Wasabi, R2, MinIO, Ceph RGW, B2 via S3 API) | restic `s3` | yes |
| Azure Blob, GCS | restic native | later |
| Everything else (Drive, Dropbox, OneDrive) | restic `rclone` | later |

`rclone` is deferred deliberately: it requires shipping and configuring a second binary on every cPanel server, which widens the attack surface on the machine we least trust.

---

## 6. Repository layout

One restic repository per **source server** per **repository target**.

```
s3://cp-backups/
├── cp01/
├── cp02/
└── cp03/
```

Each is an independent repository with its own password.

Trade-off, stated plainly: this forfeits deduplication *between* servers. Two servers hosting the same WordPress version store it twice. We accept that because:

- Repository-level corruption is contained to one server's history.
- Restic repository locking is per-repository; fleet-wide contention on a single repository would serialise the whole fleet's nightly window.
- With `--private-repos` on rest-server, per-server credentials cannot read another server's data.

Per-account repositories were considered and rejected for v1: thousands of repositories multiplies maintenance cost (each needs its own `forget --prune` and `check` run) far beyond the confidentiality benefit.

---

## 7. Chunker parameters must be decided at init, not later

Restic's documentation is unambiguous:

> "both repositories must use the same parameters for splitting large files into smaller chunks ... Chunker parameters are generated once when creating a new repository and **cannot be changed afterwards**."

Today the agent uploads independently to each target repository — N full uploads. If we later want to switch to *backup once, replicate* (`restic copy` from a primary repository to secondaries, so the cPanel server uploads once), the secondaries must already share the primary's chunker polynomial. Retrofitting is impossible; the only remedy is re-creating the repository and re-uploading everything.

**Rule:** the first repository for a server is initialised normally. Every subsequent repository for that same server is initialised as:

```bash
restic -r <secondary> init --from-repo <primary> --copy-chunker-params
```

This is free to do now and impossible to do later. The controller enforces it: a server's first repository is recorded as its chunker source, and repository provisioning refuses to run a plain `init` for any subsequent repository on that server.

`restic copy` is not used in v1. The rule exists purely to keep the option open.

---

## 8. Retention, integrity, and append-only

For destinations we control, the recommended and default configuration is `rest-server` with `--append-only`, `--private-repos`, and TLS:

```bash
rest-server \
  --path /var/backups \
  --listen 0.0.0.0:443 \
  --tls --tls-cert /etc/rest-server/cert.pem --tls-key /etc/rest-server/key.pem \
  --tls-min-ver 1.3 \
  --htpasswd-file /etc/rest-server/.htpasswd \
  --private-repos \
  --append-only \
  --prometheus
```

In append-only mode, deleting data, index, key, and snapshot objects returns HTTP 403. Deleting *locks* remains permitted, so repository maintenance is not deadlocked by a stale lock.

### The consequence the earlier draft missed

An agent writing to an append-only repository **cannot** run `restic forget --prune`, and cannot run a check that needs to clear anything. Without a separate actor, repositories grow without bound.

Hence the **maintenance runner**: a scheduled worker on trusted infrastructure that performs, per repository, serialised against the backup window:

- `restic forget --keep-daily N --keep-weekly N --keep-monthly N --prune`
- `restic check` on every cycle; `restic check --read-data-subset=<pct>%` on a rolling schedule so all data is verified over time
- Restore drills (§10)
- Repository provisioning, including the `--copy-chunker-params` rule from §7

Retention policy lives on the policy object in the controller; the maintenance runner reads it, the agent never sees it.

### Append-only needs two endpoints, not two credentials

This is easy to get wrong, and the earlier draft did: `--append-only` is a property of the **running rest-server process**, not of a credential. Enabling it stops *everyone* deleting through that endpoint — the maintenance runner included. A deployment with one append-only endpoint can never be pruned at all.

The supported shape is therefore two rest-server instances over one data directory:

```
   agents ──────► rest-server  --append-only   :443   (public / server network)
                        │
                        ▼
                  /var/backups   ◄── one data directory
                        ▲
                        │
maintenance runner ───► rest-server (no --append-only)  :8000
                                        bound to the management network only
```

A destination records both addresses; `base_url` is what agents get, `maintenance_base_url` is what the maintenance runner gets. Where a destination has no override — local, SFTP, S3 — both roles use the same address, because those backends are delete-capable already.

The separation is now network reachability, not credentials: the delete-capable endpoint must not be routable from a cPanel server. An operator who omits `maintenance_base_url` on an append-only destination gets a hard failure from `restic forget` rather than silent unbounded growth.

### Retention groups by account, so snapshot identity must be stable

A repository holds every account on its server, so `restic forget` has to be applied per account, not to the repository as a whole. Restic groups snapshots by host and paths by default; the maintenance runner overrides this to `--group-by host,tags` and the agent tags each snapshot `account:<user>` and `mode:<payload mode>`.

Two consequences that are easy to violate:

- **Snapshot paths must repeat between runs.** Staging is keyed by account, not by job id. A per-job staging path would make every night a distinct group of one, and a group of one is never pruned. For the same reason restic is pointed at the *database directory* rather than at each dump file: naming the files would change a snapshot's paths the moment an account gained or lost a database.
- **Tags must not include anything per-run.** A `job:<id>` tag would do the same damage. The job a snapshot came from is recorded in the database against its snapshot id, which is where that lookup belongs.

Both are asserted by the end-to-end suite, because the failure mode is silent: backups keep succeeding and storage grows forever.

### What append-only does and does not protect

It protects **integrity and availability** of history. An attacker with root on cp01 can upload garbage snapshots but cannot delete or rewrite yesterday's — provided the delete-capable endpoint is unreachable from that server.

It does **not** protect **confidentiality**. The agent must hold the repository password to write, so an attacker who owns the server can read every snapshot in that repository's history. Mitigations available, none free:

- Per-server repositories (already the default) bound the blast radius to one server's accounts.
- Per-account repositories would bound it further, at high maintenance cost — rejected for v1 (§6).
- Rotating the repository password does not help retroactively; old snapshots stay readable with the old key material the attacker already captured.

This is an accepted, documented limitation, not an oversight.

---

## 9. Job model: success is not a boolean

A job that reached two of three repositories is neither a success nor a failure.

```
BackupJob
  status ∈ { pending, running, success, partial_success, failed, cancelled }

BackupJobTarget            (one row per repository)
  status ∈ { pending, running, success, failed, skipped }
  snapshot_id
  bytes_added              -- restic summary: data_added
  bytes_processed          -- restic summary: total_bytes_processed
  duration
  attempt
  error
```

Rollup rules:

| Targets | Job status |
|---|---|
| all succeeded | `success` |
| some succeeded, some failed | `partial_success` |
| none succeeded, at least one failed | `failed` |
| no targets configured | `failed` (misconfiguration, not a silent pass) |

Alerting distinguishes these. `partial_success` is a warning; two good copies are not an incident. `failed` pages.

Retries are per-target with exponential backoff, bounded by attempt count and by the backup window. Re-running `restic backup` after a partial upload is safe: restic reuses the pack data already stored. Note this is another argument for `split` mode — retrying a `monolithic` compressed payload re-uploads nearly everything.

Staging is retained until all targets reach a terminal state, then removed.

---

## 10. Restore

A backup product's deliverable is restore. This section is not optional.

### Restore is a job, dispatched like any other

A restore runs **on the cPanel server**, through the same machinery as a backup: the operator queues it, the controller leases it to the agent that polls, the agent does the work and reports. One long-poll endpoint returns either kind of assignment, so there is one lease mechanism, one credential path, and one place where an abandoned attempt is re-queued.

The controller chooses the source repository. The agent is told which repository and which snapshot, and never picks for itself — the same trust boundary as a backup, where the agent is told where to write.

```
operator: gniza-controller restore -server cp01 -user customer1 -snapshot 40dc1520
        ↓
restore_jobs row, status pending
        ↓
the agent's next poll returns a restore assignment, carrying the source
repository's credentials and password resolved for this job alone
        ↓
staging space preflight, keyed "restore-<account>" so it cannot collide
with a backup of the same account
        ↓
reassembly (below)
        ↓
report: archive path, bytes restored, whether it was applied
```

An account with a backup already running is skipped until that finishes, and vice versa: both stage under the account's name.

### Rebuilding the account archive

A monolithic snapshot already holds the archive; it is restored and that is the whole job.

A split snapshot has to be put back together, which is the cost of the decision in §4:

```
1. restore <snap>:<staging>/metadata  → work/metadata
      the pkgacct archive of everything except homedir and databases
2. extract it → work/tree/<discovered top-level directory>
3. restore <snap>:/home/<user>        → work/tree/<top>/homedir
4. restore <snap>:<staging>/databases → work/tree/<top>/mysql
5. repack work/tree                   → cpmove-<user>.tar
```

Each part is fetched with restic's subpath form (`snapshot:path`), which places a subtree directly at the target instead of recreating its leading directories. No path surgery is involved.

Two deliberate cautions:

- The archive's **top-level directory name is discovered, not assumed**. It is cPanel's to choose, and reassembly fails loudly if what it extracts is not a single account tree. The `homedir/` and `mysql/` subdirectory names are constants carrying the same caveat: **they have never been verified against a live cPanel here.**
- Extraction refuses entries that escape the extraction directory, refuses symlinks pointing outside it, and drops setuid bits. The archive comes from the server being restored, which in this threat model may be the compromised one.

### Applying to a live account is opt-in

By default a restore rebuilds the archive and leaves it on the server. Nothing is overwritten. `restorepkg` runs only when the job was created with `-apply`, because materialising files is safe and overwriting a live account is not. The flag is refused for a files restore, which has no whole-account archive to hand over.

### Single-file restore

The same job kind with `-files`. It uses `restic restore --include`, which preserves the original paths under the target directory so an operator can see where each file came from — the opposite of the subpath form used for reassembly, and deliberately so.

This is the most common real-world request and does not require a full account restore.

### Restore drills

`gniza-maintenance -kind drill` rehearses a restore from trusted infrastructure: rebuild the newest snapshot for an account into scratch space, assert what can be asserted, record the result in `maintenance_runs`, delete the scratch.

The checks are structural — the archive exists and is non-empty, the extracted tree has exactly one top-level directory, the home directory contains files, every SQL dump is non-empty and contains a `CREATE` statement. Nothing here can tell you cPanel would accept the archive; only a real `restorepkg` on a real host can. But a drill that fails means the backup certainly cannot be restored, which is the question worth answering nightly.

For acceptance testing, `gniza-agent -certify-live-archive` runs on an
isolated cPanel certification host. It restores under a caller-supplied
disposable username with Restricted Restore enabled and DNS updates disabled,
checks that the account entered cPanel's registry, and removes it with
`removeacct --force`. Cleanup uses its own bounded context so cancellation of
the restore does not strand an account. The structural drill remains the safe
scheduled default; live certification is deliberately explicit and is not a
production-WHM button. Its stdout is a JSON evidence record containing start
and finish times, the archive and disposable account, completed checks, and a
failure reason when it did not pass.

An untested backup is not a backup.

---

## 11. Credentials

Two classes of secret, handled the same way and scoped differently:

- **Backend credentials** — S3 keys, SSH private keys, rest-server basic-auth passwords.
- **Repository passwords** — restic's encryption key material.

Storage: envelope encryption. A master key (KMS or an operator-supplied key file, never in the database) wraps per-secret data keys; ciphertext is stored in Postgres under `secrets`. Plaintext is never written to the database and never logged.

Delivery: the agent receives, per job, only the secrets for the repositories in that job, over mTLS, in the job payload. They are held in memory for the job's lifetime and are not persisted to the agent's disk.

Handing secrets to restic:

- Backend credentials go in the child process **environment**.
- The repository password goes in a file referenced by `RESTIC_PASSWORD_FILE`, created mode `0600` under a private staging directory, removed on completion — or `RESTIC_PASSWORD_COMMAND` where a helper is preferable.
- **Never in argv.** Process arguments are world-readable via `/proc/<pid>/cmdline` on a multi-tenant cPanel server, which is exactly the environment we are running in.

The maintenance runner holds the delete-capable credentials. Agents never receive them.

---

## 12. Staging

`pkgacct` needs free disk roughly equal to the account's size, on the same server we are trying not to disrupt.

- Staging root is configurable per server, defaulting to a path the operator sizes deliberately (not `/tmp`, not the account's own filesystem where a full disk takes the customer down with it).
- **Preflight:** estimate payload size, compare against free space with a configurable safety margin, refuse the job with a clear error rather than filling the disk.
- **Concurrency cap:** per-server limit on simultaneous staged accounts, so ten large accounts cannot collectively exhaust the volume.
- **Crash cleanup:** staging directories carry the job ID. On startup the agent removes staging directories for jobs the controller reports as terminal. Cleanup on the success path alone is not sufficient — the failure path is when the disk is already under pressure.
- Staging is removed only after every target reaches a terminal state (§9).

---

## 13. Data model (v1)

```
servers              id, hostname, agent_cert_fingerprint, pkgacct_flags,
                     staging_root, max_concurrency, chunker_source_repo_id, status

accounts             id, server_id, cpanel_user, primary_domain, size_estimate, status

destinations         id, name, type, config_json, credentials_secret_id, status

repositories         id, destination_id, server_id, path, password_secret_id,
                     chunker_source_repo_id, initialised_at, status

policies             id, name, schedule_cron, payload_mode, retention_json,
                     compression, bandwidth_limit

policy_repositories  policy_id, repository_id            -- N targets per policy

account_policies     account_id, policy_id

backup_jobs          id, account_id, policy_id, started_at, finished_at, status

backup_job_targets   id, job_id, repository_id, status, snapshot_id,
                     bytes_added, bytes_processed, duration_seconds, attempt, error

maintenance_runs     id, repository_id, kind, started_at, finished_at, status, output

secrets              id, kind, ciphertext, key_id, created_at, rotated_at
```

Two deliberate details: job targets reference `repository_id`, not `destination_id` (§5); and `bytes_added` is recorded separately from `bytes_processed`, because only the former is what the backup actually cost.

---

## 14. Open questions

1. **Restore always runs on the cPanel server** (§10). For a very large account that means pulling the whole payload through the machine being restored. A destination-side runner that reassembled and shipped only the finished archive would be faster, at the cost of more infrastructure. Worth revisiting once there is a real account size distribution to look at.
2. **Mail deltas.** Large mail accounts change constantly and dedup poorly even in `split` mode. Worth measuring before assuming `split` handles it.
3. **Bandwidth scheduling.** `--limit-upload` per policy is easy; fleet-wide coordination so 50 servers do not saturate a shared uplink at 02:00 is not.
4. **pkgacct flag probing.** Needs validation against the specific cPanel versions in the fleet before `monolithic` mode can be called supported (§4).

---

## References

- restic — Working with repositories: <https://restic.readthedocs.io/en/latest/045_working_with_repos.html>
- restic — Scripting / JSON output: <https://restic.readthedocs.io/en/latest/075_scripting.html>
- restic rest-server: <https://github.com/restic/rest-server>
