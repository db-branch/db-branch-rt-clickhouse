#!/usr/bin/env python3
"""manifest.json (and runtimes.json) of the ClickHouse module. usage: mkmodule_ch.py <module dir>"""
import hashlib, json, sys
d = sys.argv[1]
def sha(p): return hashlib.sha256(open(p, 'rb').read()).hexdigest()
open(d + '/runtimes.json', 'w').write('[]')
b = d + '/db-branch-rt-clickhouse'
m = {"schema_version": 1, "engine": "clickhouse", "display_name": "ClickHouse", "module_version": "0.1.0", "api_version": 1,
     "binary": b, "binary_sha256": sha(b), "catalog": d + '/runtimes.json', "catalog_sha256": sha(d + '/runtimes.json'),
     "run_as_user": "db-branch-rt-clickhouse", "collation_bound": False,
     "zfs_props": {"recordsize": "128K", "compression": "lz4"},
     "protocols": [{"name": "http", "primary": True}],
     "capabilities": {"query": True, "checkpoints": True},
     "systemd": {"service_type": "simple", "kill_signal": "SIGTERM", "kill_mode": "mixed", "timeout_stop_sec": 120}}
open(d + '/manifest.json', 'w').write(json.dumps(m, indent=2))
