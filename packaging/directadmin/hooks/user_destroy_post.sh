#!/bin/sh
# DirectAdmin user_destroy_post: tell Gniza that this account remove.
set -eu
GNIZA_EVENT=remove
export GNIZA_EVENT
. "$(dirname "$0")/gniza_event.sh"
