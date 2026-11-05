SHELL := /usr/bin/env bash -o pipefail

GO ?= go
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
SOURCE_DATE_EPOCH ?= $(shell git log -1 --format=%ct 2>/dev/null || date +%s)
LDFLAGS := -s -w -X github.com/takodotid/tack/internal/version.Version=$(VERSION)
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64

.PHONY: all build dist sbom lint generate verify test e2e e2e-up e2e-down tools clean

all: verify test build

## build: the tack binary for this machine, in bin/
build: tools-go
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o bin/tack ./cmd/tack

## dist: static binaries for Linux, macOS and Windows, with the man pages and the CRD, in dist/; the same bytes for the same commit; lists each archive to check it
dist: tools-go
	rm -rf dist && mkdir -p dist/man
	SOURCE_DATE_EPOCH=$(SOURCE_DATE_EPOCH) $(GO) run -ldflags '$(LDFLAGS)' ./hack/genman dist/man
	set -e; for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; dir=dist/tack-$(VERSION)-$$os-$$arch; \
		case $$os in windows) exe=.exe;; *) exe=;; esac; \
		mkdir -p $$dir/man/man1; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $$dir/tack$$exe ./cmd/tack; \
		cp dist/man/*.1 $$dir/man/man1/; \
		cp config/crd/*.yaml LICENSE README.md $$dir/; \
		case $$os in windows) ext=.zip;; *) ext=.tar.gz;; esac; \
		SOURCE_DATE_EPOCH=$(SOURCE_DATE_EPOCH) $(GO) run ./hack/archive $$dir$$ext $$dir; \
	done
	cd dist && sha256sum *.tar.gz *.zip > SHA256SUMS
	SOURCE_DATE_EPOCH=$(SOURCE_DATE_EPOCH) $(GO) run ./hack/distcheck dist $(VERSION) $(PLATFORMS)

## sbom: after make dist, a CycloneDX SBOM per archive, from the build information of its binary, in dist/; the SBOM names the tool by its hashes, so the tool is built with -trimpath
sbom: tools-go
	$(GO) build -trimpath -modfile=hack/tools/go.mod -o bin/cyclonedx-gomod github.com/CycloneDX/cyclonedx-gomod/cmd/cyclonedx-gomod
	set -e; for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; dir=dist/tack-$(VERSION)-$$os-$$arch; \
		case $$os in windows) exe=.exe;; *) exe=;; esac; \
		GOOS=$$os GOARCH=$$arch bin/cyclonedx-gomod bin -json -noserial -notimestamp \
			-version $(VERSION) -output $$dir.cdx.json $$dir/tack$$exe; \
	done

## lint: the module's checksums, known vulnerabilities, the chart, and the pins of the workflows and the e2e
lint: tools-go
	$(GO) mod verify
	$(GO) tool -modfile=hack/tools/go.mod govulncheck ./...
	helm lint --strict deploy/helm/tack
	hack/release/pinned.sh

## generate: deepcopy code and the CRD, from api/, and the chart's copy of the CRD
generate: tools-go
	$(GO) tool controller-gen object:headerFile=hack/boilerplate.go.txt paths=./api/...
	$(GO) tool controller-gen crd paths=./api/... output:crd:artifacts:config=config/crd
	$(GO) run ./hack/crdversions config/crd/failover.tako.id_sitefailovers.yaml
	rm -rf deploy/helm/tack/files/crds && mkdir -p deploy/helm/tack/files/crds && cp config/crd/*.yaml deploy/helm/tack/files/crds/

## verify: generated files are current, code is formatted and vetted
verify: generate
	@test -z "$$(gofmt -l .)" || { echo "gofmt needed:"; gofmt -l .; exit 1; }
	$(GO) vet ./...
	@git diff --exit-code -- api config deploy/helm/tack/files || { echo "generated files are stale: run make generate and commit"; exit 1; }

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
