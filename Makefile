SHELL := /usr/bin/env bash -o pipefail

GO ?= go
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
SOURCE_DATE_EPOCH ?= $(shell git log -1 --format=%ct 2>/dev/null || date +%s)
LDFLAGS := -s -w -X github.com/takodotid/tack/internal/cli.Version=$(VERSION)
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64

.PHONY: all build dist generate verify test e2e e2e-up e2e-down tools clean

all: verify test build

## build: the tack binary for this machine, in bin/
build: tools-go
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o bin/tack ./cmd/tack

## dist: static binaries for Linux and macOS, with the man pages and the CRD, in dist/
dist: tools-go
	rm -rf dist && mkdir -p dist/man
	SOURCE_DATE_EPOCH=$(SOURCE_DATE_EPOCH) $(GO) run ./hack/genman dist/man
	set -e; for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; dir=dist/tack-$(VERSION)-$$os-$$arch; \
		mkdir -p $$dir/man/man1; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $$dir/tack ./cmd/tack; \
		cp dist/man/*.1 $$dir/man/man1/; \
		cp config/crd/*.yaml LICENSE README.md $$dir/; \
		tar -C dist -czf $$dir.tar.gz $$(basename $$dir); \
	done
	cd dist && sha256sum *.tar.gz > SHA256SUMS

## generate: deepcopy code and the CRD, from api/
generate: tools-go
	$(GO) tool controller-gen object:headerFile=hack/boilerplate.go.txt paths=./api/...
	$(GO) tool controller-gen crd paths=./api/... output:crd:artifacts:config=config/crd

## verify: generated files are current, code is formatted and vetted
verify: generate
	@test -z "$$(gofmt -l .)" || { echo "gofmt needed:"; gofmt -l .; exit 1; }
	$(GO) vet ./...
	@git diff --exit-code -- api config || { echo "generated files are stale: run make generate and commit"; exit 1; }

## test: unit and golden tests
test: tools-go
	$(GO) test ./...

## e2e: the end-to-end tests on kind clusters; see test/e2e/env/README.md
e2e: tools-go
	test/e2e/run.sh

## e2e-up: create the e2e environment and leave it running
e2e-up:
	test/e2e/env/up.sh

## e2e-down: delete the e2e environment
e2e-down:
	test/e2e/env/down.sh

tools-go:
	@command -v $(GO) >/dev/null || { echo "Go is not installed or not on PATH; see README.md"; exit 1; }

clean:
	rm -rf bin dist
