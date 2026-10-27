#!/bin/sh
# Builds the container image from the release archives in packaging/dist (archive.sh makes them):
#   VERSION=0.1.0 GOOS=linux GOARCH=amd64 go/opampsupervisor/packaging/archive.sh
#   VERSION=0.1.0 go/opampsupervisor/packaging/container/build.sh
# ARCHES is the architectures, amd64 and arm64 by default, each from its archive. IMAGE is the
# image's name (default pydantic-opamp-supervisor:$VERSION). One architecture is loaded into the
# local Docker; more need PUSH=1, which pushes IMAGE instead (docker buildx, with QEMU for an
# architecture this machine doesn't run).
# BINARIES, a directory holding opampsupervisor and otelcol for the one architecture in ARCHES,
# takes them from there instead of an archive (CI builds the image from packaging/build.sh's).
set -eu
# A relative BINARIES is the caller's: made absolute before moving to this script's directory.
case ${BINARIES:-} in "" | /*) ;; *) BINARIES=$PWD/$BINARIES ;; esac
cd "$(dirname "$0")"
: "${VERSION:?set VERSION, e.g. 0.1.0}"
ARCHES=${ARCHES:-amd64 arm64}
IMAGE=${IMAGE:-pydantic-opamp-supervisor:$VERSION}

ctx=../dist/container
rm -rf "$ctx"
mkdir -p "$ctx/state" "$ctx/licenses"
platforms=
for arch in $ARCHES; do
	platforms="${platforms:+$platforms,}linux/$arch"
	mkdir -p "$ctx/linux_$arch"
	if [ -n "${BINARIES:-}" ]; then
		cp "$BINARIES/opampsupervisor" "$BINARIES/otelcol" "$ctx/linux_$arch/"
		cp ../../../../LICENSE "$ctx/licenses/LICENSE"
		cp ../../LICENSE "$ctx/licenses/LICENSE.Apache-2.0"
		mkdir -p "$ctx/licenses/licenses/core"
		cp ../../../../licenses/core/LICENSE.txt ../../../../licenses/core/THIRD_PARTY_NOTICES.txt "$ctx/licenses/licenses/core/"
		continue
	fi
	name="pydantic-opamp-supervisor_${VERSION}_linux_${arch}"
	[ -f "../dist/$name.tar.gz" ] || { echo "no ../dist/$name.tar.gz: build it with archive.sh first" >&2; exit 1; }
	tar -C "$ctx" -xzf "../dist/$name.tar.gz"
	mv "$ctx/$name/opampsupervisor" "$ctx/$name/otelcol" "$ctx/linux_$arch/"
	# The licenses are the same in every archive.
	cp -R "$ctx/$name/LICENSE" "$ctx/$name/LICENSE.Apache-2.0" "$ctx/$name/licenses" "$ctx/licenses/"
	rm -rf "${ctx:?}/$name"
done
cp Dockerfile supervisor.yaml supervisor-restricted.yaml "$ctx/"

if [ "${PUSH:-}" = 1 ]; then
	output=--push
elif [ "$platforms" = "${platforms%,*}" ]; then
	output=--load
else
	echo "more than one architecture ($platforms): set PUSH=1 to push them as IMAGE" >&2
	exit 1
fi
docker buildx build --platform "$platforms" --build-arg "VERSION=$VERSION" -t "$IMAGE" $output "$ctx"
