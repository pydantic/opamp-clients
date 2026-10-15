#!/bin/sh
# Stops the services before its files go. The config, the credentials and the storage directory
# (the issued credentials) stay, for a reinstall.
set -e
if [ -d /run/systemd/system ]; then
	systemctl disable --now pydantic-opamp-supervisor.service pydantic-otelcol.service || true
fi
