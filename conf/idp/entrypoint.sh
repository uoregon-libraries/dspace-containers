#!/bin/sh
#
# Starts go-saml's IdP, then provisions it through its REST API rather than
# its env vars, because:
#
# - IDP_USERS users have no given name or surname, and DSpace refuses to
#   autoregister without both
# - IDP_SERVICE_URL gives up after ~30 seconds, far less than DSpace needs to
#   start, and DSpace can't start first: it only reads the IdP metadata once,
#   at startup
#
# SP metadata is fetched with the same X-Forwarded-* headers Caddy sends,
# because DSpace builds its entity ID and assertion consumer URL from the
# request, and the IdP only accepts requests from, and posts assertions to,
# what's in the metadata. (A plain Host header isn't enough: with
# X-Forwarded-Proto set, Spring drops any port not in X-Forwarded-Host.)
set -eu

: "${IDP_BASE_URL:?IDP_BASE_URL must be set}"
: "${PUBLIC_URL:?PUBLIC_URL must be set}"
: "${DEV_IDP_USERS:=alice,bob}"
sp_metadata_url=http://rest:8080/server/saml2/service-provider-metadata/sso

idp &
pid=$!
trap 'kill "$pid" 2>/dev/null; exit 143' TERM INT

port=${IDP_BASE_URL##*:}
port=${port%%/*}
idp_local=http://localhost:$port

until curl -sf -o /dev/null "$idp_local/metadata"; do
  kill -0 "$pid" 2>/dev/null || { echo "idp-entrypoint: idp exited"; exit 1; }
  sleep 1
done

for name in $(echo "$DEV_IDP_USERS" | tr , ' '); do
  given=$(echo "$name" | cut -c1 | tr a-z A-Z)$(echo "$name" | cut -c2-)
  curl -sf -X PUT "$idp_local/users/$name" --data-binary @- <<EOF
{"password": "$name", "email": "$name@uoregon.edu", "given_name": "$given", "surname": "Dev", "common_name": "$given Dev"}
EOF
  echo "idp-entrypoint: registered user $name (password: $name)"
done

scheme=${PUBLIC_URL%%://*}
hostport=${PUBLIC_URL#*://}
hostport=${hostport%%/*}
echo "idp-entrypoint: waiting for DSpace SP metadata at $sp_metadata_url"
until curl -sf --max-time 10 -H "X-Forwarded-Host: $hostport" -H "X-Forwarded-Proto: $scheme" \
    -o /tmp/sp-metadata.xml "$sp_metadata_url"; do
  sleep 5
done
curl -sf -X PUT "$idp_local/services/dspace" --data-binary @/tmp/sp-metadata.xml
echo "idp-entrypoint: registered DSpace as a service provider"

wait "$pid"
