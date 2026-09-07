#!/bin/sh
# DirectAdmin user_destroy_pre: tell Gniza that this account remove-pre.
set -eu
GNIZA_EVENT=remove-pre
export GNIZA_EVENT
. "$(dirname "$0")/gniza_event.sh"
