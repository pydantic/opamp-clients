#!/bin/sh
# Builds one release archive into packaging/dist: the Supervisor and the bundled collector (ocb,
# collector.yaml), with the licenses and a README, for one OS and architecture. Run from anywhere:
#   VERSION=0.1.0 GOOS=linux GOARCH=amd64 go/opampsupervisor/packaging/archive.sh
# GOOS is linux, darwin or windows; GOARCH is amd64 or arm64. Both binaries are cross-built, without
# cgo. Linux and macOS get a .tar.gz, Windows a .zip.
set -eu
cd "$(dirname "$0")"
: "${VERSION:?set VERSION, e.g. 0.1.0}"
: "${GOOS:?set GOOS: linux, darwin or windows}"
: "${GOARCH:?set GOARCH: amd64 or arm64}"
export GOOS GOARCH CGO_ENABLED=0

exe=
[ "$GOOS" = windows ] && exe=.exe
name="pydantic-opamp-supervisor_${VERSION}_${GOOS}_${GOARCH}"
work="dist/$name"
rm -rf "$work" dist/otelcol
mkdir -p "$work"

# ocb itself runs here, so it is built for this machine; it then builds the collector for GOOS/GOARCH.
GOOS= GOARCH= GOBIN="$PWD/dist/bin" go install go.opentelemetry.io/collector/cmd/builder@v0.163.0
(cd .. && packaging/dist/bin/builder --config packaging/collector.yaml)
cp "dist/otelcol/otelcol$exe" "$work/otelcol$exe"
(cd .. && go build -trimpath -ldflags "-s -w" -o "packaging/$work/opampsupervisor$exe" .)
cp ../../../LICENSE "$work/LICENSE"
cp ../LICENSE "$work/LICENSE.Apache-2.0"
# The compiled core both binaries carry keeps its own license, with its third-party notices beside it.
mkdir -p "$work/licenses/core"
cp ../../../licenses/core/LICENSE.txt ../../../licenses/core/THIRD_PARTY_NOTICES.txt "$work/licenses/core/"
cat >"$work/README.txt" <<EOF
pydantic OpAMP Supervisor $VERSION ($GOOS/$GOARCH)

opampsupervisor$exe  the Supervisor: upstream's OpAMP Supervisor on the shared client core
otelcol$exe          an OpenTelemetry Collector built with this repository's opamp extension

Run the Supervisor with a config that names this collector as agent.executable:
  ./opampsupervisor$exe --config supervisor.yaml
Or run the collector on its own, with the opamp extension in its config:
  ./otelcol$exe --config otelcol.yaml

Licenses: LICENSE (MIT) and LICENSE.Apache-2.0 (the files derived from OpenTelemetry's, and the
collector's OpenTelemetry components); the compiled OpAMP core in both binaries is under
licenses/core/LICENSE.txt, with its third-party notices in licenses/core/THIRD_PARTY_NOTICES.txt.

Docs: https://github.com/pydantic/opamp-clients/blob/main/docs/collector.md
EOF

if [ "$GOOS" = windows ]; then
	(cd dist && rm -f "$name.zip" && zip -qr "$name.zip" "$name")
	ls -l "dist/$name.zip"
else
	tar -C dist -czf "dist/$name.tar.gz" "$name"
	ls -l "dist/$name.tar.gz"
fi
rm -rf "$work"
