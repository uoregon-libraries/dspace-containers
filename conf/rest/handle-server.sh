#!/bin/bash
# Runs the CNRI Handle server in the foreground for the "handle" service, with
# DSpace's HandlePlugin answering lookups from the DSpace database. This is
# DSpace's bin/start-handle-server minus the nohup/backgrounding (which would
# make the container exit) and the file-only logging.
#
# See docs/handle-server.md.
set -eu

DSPACE=/usr/local/dspace

# This must match the "handle-server" volume's mount point in compose.yml
HANDLEDIR=$DSPACE/handle-server

# Refuse to auto-generate anything: fresh keys wouldn't match what handle.net
# has registered for our prefix, and the result would *look* like it works.
if [ ! -f "$HANDLEDIR/config.dct" ]; then
  echo "$0: $HANDLEDIR/config.dct doesn't exist. The handle server needs" >&2
  echo "one-time setup first; see docs/handle-server.md." >&2
  exit 1
fi

# Without the plugin, the server uses its own (empty) storage and answers "not
# found" for every handle we have
if ! grep -q 'org.dspace.handle.HandlePlugin' "$HANDLEDIR/config.dct"; then
  echo "$0: $HANDLEDIR/config.dct doesn't use DSpace's HandlePlugin; re-run" >&2
  echo "handle-setup.sh (see docs/handle-server.md)." >&2
  exit 1
fi

# Remove the lock file in case the last run didn't shut down cleanly
rm -f "$HANDLEDIR/txns/lock"

exec java ${JAVA_OPTS:-} \
  -classpath "$($DSPACE/bin/dspace classpath)" \
  -Ddspace.log.init.disable=true \
  -Dlog4j2.configurationFile=$DSPACE/config/log4j2-container.xml \
  net.handle.server.Main "$HANDLEDIR"
