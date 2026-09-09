#!/bin/sh
# What DirectAdmin's plugin manager runs when Gniza is deleted.
#
# The uninstaller beside it stops the service and removes what was
# installed. What it does not remove is the backups, the destinations or
# the state database: deleting a plugin must not delete the only copy of
# a customer's site. See uninstall.sh for what is left behind and where.
set -u

PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
export PATH

HERE=$(cd "$(dirname "$0")" && pwd)

printf '<h2>Removing Gniza</h2>\n<pre>\n'
if sh "$HERE/../uninstall.sh" 2>&1; then
	status=0
else
	status=$?
fi
printf '</pre>\n'
exit "$status"
