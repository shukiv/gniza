# Destinations

A destination is a place backups go, plus the credentials to reach it and the
key that encrypts what lands there. One server can write to several.

## The four kinds

| Kind | Use it for | Needs |
|---|---|---|
| **Another Linux server (SFTP)** | a second box you already own | host, port, SSH user, and a key Gniza can generate for you |
| **Backup server (restic REST)** | a dedicated restic REST server, which can be made append-only | URL, credentials |
| **S3 or S3-compatible** | Backblaze B2, Wasabi, MinIO, AWS | endpoint, bucket, access key, secret |
| **Local disk or mounted NAS** | an attached disk or an already-mounted share | a path |

Each destination also takes a **folder inside the destination** — the
repository path. It defaults to this server's hostname, so several cPanel
servers can share one bucket without colliding.

## Adding one

**Destinations → Add a destination.** Gniza tests the connection, then
initialises the repository, before the destination is saved. A destination that
appears in the list is one that answered.

Then it shows the **recovery key** once. Keep it with your other break-glass
material. A destination whose key is lost is a destination whose backups are
noise.

## Logging in to another Linux server

The form asks how the backups log in, under *Log in with*:

- **The user's password.** Type it once. Gniza checks that it opens a
  login and creates the folder before anything is saved, then keeps the
  password encrypted on this server and logs in with it at every backup.
  A wrong password is refused on the spot, and later by **Test** on the
  card, in words.
- **A key Gniza makes.** One per destination, so revoking one does not
  lock it out of the others. Its public half has to be in the SSH user's
  `~/.ssh/authorized_keys` on the backup server, and there are two ways
  round:
  - **You have the remote password.** Type it into *Password* once. Gniza
    installs the public key on that server, checks that logging in with it
    works, creates the folder, and forgets the password. It is never
    stored.
  - **You do not, or somebody else administers that server.** Press **Make
    the key now** in the form. The public key appears with a **Copy**
    button next to it — before the destination exists, so there is
    something to hand over. Have that line added there, then save the
    destination. The form keeps the key it made; saving does not generate
    a second one.

Either way there is no `ssh-keygen`, no `ssh-copy-id`, and no `known_hosts` to
write: the host key is learnt on the first connection and shown to you to agree
to, which is the one decision only a person can make. That page comes back
without the password in it — no secret is carried between pages — so if you
typed one, type it again there; the page says so, and will not save without
it.

**Edit** on a destination's card can switch it to a password. One added with a
password has no key: to log in with a key instead, remove it and add it
again.

A key made and never used is removed after a week. One a destination is using
is left alone however old it is.

Each destination's public key is also on the list, under **Public key for
…**, with the same Copy button — for the day authentication starts failing and
you need to know what to put back.

## The list

Each row carries the name, where the backups actually are — repository path,
the machine under it, whether it was reachable and when that was last checked —
and how much room is left there.

Free space is measured honestly: `statfs` for a local path, `df -Pk` over SSH
for SFTP. Object stores do not report a size, so they say so rather than
inventing one.

**Edit** sits on the row. Everything else — test the connection, browse what it
holds, remove it — is under the row menu.

## Credentials

Stored encrypted with the key in `/etc/gniza/master.key`. **Back that file up
somewhere other than this server.** Without it the stored credentials cannot be
read, which on a rebuilt machine means re-entering every destination by hand —
and re-entering a recovery key you may not have kept.

## Reading backups another server made

That is [disaster recovery](restoring.md#disaster-recovery), on the Restore
page: attach the old server's destination and its recovery key, and its
accounts become restorable here.
