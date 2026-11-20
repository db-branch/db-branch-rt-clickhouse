# db-branch-rt-clickhouse: ClickHouse as a db-branch runtime module.
VERSION ?= 0.0.0
GOARCH ?= amd64
BIN = db-branch-rt-clickhouse
MODDIR = /usr/lib/db-branch/runtimes/clickhouse

.PHONY: build test integration conformance deb clean

build:
	CGO_ENABLED=0 GOOS=linux GOARCH=$(GOARCH) go build -trimpath -ldflags '-s -w -X main.version=$(VERSION)' -o bin/$(BIN) .

test:
	go vet ./...
	go test -cover ./...

# Against a real ClickHouse, as the current user (no root, no systemd):
#   make integration CH_BINARY=/path/to/clickhouse
integration: build
	DBB_CH_MODULE=$(CURDIR)/bin/$(BIN) DBB_CH_BINARY=$(CH_BINARY) go test -tags integration -count=1 -v -run TestRealClickHouse ./...

# The contract check of db-branch (as root on a node with db-branch installed):
#   sudo make conformance
# It installs the module under $(MODDIR) for the run and removes it afterwards.
conformance: build
	scripts/conformance.sh bin/$(BIN) $(CH_BINARY)

deb: build
	mkdir -p dist
	VERSION=$(VERSION) GOARCH=$(GOARCH) nfpm package --packager deb --config packaging/nfpm.yaml --target dist/

clean:
	rm -rf bin dist
