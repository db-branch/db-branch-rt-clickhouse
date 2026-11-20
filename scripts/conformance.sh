#!/bin/bash
# Runs db-branch-conformance --lifecycle against this module and a real ClickHouse binary.
# usage (root): scripts/conformance.sh <module binary> <clickhouse binary>
set -eu
mod=$1; ch=$2
D=/usr/lib/db-branch/runtimes/clickhouse
R=/var/lib/db-branch/runtimes/clickhouse/0.0
here=$(cd "$(dirname "$0")" && pwd)
cleanup() {
  systemctl stop db-branch-rt@conformance-clickhouse.service 2>/dev/null || true
  rm -rf /run/systemd/system/db-branch-rt@conformance-clickhouse.service.d /run/db-branch-rt/clickhouse \
         /var/lib/db-branch/conformance /var/lib/db-branch/instances/conformance-clickhouse /var/lib/db-branch/runtimes/clickhouse "$D"
  systemctl daemon-reload
  userdel db-branch-rt-clickhouse 2>/dev/null || true
}
trap cleanup EXIT
id db-branch-rt-clickhouse >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin db-branch-rt-clickhouse
install -d -m 0755 -o root -g root "$D" "$R"
install -m 0755 -o root -g root "$mod" "$D/db-branch-rt-clickhouse"
install -m 0755 -o root -g root "$ch" "$R/clickhouse"
python3 "$here/mkmanifest.py" "$D"
db-branch-conformance --manifest "$D/manifest.json" --lifecycle
