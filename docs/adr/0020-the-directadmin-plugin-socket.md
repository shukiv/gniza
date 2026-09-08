# 0020 — DirectAdmin's plugin is not root

Status: accepted
Date: 2026-09-08

## Context

On cPanel, Gniza's WHM plugin runs as root. The socket it connects to,
`/var/run/gniza/admin/ui.sock`, is root-owned and mode `0600`, and that
mode is the whole of its authorization: a connection to it means a
process that is already root, which means it could have done anything the
daemon does without asking the daemon. The socket is a convenience, not a
boundary.

DirectAdmin does not run a plugin as root, and will not be made to. Its
`plugin.conf` takes `admin_run_as`, `reseller_run_as` and `user_run_as`,
and 1.709 refuses any of them that resolves to uid 0:

    User %s is not a valid apache user.  Must not be uid=0 and must exist.

A probe installed on a live DirectAdmin 1.709 host settled the two
questions that decided this. With `admin_run_as=nobody` the administrator
page ran as `uid=65534 user=nobody` — DirectAdmin changed the user, as
documented — and it still received `SESSION_ID` and `SESSION_KEY` in its
environment. The account-level page, with no `user_run_as`, ran as the
logged-in account itself (`uid=1000 user=admin`) and received the same
two values.

So there is a way to run Gniza's administrator page without a setuid-root
binary and without a root-owned plugin: a service account of Gniza's own,
named in `admin_run_as`. What that costs is the meaning of the uid on the
other end of the socket.

## Decision

The existing administrative socket is not widened. It stays root-owned
and mode `0600`, for the command line and for standalone mode, where a
connection still means an operator who is already root.

DirectAdmin gets a socket of its own, owned by the service account named
in `admin_run_as`, and the daemon treats a connection to it as
anonymous. `SO_PEERCRED` on that socket says only "DirectAdmin's plugin
runner": one service account, acting for whoever DirectAdmin let through
to the page, and possibly for several people at once. It is not a person
and it is not a role.

Authorization is therefore the session, and only the session. Every
request on that socket carries the `SESSION_ID` and `SESSION_KEY` the
plugin was given, and the daemon puts them to DirectAdmin's own API
before the handler runs:

1. `GET /CMD_API_SHOW_USER_CONFIG` on `https://127.0.0.1:2222`, with the
   two values replayed as `Cookie: session=<id>; key=<key>` exactly as
   DirectAdmin's own plugins replay them, and nothing naming an account
   in the request — a request that names one asks about that account
   rather than about the session, which is the opposite of what this is
   for.
2. DirectAdmin answers with the session's own `username` and `usertype`.
   Anything else — a redirect to the login page, an `error=1`, an HTML
   page, two names, a type DirectAdmin does not have — is a refusal.
3. The handler runs only when `usertype` is `admin`. A reseller session
   is refused by the daemon, not by the page.

`CMD_API_GET_SESSION` would answer the same question and was tried first.
It requires POST, and its answer carries the session's password. The
endpoint that is asked instead answers a GET and names the account
without one.

The account-facing page is a different case and keeps its own socket. It
runs as the account, so `SO_PEERCRED` still names a person, and the
session is checked on top of the uid rather than instead of it: the
verified `username` must be the account the peer uid resolves to.

## Failure behavior

- No session values, unusable ones, or a session DirectAdmin does not
  recognize: refused before the handler runs.
- A session DirectAdmin recognizes whose `usertype` is not `admin`:
  refused. There is no fallback to the peer uid, which is the service
  account and proves nothing.
- A redirect is never followed. Following one would replay the session to
  wherever it points.
- DirectAdmin unreachable or silent: refused, on a timeout rather than a
  wait without end.
- The session values and DirectAdmin's answer are never quoted in an
  error or written to the log. The answer carries the session password,
  which is dropped before anything else reads the fields.

## Consequences

No setuid-root binary is installed on a DirectAdmin host, and no part of
Gniza's plugin runs as root. What replaces the root check is a call to
the panel on every request, which is the same trade ADR 0013 made on
cPanel's account socket for the same reason: a unix uid is not a login.

It also means the daemon depends on DirectAdmin answering. If
DirectAdmin's API is down, the administrator page stops working —
correctly, because nothing can then establish who is asking. The command
line over the root-only socket is unaffected, which is what an operator
recovering a host has.

The service account is created at install time and is not removed at
uninstall. Removing an account that may own files elsewhere is a
decision for the host's administrator, in the same way the master key is
left behind.
