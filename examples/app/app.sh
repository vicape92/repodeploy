#!/bin/sh
set -eu
if [ "${FAIL_START:-0}" = "1" ]; then
  exit 1
fi
mkdir -p /www /data
cp /version.txt /www/index.html
if [ "${FAIL_HEALTH:-0}" != "1" ]; then
  printf 'healthy\n' > /www/health
fi
if [ ! -f /data/marker ]; then
  printf 'persistent data\n' > /data/marker
fi
exec httpd -f -p 8080 -h /www
