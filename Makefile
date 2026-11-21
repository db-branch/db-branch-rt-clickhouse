# db-branch-rt-clickhouse: ClickHouse as a db-branch runtime module.
VERSION_SET := $(VERSION)
VERSION ?= 0.0.0
GOARCH ?= amd64
BIN = db-branch-rt-clickhouse
MODDIR = /usr/lib/db-branch/runtimes/clickhouse

.PHONY: build test integration conformance deb release clean

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

release:
	@if [ -z "$(VERSION_SET)" ] || [ -z "$(SIGNING_KEY)" ] || [ -z "$(SIGNING_KEY_ID)" ]; then \
		echo "usage: make release VERSION=<semver> SIGNING_KEY=/path/to/key.pem SIGNING_KEY_ID=<key-id>" >&2; \
		exit 1; \
	fi
	rm -rf dist/release
	mkdir -p dist/release
	SIGNING_KEY="$(SIGNING_KEY)" SIGNING_KEY_ID="$(SIGNING_KEY_ID)" scripts/release-asset.sh "$(VERSION)" amd64 dist/release
	SIGNING_KEY="$(SIGNING_KEY)" SIGNING_KEY_ID="$(SIGNING_KEY_ID)" scripts/release-asset.sh "$(VERSION)" arm64 dist/release
	@cd dist/release && \
	if command -v sha256sum >/dev/null 2>&1; then \
		sha256sum "$(BIN)_$(VERSION)_linux_amd64.tar.gz" "$(BIN)_$(VERSION)_linux_arm64.tar.gz" > SHA256SUMS; \
	elif command -v shasum >/dev/null 2>&1; then \
		shasum -a 256 "$(BIN)_$(VERSION)_linux_amd64.tar.gz" "$(BIN)_$(VERSION)_linux_arm64.tar.gz" > SHA256SUMS; \
	else \
		echo "error: neither sha256sum nor shasum found" >&2; \
		exit 1; \
	fi
	@fp=$$(python3 -c 'import base64, hashlib, sys; raw = base64.b64decode(open(sys.argv[1]).read().strip()); assert len(raw) == 32; print("sha256:" + hashlib.sha256(raw).hexdigest())' "dist/stage-amd64/$(SIGNING_KEY_ID).pub") && \
	echo "Public key fingerprint ($(SIGNING_KEY_ID)): $$fp"

clean:
	rm -rf bin dist
