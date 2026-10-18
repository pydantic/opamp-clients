#!/bin/sh
# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
#
# Builds the macOS package into packaging/dist, on a Mac: the bundled collector (ocb,
# ../collector.yaml), the Supervisor, then a .pkg installing them under /opt/pydantic-opamp with
# a LaunchDaemon. Run from anywhere:
#   VERSION=0.1.0 ARCH=arm64 go/opampsupervisor/packaging/macos/build.sh
# ARCH is a Go architecture (arm64 or amd64). Signing is optional: CODESIGN_IDENTITY (a
# "Developer ID Application" identity) signs the binaries, INSTALLER_IDENTITY ("Developer ID
# Installer") the package, and NOTARY_PROFILE (a notarytool keychain profile) notarizes it.
set -eu
cd "$(dirname "$0")/.."
: "${VERSION:?set VERSION, e.g. 0.1.0}"
ARCH=${ARCH:-arm64}
export VERSION ARCH GOOS=darwin GOARCH="$ARCH" CGO_ENABLED=0

stage=dist/macos-$ARCH
rm -rf "$stage"
bin=$stage/root/opt/pydantic-opamp/bin
doc=$stage/root/opt/pydantic-opamp/share/doc
mkdir -p "$bin" "$doc" "$stage/root/Library/LaunchDaemons"
(cd .. && go run go.opentelemetry.io/collector/cmd/builder@v0.163.0 --config packaging/collector.yaml)
cp dist/otelcol/otelcol "$bin/otelcol"
(cd .. && go build -trimpath -o "packaging/$bin/opampsupervisor" .)
install -m 0755 macos/pydantic-opamp-enroll "$bin/"
install -m 0644 macos/dev.pydantic.opamp-supervisor.plist "$stage/root/Library/LaunchDaemons/"
install -m 0644 ../../../LICENSE "$doc/LICENSE"
install -m 0644 ../LICENSE "$doc/LICENSE.Apache-2.0"
mkdir -p "$doc/licenses/core"
install -m 0644 ../../../licenses/core/LICENSE.txt ../../../licenses/core/THIRD_PARTY_NOTICES.txt "$doc/licenses/core/"

if [ -n "${CODESIGN_IDENTITY:-}" ]; then
	codesign --force --options runtime --timestamp --sign "$CODESIGN_IDENTITY" "$bin/opampsupervisor" "$bin/otelcol"
fi
pkg=dist/pydantic-opamp-supervisor-$VERSION-darwin-$ARCH.pkg
set -- --root "$stage/root" --scripts macos/scripts --identifier dev.pydantic.opamp-supervisor \
	--version "$VERSION" --install-location /
if [ -n "${INSTALLER_IDENTITY:-}" ]; then
	set -- "$@" --sign "$INSTALLER_IDENTITY" --timestamp
fi
pkgbuild "$@" "$pkg"
if [ -n "${NOTARY_PROFILE:-}" ]; then
	xcrun notarytool submit "$pkg" --keychain-profile "$NOTARY_PROFILE" --wait
	xcrun stapler staple "$pkg"
fi
ls -l "$pkg"
