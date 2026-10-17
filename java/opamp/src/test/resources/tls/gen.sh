#!/bin/sh
# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
#
# Makes the certificates TransportTest uses, once (they last a century): a CA, a server
# certificate it signed (127.0.0.1 and localhost), a client certificate it signed, and a second CA
# that signed nothing here. EC P-256 keys, PKCS #8. The CAs' keys are not kept.
#
#   cd java/opamp/src/test/resources/tls && ./gen.sh
set -eu
days=36500
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
key() { openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out "$1"; }

key "$tmp/ca-key.pem"
openssl req -x509 -new -key "$tmp/ca-key.pem" -subj "/CN=opamp-test-ca" -days $days \
  -addext "basicConstraints=critical,CA:TRUE" -addext "keyUsage=critical,keyCertSign,cRLSign" -out ca.pem

key "$tmp/other-key.pem"
openssl req -x509 -new -key "$tmp/other-key.pem" -subj "/CN=opamp-test-other-ca" -days $days \
  -addext "basicConstraints=critical,CA:TRUE" -addext "keyUsage=critical,keyCertSign,cRLSign" -out other-ca.pem

leaf() { # name, subject, extensions
  key "$1-key.pem"
  openssl req -new -key "$1-key.pem" -subj "$2" -out "$tmp/$1.csr"
  printf '%s\n' "$3" > "$tmp/$1.ext"
  openssl x509 -req -in "$tmp/$1.csr" -CA ca.pem -CAkey "$tmp/ca-key.pem" -CAcreateserial \
    -CAserial "$tmp/ca.srl" -days $days -extfile "$tmp/$1.ext" -out "$1.pem"
}
leaf server "/CN=localhost" "subjectAltName=IP:127.0.0.1,DNS:localhost
extendedKeyUsage=serverAuth"
leaf client "/CN=opamp-test-agent" "extendedKeyUsage=clientAuth"
