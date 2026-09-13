# 0026 — An SFTP destination logs in with a password or a key

Status: accepted, 2026-09-13.

## Context

A destination on another Linux server logged in with a key Gniza made,
and with nothing else. The form had a password field marked optional:
given the remote password once, Gniza installed the key with it and
forgot it. That is the right shape for a server somebody else runs, and
it is what the guide describes. It was not what the first operator to
add one on a server without a panel expected, and the form set a trap
for them: adding a destination comes back once, for the host key to be
agreed to, and the form that comes back has no password in it -- no
secret is carried through a page. They typed the password, agreed to
the fingerprint, and the destination was saved as one whose key they
would install by hand. Nothing said so. The card then read "does not
accept Gniza's key", and the operator, who had given the password,
reasonably concluded the password was not used. It was not.

The operator asked for the choice to be explicit: a key or a password.

## Decision

The SFTP form asks how the backups log in, before it asks for anything
secret:

- **The user's password.** It is sealed in the vault like an S3 key or
  a REST password, under the destination's credentials, and every
  backup logs in with it. Adding the destination proves the password
  opens a login and makes the directory before anything is saved; a
  wrong password is refused there, in words, and the test on the card
  says "does not accept the password" rather than talking about a key.
- **A key Gniza makes.** As before: the public half goes into the
  user's `authorized_keys`, by the operator or by Gniza given the
  password once. The password is used for that and forgotten.

The form that comes back for the host key remembers that a password was
typed (`had_password`), says so beside the empty field, and a second
post without one is sent back with "Type the password again" rather
than taken as a login without it. The terminal keeps the form in memory
across that step and never lost the password; it gets the same choice.
An existing destination can be edited to a password; one added with a
password has no key and is removed and added again to get one.

### How the password reaches ssh

restic drives the system `ssh`, and ssh asks for a password on its
terminal. It runs the program named by `SSH_ASKPASS` instead when it has
no terminal to ask on, which is how the agent runs as a service, and an
OpenSSH of 8.4 or later runs it regardless with
`SSH_ASKPASS_REQUIRE=force`. `DISPLAY` has to be set for an older ssh to
consider the program at all. So a destination that logs in with a
password puts four variables in restic's environment, where the
repository password already travels and where nothing but root can
read them: `SSH_ASKPASS` pointing at a three-line program Gniza writes
under its configuration directory at mode 0700, `SSH_ASKPASS_REQUIRE`,
`DISPLAY`, and `GNIZA_SSH_PASSWORD`, which the program prints. ssh is
told `PubkeyAuthentication=no`, `PreferredAuthentications=password,
keyboard-interactive` and `NumberOfPasswordPrompts=1`, and not
`BatchMode`, which forbids a password altogether. The key login's
arguments are unchanged: a destination stored before this decision
builds exactly as it did. The test on the card runs ssh the same way,
in its own session so that an agent started from a terminal does not
sit at a prompt nobody sees.

`sshpass` is not used: it is not on the fleet, and ssh's own mechanism
does the same thing without a dependency. The fleet's OpenSSH 8.0 has
no `SSH_ASKPASS_REQUIRE`, but under systemd there is no controlling
terminal, so it takes the askpass path; the Debian test server's
OpenSSH 10 was seen to, through restic, with a wrong password refused
in words.

## Consequences

- The password is a stored secret. Someone who takes the master key and
  the state database has the remote account's password, where before
  they had a key that could be revoked from the far side alone. The
  recovery card says the destination logs in with a password and does
  not print it; the operator who has it, has it.
- The askpass program is one file for every password destination. The
  test on the card refuses one that is readable or writable by anyone
  but root, since whoever can replace it is handed the password.
- Configuration gains `auth=password` and `askpass_file`; `Build`
  reads both and needs no `identity_file` when they are set.
- A password login's failure looks like a key login's to ssh
  ("Permission denied"); the destination knows which it is and says so.
