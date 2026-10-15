#!/bin/sh
# Makes the service's user, and tells systemd about the unit. The service starts once
# pydantic-opamp-enroll has run.
set -e
if ! getent group pydantic-opamp >/dev/null; then
	groupadd --system pydantic-opamp
fi
if ! getent passwd pydantic-opamp >/dev/null; then
	useradd --system --gid pydantic-opamp --home-dir /var/lib/pydantic-opamp --no-create-home \
		--shell /usr/sbin/nologin pydantic-opamp
fi
if [ -d /run/systemd/system ]; then
	systemctl daemon-reload
fi
