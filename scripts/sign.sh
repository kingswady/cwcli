#!/bin/sh
# sign.sh FILE SIGNATURE — signs FILE with the release key (ed25519 PEM in
# CW_SIGNING_KEY): a raw 64-byte signature, what cw update checks. GoReleaser
# runs it for checksums.txt.
set -eu
: "${CW_SIGNING_KEY:?the release key is not set}"
key="$(mktemp)"
trap 'rm -f "$key"' EXIT
printf '%s\n' "$CW_SIGNING_KEY" > "$key"
openssl pkeyutl -sign -rawin -inkey "$key" -in "$1" -out "$2"
