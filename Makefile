# SGLang containerised instance capacity control plane.
#
# Build with Go; Python runs bootstrap/client/provision tests. The SSH
# installer tests also require provision/requirements.txt (Paramiko).

GO ?= go
PYTHON ?= python3
BIN_DIR ?= bin
VERSION ?= dev
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_TIME ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -X github.com/tai-core/tai-talea/internal/buildinfo.Version=$(VERSION) \
           -X github.com/tai-core/tai-talea/internal/buildinfo.Commit=$(COMMIT) \
           -X github.com/tai-core/tai-talea/internal/buildinfo.BuildTime=$(BUILD_TIME)

.PHONY: all build test test-go test-bootstrap test-client test-provision vet fmt run check-config clean bootstrap-check

all: fmt vet test build

build:
	mkdir -p $(BIN_DIR)
	$(GO) build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/tai-talea ./cmd/tai-talea
	cp tools/talea.py $(BIN_DIR)/talea
	cp tools/talea_benchmark.py $(BIN_DIR)/talea_benchmark.py
	chmod +x $(BIN_DIR)/talea

# Full test suite: Go, bootstrap, client/benchmark and SSH provisioning.
test: test-go test-bootstrap test-client test-provision

test-provision:
	$(PYTHON) -m unittest discover -s provision/tests -v

test-client:
	$(PYTHON) -m unittest discover -s tools/tests -v

test-go:
	$(GO) test ./... -count=1

# The bootstrap is stdlib only, so unittest is enough.
test-bootstrap:
	$(PYTHON) -m unittest discover -s bootstrap/tests -t bootstrap -v

vet:
	$(GO) vet ./...

fmt:
	$(GO) fmt ./...

# Validate a configuration file without starting the control plane.
check-config:
	$(GO) run ./cmd/tai-talea --config $(CONFIG) --check-config

# Validate the controlled image profile inside a container.
bootstrap-check:
	$(PYTHON) -m tai_talea_bootstrap check --profile deploy/container/profile.json

run: build
	$(BIN_DIR)/tai-talea --config $(CONFIG)

clean:
	rm -rf $(BIN_DIR)
