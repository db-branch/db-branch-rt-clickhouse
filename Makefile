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

deb:
	@if [ -z "$(SIGNING_KEY)" ] || [ -z "$(SIGNING_KEY_ID)" ]; then \
		echo "usage: make deb SIGNING_KEY=/path/to/key.pem SIGNING_KEY_ID=<key-id> [VERSION=$(VERSION)] [GOARCH=$(GOARCH)]" >&2; \
		exit 1; \
	fi
	$(MAKE) build VERSION=$(VERSION) GOARCH=$(GOARCH)
	rm -rf dist/stage
	mkdir -p dist/stage
	cp bin/$(BIN) dist/stage/$(BIN)
	python3 scripts/mkmanifest.py $(MODDIR) dist/stage $(VERSION)
	scripts/sign-manifest.sh dist/stage/manifest.json "$(SIGNING_KEY)" "$(SIGNING_KEY_ID)"
	VERSION=$(VERSION) GOARCH=$(GOARCH) nfpm package --packager deb --config packaging/nfpm.yaml --target dist/

clean:
	rm -rf bin dist
