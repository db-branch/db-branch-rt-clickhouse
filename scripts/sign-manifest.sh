#!/usr/bin/env bash
# Sign manifest.json with an Ed25519 private key and write manifest.json.sig.
# Usage: scripts/sign-manifest.sh <manifest.json> <private-key.pem> <key_id>
set -euo pipefail

if [ "$#" -ne 3 ]; then
  echo "usage: $0 <manifest.json> <private-key.pem> <key_id>" >&2
  exit 1
fi

manifest=$1
key_file=$2
key_id=$3

if [ ! -f "$manifest" ] || [ ! -r "$manifest" ]; then
  echo "error: manifest not found or not readable: $manifest" >&2
  exit 1
fi

if [ ! -f "$key_file" ] || [ ! -r "$key_file" ]; then
  echo "error: private key not found or not readable: $key_file" >&2
  exit 1
fi

# Reject private keys with permissions looser than 0600 (any group/other, exec, or special bits).
# Try GNU stat (-c) before BSD stat (-f), since GNU stat uses -f for filesystem info.
mode=""
if mode=$(stat -L -c '%a' "$key_file" 2>/dev/null); then
  :
elif mode=$(stat -L -f '%Lp' "$key_file" 2>/dev/null); then
  :
else
  echo "error: could not inspect permissions of $key_file" >&2
  exit 1
fi

if [ -z "$mode" ] || ! printf '%s' "$mode" | LC_ALL=C grep -Eq '^[0-7]{1,4}$'; then
  echo "error: unexpected permission format for $key_file: $mode" >&2
  exit 1
fi

if [ $(( 8#$mode & 07177 )) -ne 0 ]; then
  echo "error: private key permissions ($mode) are too open; must be 0600 or stricter" >&2
  exit 1
fi

# Validate key_id: ^[A-Za-z0-9._-]{1,64}$
case "$key_id" in
  *$'\n'*|*$'\r'*)
    echo "error: invalid key_id (must match ^[A-Za-z0-9._-]{1,64}\$)" >&2
    exit 1
    ;;
esac

if [ "${#key_id}" -lt 1 ] || [ "${#key_id}" -gt 64 ] || ! printf '%s' "$key_id" | LC_ALL=C grep -Eq '^[A-Za-z0-9._-]{1,64}$'; then
  echo "error: invalid key_id (must match ^[A-Za-z0-9._-]{1,64}\$)" >&2
  exit 1
fi

# Prefer Homebrew OpenSSL on macOS; reject LibreSSL and OpenSSL < 3.
OPENSSL=""
if [ -x /opt/homebrew/bin/openssl ]; then
  OPENSSL=/opt/homebrew/bin/openssl
elif OPENSSL=$(command -v openssl 2>/dev/null); then
  :
else
  echo "error: openssl not found; OpenSSL 3+ is required" >&2
  exit 1
fi

openssl_ver=$("$OPENSSL" version 2>/dev/null || true)
case "$openssl_ver" in
  *LibreSSL*)
    echo "error: LibreSSL ($openssl_ver) is not supported; install OpenSSL 3+" >&2
    exit 1
    ;;
  OpenSSL\ [3-9].*|OpenSSL\ [1-9][0-9].*)
    ;;
  *)
    echo "error: OpenSSL 3+ is required (found: ${openssl_ver:-unknown})" >&2
    exit 1
    ;;
esac

sig_path="${manifest}.sig"
tmpdir=""
succeeded=0

cleanup() {
  if [ -n "$tmpdir" ] && [ -d "$tmpdir" ]; then
    rm -rf "$tmpdir"
  fi
  if [ "$succeeded" -ne 1 ]; then
    rm -f "$sig_path"
  fi
}
trap cleanup EXIT

umask 077
tmpdir=$(mktemp -d)

# Sign the exact bytes of manifest.json.
"$OPENSSL" pkeyutl -sign -inkey "$key_file" -rawin -in "$manifest" -out "$tmpdir/sig.bin"

sig_len=$(wc -c < "$tmpdir/sig.bin" | tr -d '[:space:]')
if [ "$sig_len" -ne 64 ]; then
  echo "error: expected 64-byte Ed25519 signature, got $sig_len bytes" >&2
  exit 1
fi

sig_b64=$(base64 < "$tmpdir/sig.bin" | tr -d '\r\n')

# Derive the public key and verify the base64-decoded signature against manifest.json.
"$OPENSSL" pkey -in "$key_file" -pubout -out "$tmpdir/pub.pem"
printf '%s' "$sig_b64" | "$OPENSSL" base64 -d -A -out "$tmpdir/sig.verify"
"$OPENSSL" pkeyutl -verify -pubin -inkey "$tmpdir/pub.pem" -rawin -in "$manifest" -sigfile "$tmpdir/sig.verify" >/dev/null

# Write manifest.json.sig with mode 0644.
printf '{"key_id":"%s","signature":"%s"}\n' "$key_id" "$sig_b64" > "$tmpdir/out.sig"
cp "$tmpdir/out.sig" "$sig_path"
chmod 0644 "$sig_path"

succeeded=1
