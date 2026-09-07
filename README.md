# Gniza

**Backup. Restore. Repeat.**

<!-- A gniza is the room a text is kept in rather than destroyed. "gniza" is
     also the name of the binaries, the service, the directories and the Go
     module. Three things kept the old name on purpose — the release
     tarball, the directory inside it and the first word of SHA256SUMS —
     because servers running a release from before the rename ask for
     exactly those; see internal/update/install.go. -->

Hosting-panel fleet backup orchestration on top of
[restic](https://restic.net/). cPanel/WHM is the panel it runs on today;
DirectAdmin and Plesk are next. The name, the paths and the interface are
the product's own rather than any one panel's, so a server is a server
whatever is managing it.

Backup data flows straight from each server to its destinations. The
controller schedules work and records state; it never carries backup bytes,
so adding servers does not turn it into a bandwidth bottleneck.

Running it: **[docs/](docs/README.md)** — install, destinations, schedules,
restores, and what to do when the server is gone.

Full design, including the reasoning behind each component:
**[docs/DESIGN.md](docs/DESIGN.md)**.

## Two ways to run it

**Standalone** — one cPanel server, backing itself up, managed from a WHM
plugin. No controller, no PostgreSQL, no second machine. This is the way in.

**Fleet** — many cPanel servers, a controller that schedules them, and a
separate maintenance host that holds the credentials able to delete
backups. More apparatus, and the only shape where an attacker with root on
a cPanel server cannot destroy your backup history.

Both run the same code for the parts that make a backup correct. See
[ADR 7](docs/adr/0007-standalone-mode.md) for what standalone gives up.

## Installing the WHM plugin

On the cPanel server, as root:

```bash
curl -fsSL https://github.com/shukiv/gniza/releases/latest/download/get.sh | sh
```

That fetches the newest release, checks it against the checksums published
beside it, and runs the installer inside it. `GNIZA_VERSION=v1.2.3` before
`sh` pins a particular release instead of the newest.

Piping a script into a root shell is a reasonable thing to refuse, given what
this one installs. The same install, read first:

```bash
curl -fsSLO https://github.com/shukiv/gniza/releases/latest/download/get.sh
curl -fsSLO https://github.com/shukiv/gniza/releases/latest/download/SHA256SUMS
sha256sum -c --ignore-missing SHA256SUMS
less get.sh
sh get.sh
```

The checksums are signed, and `get.sh` carries the public half of the release
key rather than fetching it beside them. A checksum from the same page as the
tarball only says a download arrived whole; the signature over it says the
release came from whoever holds the release key. Both are checked before
anything is unpacked, and either failing stops the install.

`GNIZA_TARBALL=/path` installs a tarball already on the machine. Nothing is
downloaded, so nothing is verified: that one is yours to trust.

Then open WHM and look for **Gniza Backups** in the left sidebar's
**Plugins** group.

Not on the **Manage Plugins** page — that lists cPanel's own RPM addons.
A plugin registered through AppConfig appears in the sidebar, and its
registration under **Development → Apps Managed by AppConfig**.

The installer refuses anything that is not a cPanel server, installs restic
if the server has none — the version Gniza is built against, checked against
restic's own published checksum — installs the service and the plugin,
registers it with WHM through AppConfig, confirms WHM kept the registration,
and registers cPanel Standardized Hooks for account create, modify, suspend,
unsuspend and remove. Running it again upgrades in place.

### Keeping it current

Every page says so when a newer release exists, with what changed and the
version running here. **Settings → This copy of Gniza** installs it: one
button, a confirmation, and a card that says how it went. It downloads the
release, checks the release key signed it, runs the same installer as a hand
install and restarts the service — destinations, schedules, backups and
history stay. Queued backups stay queued and run afterwards; one that is
running is why the button refuses until it has finished. Nothing that the
release key did not sign is unpacked, let alone run.

**Settings → Where updates come from** offers a second channel: the **dist
branch**, which carries whatever was last built rather than whatever was last
released. Publishing to it is one command on the machine that holds the
release key:

```bash
make release GNIZA_SIGNING_KEY_FILE=~/.gniza/gniza-release.pem
```

That builds the plugin, signs the checksums, and pushes three files — the
tarball, the checksums, the signature — to the `dist` branch. A server on that
channel reads them over https and checks them exactly as it checks a release:
same key, same signature, same refusal if either is wrong. What it saves is
having to decide that a commit deserves a version number, not any of the
checking. Builds of a branch carry the commit they were made from, and that is
what says which of two is later; an older build put back on the branch is
refused rather than installed backwards.

The check reads a version number from GitHub once a day and installs nothing
on its own — an update that arrives unattended is a way for whoever can
publish a release to reach every server running this, as root, with nobody
looking. Settings turns the check off, and **Check now** asks straight away.
The install command still works, for a server that would rather do it that
way.

### Building it yourself

No release needed, and the way to install a change you have made. On a machine
with Go:

```bash
# The tarball keeps the name this project had before Gniza: a server on an
# older release asks for that exact file, and would never find a renamed one.
make plugin           # builds bin/cprest-plugin-amd64.tar.gz
```

Copy the tarball over:

```bash
scp bin/cprest-plugin-amd64.tar.gz root@your-server:/root/
```

Then unpack it and run the installer there, as root:

```bash
tar xzf cprest-plugin-amd64.tar.gz
sh cprest-plugin/install.sh
```

Through `sh` because cPanel mounts `/tmp` and `/var/tmp` noexec, and an
installer unpacked there will not run otherwise.

A tag beginning `v` builds and publishes that tarball, its checksums and
`get.sh` as a GitHub release
([the workflow](.github/workflows/release.yml)). It runs the same
`make plugin`, so the `gniza-agent` in a release is what that tag builds
here: statically linked and `-trimpath`, with no path or host of the build
machine in it.

From there: add a destination, add a schedule, and the server backs itself
up. Restores are on the Restore page — a whole account, or named files —
and by default a restore rebuilds the archive and leaves it for you rather
than overwriting anything.

Its pages are addressed as `gniza.cgi?p=destinations` and so on: cpsrvd
will not route a path after the script name, so routes travel in a query
parameter ([ADR 8](docs/adr/0008-query-string-routing-under-whm.md)).

Some plugin behavior worth knowing:

- The account hooks act as well as record. A new account gets an immediate
  baseline backup, under a complete all-account schedule where there is one and
  otherwise the widest coverage there is. A rename keeps its named-policy
  membership, and a terminated name cannot turn a one-account policy into an
  all-account policy. The latest 100 hook outcomes are on the WHM overview; raw
  cPanel hook payloads are not kept.
- The overview treats policy promises as coverage, not merely the existence
  of an old backup. It reports overdue, partial, failed and unscheduled
  accounts, and flags each destination whose required copy is missing or
  stale according to that destination's own schedule. **Repair copies** runs
  the covering policy that fixes the most gaps, after the service validates
  that the policy still covers the account.
- Settings can opt in to blocking cPanel account termination when the account
  lacks recent complete copies at every destination promised by a full-account
  schedule. Enabling it requires a new successful backup: older job records do
  not prove which payload exclusions were in force. Gniza performs only a
  local history check inside cPanel's synchronous hook; it never tries to run a
  long backup while WHM is waiting. The Accounts list and account detail page
  preview the same decision before an administrator opens cPanel's termination
  flow. **Prepare for termination** queues the smallest combination of
  full-account schedules that refreshes every missing promised destination;
  multiple jobs run sequentially and the action never deletes the account. If
  the service itself is unavailable, the hook logs the failure and allows
  removal so cPanel administration cannot be wedged. See
  [ADR 14](docs/adr/0014-account-termination-safety.md).
- Settings can also opt in to preserving an account as soon as cPanel suspends
  it. The post-suspension hook atomically queues the smallest set of enabled
  full-account schedules needed to reach every destination those schedules
  promise, then returns without waiting for `pkgacct` or remote storage.
  Repeated suspension events do not add work while that account already has a
  backup or restore queued or running. Unsuspension is recorded for diagnosis
  but does not queue a backup. See
  [ADR 15](docs/adr/0015-suspension-preservation.md).
- The interface listens on a unix socket, not a port. cPanel servers are
  multi-tenant and this interface can read every stored credential, so it
  is not reachable over the network at all; the WHM plugin proxies to it and
  refuses any WHM user that is not root.
- The account-facing cPanel tile accepts the account owner and Administrator
  Team users. The root service verifies the live cpsrvd session itself and
  gives the proxy a 20-second, one-use token for the exact request; sharing
  the account's Unix uid is not sufficient to use the public socket. A WHM
  root administrator using cPanel's **Log in to cPanel** flow is accepted only
  when the root-owned session record carries cPanel's temporary-user and
  cPHulk markers.
- The account recovery centre follows restore point → category → item. It can
  prepare a full cPanel account archive, the home directory, cron jobs,
  databases and users, domains and DNS, SSL material, mail, or FTP settings.
  These self-service requests always produce a private download; they cannot
  set the operator-only `Apply` flag or overwrite the live account.
- Destination credentials are encrypted with a key in `/etc/gniza/master.key`.
  Back that file up somewhere other than this server. Without it the stored
  credentials cannot be read.

Standalone mode makes one deliberate trade. Retention runs on the cPanel
server, so the credential able to delete backups lives on the machine an
attacker would compromise. Fleet mode keeps it on a separate host and an
append-only endpoint then means backup history cannot be destroyed at all.
[ADR 7](docs/adr/0007-standalone-mode.md) sets out what you give up.

## Removing it

Two ways, and they do the same thing:

- **Settings → Remove Gniza from this server**, the last card on the page.
  It asks first and says what goes and what stays.
- In a root shell:

  ```bash
  sh /usr/local/share/gniza/uninstall.sh
  ```

The installer leaves that copy of the uninstaller on the server, so removing
the plugin never means finding the package again.

**What it removes**

| | |
|---|---|
| The service | stopped, disabled, `/etc/systemd/system/gniza.service` gone |
| The WHM plugin | `gniza.cgi`, its AppConfig registration, the sidebar icon |
| The cPanel hooks | account create, remove, suspend and unsuspend, unregistered through `manage_hooks` |
| The account tile | the customer-facing plugin, its UAPI module and AdminBin |
| The programs | `/usr/local/bin/gniza-agent`, `/usr/local/cpanel/3rdparty/bin/gniza-hook` |
| restic's cache | `/var/cache/gniza`, rebuilt from the repository if you reinstall |
| Itself | `/usr/local/share/gniza`, last, since the script is running out of it |

**What it does not touch**

- **Your backups.** Nothing on any destination is read, written or deleted.
  Removing the plugin is not removing the backups, and there is no path
  through the uninstaller that reaches them.
- `/etc/gniza/master.key` — the key your stored destination credentials are
  encrypted with.
- `/var/lib/gniza/state.db` — destinations, schedules, and the history of
  every run.
- Anything a restore left in `/var/lib/gniza/staging` for collection, and
  `restic` itself, which the installer put in `/usr/local/bin` and other
  things may be using.

Reinstalling picks up from those two files: the same destinations, the same
schedules, the same history. Delete them only when you are certain you will
not, and never before you have another way to read what is on your
destinations — without the key file, the stored credentials cannot be
recovered, and credentials nobody can read are backups nobody can reach.

## What runs

```
Controller ──control only──> Agent (per cPanel server) ──data──> Destinations
                                                                      ▲
                             Maintenance runner ───────delete─────────┘
```

| Binary | Runs on | Does |
|---|---|---|
| `gniza-agent -standalone` | one cPanel server | the whole thing on its own: local state, its own schedule, the interface behind the WHM plugin |
| `gniza.cgi` | one cPanel server | the WHM plugin: proxies WHM to that interface, root only |
| `gniza-controller` | trusted infrastructure | agent API over mTLS, scheduler, credential vault, administration CLI |
| `gniza-agent` | every cPanel server | polls for jobs, stages a payload once, uploads it to each target repository |
| `gniza-maintenance` | trusted infrastructure | provisions repositories, applies retention, verifies integrity, rehearses restores |

The **maintenance runner** is not optional. Destinations we control run
`rest-server --append-only`, which rejects deletes — so nothing on a cPanel
server can prune, and without a separate trusted actor repositories grow
forever.

One deployment detail matters and is easy to get wrong: `--append-only` is a
property of the rest-server **process**, not of a credential, so it blocks
the maintenance runner too. An append-only destination needs a second
rest-server over the same data directory with append-only off, reachable
only from the management network. Record it as `maintenance_base_url` on the
destination; the maintenance runner uses it, agents never see it. See
DESIGN §8.

## Status

The pipeline works end to end and is covered by a suite that drives real
PostgreSQL, real restic and a real append-only rest-server.

| Area | State |
|---|---|
| Controller: agent API, mTLS auth, job leasing, scheduler, vault | working |
| Agent: enrolment, polling, staging, multi-target upload, reporting | working |
| Maintenance: provision, retention, integrity check, restore drills | working |
| Restore: whole account, single file, opt-in apply to the live account | working |
| Administration CLI (`gniza-controller <command>`) | working |
| Real cPanel provider (`pkgacct`, `mysqldump`) | working; verified on cPanel 136 |
| `restorepkg` (applying a restore) | implemented; live certification command provided for an isolated cPanel host |
| Downloading a rebuilt account archive | working |
| Standalone mode + WHM plugin GUI | working |
| Controller web UI | not built; the API and CLI are the fleet-mode interface |
| Azure/GCS/rclone destinations | not built |

## Five decisions worth knowing before reading the code

1. **Never feed restic a compressed archive.** `pkgacct` compresses by
   default, and a gzip stream defeats content-defined chunking: every
   nightly run then stores a full copy, in every destination. Payload mode
   `split` is the default for this reason, and the end-to-end suite asserts
   that a second unchanged backup stores under a tenth of what it reads.
   See DESIGN §4.
2. **Chunker parameters are chosen once, forever.** Every repository after a
   server's first is created with `--copy-chunker-params`, so switching to
   "back up once, replicate with `restic copy`" stays possible. Retrofitting
   is impossible. The store fills the source in automatically and a database
   trigger enforces it. See DESIGN §7.
3. **Restore is a job like any other.** It runs on the cPanel server, is
   leased through the same endpoint as a backup, and reads only the
   repository the controller names. Applying the result to the live
   account is opt-in; by default the rebuilt archive is left in place and
   nothing is overwritten. See DESIGN §10.
4. **Secrets never reach argv.** `/proc/<pid>/cmdline` is world-readable on a
   multi-tenant cPanel server. Backend credentials go in the child
   environment; repository passwords go in transient mode-0600 files. File
   *paths* are not secrets, so ssh's identity and known-hosts files do travel
   in `-o sftp.args`. See DESIGN §5, §11.
5. **A backup can succeed and still be incomplete.** One database that will
   not dump no longer costs an account its whole backup; what could not be
   taken is recorded, named on the row and in the notification, and makes
   the run something termination safety refuses to act on. Target rollup
   says where the copies went, not what went into them, so `complete_account`
   and not `status` is what may be read as "safe to delete". See DESIGN §9
   and [ADR 17](docs/adr/0017-a-backup-carries-on-past-a-database-it-cannot-dump.md).

## Build and test

```bash
make            # fmt, vet, test, build
make test       # unit tests; suites needing external services skip themselves
make tools      # install the pinned restic and rest-server used by make e2e
make e2e        # full pipeline against real PostgreSQL, restic and rest-server
```

Go 1.25. Dependencies: `pgx/v5` and `robfig/cron/v3`, nothing else.

`make e2e` needs PostgreSQL (any local installation; the suite starts its own
throwaway cluster on a unix socket), restic v0.19.1 and rest-server v0.14.0.
It writes scratch data to `.tmp/` rather than `/tmp`, which is often a tmpfs
too small for a Postgres cluster plus restic repositories. The suite is
behind the `e2e` build tag so `make test` stays fast and hermetic.

What it proves, against the real binaries:

- repositories provisioned, with the second one's chunker polynomial
  verified equal to the first's via `restic cat config`
- an account backed up to a local destination and an append-only
  rest-server, snapshots and tags read back with `restic snapshots --json`
- a second unchanged run storing under a tenth of what it reads
- the append-only endpoint returning 403 to a delete, and the maintenance
  endpoint over the same data directory pruning successfully
- retention actually removing a snapshot, which requires the paths and tags
  of two runs to be identical
- one unreachable destination producing `partial_success`, with the
  reachable copy intact
- a client certificate that is not registered being refused
- an account too large for the staging volume refused before pkgacct runs
- a whole account restored and compared **byte for byte** against the
  original tree, with its database dumps and metadata
- a single file recovered at its original path, and nothing else with it
- `restorepkg` invoked only when the restore asked for it
- a restore read back through the append-only endpoint, which never
  restricted reads
- a drill rehearsing a restore and recording what it checked
- a second restore of an account superseding the archive the first left

## Getting started

```bash
# 1. Secrets and PKI
gniza-controller keygen -out /etc/gniza/master.key
gniza-controller init-ca -dir /etc/gniza/pki
gniza-controller issue-cert -kind server -name controller.example.com \
    -ca-dir /etc/gniza/pki -hosts controller.example.com

export GNIZA_DATABASE_URL=postgres://gniza@localhost/gniza
export GNIZA_MASTER_KEY=/etc/gniza/master.key
gniza-controller migrate

# 2. Register a cPanel server. The agent certificate's fingerprint is the
#    identity: a certificate signed by the CA is not enough on its own.
gniza-controller issue-cert -kind agent -name cp01.example.com -ca-dir /etc/gniza/pki
gniza-controller add-server -hostname cp01.example.com -cert /etc/gniza/pki/cp01.example.com.pem

# 3. Destinations. Keep credentials out of the command line with @file or $ENVVAR.
gniza-controller add-destination -name "Bogota rest-server" -type rest -append-only \
    -config '{"base_url":"https://backup.example.com",
              "maintenance_base_url":"https://backup.internal:8000"}' \
    -secrets 'username=cp01,password=@/etc/gniza/rest.pass'
gniza-controller add-destination -name "Wasabi Miami" -type s3 \
    -config '{"endpoint":"s3.us-east-1.wasabisys.com","bucket":"cp-backups","region":"us-east-1"}' \
    -secrets 'access_key_id=$WASABI_KEY,secret_access_key=$WASABI_SECRET'

# 4. Repositories, policy, account
gniza-controller add-repository -server <server-id> -destination <dest-id> -path cp01
gniza-controller add-policy -name nightly -cron '0 2 * * *' -keep-daily 7 -keep-monthly 6
gniza-controller attach -policy <policy-id> -repository <repo-id>
gniza-controller add-account -server <server-id> -user customer1
gniza-controller attach -policy <policy-id> -account <account-id>

# 5. Create the repositories on their destinations, then serve
gniza-maintenance -kind provision
gniza-controller serve -listen :8443 \
    -tls-cert /etc/gniza/pki/controller.example.com.pem \
    -tls-key  /etc/gniza/pki/controller.example.com-key.pem \
    -client-ca /etc/gniza/pki/ca.pem \
    -master-key /etc/gniza/master.key
```

On the cPanel server:

```bash
gniza-agent -preflight    # check restic, staging space and pkgacct flags
gniza-agent -controller https://controller.example.com:8443 \
    -client-cert /etc/gniza/cp01.example.com.pem \
    -client-key  /etc/gniza/cp01.example.com-key.pem \
    -ca-bundle   /etc/gniza/ca.pem
```

Then, from cron on the maintenance host:

```bash
gniza-maintenance -kind forget            # retention and prune
gniza-maintenance -kind check -read-data-subset 5
gniza-maintenance -kind drill             # rehearse a restore and record it
```

## Restoring

```bash
# What is there?
gniza-controller snapshots -server cp01.example.com -user customer1

# One file back, at the path it came from. Nothing else is touched.
gniza-controller restore -server cp01.example.com -user customer1 \
    -snapshot 40dc1520 \
    -files /home/customer1/public_html/index.php -target /root/recovered

# The whole account. By default the agent rebuilds the cpmove archive and
# leaves it on the server; add -apply to hand it to restorepkg, which
# overwrites the live account.
gniza-controller restore -server cp01.example.com -user customer1 -snapshot 40dc1520

gniza-controller restore-status -job <restore-id>
```

To prove that cPanel itself accepts a rebuilt archive, copy it to a dedicated
certification cPanel host and run:

```bash
gniza-agent -certify-live-archive /root/cpmove-customer1.tar \
    -certify-user cprv1234 -certify-isolated-host
```

This performs a restricted `restorepkg --newuser`, disables DNS-zone
updates, confirms cPanel registered the disposable account, then runs
`removeacct --force`. Do not run it on the production source server: domain
ownership can conflict, and the command intentionally creates and deletes a
cPanel account. The explicit isolation flag is required. The command writes a
JSON audit record to stdout with timestamps, the disposable username, every
completed check, and any failure.

The rebuilt archive stays on the cPanel server under the staging root until
the next restore of that account, or until the agent restarts — its startup
sweep cannot tell a finished restore's output from a crashed one's debris.
Collect it promptly; `restore-status` prints where it is.

`gniza-agent -fake-cpanel-root <dir>` swaps in a synthetic cPanel provider,
which is how the agent is exercised on a machine with no cPanel.

## Layout

```
cmd/
  agent/         runs on each cPanel server
  controller/    agent API, scheduler and administration CLI
  maintenance/   provisioning, retention, integrity checks
  whmcgi/        the WHM plugin CGI
internal/
  agent/         the job loop and the controller client
  certs/         CA and certificate issuance for agent mTLS
  controller/    service layer, agent API, scheduler
  cpanel/        payload production: real pkgacct provider and a synthetic one
  destination/   storage endpoints: URI, environment, ssh options, preflight
  e2e/           the whole pipeline against real dependencies
  job/           job and per-target status model
  maintenance/   repository upkeep with delete-capable credentials
  pkgacct/       capability probing and payload planning
  node/          standalone engine: scheduling and running work with no controller
  nodestore/     standalone state: destinations, schedules and history in bbolt
  protocol/      wire types shared by agent and controller
  reassemble/    rebuilding a cpmove archive from split backup parts
  webui/         the interface the WHM plugin proxies to
  repobuild/     the single path from sealed credentials to a usable repository
  resticrun/     the single restic execution path
  staging/       scratch space allocation and reclamation
  store/         PostgreSQL queries and the migration runner
  testsupport/   throwaway PostgreSQL and binary discovery for tests
  vault/         envelope encryption for stored credentials
migrations/      PostgreSQL schema (fleet mode)
packaging/whm/   WHM plugin installer
docs/            DESIGN.md and architecture decision records
```

Notes:

- The module path `github.com/shukiv/gniza` is a placeholder; no remote
  exists yet.
- `make clean` uses `trash` rather than `rm`.
