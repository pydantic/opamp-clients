#!/bin/sh
# The rpm's last step, after the old package's scripts on an upgrade: start the collector this
# host ran again (pydantic-opamp-after-upgrade). A first install has nothing to start.
/usr/lib/pydantic-opamp/pydantic-opamp-after-upgrade || true
