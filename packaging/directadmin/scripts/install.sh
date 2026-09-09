#!/bin/sh
# What DirectAdmin's plugin manager runs after unpacking Gniza.
#
# The manager has already put the plugin's files where they belong and
# runs this as root from inside them. Everything this does is done by the
# installer beside it, which is the same installer somebody unpacking the
# release tarball runs by hand: one copy of the work, reached two ways.
#
# Whatever this prints is the page the administrator is shown afterwards,
# inside DirectAdmin's own skin, so the transcript is preformatted.
set -u

PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
export PATH

HERE=$(cd "$(dirname "$0")" && pwd)

printf '<h2>Installing Gniza</h2>\n<pre>\n'
if sh "$HERE/../install.sh" 2>&1; then
	status=0
else
	status=$?
fi
printf '</pre>\n'

if [ "$status" -eq 0 ]; then
	printf '<p><a href="/CMD_PLUGINS_ADMIN/gniza/index.html">Open Gniza</a></p>\n'
else
	printf '<p><b>Gniza was not installed.</b> The transcript above says why.</p>\n'
fi
exit "$status"
