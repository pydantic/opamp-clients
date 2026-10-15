#!/usr/bin/env bash
# Copyright (c) 2026 Bill Easton. All rights reserved.
# SPDX-License-Identifier: LicenseRef-All-Rights-Reserved
#
# The crash scenarios on a real file system that loses what wasn't flushed: builds the image
# of this directory (LazyFS and one toolchain per runtime), checks that a container may mount FUSE,
# and runs inside.sh there on a copy of the repository. Results: conformance/out/lazyfs/.
#
#   conformance/lazyfs/run.sh                   # 25 seeds from today's, every driver of ours
#   LAZYFS_SEEDS=2 LAZYFS_CLIENTS==go-core conformance/lazyfs/run.sh
#   LAZYFS_SEED_BASE=20261002000 LAZYFS_SEEDS=1 ...    # replay: the seed a failure printed
#
# Without FUSE (no /dev/fuse, or no right to mount it) it says why and exits 0, skipped; with
# LAZYFS_REQUIRED=1, 1. The container needs /dev/fuse, CAP_SYS_ADMIN and no AppArmor confinement
# of mount (the probe below).
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
image=${LAZYFS_IMAGE:-opamp-lazyfs:local}
out=$root/conformance/out/lazyfs
fuse=(--device /dev/fuse --cap-add SYS_ADMIN --security-opt apparmor:unconfined)

skip() {
  echo "LazyFS job skipped: $1" >&2
  if [ -n "${GITHUB_ACTIONS:-}" ]; then echo "::warning title=LazyFS skipped::$1"; fi
  if [ "${LAZYFS_REQUIRED:-}" = 1 ]; then exit 1; fi
  exit 0
}

command -v docker >/dev/null || skip "no docker"
if [ -z "${LAZYFS_NO_BUILD:-}" ]; then
  docker build -q -t "$image" "$here" >/dev/null
fi
# A real mount, as the runs will make: FUSE in the container, and the right to mount it.
probe='mkdir -p /p/r /p/m && printf "[faults]\nfifo_path=\"/p/f\"\n[cache]\napply_eviction=false\n[cache.simple]\ncustom_size=\"0.01gb\"\nblocks_per_page=1\n[filesystem]\nlog_all_operations=false\nlogfile=\"/p/log\"\n" > /p/c.toml
  ($LAZYFS /p/m --config-path /p/c.toml -o modules=subdir -o subdir=/p/r -f >/p/out 2>&1 &)
  for i in $(seq 100); do mountpoint -q /p/m && exit 0; sleep 0.05; done; cat /p/out; exit 1'
if ! why=$(docker run --rm "${fuse[@]}" "$image" bash -c "$probe" 2>&1); then
  skip "a container can't mount FUSE here (it needs /dev/fuse, CAP_SYS_ADMIN and no AppArmor confinement of mount): ${why:-no output}"
fi

mkdir -p "$out"
# Today's seeds: a new base every day, printed (a failure names its seed).
base=${LAZYFS_SEED_BASE:-$(( $(date -u +%s) / 86400 * 1000 ))}
echo "LazyFS crash seeds ${base} to $(( base + ${LAZYFS_SEEDS:-25} - 1 ))"
docker run --rm "${fuse[@]}" \
  -v "$root:/src:ro" -v "$out:/results" -v /lazyfs -v "${LAZYFS_CACHE:-opamp-lazyfs-cache}:/cache" \
  -e LAZYFS_SEEDS="${LAZYFS_SEEDS:-25}" -e LAZYFS_SEED_BASE="$base" -e LAZYFS_JOBS="${LAZYFS_JOBS:-2}" \
  -e LAZYFS_CLIENTS="${LAZYFS_CLIENTS:-=rust,=go-core,=node,=python-core,=java-core,=dotnet-core}" \
  -e LAZYFS_SAVES="${LAZYFS_SAVES:-1}" -e HOST_IDS="$(id -u):$(id -g)" \
  "$image" bash /src/conformance/lazyfs/inside.sh
