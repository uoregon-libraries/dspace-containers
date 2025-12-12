#!/bin/bash

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

# When user requests bash or sh, don't run the init scripts
case "$@" in
  bash | sh )
  ;;

  *)
  /usr/local/scripts/migrate-db.sh
  run_init
esac

echo Executing "$@"
exec "$@"
