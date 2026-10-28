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
chgrp pydantic-opamp /etc/pydantic-opamp
# Once per host: lets the Collector read the system logs host configs collect. adm owns
# /var/log/syslog and auth.log on Debian and Ubuntu, systemd-journal the journal. The marker keeps
# an operator's later removal from a group (gpasswd -d pydantic-opamp adm) across upgrades.
if [ ! -e /etc/pydantic-opamp/log-groups ]; then
	for group in adm systemd-journal; do
		if getent group "$group" >/dev/null; then
			usermod -a -G "$group" pydantic-opamp
		fi
	done
	: >/etc/pydantic-opamp/log-groups
fi
if [ -d /run/systemd/system ]; then
	systemctl daemon-reload
fi
