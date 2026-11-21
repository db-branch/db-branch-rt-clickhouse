#!/usr/bin/env python3
"""manifest.json (and runtimes.json) of the ClickHouse module.

usage: mkmanifest.py <install dir> [<stage dir>] [<module_version>]
       mkmanifest.py [--version <module_version>] <install dir> [<stage dir>]
"""
import argparse
import hashlib
import json
import os


def sha(path):
    with open(path, "rb") as f:
        return hashlib.sha256(f.read()).hexdigest()


def main():
    parser = argparse.ArgumentParser(
        description="Generate manifest.json and runtimes.json for the ClickHouse module."
    )
    parser.add_argument("install_dir", help="Installed module directory path written into manifest.json")
    parser.add_argument(
        "stage_dir",
        nargs="?",
        default=None,
        help="Staging directory with the binary and where files are written (default: install_dir)",
    )
    parser.add_argument(
        "version_pos",
        nargs="?",
        default=None,
        help="module_version value (default: 0.1.0)",
    )
    parser.add_argument(
        "--version",
        dest="version_opt",
        default=None,
        help="module_version value (default: 0.1.0)",
    )
    args = parser.parse_args()

    install_dir = args.install_dir.rstrip("/")
    stage_dir = args.stage_dir if args.stage_dir is not None else args.install_dir
    module_version = args.version_opt or args.version_pos or "0.1.0"

    stage_runtimes = os.path.join(stage_dir, "runtimes.json")
    stage_binary = os.path.join(stage_dir, "db-branch-rt-clickhouse")
    stage_manifest = os.path.join(stage_dir, "manifest.json")

    with open(stage_runtimes, "w") as f:
        f.write("[]")

    m = {
        "schema_version": 1,
        "engine": "clickhouse",
        "display_name": "ClickHouse",
        "module_version": module_version,
        "api_version": 1,
        "binary": install_dir + "/db-branch-rt-clickhouse",
        "binary_sha256": sha(stage_binary),
        "catalog": install_dir + "/runtimes.json",
        "catalog_sha256": sha(stage_runtimes),
        "run_as_user": "db-branch-rt-clickhouse",
        "collation_bound": False,
        "zfs_props": {"recordsize": "128K", "compression": "lz4"},
        "protocols": [{"name": "http", "primary": True}],
        "capabilities": {
            "query": True,
            "checkpoints": True,
            "live_freeze": True,
            "upgrade": True,
        },
        "systemd": {
            "service_type": "simple",
            "kill_signal": "SIGTERM",
            "kill_mode": "mixed",
            "timeout_stop_sec": 120,
        },
    }
    with open(stage_manifest, "w") as f:
        f.write(json.dumps(m, indent=2))


if __name__ == "__main__":
    main()
