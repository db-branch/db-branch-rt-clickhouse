# Naming of db-branch module repositories

`rt` is short for **runtime**: the database engine a module runs (the same word as `db-branch runtime install`,
`db-branch-rt@.service`).

## Runtime modules (a database engine)

| Thing | Name | Example |
|---|---|---|
| repository | `db-branch-rt-<engine>` | `db-branch/db-branch-rt-clickhouse` |
| Go module | `github.com/<org>/db-branch-rt-<engine>` | `github.com/db-branch/db-branch-rt-clickhouse` |
| binary | `db-branch-rt-<engine>` | `/usr/lib/db-branch/runtimes/clickhouse/db-branch-rt-clickhouse` |
| apt package | `db-branch-rt-<engine>` | `apt install db-branch-rt-clickhouse` |
| OS user | `db-branch-rt-<engine>` | `db-branch-rt-clickhouse` |
| module name in the cluster (`ModuleSpec`) | `rt-<engine>` | `rt-clickhouse` |
| module directory | `/usr/lib/db-branch/runtimes/<engine>/` | |
| runtime (the engine's own files) | `/var/lib/db-branch/runtimes/<engine>/<version>/` | |
| systemd unit of an instance | `db-branch-rt@<instance-id>.service` | |
| git tags | `vMAJOR.MINOR.PATCH`; `module_version` in `manifest.json` is the same number | `v0.1.0` |
| GitHub topic | `db-branch-runtime-module` | |

Rules:

- `<engine>` matches `^[a-z][a-z0-9-]*$`, is at most 19 characters (the OS user name is limited to 32), and is not
  `mysql` or `postgres` (built into db-branch). It is the same string in `manifest.json` (`engine`), in
  `Describe()`, in `engine@version` and in the paths above.
- The binary, the apt package, the OS user and the repository carry the same name, so one can be told from another
  at a glance (`ps`, `systemctl`, `apt`, `git`).
- The `rt-` in the middle keeps runtime modules apart from the other modules (`db-branch-<name>`, below) even when
  an engine and a plugin share a name.
- Modules of other organisations use the same names under their own organisation. Two modules for one engine are
  told apart by the organisation, not by the name; a node installs one module per engine.

## Other modules (plugins of the agent)

`db-branch-<name>` for the repository, binary and apt package (`db-branch-greenmask`, `db-branch-proxysql`,
`db-branch-anon`). They run under the agent's modules directory (`/usr/lib/db-branch/modules`) and are recorded in
the cluster under `<name>`.

## A runtime module repository contains

```
README.md  Makefile  go.mod  main.go
manifest        scripts/mkmanifest.py (digests), signed on release
packaging/      nfpm.yaml, postinst (creates the OS user)
integration_test.go  (tag integration, a real engine)
scripts/conformance.sh   db-branch-conformance --lifecycle
```
