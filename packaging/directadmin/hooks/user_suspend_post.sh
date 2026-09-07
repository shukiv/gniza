#!/bin/sh
# DirectAdmin user_suspend_post: tell Gniza that this account suspend.
set -eu
GNIZA_EVENT=suspend
export GNIZA_EVENT
. "$(dirname "$0")/gniza_event.sh"
