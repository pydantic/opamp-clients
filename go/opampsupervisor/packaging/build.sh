#!/bin/sh
# Builds the Linux packages into packaging/dist: the bundled collector (ocb, collector.yaml), the
# Supervisor, then a deb and an rpm (nfpm.yaml). Run from anywhere:
#   VERSION=0.1.0 ARCH=amd64 go/opampsupervisor/packaging/build.sh
# ARCH is a Go architecture (amd64 or arm64); both binaries are cross-built, without cgo.
# With SIGNING_KEY_FILE (an armored OpenPGP secret key, unlocked by SIGNING_KEY_PASSPHRASE if it
# has one) the rpm is signed; repo.sh then puts the packages in signed apt and yum repositories.
set -eu
# A relative key path is the caller's: made absolute before moving to this script's directory.
case ${SIGNING_KEY_FILE:-} in "" | /*) ;; *) SIGNING_KEY_FILE=$PWD/$SIGNING_KEY_FILE ;; esac
export SIGNING_KEY_FILE="${SIGNING_KEY_FILE:-}"
cd "$(dirname "$0")"
: "${VERSION:?set VERSION, e.g. 0.1.0}"
ARCH=${ARCH:-amd64}
export VERSION ARCH GOOS=linux GOARCH="$ARCH" CGO_ENABLED=0
export NFPM_PASSPHRASE="${SIGNING_KEY_PASSPHRASE:-}"

mkdir -p dist
(cd .. && go run go.opentelemetry.io/collector/cmd/builder@v0.164.0 --config packaging/collector.yaml)
(cd .. && go build -trimpath -o packaging/dist/opampsupervisor .)
for packager in deb rpm; do
	GOOS= GOARCH= go run github.com/goreleaser/nfpm/v2/cmd/nfpm@v2.43.0 package \
		--config nfpm.yaml --packager "$packager" --target dist/
done
ls -l dist/*.deb dist/*.rpm
