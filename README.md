# Gniza

**Backup. Restore. Repeat.**

<!-- A gniza is the room a text is kept in rather than destroyed. "gniza" is
     also the name of the binaries, the service, the directories and the Go
     module. Three things kept the old name on purpose — the release
     tarball, the directory inside it and the first word of SHA256SUMS —
     because servers running a release from before the rename ask for
     exactly those; see internal/update/install.go. -->

Server backup orchestration on top of [restic](https://restic.net/). It
runs on **cPanel/WHM**, on **DirectAdmin**, and on a **server with no
panel** — a LAMP box, a docker or podman host. One server, one panel; the
scheduler, the repositories and the restore machinery do not care which
([ADR 19](docs/adr/0019-the-panel-behind-an-interface.md)).

Backup data flows straight from each server to its destinations. In fleet
mode the controller schedules work and records state; it never carries
backup bytes.

Running it: **[docs/](docs/README.md)** — install, destinations, schedules,
restores, and what to do when the server is gone. The design, with the
reasoning behind each part: **[docs/DESIGN.md](docs/DESIGN.md)**.

## Two ways to run it

**Standalone** — one server backing itself up, managed from its panel's
plugin or from the terminal. No controller, no PostgreSQL, no second
machine. This is the way in.

**Fleet** — many servers, a controller that schedules them, and a separate
maintenance host that holds the credentials able to delete backups. More
apparatus, and the only shape where an attacker with root on a server
cannot destroy your backup history.

Both run the same code for the parts that make a backup correct.
[ADR 7](docs/adr/0007-standalone-mode.md) sets out what standalone gives
up: retention runs on the server itself, so the credential able to delete
backups lives on the machine an attacker would compromise.

## Installing

On the server, as root:

```bash
curl -fsSL https://github.com/shukiv/gniza/releases/latest/download/get.sh | sh
```

That fetches the newest release, checks it against the signed checksums
published beside it, and runs the installer for the server it is on:
`/usr/local/cpanel` means the WHM plugin, `/usr/local/directadmin` the
DirectAdmin plugin, and neither means the plain package. A release carries
all three tarballs, signed in one `SHA256SUMS`. `get.sh` carries the public
half of the release key rather than fetching it beside the checksums, and
either check failing stops the install before anything is unpacked.
`GNIZA_VERSION=v1.2.3` pins a release; `GNIZA_TARBALL=/path` installs one
already on the machine, unverified.

To read the script before a root shell does:

```bash
curl -fsSLO https://github.com/shukiv/gniza/releases/latest/download/get.sh
curl -fsSLO https://github.com/shukiv/gniza/releases/latest/download/SHA256SUMS
sha256sum -c --ignore-missing SHA256SUMS
less get.sh
sh get.sh
```

Each installer installs restic if the server has none — the version Gniza
is built against, checked against restic's own checksum — installs the
service, and registers the panel's account hooks so a created, suspended
or removed account is handled the moment it happens. Running it
again upgrades in place. Then:

| Server | Where the interface is |
|---|---|
| cPanel/WHM | WHM sidebar, **Plugins → Gniza Backups**. Customers get a tile in cPanel with their own restore points. |
| DirectAdmin | **Admin Tools → Gniza**, in the Evolution skin. |
| No panel | `gniza-agent -tui` as root, or a browser at the address the installer asks about, behind a password. |

From there: add a destination, add a schedule, and the server backs itself
up. Destinations are SFTP, S3, a restic REST server or a local disk.
Restores are a whole account, a deleted account, or named files, and by
default a restore rebuilds the archive and leaves it for you rather than
overwriting anything.

### What each panel gets

**cPanel/WHM** is the reference. Split-payload backups through `pkgacct`,
account hooks, restore through `restorepkg`, single-item restore of files,
databases, mail, cron, domains, DNS and SSL, a customer-facing tile,
termination and suspension safety
([ADR 14](docs/adr/0014-account-termination-safety.md),
[ADR 15](docs/adr/0015-suspension-preservation.md)).

**DirectAdmin** since v0.3.0. Accounts are listed, hooked, backed up and
restored through DirectAdmin's own tools. A backup asks DirectAdmin for the
account's records without its files and hands restic the home directory as
a path, which is what makes nightly runs store only what changed; a
restore drill proved the rebuilt archive on two servers before that became
the default ([ADR 21](docs/adr/0021-the-account-is-read-where-it-lies.md)).
An account the server no longer has is restored, not refused. Not yet:
restoring one database or one mailbox out of a backup, schedule exclusions
beyond the home directory, the server's own configuration, and the
customer-facing pages. The plugin says so where an operator reads it rather
than guessing. [The DirectAdmin guide](docs/guide/directadmin.md) has the
full list.

**A server without a panel** since v0.3.6
([ADR 22](docs/adr/0022-a-server-without-a-panel.md)). An account is a
directory directly under one of the roots in `/etc/gniza/plain.env`,
`/var/www`, `/srv` and `/opt` by default. A MySQL database named after the
account, alone or with an underscore and a suffix, is backed up with it.
The system backup takes the web, PHP, database, container, cron and SSH
configuration, the certificates, the unit files and a manifest of packages,
containers and volumes. Restore of files and databases is written and not
yet proved on a live server; the provider says so at start.

The installer asks where a browser may reach the pages: this machine
only on `127.0.0.1:8443`, over `ssh -L`; every address on `0.0.0.0:8443`,
over TLS; or nowhere. Either way the door is a password, set on the
terminal without echo and changed with `gniza-agent -web-set-password`,
with a lockout behind it
([ADR 24](docs/adr/0024-a-browser-on-a-server-without-a-panel.md)).

### The terminal interface

```bash
gniza-agent -tui
```

The same screens the plugins draw, in the terminal: overview with the
verdict, destinations, schedules, accounts, the five log tabs, restore and
settings. It adds destinations — agreeing to an sftp host key before
anything is sent, showing the recovery key once — adds, edits, runs and
removes schedules, and backs an account up now. Asking for a restore or
changing a setting from it is not written yet; the browser interface does
both, and the [plain-server guide](docs/guide/plain-server.md) keeps the
`curl` forms for scripts.

Under it, every page answers as data when asked with
`Accept: application/json`. That is what the terminal reads, and what a
script can.

### Keeping it current

Every page says so when a newer release exists. **Settings → This copy of
Gniza** installs it: one button, a confirmation, and a card that says how
it went. Nothing the release key did not sign is unpacked. The check reads
a version number from GitHub once a day and installs nothing on its own;
Settings turns it off.

**Settings → Where updates come from** offers the **dist branch** as well:
whatever was last built rather than last released, checked with the same
key. `make release GNIZA_SIGNING_KEY_FILE=~/.gniza/gniza-release.pem`
publishes to it from the machine that holds the key.

### Building it yourself

```bash
make plugin      # bin/cprest-plugin-amd64.tar.gz, gniza-directadmin-amd64.tar.gz, gniza-plain-amd64.tar.gz
```

Copy the tarball for the server over, unpack it there and run
`sh <dir>/install.sh` as root — through `sh`, because cPanel mounts `/tmp`
noexec. A tag beginning `v` builds the same three tarballs, their checksums
and `get.sh` as a GitHub release
([the workflow](.github/workflows/release.yml)); the binaries are static
and `-trimpath`.

## Worth knowing

- The interface listens on a unix socket, not a port. The service can read
  every stored credential, so it is not reachable over the network; the
  plugin proxies to it and refuses any panel user that is not the
  administrator. The customer-facing socket proves the request came from
  the customer's own live session
  ([ADR 12](docs/adr/0012-the-account-facing-socket.md)).
- The overview treats a schedule's promise as coverage, not the existence
  of an old backup: overdue, partial, failed and unscheduled accounts, and
  each destination whose required copy is missing or stale.
  **Repair copies** runs the policy that closes the most gaps.
- A backup can succeed and still be incomplete. A database that will not
  dump no longer costs the account its backup; what was left out is
  recorded, named on the row and in the notification, and nothing that
  deletes an account will act on it
  ([ADR 17](docs/adr/0017-a-backup-carries-on-past-a-database-it-cannot-dump.md)).
- Notifications go to SMTP, ntfy, Telegram or a webhook.
- Destination credentials are encrypted with `/etc/gniza/master.key`.
  **Copy that file somewhere that is not this server.** Without it the
  stored credentials cannot be read, and credentials nobody can read are
  backups nobody can reach.

## Removing it

**Settings → Remove Gniza from this server**, or in a root shell:

```bash
sh /usr/local/share/gniza/uninstall.sh
```

Every installer leaves that copy behind. It removes the service, the
plugin and its hooks, the programs and restic's cache. It never touches
your backups: nothing on any destination is read, written or deleted. It
also leaves `/etc/gniza/master.key` and `/var/lib/gniza/state.db`, so a
reinstall picks up the same destinations, schedules and history.

## What runs

```
Controller ──control only──> Agent (per server) ──data──> Destinations
                                                               ▲
                             Maintenance runner ─────delete────┘
```

| Binary | Runs on | Does |
|---|---|---|
| `gniza-agent -standalone` | one server | the whole thing on its own: local state, its own schedule, the interface behind the plugin and the terminal |
| `gniza.cgi` / the DirectAdmin plugin | that server | proxies the panel's administrator to the interface |
| `gniza-controller` | trusted infrastructure | agent API over mTLS, scheduler, credential vault, administration CLI |
| `gniza-agent` | every server | polls for jobs, stages a payload once, uploads it to each target repository |
| `gniza-maintenance` | trusted infrastructure | provisions repositories, applies retention, verifies integrity, rehearses restores |

In fleet mode the maintenance runner is not optional: destinations run
`rest-server --append-only`, which rejects deletes, so nothing on a server
can prune. `--append-only` is a property of the rest-server process, so an
append-only destination needs a second rest-server over the same data
directory, reachable only from the management network, recorded as
`maintenance_base_url` (DESIGN §8).

## Status

| Area | State |
|---|---|
| cPanel/WHM: provider, plugin, customer tile, hooks, termination and suspension safety | working, in production |
| DirectAdmin: provider, plugin, hooks, lean backup, restore of existing and deleted accounts | working, in production; granular restore and customer pages not built |
| Plain server: provider, package, system backup, browser interface behind a password | working; restore not yet proved live |
| Terminal interface | destinations, schedules, backups, logs; restore and settings read-only |
| Standalone: scheduling, retention, drills, update from release or dist branch | working |
| Fleet: controller API, mTLS, job leasing, scheduler, vault, maintenance runner | working, covered by the e2e suite |
| Controller web UI | not built; the API and CLI are the fleet interface |
| Azure/GCS/rclone destinations | not built |

## Five decisions worth knowing before reading the code

1. **Never feed restic a compressed archive.** A gzip stream defeats
   content-defined chunking. Every provider reads the account's files
   where they lie, and the e2e suite asserts a second unchanged backup
   stores under a tenth of what it reads (DESIGN §4).
2. **Chunker parameters are chosen once, forever.** Every repository
   after a server's first is created with `--copy-chunker-params`, so
   replicating with `restic copy` stays possible (DESIGN §7).
3. **Restore is a job like any other.** It runs on the server, reads only
   the repository named, and applies to the live account only when asked
   (DESIGN §10).
4. **Secrets never reach argv.** `/proc/<pid>/cmdline` is world-readable
   on a shared server. Credentials go in the child environment and
   passwords in transient mode-0600 files (DESIGN §5, §11).
5. **The panel is behind an interface.** A provider says what it can do
   and a layout says where it keeps things; nothing downstream has a
   cPanel default to fall back to
   ([ADR 19](docs/adr/0019-the-panel-behind-an-interface.md)).

## Build and test

```bash
make            # fmt, vet, test, build
make test       # unit tests; suites needing external services skip themselves
make css        # recompile the plugin stylesheet (Tailwind + daisyUI); the result is committed
make tools      # install the pinned restic and rest-server used by make e2e
make e2e        # full pipeline against real PostgreSQL, restic and rest-server
```

Go 1.26. Direct dependencies: `pgx/v5`, `robfig/cron/v3`, `x/crypto`,
`klauspost/compress`, and bubbletea with lipgloss and bubbles for the
terminal. `make e2e` writes to `.tmp/`, not `/tmp`, and sits behind the
`e2e` build tag. `make clean` uses `trash` rather than `rm`.

`gniza-agent -fake-cpanel-root <dir>` swaps in a synthetic cPanel provider,
which is how the agent is exercised on a machine with no panel.

## Layout

```
cmd/
  agent/          runs on each server: standalone, fleet agent, terminal interface
  controller/     agent API, scheduler and administration CLI
  maintenance/    provisioning, retention, integrity checks
internal/
  panel/          the Provider and Layout interfaces every panel implements
  cpanel/         cPanel provider: pkgacct, restorepkg, and a synthetic one
  directadmin/    DirectAdmin provider
  plain/          a server with no panel
  layout/         where each panel keeps things inside an archive
  agent/          the job loop and the controller client
  node/           standalone engine: scheduling and running work with no controller
  nodestore/      standalone state in bbolt
  webui/          the pages the plugins proxy to
  answer/         a page as JSON, for the terminal and for scripts
  tui/            the terminal interface
  granular/       single-item restore
  reassemble/     rebuilding an archive from split backup parts
  update/         installing a release or a dist-branch build in place
  notify/         SMTP, ntfy, Telegram, webhook
  destination/    storage endpoints: URI, environment, ssh options, preflight
  repobuild/      from sealed credentials to a usable repository
  resticrun/      the single restic execution path
  staging/        scratch space allocation and reclamation
  controller/     fleet service layer, agent API, scheduler
  maintenance/    repository upkeep with delete-capable credentials
  store/          PostgreSQL queries and the migration runner
  vault/          envelope encryption for stored credentials
  e2e/            the whole pipeline against real dependencies
migrations/       PostgreSQL schema (fleet mode)
packaging/
  whm/            WHM plugin, get.sh, installers
  cpanel/         the customer-facing cPanel tile
  directadmin/    DirectAdmin plugin
  plain/          the plain package
docs/             guide, DESIGN.md, release notes, architecture decision records
```
