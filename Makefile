.PHONY: build build-hap-and-reload reinstall-hap-from-github
# SHELL := /bin/bash is required for `set -o pipefail` in *-dev targets (tee log files).
# This affects ALL Make recipes in this file — all recipes must remain bash-compatible.
SHELL := /bin/bash

# A local build needs a distinct daemon version so `daemon --ensure` can
# replace an already-running development daemon. Callers may override VERSION
# for a release build.
VERSION ?= dev-$(shell date -u +%Y%m%d%H%M%S)

build:
	@mkdir -p bin
	@case "$$(uname -s)" in \
		Linux)  rpath='-Wl,-rpath,$$ORIGIN/../lib' ;; \
		Darwin) rpath='-Wl,-rpath,@loader_path/../lib' ;; \
		*) echo "unsupported OS: $$(uname -s)" >&2; exit 1 ;; \
	esac; \
	CGO_ENABLED=1 go build -tags "vectors cpu" \
		-ldflags "-X github.com/0xGosu/herdr-auto-pilot/internal/buildinfo.Version=$(VERSION) -extldflags '$$rpath'" \
		-o bin/hap ./cmd/hap

bin/hap:
	@$(MAKE) build

build-hap-and-reload: build
	bin/hap daemon --ensure

reinstall-hap-from-github:
	herdr plugin uninstall herd-auto-prompter || true
	herdr plugin install 0xGosu/herdr-auto-pilot --yes
	hap daemon --ensure
