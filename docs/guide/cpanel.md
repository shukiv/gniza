# cPanel/WHM

cPanel/WHM is the panel Gniza was built on and the one everything else is
measured against: the cPanel provider runs in production on cPanel 136. The
other guides are written for it, so this page is the short version of what is
cPanel's own, with a link to where each part is explained in full.

## Where it is

As root, the install command in [Getting started](getting-started.md)
installs the WHM plugin on a server with `/usr/local/cpanel`. WHM then shows
**Gniza Backups** in the sidebar's **Plugins** group. Not under **Manage
Plugins**: that page lists cPanel's own RPM addons, and an AppConfig plugin's
registration is under **Development → Apps Managed by AppConfig** instead.

The plugin refuses every WHM user that is not root.

## What a backup is

A split backup, the default, takes an account apart so that restic sees real
files rather than one compressed archive:

| Part | Where it comes from |
|---|---|
| The account's records: cPanel configuration, DNS zones, mail configuration, cron, SSL | `pkgacct`, with the home directory left out |
| The home directory | read where it lies, from `/home/<user>` |
| Each database | one uncompressed SQL dump of its own |

So an account that did not change costs almost nothing to back up again. A
single-archive schedule keeps one `pkgacct` archive per run instead, with
pkgacct's own compression turned off, and costs far more to repeat. See
[Schedules](schedules.md#what-goes-in) and DESIGN §4.

`/etc/cpbackup-exclude.conf` and each account's own `cpbackup-exclude.conf`
are obeyed: a path written there does not leave the server. The archives the
one-click installers write on every update are left out of every home
directory. See [What a backup contains](settings.md#what-a-backup-contains).

## Restoring

A restore puts the parts back together as a `cpmove` archive. By default it is
rebuilt and left on the server for you to inspect; handing it to cPanel's
`restorepkg`, or overwriting the live account, is a separate tick behind a
confirmation page. An account cPanel no longer has is created again. Single
items come back on their own: files, databases and their users, mail, cron,
domains, DNS, certificates and FTP. See [Restoring](restoring.md).

## Account hooks

The installer registers cPanel hooks for account create, modify, suspend,
unsuspend and remove. They record who an account was, so a renamed account
keeps its history and a username handed to a new customer does not inherit the
previous customer's backups ([Accounts](accounts.md#renames-and-recycled-names)).
Two optional switches in [Settings](settings.md) act on them:

- **Termination safety** blocks removing an account while it lacks a recent
  complete copy at every destination a full-account schedule promises. The
  check runs inside cPanel's hook and never starts a long backup while WHM
  waits; if the service is down, removal is allowed. See
  [Termination safety](accounts.md#termination-safety).
- **Suspension preservation** queues the smallest set of full-account
  schedules that reaches every destination they promise, the moment cPanel
  suspends an account. See
  [Suspension preservation](accounts.md#suspension-preservation).

## Your customers

Each account holder gets a tile in cPanel with their own restore points:
an overview, restore and download, and the record of their own backups. The
**gniza** feature in cPanel's Feature Manager turns it on or off. What they can
put back into the live account, what they can only download, and who is let in
are in [For account holders](for-account-holders.md).

## Removing it

`uninstall.sh` deletes nothing: it moves the plugin, the binaries and the unit
file aside, and leaves the repositories, the state and `/etc/gniza` alone. See
[Uninstalling](getting-started.md#uninstalling).
