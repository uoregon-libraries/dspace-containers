#!/bin/sh

init() {
  # Replace values with env vars in any custom file where we might *ever* need
  # separate setup per environment (and where env vars aren't easy to otherwise
  # inject into the config)
  cat /etc/shibboleth/shibboleth2.base | env-replace > /etc/shibboleth/shibboleth2.xml
  cat /etc/httpd/conf.d/10-sb-proxy.base | env-replace > /etc/httpd/conf.d/10-sb-proxy.conf
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
