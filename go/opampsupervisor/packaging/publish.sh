#!/bin/sh
# Copyright (c) 2026 Bill Easton. All rights reserved.
# SPDX-License-Identifier: LicenseRef-All-Rights-Reserved
#
# Publishes the packages build.sh made (dist/*.deb, dist/*.rpm) to the apt and yum repositories in
# a bucket, with `gcloud storage`: GCS (gs://, with Google credentials such as the workflow's
# Workload Identity Federation), or S3 and any S3-compatible store (s3://, with AWS credentials;
# CLOUDSDK_STORAGE_S3_ENDPOINT_URL picks one other than S3):
#   REPO_BUCKET=gs://bucket REPO_URL=https://packages.pydantic.dev SIGNING_KEY_FILE=key.asc \
#     packaging/publish.sh
# Fetches the packages already published, adds these with repo.sh and uploads the result, so the
# earlier releases stay installable. Refuses to sign with a key other than the published one, or to
# replace a published package with different contents: either breaks every machine already using
# the repositories. Needs gcloud, and what repo.sh needs.
set -eu
: "${REPO_BUCKET:?set REPO_BUCKET, the bucket (gs://name or s3://name, with an optional /prefix) to publish to}"
: "${REPO_URL:?set REPO_URL, where REPO_BUCKET is served from}"
: "${SIGNING_KEY_FILE:?set SIGNING_KEY_FILE, the armored secret key to sign with}"
case $SIGNING_KEY_FILE in /*) ;; *) SIGNING_KEY_FILE=$PWD/$SIGNING_KEY_FILE ;; esac
export SIGNING_KEY_FILE
cd "$(dirname "$0")"
REPO_BUCKET=${REPO_BUCKET%/}

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
storage() { gcloud storage "$@"; }
# rsync takes regexes of paths to leave out, not globs: this one leaves out all but the rpms.
rpms='^(?!.*\.rpm$)'

# What's published: the packages, which the new indexes must list too, and the key. rsync refuses
# a prefix with nothing under it, so only what the listing shows is fetched; a listing that fails
# for any reason but there being nothing yet stops here, before an index could drop a package.
if ! listing=$(storage ls --recursive "$REPO_BUCKET/**" 2>"$work/ls.err"); then
	grep -q 'matched no objects' "$work/ls.err" || { cat "$work/ls.err" >&2; exit 1; }
	listing=
fi
listed() { printf '%s\n' "$listing" | grep -q "^$REPO_BUCKET/$1"; }
mkdir -p "$work/repo/deb/pool" "$work/repo/rpm" "$work/published"
if listed deb/pool/; then storage rsync --recursive "$REPO_BUCKET/deb/pool" "$work/repo/deb/pool"; fi
if listed 'rpm/.*\.rpm$'; then storage rsync --recursive --exclude "$rpms" "$REPO_BUCKET/rpm" "$work/repo/rpm"; fi
if listed 'pydantic-opamp\.asc$'; then storage cp "$REPO_BUCKET/pydantic-opamp.asc" "$work/published/"; fi

fingerprint() { gpg --batch --with-colons --show-keys "$1" | awk -F: '$1 == "fpr" { print $10; exit }'; }
if [ -f "$work/published/pydantic-opamp.asc" ]; then
	published=$(fingerprint "$work/published/pydantic-opamp.asc")
	signing=$(fingerprint "$SIGNING_KEY_FILE")
	if [ "$published" != "$signing" ]; then
		echo "publish.sh: the repositories are signed with $published, not $signing: refusing to re-sign them" >&2
		exit 1
	fi
fi
for file in dist/*.deb dist/*.rpm; do
	case $file in *.deb) at=$work/repo/deb/pool/main ;; *) at=$work/repo/rpm ;; esac
	if [ -f "$at/${file#dist/}" ] && ! cmp -s "$file" "$at/${file#dist/}"; then
		echo "publish.sh: ${file#dist/} is already published, with other contents: give this build another version" >&2
		exit 1
	fi
done

REPO_DIR="$work/repo" ./repo.sh >/dev/null

# Packages first, then the indexes listing them, their signatures last: a client fetching mid-way
# gets the old indexes, or new ones whose packages are all there. Packages never change, so they
# may be cached for good; the indexes may not be cached at all.
packages='public, max-age=31536000, immutable'
indexes='no-cache'
storage rsync --recursive --cache-control "$packages" "$work/repo/deb/pool" "$REPO_BUCKET/deb/pool"
storage rsync --recursive --cache-control "$packages" --exclude "$rpms" "$work/repo/rpm" "$REPO_BUCKET/rpm"
storage rsync --recursive --cache-control "$indexes" --exclude '^repomd\.xml' "$work/repo/rpm/repodata" "$REPO_BUCKET/rpm/repodata"
for file in repomd.xml repomd.xml.asc; do
	storage cp --cache-control "$indexes" "$work/repo/rpm/repodata/$file" "$REPO_BUCKET/rpm/repodata/$file"
done
storage rsync --recursive --cache-control "$indexes" --exclude '^stable/(Release|Release\.gpg|InRelease)$' \
	"$work/repo/deb/dists" "$REPO_BUCKET/deb/dists"
for file in Release Release.gpg InRelease; do
	storage cp --cache-control "$indexes" "$work/repo/deb/dists/stable/$file" "$REPO_BUCKET/deb/dists/stable/$file"
done
for file in pydantic-opamp.asc pydantic-opamp.gpg pydantic-opamp.sources pydantic-opamp.repo; do
	storage cp --cache-control "$indexes" "$work/repo/$file" "$REPO_BUCKET/$file"
done
echo "published to $REPO_URL:" dist/*.deb dist/*.rpm
