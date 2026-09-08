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
   Anything else — a status that is not 200, an `error=1`, an HTML page,
   two names, a type DirectAdmin does not have — is a refusal. On the
   1.709 host a session it does not know is answered `401
   Unauthorized`, not with the login page a browser gets.
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

## What the live panel answered — 2026-09-08

Run against DirectAdmin 1.709 on the production host, read-only, asking
about a session that does not exist. Nothing was installed and no
account was touched.

- `PanelURL` read `https://uscp.linux-hosting.network:2222` out of
  `directadmin.conf`, which is the name on the panel's own certificate.
- A session DirectAdmin does not know was answered `401 Unauthorized`,
  and read as a refusal rather than as an outage — which is the
  distinction that matters, because an outage is what a wrong address or
  an unverifiable certificate would look like.
- The same request to `https://127.0.0.1:2222`, which reaches the same
  panel, failed verification: "cannot validate certificate for 127.0.0.1
  because it doesn't contain any IP SANs". The certificate is being
  checked, which is why the address is read from DirectAdmin's
  configuration rather than assumed to be loopback.

What this does not establish is the accepting path: that an
administrator's live session is answered with their name and
`usertype=admin`. That needs somebody logged into the panel, and the
page installed for them to open.

## What the plugin runner actually does — 2026-09-08

Measured on the 1.709 host with a diagnostic plugin, at both levels,
including under login-as. The plugin was removed afterwards.

**The service account works.** With `admin_run_as=gniza-plugin` and that
account created by `useradd --system`, DirectAdmin ran the page as it:
`uid=979 user=gniza-plugin`, and DirectAdmin's own `RUNNING_AS` agreed.
The account page, with no `user_run_as`, ran as the account —
`uid=1168 user=gzv0908a`, `RUNNING_AS=gzv0908a` — so `SO_PEERCRED` still
names a person there.

**A form does not arrive in the request body.** DirectAdmin reads the
body itself and hands the plugin the whole url-encoded form in a `POST`
environment variable. `CONTENT_LENGTH` is empty and stdin is at end of
file: a two-field form arrived as
`POST=gniza_probe_field=ABCDEF0123456789&csrf=second-field` with zero
bytes readable. A proxy that reads stdin therefore forwards an empty
body to every save — the browser reloads, nothing is written, and it
looks like success. That is what the page now avoids.

It also puts a ceiling on a form. An environment variable is bounded by
the kernel's per-variable limit, 128 KiB on Linux, well under the 1 MiB
Gniza allows a form elsewhere. The one form that could approach it is a
granular restore naming thousands of paths.

**Routing survives.** `QUERY_STRING` carries what a relative link sets:
`?p=link-test&two=2` arrived as `QUERY_STRING=p=link-test&two=2`. That
holds both at the plugin's own address and inside Evolution's wrapper at
`/evo/plugin?src=%2FCMD_PLUGINS%2F…`, where the page runs in a frame and
relative links stay inside it. So ADR 0008's query-string routing works
here unchanged.

The addresses are not symmetrical: the administrator's page is at
`/CMD_PLUGINS_ADMIN/<id>/index.html`, the account's at
`/CMD_PLUGINS/<id>/index.html` — not `CMD_PLUGINS_USER`, which redirects
for every plugin on the host, Gniza's and the others'.

**The account type is what authorizes.** An administrator's session
answered `usertype=admin`; a customer's answered `usertype=user`. The
daemon's check is against the right field.

**Login-as is visible to the plugin.** Impersonating a customer, the
page received `IS_LOGIN_AS=1` and `LOGIN_AS_MASTER=admin` while
`USERNAME` and `RUNNING_AS` were the customer. So the audit gap ADR 0019
left open — a restore recorded against the customer when the host did it
— can be closed: the panel says who is really at the keyboard. Neither
value is trusted on its own; both come from the same environment as the
session, and what makes them worth anything is that the session beside
them verifies.

**Two things to be careful with.** DirectAdmin url-encodes the values in
its answers, and not only where it must: the account `gzv0908a` came
back as `gzv%30%39%30%38a`, every digit escaped, and field names arrive
encoded too. Decoding the answer rather than splitting it is therefore
required, not tidiness. And the environment carries `HTTP_COOKIE` with
the raw session in it, beside `SESSION_ID` and `SESSION_KEY` — a third
copy of the same credential, and one more place not to print.
