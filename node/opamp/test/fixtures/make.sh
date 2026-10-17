#!/bin/sh
# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
#
# Makes the certificates the TLS tests use (checked in; test only, valid for 100 years):
#   ca.pem             the CA the client is told to trust
#   server.pem/-key    the server's, signed by ca.pem, for localhost, 127.0.0.1 and ::1
#   client.pem/-key    a client certificate signed by ca.pem (CN=test-agent)
#   other-server.pem/-key  a server certificate signed by another CA, which nobody trusts
set -eu
cd "$(dirname "$0")"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
key() { openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out "$1"; }
ca() { # name subject
  key "$tmp/$1-key.pem"
  openssl req -x509 -new -key "$tmp/$1-key.pem" -subj "/CN=$2" -days 36500 -out "$tmp/$1.pem" \
    -addext basicConstraints=critical,CA:true -addext keyUsage=critical,keyCertSign,cRLSign
}
leaf() { # name subject ca usage [san]
  key "$1-key.pem"
  openssl req -new -key "$1-key.pem" -subj "/CN=$2" -out "$tmp/$1.csr"
  {
    echo "basicConstraints=critical,CA:false"
    echo "keyUsage=critical,digitalSignature"
    echo "extendedKeyUsage=$4"
    if [ -n "${5:-}" ]; then echo "subjectAltName=$5"; fi
  } >"$tmp/$1.ext"
  openssl x509 -req -in "$tmp/$1.csr" -CA "$tmp/$3.pem" -CAkey "$tmp/$3-key.pem" -CAcreateserial \
    -days 36500 -extfile "$tmp/$1.ext" -out "$1.pem"
}
ca ca "opamp test CA"
ca other "some other CA"
cp "$tmp/ca.pem" ca.pem
leaf server localhost ca serverAuth "DNS:localhost,IP:127.0.0.1,IP:::1"
leaf client test-agent ca clientAuth
leaf other-server localhost other serverAuth "DNS:localhost,IP:127.0.0.1,IP:::1"
