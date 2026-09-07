# Gniza documentation

Hosting-panel backup orchestration on top of [restic](https://restic.net/).

cPanel/WHM is the panel Gniza runs on today, and everything below describes
it. DirectAdmin is partly implemented and refuses what it cannot yet do —
see [DirectAdmin](guide/directadmin.md) for exactly how far it goes; Plesk
is planned. Where a page says cPanel it means the panel this server runs,
and the parts that are genuinely cPanel's own — `pkgacct`, `restorepkg`,
Feature Manager — say so by name.

## For operators

Start here if you run the WHM plugin.

| Page | What it covers |
|---|---|
| [Getting started](guide/getting-started.md) | Install the plugin, add a destination, take the first backup |
| [Destinations](guide/destinations.md) | Where backups go: SFTP, S3, restic REST, local disk — and the recovery key |
| [Schedules](guide/schedules.md) | When backups run, what they contain, how long they are kept |
| [Accounts](guide/accounts.md) | Coverage per account, backing one up now, termination and suspension safety |
| [Restoring](guide/restoring.md) | Whole accounts, deleted accounts, single files and items, disaster recovery |
| [Logs](guide/logs.md) | Backups, system backups, restores, cPanel events, and the service's own log |
| [Settings](guide/settings.md) | Concurrency, staging, log level, alerting channels, payload contents |
| [For account holders](guide/for-account-holders.md) | The cPanel tile your customers see |
| [Troubleshooting](guide/troubleshooting.md) | When something fails, where to look, and how to report it |
| [DirectAdmin](guide/directadmin.md) | What works there, what is refused, and why |

## For the curious and the contributing

- [DESIGN.md](DESIGN.md) — the whole system, and why each part is shaped that way
- [Architecture decisions](adr/) — one file per decision, with what it costs

## The one thing to do today

Copy `/etc/gniza/master.key` somewhere that is not this server. Destination
credentials are encrypted with it. Without that file, a rebuilt machine cannot
read its own stored credentials — and cannot reach the backups it wrote.
