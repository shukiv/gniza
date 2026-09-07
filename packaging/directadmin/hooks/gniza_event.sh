#!/bin/sh
# Tell the Gniza service that an account changed hands.
#
# DirectAdmin hands a hook the contents of the account's user.conf in the
# environment; the service reads JSON. This turns the one field that
# matters into the shape the service already understands, and nothing
# else: what a hook is for is the boundary between one customer and the
# next on the same username, not a copy of the account's settings.
#
# It is sourced by each of the event scripts beside it, which supply
# GNIZA_EVENT.
set -eu

PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
export PATH

GNIZA_HOOK=${GNIZA_HOOK:-/usr/local/bin/gniza-hook}
[ -x "$GNIZA_HOOK" ] || exit 0

# username is set by every user hook DirectAdmin documents. Without it
# there is no account to report, and inventing one would file a boundary
# against the wrong customer.
[ -n "${username:-}" ] || exit 0

# Only the name, and only as JSON. Quotes and backslashes cannot appear
# in a DirectAdmin username, but the value arrives from outside this
# script, so it is checked rather than trusted.
case "$username" in
  *[!A-Za-z0-9_-]*) exit 0 ;;
esac

printf '{"username":"%s"}' "$username" | "$GNIZA_HOOK" --panel-hook="$GNIZA_EVENT"
