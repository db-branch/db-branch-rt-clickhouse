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
| features | `query`, `checkpoints` |
| OS user | `db-branch-rt-clickhouse` |
| module files | `/usr/lib/db-branch/runtimes/clickhouse/` |
| runtime | `/var/lib/db-branch/runtimes/clickhouse/<version>/clickhouse` |
| stop | `KillMode=mixed`: `clickhouse server` is a watchdog and a server, and a SIGTERM to both makes the server skip its clean shutdown |

## Layout

```
main.go         runtimeplugin.Serve
module.go       the Runtime gRPC service
pure.go         config.xml, users.xml, result parsing (no I/O)
*_test.go       unit tests; integration_test.go (tag integration) runs a real ClickHouse
scripts/        mkmanifest.py (manifest.json with the digests), sign-manifest.sh, conformance.sh
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

```
apt install ./db-branch-rt-clickhouse_<version>_amd64.deb
```

The package ships a **signed** `manifest.json` (Ed25519; the key is in `/etc/db-branch/trusted-keys/`). A module
without a signature loads only with `allow_unsigned_runtimes = true` in db-branch's `config.toml`, for development.
Put the ClickHouse binary of the version you want in `/var/lib/db-branch/runtimes/clickhouse/<version>/` until the
module lists pinned downloads.

## Release

### 1. Generate a signing key pair (OpenSSL 3+)

Create an Ed25519 private key **outside the repository** (never commit private keys to git; `*.pem` and `*.key` are gitignored as a safeguard). On macOS, use OpenSSL 3+ (e.g. `/opt/homebrew/bin/openssl`), not LibreSSL:

```bash
openssl genpkey -algorithm ED25519 -out /path/to/release-key.pem
chmod 600 /path/to/release-key.pem
openssl pkey -in /path/to/release-key.pem -pubout -outform DER | tail -c 32 | base64 > <key_id>.pub
```

`key_id` must match `^[A-Za-z0-9._-]{1,64}$` (for example `db-branch-2026`).

### 2. Build the signed `.deb` package

```bash
make deb SIGNING_KEY=/path/to/release-key.pem SIGNING_KEY_ID=<key_id> VERSION=0.1.0
```

`make deb` stages the binary, generates `manifest.json` and `runtimes.json`, signs `manifest.json` into `manifest.json.sig` (rejecting keys with permissions looser than `0600` and self-verifying the signature), and packages `dist/db-branch-rt-clickhouse_<version>_<arch>.deb` with `nfpm`.

### 3. Install the public key on target nodes

Operators install `<key_id>.pub` (base64-encoded 32-byte raw Ed25519 public key) under `/etc/db-branch/trusted-keys/` owned by `root:root` and not writable by group or others:

```bash
sudo install -d -m 0755 -o root -g root /etc/db-branch/trusted-keys
sudo install -m 0644 -o root -g root <key_id>.pub /etc/db-branch/trusted-keys/<key_id>.pub
```

### 4. Key compromise / rotation

If a signing key is leaked or suspected compromised:

1. Generate a new Ed25519 key pair with a new `<new_key_id>`.
2. Deploy `/etc/db-branch/trusted-keys/<new_key_id>.pub` to all nodes.
3. Build and distribute a new `.deb` signed with the new key.
4. Remove the compromised key's `/etc/db-branch/trusted-keys/<old_key_id>.pub` from all nodes.

## Naming

This repository follows the naming of db-branch's module repositories: see `docs/module-repositories.md`.
