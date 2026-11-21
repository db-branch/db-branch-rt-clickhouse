# db-branch-rt-clickhouse

ClickHouse as a [db-branch](https://github.com/db-branch/db-branch) **runtime module**: with it, `db-branch` branches
ClickHouse databases the way it branches MySQL and PostgreSQL (ZFS clones, per-branch logins, checkpoints).

> Status: **beta**. Only the HTTP interface is opened (no native protocol on 9000). The module lists ClickHouse
> 26.3 LTS (x86_64) and db-branch downloads and verifies it; arm64 is not listed yet. It needs a db-branch that
> has the runtime-module support (not in a release yet, see "Develop"). The package is not signed yet: until it
> is, load it with `allow_unsigned_runtimes = true`. Tested on a 4-node cluster (create, branch, isolation,
> restricted logins, checkpoint restore, reset); not load-tested.

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
scripts/        mkmanifest.py (manifest.json with the digests), conformance.sh
packaging/      nfpm.yaml, postinst
```

## Develop

db-branch's `pkg/ext/proto/runtime/v1` and `pkg/ext/runtimeplugin` are not in a release yet, so this repository is
built against a checkout of db-branch through `go.work` (not committed):

```
go 1.27.3
use (
	.
	../db-branch      # a checkout that has pkg/ext/runtimeplugin
)
```

```
make test                                  # unit tests
make integration CH_BINARY=/path/clickhouse   # a real ClickHouse, current user, no systemd
sudo make conformance CH_BINARY=/path/clickhouse   # db-branch-conformance --lifecycle (root, systemd)
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

## Naming

This repository follows the naming of db-branch's module repositories: see `docs/module-repositories.md`.
