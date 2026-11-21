#!/usr/bin/env bash
# Build and package a signed db-branch runtime module release archive (.tar.gz).
# Usage:
#   SIGNING_KEY=/path/to/key.pem SIGNING_KEY_ID=<key_id> \
#     scripts/release-asset.sh <version> <arch> <out_dir>
set -euo pipefail

usage() {
  echo "usage: SIGNING_KEY=/path/to/key.pem SIGNING_KEY_ID=<key-id> $0 <version> <arch> <out_dir>" >&2
}

if [ "$#" -ne 3 ]; then
  usage
  exit 1
fi

version=$1
arch=$2
out_dir=$3

signing_key="${SIGNING_KEY:-}"
key_id="${SIGNING_KEY_ID:-}"
mtime="${SOURCE_DATE_EPOCH:-0}"

if [ -z "$signing_key" ] || [ -z "$key_id" ]; then
  echo "error: SIGNING_KEY and SIGNING_KEY_ID environment variables are required" >&2
  usage
  exit 1
fi

if [ -z "$out_dir" ]; then
  echo "error: output directory must not be empty" >&2
  usage
  exit 1
fi

# Validate version: SemVer 2.0.0 without leading 'v', max 64 chars.
case "$version" in
  ""|*$'\n'*|*$'\r'*)
    echo "error: invalid version '$version' (must be SemVer 2.0.0 without leading 'v')" >&2
    exit 1
    ;;
esac

pre_id='(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)'
semver_re="^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-${pre_id}(\.${pre_id})*)?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$"
if [ "${#version}" -gt 64 ] || ! printf '%s\n' "$version" | LC_ALL=C grep -Eq -- "$semver_re"; then
  echo "error: invalid version '$version' (must be SemVer 2.0.0 without leading 'v')" >&2
  exit 1
fi

# Validate arch: only amd64 and arm64 are supported.
case "$arch" in
  amd64|arm64) ;;
  *)
    echo "error: unsupported arch '$arch' (must be amd64 or arm64)" >&2
    exit 1
    ;;
esac

# Validate SOURCE_DATE_EPOCH (non-negative integer).
case "$mtime" in
  ""|*$'\n'*|*$'\r'*)
    echo "error: invalid SOURCE_DATE_EPOCH '$mtime' (must be a non-negative integer)" >&2
    exit 1
    ;;
esac
if ! printf '%s\n' "$mtime" | LC_ALL=C grep -Eq -- '^[0-9]+$'; then
  echo "error: invalid SOURCE_DATE_EPOCH '$mtime' (must be a non-negative integer)" >&2
  exit 1
fi

# Validate key_id: matches sign-manifest.sh and db-branch ExtractToStaging entry rules.
case "$key_id" in
  *$'\n'*|*$'\r'*|.*|*..*)
    echo "error: invalid SIGNING_KEY_ID '$key_id' (must match ^[A-Za-z0-9._-]{1,64}\$ without leading '.' or '..')" >&2
    exit 1
    ;;
esac
if [ "${#key_id}" -lt 1 ] || [ "${#key_id}" -gt 64 ] || ! printf '%s\n' "$key_id" | LC_ALL=C grep -Eq -- '^[A-Za-z0-9._-]{1,64}$'; then
  echo "error: invalid SIGNING_KEY_ID '$key_id' (must match ^[A-Za-z0-9._-]{1,64}\$)" >&2
  exit 1
fi

# Fail fast on missing/unreadable/loose private key before running go build.
if [ ! -f "$signing_key" ] || [ ! -r "$signing_key" ]; then
  echo "error: private key not found or not readable: $signing_key" >&2
  exit 1
fi

mode=""
if mode=$(stat -L -c '%a' "$signing_key" 2>/dev/null); then
  :
elif mode=$(stat -L -f '%Lp' "$signing_key" 2>/dev/null); then
  :
else
  echo "error: could not inspect permissions of $signing_key" >&2
  exit 1
fi
if [ -z "$mode" ] || ! printf '%s\n' "$mode" | LC_ALL=C grep -Eq -- '^[0-7]{1,4}$'; then
  echo "error: unexpected permission format for $signing_key: $mode" >&2
  exit 1
fi
if [ $(( 8#$mode & 07177 )) -ne 0 ]; then
  echo "error: private key permissions ($mode) are too open; must be 0600 or stricter" >&2
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

repo_root=$(cd "$(dirname "$0")/.." && pwd)
bin_name="db-branch-rt-clickhouse"
install_dir="/usr/lib/db-branch/runtimes/clickhouse"
stage_dir="${repo_root}/dist/stage-${arch}"
archive_name="${bin_name}_${version}_linux_${arch}.tar.gz"

tmpdir=""
cleanup() {
  if [ -n "$tmpdir" ] && [ -d "$tmpdir" ]; then
    rm -rf "$tmpdir"
  fi
}
trap cleanup EXIT

tmpdir=$(mktemp -d)
chmod 0700 "$tmpdir"

rm -rf "$stage_dir"
mkdir -p "$stage_dir"

# 1. Build the static Linux binary for the target architecture.
(
  cd "$repo_root"
  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" \
    go build -trimpath -buildvcs=false -ldflags "-s -w -X main.version=${version}" \
    -o "${stage_dir}/${bin_name}" .
)

# 2. Generate manifest.json and runtimes.json targeting the installed path.
python3 "${repo_root}/scripts/mkmanifest.py" "$install_dir" "$stage_dir" "$version"

# 3. Sign manifest.json -> manifest.json.sig (also verifies key/permissions/signature).
"${repo_root}/scripts/sign-manifest.sh" "${stage_dir}/manifest.json" "$signing_key" "$key_id"

# 4. Derive <key_id>.pub (base64 of the raw 32-byte Ed25519 public key).
"$OPENSSL" pkey -in "$signing_key" -pubout -outform DER > "${tmpdir}/pub.der"
der_len=$(wc -c < "${tmpdir}/pub.der" | tr -d '[:space:]')
if [ "$der_len" -ne 44 ]; then
  echo "error: expected 44-byte Ed25519 SubjectPublicKeyInfo DER, got $der_len bytes" >&2
  exit 1
fi
tail -c 32 "${tmpdir}/pub.der" > "${tmpdir}/pub.raw"
raw_len=$(wc -c < "${tmpdir}/pub.raw" | tr -d '[:space:]')
if [ "$raw_len" -ne 32 ]; then
  echo "error: expected 32-byte raw Ed25519 public key, got $raw_len bytes" >&2
  exit 1
fi
pub_b64=$(base64 < "${tmpdir}/pub.raw" | tr -d '\r\n')
printf '%s\n' "$pub_b64" > "${stage_dir}/${key_id}.pub"

# 5. Normalize permissions (0755 binary, 0644 others) and fixed mtime.
chmod 00755 "${stage_dir}/${bin_name}"
chmod 00644 \
  "${stage_dir}/manifest.json" \
  "${stage_dir}/manifest.json.sig" \
  "${stage_dir}/runtimes.json" \
  "${stage_dir}/${key_id}.pub"

python3 -c 'import os, sys
ts = int(sys.argv[1])
for p in sys.argv[2:]:
    os.utime(p, (ts, ts))
' "$mtime" \
  "${stage_dir}/${bin_name}" \
  "${stage_dir}/manifest.json" \
  "${stage_dir}/manifest.json.sig" \
  "${stage_dir}/runtimes.json" \
  "${stage_dir}/${key_id}.pub"

# 6. Create deterministic, flat, ustar .tar.gz with uid/gid 0, no owner names, fixed order.
entries=(
  "$bin_name"
  "manifest.json"
  "manifest.json.sig"
  "runtimes.json"
  "${key_id}.pub"
)

tmp_archive="${tmpdir}/${archive_name}"
tar_ver=$(tar --version 2>/dev/null || true)
case "$tar_ver" in
  *"GNU tar"*)
    tar --format=ustar \
      --owner=0 --group=0 --numeric-owner \
      --mtime="@${mtime}" \
      -C "$stage_dir" \
      -cf - "${entries[@]}" | gzip -n > "$tmp_archive"
    ;;
  *bsdtar*|*libarchive*)
    COPYFILE_DISABLE=1 tar --format=ustar \
      --no-mac-metadata --no-xattrs \
      --uid 0 --gid 0 --uname "" --gname "" \
      -C "$stage_dir" \
      -cf - "${entries[@]}" | gzip -n > "$tmp_archive"
    ;;
  *)
    echo "error: unsupported tar implementation: ${tar_ver:-unknown}" >&2
    exit 1
    ;;
esac

chmod 0644 "$tmp_archive"
mkdir -p "$out_dir"
mv -f "$tmp_archive" "${out_dir}/${archive_name}"
