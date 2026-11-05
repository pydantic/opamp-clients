#!/bin/sh
# Stops the services before its files go. The config, the credentials and the storage directory
# (the issued credentials) stay, for a reinstall.
set -e
# An upgrade, not a removal (deb: upgrade or failed-upgrade; rpm: packages left, 1 or more): the
# services keep running on the old files until the new package restarts them.
case ${1:-} in
upgrade | failed-upgrade | [1-9]*) exit 0 ;;
esac
if [ -d /run/systemd/system ]; then
	systemctl disable --now pydantic-opamp-supervisor.service pydantic-otelcol.service || true
fi
