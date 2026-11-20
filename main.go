// Command db-branch-rt-clickhouse is a prototype runtime module that lets
// db-branch branch ClickHouse (docs/runtime-modules-standard.md). It serves
// the Runtime gRPC service as db-branch-rt-clickhouse and has the core run
// `clickhouse server` in a db-branch-rt@ unit. Only the HTTP interface is
// opened; the runtime is the single `clickhouse` binary in the runtime
// directory.
package main

import (
	"github.com/db-branch/db-branch/pkg/ext/runtimeplugin"
)

func main() { runtimeplugin.Serve(newModule()) }
