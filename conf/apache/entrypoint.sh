#!/bin/sh

init() {
  cat /etc/shibboleth/shibboleth2.base.xml | env-replace > /etc/shibboleth/shibboleth2.xml
}

init

case "$@" in
  bash | sh )
    exec "$@"
  ;;

  *)
  echo "Running $@..."
  exec "$@"
esac
