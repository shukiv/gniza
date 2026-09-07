#!/bin/sh
# DirectAdmin user_create_post: tell Gniza that this account create.
set -eu
GNIZA_EVENT=create
export GNIZA_EVENT
. "$(dirname "$0")/gniza_event.sh"
