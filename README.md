# db-branch-rt-clickhouse

ClickHouse as a [db-branch](https://github.com/db-branch/db-branch) **runtime module**: with it, `db-branch` branches
ClickHouse databases the way it branches MySQL and PostgreSQL (ZFS clones, per-branch logins, checkpoints).

> Status: **beta**. Only the HTTP interface is opened (no native protocol on 9000). The module lists ClickHouse
> 26.3 LTS (x86_64) and db-branch downloads and verifies it; arm64 is not listed yet. It needs a db-branch with
> runtime-module support (on db-branch `main`, not in a tagged release yet; see "Develop"). The package is not
> signed yet: until it is, load it with `allow_unsigned_runtimes = true`. Tested on a 4-node cluster (create,
> branch, isolation, restricted logins, checkpoint restore, reset); not load-tested.

## What it is

A runtime module is a program db-branch starts as its own OS user and talks to over gRPC
(`pkg/ext/proto/runtime/v1` of db-branch). It prepares and inspects instances; **db-branch itself starts and stops
ClickHouse** (systemd unit `db-branch-rt@<instance>.service`), so the module never gets root or `systemctl`.

| What | Value |
|---|---|
| engine | `clickhouse` |
| protocol | `http` (primary) |
| features | `query`, `checkpoints`, `live_freeze`, `upgrade`, `source_dump` |
| OS user | `db-branch-rt-clickhouse` |
| module files | `/usr/lib/db-branch/runtimes/clickhouse/` |
| runtime | `/var/lib/db-branch/runtimes/clickhouse/<version>/clickhouse` |
| stop | `KillMode=mixed`: `clickhouse server` is a watchdog and a server, and a SIGTERM to both makes the server skip its clean shutdown |

### Source dump (`DumpSource` / `Load`) limitations

- **Plaintext HTTP only**: Because `RefreshSource` currently carries no TLS settings, `DumpSource` connects to the source ClickHouse server over plaintext HTTP (Basic Auth, no redirects). Use it only over a closed network such as a VPN or private VPC.
- **Supported engines only**: Only local ClickHouse table engines (`MergeTree` family, `Log` family, `Memory`, `Null`, `View`, `MaterializedView`) are supported. Databases containing external-resource engines (`URL`, `S3`, `HDFS`, `File`, `MySQL`, `PostgreSQL`, `MongoDB`, `Kafka`, `RabbitMQ`, `JDBC`, `ODBC`, `Redis`, `Distributed`, `Executable`, `Dictionary`, etc.) or DDLs invoking external table functions abort the dump with a list of offending tables. System databases (`system`, `INFORMATION_SCHEMA`, `information_schema`) are always refused.

## Layout

```
main.go         runtimeplugin.Serve
module.go       the Runtime gRPC service
pure.go         config.xml, users.xml, result parsing (no I/O)
dump.go         framed dump reader/writer, identifier and DDL/engine safety checks
*_test.go       unit tests; integration_test.go (tag integration) runs a real ClickHouse
scripts/        mkmanifest.py (manifest.json with the digests), sign-manifest.sh, release-asset.sh, conformance.sh
packaging/      nfpm.yaml, postinst
```

## Develop

No `go.work` is needed: `go.mod` pins `github.com/db-branch/db-branch` to a pseudo-version from `main` (to be
updated once a tagged release with `pkg/ext/proto/runtime/v1` and `pkg/ext/runtimeplugin` is published).

```
make test                                  # unit tests
make integration CH_BINARY=/path/clickhouse   # a real ClickHouse, current user, no systemd
sudo make conformance CH_BINARY=/path/clickhouse   # db-branch-conformance --lifecycle (root, systemd)
```

If you are developing against local changes in `db-branch` at the same time, create a `go.work` (gitignored) next
to `go.mod`:

```
go 1.27.3
use (
	.
	../db-branch
)
```

`db-branch-conformance` is the contract check of db-branch (`cmd/db-branch-conformance`): the manifest equals
`Describe()`, the declared features are the ones answered, the launch plan is safe, and one instance goes through
initialise, start, clean stop, restart, clone, crash recovery and remove under systemd.

## Install

### Recommended: `db-branch module install` (GitHub Release `.tar.gz`, no `.deb`)

For third-party deployments or operators who do not use db-branch's APT repository, install the signed release archive directly with `db-branch module install`:

```bash
sudo db-branch module install github.com/db-branch/db-branch-rt-clickhouse@v<version> \
  --trust-key sha256:<fingerprint>
```

Verify the `sha256:<fingerprint>` value against the [Public key fingerprint](#公開鍵の指紋) section below (or an out-of-band source) before trusting it. Because the release archive ships a signed `manifest.json` verified against `/etc/db-branch/trusted-keys/<key_id>.pub`, `allow_unsigned_runtimes = true` is **not** needed.

### Alternative: `.deb` package

```bash
apt install ./db-branch-rt-clickhouse_<version>_amd64.deb
```

The package ships a **signed** `manifest.json` (Ed25519; the key is in `/etc/db-branch/trusted-keys/`). A module without a signature loads only with `allow_unsigned_runtimes = true` in db-branch's `config.toml`, for development.
Put the ClickHouse binary of the version you want in `/var/lib/db-branch/runtimes/clickhouse/<version>/` until the module lists pinned downloads.

## 公開鍵の指紋

Release archives bundle `<key_id>.pub` (base64-encoded raw 32-byte Ed25519 public key). Verify its SHA-256 fingerprint before passing `--trust-key`:

| Key ID | SHA-256 Fingerprint |
|---|---|
| `db-branch-rt-2026-1` | `sha256:83fa766fc8df1aab8a4e79f3eb252dbb9820bb67945875f6d74c1a9c33a457cd` |

## Release

### 1. Generate a signing key pair (OpenSSL 3+)

Create an Ed25519 private key **outside the repository** (never commit private keys to git; `*.pem` and `*.key` are gitignored as a safeguard). On macOS, use OpenSSL 3+ (e.g. `/opt/homebrew/bin/openssl`), not LibreSSL:

```bash
openssl genpkey -algorithm ED25519 -out /path/to/release-key.pem
chmod 600 /path/to/release-key.pem
openssl pkey -in /path/to/release-key.pem -pubout -outform DER | tail -c 32 | base64 > <key_id>.pub
```

`key_id` must match `^[A-Za-z0-9._-]{1,64}$` (without a leading `.` or `..`, for example `db-branch-2026`).

### 2. Build signed GitHub Release archives (no `.deb` required)

```bash
make release VERSION=<version> SIGNING_KEY=/path/to/release-key.pem SIGNING_KEY_ID=<key_id>
```

`make release` runs `scripts/release-asset.sh` for both `amd64` and `arm64`, producing reproducible flat `.tar.gz` archives and `SHA256SUMS` under `dist/release/`:

- `dist/release/db-branch-rt-clickhouse_<version>_linux_amd64.tar.gz`
- `dist/release/db-branch-rt-clickhouse_<version>_linux_arm64.tar.gz`
- `dist/release/SHA256SUMS`

At the end, `make release` prints the public key fingerprint `sha256:<hex>` (SHA-256 of the raw 32-byte public key). Record this fingerprint in the [公開鍵の指紋](#公開鍵の指紋) section above.

### 3. Publish the GitHub Release

Attach the two `.tar.gz` archives and `SHA256SUMS` to a non-draft, non-prerelease GitHub Release whose tag matches `v<version>` (example):

```bash
gh release create v<version> dist/release/* \
  --title "v<version>" \
  --notes "Release v<version>"
```

Operators can then install the release without `.deb` or `allow_unsigned_runtimes = true`:

```bash
sudo db-branch module install github.com/db-branch/db-branch-rt-clickhouse@v<version> \
  --trust-key sha256:<fingerprint>
```

### 4. Optional: Build the signed `.deb` package

When distributing via `.deb` instead of `db-branch module install` (note: third parties and operators not using db-branch's APT repository should use `db-branch module install` above):

```bash
make deb SIGNING_KEY=/path/to/release-key.pem SIGNING_KEY_ID=<key_id> VERSION=0.1.0
```

`make deb` stages the binary, generates `manifest.json` and `runtimes.json`, signs `manifest.json` into `manifest.json.sig` (rejecting keys with permissions looser than `0600` and self-verifying the signature), and packages `dist/db-branch-rt-clickhouse_<version>_<arch>.deb` with `nfpm`.

When using `.deb`, operators install `<key_id>.pub` (base64-encoded 32-byte raw Ed25519 public key) under `/etc/db-branch/trusted-keys/` owned by `root:root` and not writable by group or others:

```bash
sudo install -d -m 0755 -o root -g root /etc/db-branch/trusted-keys
sudo install -m 0644 -o root -g root <key_id>.pub /etc/db-branch/trusted-keys/<key_id>.pub
```

### 5. Key compromise / rotation

If a signing key is leaked or suspected compromised:

1. Generate a new Ed25519 key pair with a new `<new_key_id>`.
2. Publish the new fingerprint and deploy `/etc/db-branch/trusted-keys/<new_key_id>.pub` (or `db-branch module trust add`) to all nodes.
3. Build and publish a new release signed with the new key (`make release`, and `make deb` if distributing `.deb`).
4. Remove the compromised key's `/etc/db-branch/trusted-keys/<old_key_id>.pub` (or `sudo db-branch module trust remove <old_key_id>`) from all nodes.

## Naming

This repository follows the naming of db-branch's module repositories: see `docs/module-repositories.md`.
