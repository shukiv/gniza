#!/bin/sh
# DirectAdmin user_activate_post: tell Gniza that this account unsuspend.
set -eu
GNIZA_EVENT=unsuspend
export GNIZA_EVENT
. "$(dirname "$0")/gniza_event.sh"
