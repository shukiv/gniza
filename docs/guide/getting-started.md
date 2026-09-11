# Getting started

One cPanel server, backing itself up, managed from WHM. No controller, no
second machine, no database to run.

## Before you install

- **root on the cPanel server.** The plugin refuses every WHM user that is not root.
- **restic.** The installer fetches it if this server has none, checked
  against restic's own published checksum.
- **Somewhere to put backups.** Another Linux box over SSH, an S3 bucket, a restic
  REST server, or a mounted disk. See [Destinations](destinations.md).
- **Room to stage.** A backup rebuilds one account in full on local disk before it
  is uploaded, so the staging volume needs room for your largest account.

## Install

On the cPanel server, as root:

```bash
curl -fsSL https://github.com/shukiv/gniza/releases/latest/download/get.sh | sh
```

One command. It fetches the newest release, checks it against the checksums
published beside it, and runs the installer inside it. To read the script
before a root shell does, download it with its checksums first:

```bash
curl -fsSLO https://github.com/shukiv/gniza/releases/latest/download/get.sh
curl -fsSLO https://github.com/shukiv/gniza/releases/latest/download/SHA256SUMS
sha256sum -c --ignore-missing SHA256SUMS
less get.sh
sh get.sh
```

`GNIZA_VERSION=v1.2.3` before `sh` installs that release rather than the
newest.

The checksums are signed and `get.sh` carries the public half of the release
key, so the check is not "these two files from the same page agree" but "this
release came from whoever holds the key". A download that fails either check
installs nothing.

The installer installs restic if this server has none, installs the service,
registers the plugin with WHM through AppConfig, confirms WHM kept the
registration, and registers cPanel hooks for account create, modify, suspend,
unsuspend and remove. Running it again upgrades in place.

Then open WHM and look for **Gniza Backups** in the sidebar's **Plugins**
group. Not under **Manage Plugins** — that page lists cPanel's own RPM addons;
an AppConfig plugin appears in the sidebar, and its registration under
**Development → Apps Managed by AppConfig**.

### From source instead

For a change you have made, or a machine you would rather not download to.
On a machine with Go:

```bash
# The tarball keeps the name this project had before Gniza: a server on an
# older release asks for that exact file, and would never find a renamed one.
make plugin           # builds bin/cprest-plugin-amd64.tar.gz
scp bin/cprest-plugin-amd64.tar.gz root@your-server:/root/
```

Then on the cPanel server, as root:

```bash
tar xzf cprest-plugin-amd64.tar.gz
sh cprest-plugin/install.sh
```

Through `sh` because cPanel mounts `/tmp` and `/var/tmp` noexec: an installer
unpacked there will not run otherwise.

## The first ten minutes

1. **Destinations → Add a destination.** Fill in where the backups go. Gniza
   tests the connection and initialises the repository before saving anything.
2. **Write down the recovery key.** It is shown once, on the recovery card. Without
   it those backups cannot be read — not by this program, not by the machine
   holding them, not by you.
3. **Schedules → Add a schedule.** Nightly, every account, split mode, whatever
   retention you want. See [Schedules](schedules.md).
4. **Accounts → Back up now** on one account, to watch a real run finish rather
   than waiting until 02:00 to find out something was wrong.
5. **Overview** should then read *n of n accounts have a usable backup*.

## Keeping it current

Every page says so when a newer Gniza has been released: the version, what
changed, and the version this server runs.

**Settings → This copy of Gniza** installs it. The button names the
version; the page that follows says what happens and asks for a tick, because
this replaces the program on the server and restarts it. Then the card follows
it through — downloading, installing, and what the installer said — and keeps
following it across the restart, so the page an operator is watching is the
page that tells them it worked.

What it does is what a hand install does: download the release, check it, run
`install.sh`. What is different is what happens before the installer is handed
anything. The checksums published with a release are signed with the Gniza
release key, which is compiled into this program; a release whose signature
does not verify, or which arrives without one, stops there, with nothing
unpacked and nothing run. Then the tarball is checked against those signed
checksums. Only then is anything unpacked, and only ordinary files under
`cprest-plugin/` are written — a path leading out of that directory, or a
symlink, is refused rather than followed.

The installer runs outside this service, as a transient systemd unit, because
installing restarts the service that started it. Backups already queued stay
queued and run afterwards; a backup that is *running* is why the button
refuses until it has finished, since a restart would fail it.

### Following the work instead of the releases

**Settings → Where updates come from** has two answers. *Published releases* is
the default: versions somebody decided to make. *The dist branch* is whatever
was built last.

Publishing to it is a run of the release workflow started by hand, since the
release key lives in the repository's secrets and nowhere else:

```bash
gh workflow run release --repo shukiv/gniza --ref master
```

It builds the plugin from master, signs the checksums with that key, and
pushes the tarballs, the checksums and the signature to the `dist` branch —
written with git's plumbing, so nothing is checked out. A tag push publishes a
release instead and leaves the branch alone. On a machine that does hold the
key, `make release GNIZA_SIGNING_KEY_FILE=…` does the same from a working
tree, and `GNIZA_DIST_PUSH=0` stops it before the push if you would rather
look first.

Servers on that channel read those three files and check them exactly as they
check a release. Nothing about the checking is relaxed; what is relaxed is
having to tag. Two consequences worth knowing:

- Builds of a branch have no version numbers that can be compared, so each one
  carries the commit it was made from. That is what says which of two builds is
  later, and it is why an older build put back on the branch is refused instead
  of installed backwards.
- A branch moves. What is published between the page being read and the button
  being pressed is not what was agreed to, so the version is checked again at
  the moment of fetching and the install stops rather than taking something
  else.

Changing the channel forgets what the other one had found, because that was
about somewhere else. Press **Check now** afterwards.

The check asks GitHub once a day for a version number and installs nothing.
Settings → **Check for new versions** turns the daily ask off, and the banner
above every page with it; this card stays, saying what was last found, and
**Check now** asks again whenever you press it. A build that is not exactly a
release — a working tree, `v0.1.0-3-gabc1234-dirty` — is never offered an
upgrade.

The install command still works. Upgrading by hand is the same thing without
the button.

## Uninstalling

**Settings → Remove Gniza from this server** is at the bottom of the
settings page. It asks first, on a page that says the two things worth
knowing: this removes the interface you are standing in, and it does not
touch a single backup. What runs is the script below, started as a transient
systemd unit a few seconds later — otherwise it would stop the service
halfway through answering you.

In a root shell it is:

```bash
sh /usr/local/share/gniza/uninstall.sh
```

The installer leaves that copy on the server, so this never means finding the
package again. It stops the service and unregisters the WHM plugin, the cPanel
hooks and the account tile, and clears restic's cache.

What it takes off the server it moves rather than deletes. The binaries, the
hook, the unit file, the plugin files and the spooled account events all go
into one dated directory under `/var/lib/gniza/removed`, and the script prints
where. Moving them back undoes the uninstall; deleting them is left for you to
do on purpose. restic's cache is the exception, and is deleted: it is rebuilt
from the repository on the next backup and it is the one thing here that
reaches gigabytes, so moving it aside would free no disk on a server you are
uninstalling to make room on.

It keeps `/etc/gniza/master.key` and `/var/lib/gniza/state.db`, so a
reinstall comes back with the same destinations, schedules and history.
Deleting the key deletes the only way to read the backups in those
destinations; that one is left for you to do on purpose, and only once you can
read them another way.

Backups already written to a destination are not touched either way.

## Where things live

| Path | What |
|---|---|
| `/usr/local/bin/gniza-agent` | the service |
| `/usr/local/cpanel/whostmgr/docroot/cgi/gniza.cgi` | the WHM plugin |
| `/etc/gniza/master.key` | the key that encrypts stored destination credentials |
| `/var/lib/gniza/state.db` | jobs, schedules, destinations, account identities |
| `/var/lib/gniza/staging` | where an account is rebuilt before upload |
| `/var/run/gniza/admin/ui.sock` | the interface, root only |
| `/var/run/gniza/account/user.sock` | the account-facing socket |

The interface listens on a unix socket, not a port. cPanel servers are
multi-tenant and this interface can read every stored credential, so it is not
reachable over the network at all: the WHM plugin proxies to it.

```bash
systemctl status gniza
journalctl -u gniza -f
```

The same log is readable without a shell under
[Logs → Service log](logs.md#the-service-log), with a level, a span and an
account to filter by.
