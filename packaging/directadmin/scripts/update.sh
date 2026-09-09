#!/bin/sh
# What DirectAdmin's plugin manager runs to upgrade Gniza in place.
#
# Installing again is the upgrade: the installer replaces the agent, the
# hooks and the page, leaves the configuration and the state database
# alone, and restarts the service. See install.sh.
set -u

PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
export PATH

HERE=$(cd "$(dirname "$0")" && pwd)

printf '<h2>Updating Gniza</h2>\n<pre>\n'
if sh "$HERE/../install.sh" 2>&1; then
	status=0
else
	status=$?
fi
printf '</pre>\n'

if [ "$status" -eq 0 ]; then
	printf '<p><a href="/CMD_PLUGINS_ADMIN/gniza/index.html">Open Gniza</a></p>\n'
else
	printf '<p><b>Gniza was not updated.</b> The transcript above says why.</p>\n'
fi
exit "$status"
