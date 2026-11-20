module github.com/db-branch/db-branch-rt-clickhouse

go 1.27.3

// github.com/db-branch/db-branch is provided by go.work until a release that has
// pkg/ext/proto/runtime/v1 and pkg/ext/runtimeplugin is published; then
// `go get github.com/db-branch/db-branch@<that release>`.
require (
	github.com/hashicorp/go-plugin v1.8.0
	google.golang.org/grpc v1.85.0
	google.golang.org/protobuf v1.36.12
)
