#!/bin/bash
# Generates (or regenerates) the Handle server's config and the sitebndl.zip
# that handle.net needs in order to point our prefix at this server. This is
# DSpace's bin/make-handle-config, except:
#
# - The public IP is an argument rather than a DNS lookup of the DSpace
#   hostname, since the site sits behind a proxy and that lookup gives the
#   proxy's address.
# - The server binds to 0.0.0.0: the public IP doesn't exist inside the
#   container.
# - The setup tool's output isn't hidden, so errors are visible.
#
# Existing keys in the handle dir are kept, so this is also how to move an
# existing handle server to a new IP. See docs/handle-server.md.
set -eu

DSPACE=/usr/local/dspace

# This must match the "handle-server" volume's mount point in compose.yml
HANDLEDIR=$DSPACE/handle-server

usage() {
  echo "usage: $0 [--no-udp] <public IPv4 address>" >&2
  exit 1
}

udp=y
if [ "${1:-}" = "--no-udp" ]; then
  udp=n
  shift
fi
[ $# -eq 1 ] || usage
ip=$1
[[ "$ip" =~ ^[0-9]{1,3}(\.[0-9]{1,3}){3}$ ]] || usage

prefix=$($DSPACE/bin/dspace dsprop --property handle.prefix)
dsname=$($DSPACE/bin/dspace dsprop --property dspace.name)
email=$($DSPACE/bin/dspace dsprop --property mail.admin)
if [ -z "$prefix" ] || [ "$prefix" = 123456789 ]; then
  echo "$0: handle.prefix is '$prefix'; set HANDLE_PREFIX in .env" >&2
  exit 1
fi

echo "Setting up handle server for prefix $prefix at $ip (UDP: $udp)"
echo

# Answers to SimpleSetup's prompts, in order. When keys already exist, the two
# "encrypt?" prompts become "create new keys?" -- "n" is right either way. The
# final "n" declines copying admin.war, a prompt that's only asked on reruns.
disable_udp=n
[ $udp = y ] || disable_udp=y
{
  echo ""                       # Primary server? (default y)
  echo ""                       # Dual-stack IPv4/IPv6? (default n)
  echo "$ip"                    # Public IP address
  echo "0.0.0.0"                # Bind address
  echo ""                       # Port (default 2641)
  echo ""                       # HTTP port (default 8000)
  echo "n"                      # Log all accesses?
  echo ""                       # Site version/serial (default 1)
  echo "$dsname Handle Server"  # Server description
  echo "$dsname"                # Organization name
  echo ""                       # Contact name
  echo ""                       # Contact phone
  echo "$email"                 # Contact email
  echo "$disable_udp"           # Disable UDP?
  echo "n"                      # Encrypt server key? / New server keys?
  echo "n"                      # Encrypt admin key? / New admin keys?
  echo "n"                      # Copy admin.war?
} | java -classpath "$($DSPACE/bin/dspace classpath)" \
  net.handle.server.SimpleSetup "$HANDLEDIR"

# SimpleSetup always writes a fresh config.dct (moving any old one aside), so
# this edit is never applied twice: fill in our prefix and wire in DSpace's
# HandlePlugin so lookups come from the DSpace database.
tmp=$(mktemp)
sed "s/YOUR_PREFIX/$prefix/" "$HANDLEDIR/config.dct" | awk '
  { print }
  /"server_config" = {/ {
    print "\"storage_type\" = \"CUSTOM\""
    print "\"storage_class\" = \"org.dspace.handle.HandlePlugin\""
    print "\"enable_txn_queue\" = \"no\""
    print ""
  }' > "$tmp"
cat "$tmp" > "$HANDLEDIR/config.dct"
rm -f "$tmp"

echo
echo "Done. Send $HANDLEDIR/sitebndl.zip to handle.net."
