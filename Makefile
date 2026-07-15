.PHONY: native-deps build lint test test-race test-corpus check build-hap-and-reload reinstall-hap-from-github
# SHELL := /bin/bash is required for `set -o pipefail` in *-dev targets (tee log files).
# This affects ALL Make recipes in this file — all recipes must remain bash-compatible.
SHELL := /bin/bash

# A local build needs a distinct daemon version so `daemon --ensure` can
# replace an already-running development daemon. Callers may override VERSION
# for a release build.
VERSION ?= dev-$(shell date -u +%Y%m%d%H%M%S)
GO_BUILD_TAGS ?= vectors cpu
GOLANGCI_BUILD_TAGS ?= vectors,cpu
GO_TEST_TIMEOUT ?= 15m
RACE_PACKAGES := ./internal/store/... ./internal/domain/... ./internal/control/... ./internal/embedder/... ./internal/match/... ./internal/daemon/...

native-deps:
	bash scripts/setup-native.sh

build:
	@mkdir -p bin
	@case "$$(uname -s)" in \
		Linux)  rpath='-Wl,-rpath,$$ORIGIN/../lib' ;; \
		Darwin) rpath='-Wl,-rpath,@loader_path/../lib' ;; \
		*) echo "unsupported OS: $$(uname -s)" >&2; exit 1 ;; \
	esac; \
	CGO_ENABLED=1 go build -tags "$(GO_BUILD_TAGS)" \
		-ldflags "-X github.com/0xGosu/herdr-auto-pilot/internal/buildinfo.Version=$(VERSION) -extldflags '$$rpath'" \
		-o bin/hap ./cmd/hap

lint: native-deps
	@out="$$(gofmt -l . | grep -v '^submodule/' || true)"; \
	if [ -n "$$out" ]; then \
		echo "gofmt violations:"; \
		echo "$$out"; \
		exit 1; \
	fi
	go vet -tags "$(GO_BUILD_TAGS)" ./...
	golangci-lint run --build-tags "$(GOLANGCI_BUILD_TAGS)"

test: native-deps
	go build -tags "$(GO_BUILD_TAGS)" ./...
	go test -tags "$(GO_BUILD_TAGS)" ./... -count=1 -timeout "$(GO_TEST_TIMEOUT)"

test-race: native-deps
	go test -tags "$(GO_BUILD_TAGS)" $(RACE_PACKAGES) -race -count=1 -timeout "$(GO_TEST_TIMEOUT)"

test-corpus:
	@set -o pipefail; \
	out="$$(mktemp)"; \
	trap 'rm -f "$$out"' EXIT; \
	go test ./internal/domain/ -run 'TestSeedNeverAutoCatchesCorpus$$' -v -count=1 | tee "$$out"; \
	grep -q -- '--- PASS: TestSeedNeverAutoCatchesCorpus' "$$out" || { \
		echo "corpus gate did not execute TestSeedNeverAutoCatchesCorpus" >&2; \
		exit 1; \
	}

check: lint test test-race test-corpus

bin/hap:
	@$(MAKE) build

build-hap-and-reload: build
	bin/hap daemon --ensure

reinstall-hap-from-github:
	herdr plugin uninstall herd-auto-prompter || true
	herdr plugin install 0xGosu/herdr-auto-pilot --yes
	hap daemon --ensure
