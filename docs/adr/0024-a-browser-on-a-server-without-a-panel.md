# 0024 — A browser on a server without a panel

Status: accepted, 2026-09-12. Amends [ADR 0022](0022-a-server-without-a-panel.md).

## Context

ADR 0022 gave a server with no panel a terminal interface and said why
not a port: "a loopback port holding every destination credential is
still the wrong shape". The operator wants the pages in a browser there
all the same, and asked that the installer offer a choice between this
machine only and every address.

What made the socket safe was its mode. `/var/run/gniza/admin/ui.sock`
is root's and `0600`, so a connection to it *is* the authorization: the
peer could have done anything the service does without asking. A TCP
port has no mode. Whatever listens on one has to decide for itself who
is on the other end, and a service that runs as root, holds every
destination credential and can restore over any account had better
decide well.

The pages also assumed a panel around them. What `layout.html` writes is
a fragment — a script, a style, one `div` — and WHM's `defheader` or
DirectAdmin's Evolution skin supplies `<html>` and `<head>`. On a port
nothing does.

## Decision

**The door is a password, a session and a lockout; off the loopback
interface it is also TLS.** `gniza-agent -web-listen host:port` serves
the operator's pages on that address, behind:

- One password, kept as a bcrypt hash in `/etc/gniza/web/password`,
  written by `gniza-agent -web-set-password` and read on every sign-in,
  so a change applies to the next one without a restart. Ten characters
  at least; seventy-two bytes at most, which is where bcrypt stops
  reading.
- A session cookie, `HttpOnly`, `SameSite=Strict`, `Secure` when the
  listener is TLS, ending twelve hours after it began or an hour after it
  was last used. Sessions live in memory: a restart signs everybody out,
  which is right for a service that restarts to upgrade itself.
- A lockout per address: from the fifth wrong password the next attempt
  waits thirty seconds, and the wait doubles with each failure after it
  up to a quarter of an hour. bcrypt's own cost makes the first four slow
  enough on their own.
- TLS whenever the bind host is not loopback. A self-signed pair is made
  on first start into `/etc/gniza/web/cert.pem` and `key.pem`, naming
  the hostname and the machine's addresses, and its SHA-256 fingerprint
  is logged so the operator can compare it with what the browser shows.
  A pair the operator puts there is used instead. On loopback the
  listener is plain HTTP, because the traffic never leaves the machine
  and an ssh tunnel or a reverse proxy in front of it brings its own.

**The port serves the same handlers as the socket, with a document
around them.** Nothing is written twice. A request through the door is
marked, and the layout opens and closes `<html>`, `<head>` and `<body>`
around the fragment it always wrote; the sign-in page is a small
document of its own. The rail gains a sign-out when the page is a
document, and nothing when it is a fragment. Fonts are served to the
sign-in page too, since it is set in them. `X-Frame-Options: DENY` and
`frame-ancestors 'none'` go on every response; HSTS on TLS.

**The installer asks, once.** On a terminal it offers this machine only
(`127.0.0.1:8443`, reached over `ssh -L`), every address
(`0.0.0.0:8443`), or none, and asks for the password without echo. The
answer is written to `/etc/gniza/plain.env` as `GNIZA_WEB_LISTEN`, which
the agent reads as the flag's default, so the unit is unchanged and an
upgrade keeps the answer. `GNIZA_WEB_LISTEN` and `GNIZA_WEB_PASSWORD`
set before the script answer for an unattended install. With no terminal
and no password given -- which is also how the service's own updater runs
the script -- the address is written and no password is: the door stays
shut until `-web-set-password` has been run, and the log and the
installer's closing block both say so. A password made up by the script
would have to be printed, and what the script prints is kept by the
updater and by journald.

**A door that cannot be opened costs the backups nothing.** No password
set, the address in use, half a TLS pair: the service logs why and runs
on. The socket and the terminal still work, and a service that exited
over its own front door would restart into the same refusal forever.

## Consequences

- A root service now answers on a network port when asked to. The
  password is its only door there. The installer says so where the
  operator chooses, recommends the loopback choice by making it the
  default, and the guide says the same. An operator who cannot keep a
  password safe should choose loopback and an ssh key.
- The CSRF token is still one per process, as it is on the socket, and
  it appears in the sign-in form. That is not a weakening: a page in the
  browser that could read it is already signed in, and `SameSite=Strict`
  keeps another site's form from carrying the cookie.
- The self-signed certificate is trust on first use. The fingerprint is
  in the service log and can be read from the file with `openssl`; the
  guide shows both. An operator who wants better puts a real pair in
  place of the generated one.
- The door is available on any standalone server, panel or not, since
  the flag is the agent's; the panel packages neither ask about it nor
  set it, and their pages stay behind the panel's own sign-in.
- Restore and settings, which the terminal does not yet ask for
  (`cprest-91g.2`, `cprest-91g.3`), are reachable from a browser on a
  plain server now, as they always were on a panel one.
