#!/bin/bash
set -e

DSPACE=/usr/local/dspace

run_init() {
  for f in /docker-entrypoint/*; do
    case "$f" in

      *.sh)
      echo "$0: running $f"
      . "$f"
      ;;

      *)
      echo "$0: ignoring $f"
      ;;

    esac
    echo
  done
}

# DSpace subcommands (e.g., create-administrator) are routed through the
# launcher, while "real" commands (containing a slash or any executable on
# PATH) are run directly.
#
# Note that `type -P` ignores shell builtins. This is rarely going to matter,
# if ever, but be aware that you can't run builtins. This is necessary because
# "read" is a builtin but also a DSpace subcommand....
if [ $# -gt 0 ] && [[ "$1" != */* ]] && ! type -P "$1" >/dev/null; then
  set -- "$DSPACE/bin/dspace" "$@"
fi

# `catalina.sh` gets special initialization treatment since it's the
# long-running web server.
if [ "$1" = "catalina.sh" ]; then
  /usr/local/scripts/migrate-db.sh
  run_init
fi

echo "Executing $*"
exec "$@"
