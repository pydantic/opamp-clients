#!/bin/sh
# Copyright (c) 2026 Bill Easton. All rights reserved.
# SPDX-License-Identifier: LicenseRef-All-Rights-Reserved
#
# Adds the packages build.sh made (dist/*.deb, dist/*.rpm) to an apt and a yum repository in
# REPO_DIR, and signs both with the OpenPGP key in SIGNING_KEY_FILE (armored, secret):
#   REPO_DIR=out REPO_URL=https://packages.example.com SIGNING_KEY_FILE=key.asc packaging/repo.sh
# Packages already in REPO_DIR stay, so publishing a release keeps the earlier ones installable.
# SIGNING_KEY_PASSPHRASE unlocks the key, if it has one. Needs gpg, apt-ftparchive (apt-utils)
# and createrepo_c. Writes:
#   deb/                    the apt repository: dists/stable (Release, InRelease, Release.gpg), pool/
#   rpm/                    the yum repository: the rpms, repodata/ and repodata/repomd.xml.asc
#   pydantic-opamp.asc      the public key; pydantic-opamp.gpg is the same key for apt's Signed-By
#   pydantic-opamp.sources  the apt source; pydantic-opamp.repo the yum one; both point at REPO_URL
set -eu
: "${REPO_DIR:?set REPO_DIR, the repository to add the packages to}"
: "${REPO_URL:?set REPO_URL, where REPO_DIR is served from}"
: "${SIGNING_KEY_FILE:?set SIGNING_KEY_FILE, the armored secret key to sign with}"
# Relative paths are the caller's: made absolute before moving to this script's directory.
case $REPO_DIR in /*) ;; *) REPO_DIR=$PWD/$REPO_DIR ;; esac
case $SIGNING_KEY_FILE in /*) ;; *) SIGNING_KEY_FILE=$PWD/$SIGNING_KEY_FILE ;; esac
cd "$(dirname "$0")"
REPO_URL=${REPO_URL%/}

# The key goes into a keyring of its own, thrown away after.
GNUPGHOME=$(mktemp -d)
export GNUPGHOME
trap 'rm -rf "$GNUPGHOME"' EXIT
gpg --batch --quiet --import "$SIGNING_KEY_FILE"
fingerprint=$(gpg --batch --with-colons --list-secret-keys | awk -F: '$1 == "fpr" { print $10; exit }')
sign() {
	printf '%s' "${SIGNING_KEY_PASSPHRASE:-}" | gpg --batch --yes --quiet --pinentry-mode loopback \
		--passphrase-fd 0 --local-user "$fingerprint" "$@"
}

mkdir -p "$REPO_DIR/deb/pool/main" "$REPO_DIR/rpm"
cp dist/*.deb "$REPO_DIR/deb/pool/main/"
cp dist/*.rpm "$REPO_DIR/rpm/"

# apt: one suite (stable) and component (main), a Packages index per architecture.
(
	cd "$REPO_DIR/deb"
	for arch in amd64 arm64; do
		mkdir -p "dists/stable/main/binary-$arch"
		apt-ftparchive --arch "$arch" packages pool >"dists/stable/main/binary-$arch/Packages"
		gzip -9nkf "dists/stable/main/binary-$arch/Packages"
	done
	# Written beside dists/stable, not in it: Release lists the files there.
	apt-ftparchive \
		-o APT::FTPArchive::Release::Origin=Pydantic \
		-o APT::FTPArchive::Release::Label="Pydantic OpAMP" \
		-o APT::FTPArchive::Release::Suite=stable \
		-o APT::FTPArchive::Release::Codename=stable \
		-o APT::FTPArchive::Release::Architectures="amd64 arm64" \
		-o APT::FTPArchive::Release::Components=main \
		release dists/stable >dists/Release.new
	mv dists/Release.new dists/stable/Release
	sign --clearsign --output dists/stable/InRelease dists/stable/Release
	sign --armor --detach-sign --output dists/stable/Release.gpg dists/stable/Release
)

# yum: repodata/ over every rpm in rpm/, its repomd.xml signed (repo_gpgcheck).
createrepo_c --quiet --update "$REPO_DIR/rpm"
sign --armor --detach-sign --output "$REPO_DIR/rpm/repodata/repomd.xml.asc" "$REPO_DIR/rpm/repodata/repomd.xml"

gpg --batch --armor --export "$fingerprint" >"$REPO_DIR/pydantic-opamp.asc"
gpg --batch --export "$fingerprint" >"$REPO_DIR/pydantic-opamp.gpg"
cat >"$REPO_DIR/pydantic-opamp.sources" <<EOF
Types: deb
URIs: $REPO_URL/deb
Suites: stable
Components: main
Signed-By: /etc/apt/keyrings/pydantic-opamp.gpg
EOF
cat >"$REPO_DIR/pydantic-opamp.repo" <<EOF
[pydantic-opamp]
name=Pydantic OpAMP
baseurl=$REPO_URL/rpm
enabled=1
gpgcheck=1
repo_gpgcheck=1
gpgkey=$REPO_URL/pydantic-opamp.asc
EOF
find "$REPO_DIR" -name '*.deb' -o -name '*.rpm' | sort
