#!/usr/bin/env bash
# Devcontainer post-create setup for hap development.
#
# Runs once after the container is created (invoked from
# .devcontainer/devcontainer.json's postCreateCommand). Keep every step
# idempotent — a rebuilt container re-runs this.
#
#   - libopenblas0: FAISS's runtime BLAS backend. The CGO-linked hap binary
#     and `go test -tags "vectors cpu"` load libfaiss.so, which needs
#     libopenblas.so.0 at startup — without it the binary fails with
#     "error while loading shared libraries: libopenblas.so.0". (Building the
#     native libs from source via scripts/setup-native.sh installs the fuller
#     libopenblas-dev; this covers just running a prebuilt/linked binary.)
#   - golangci-lint: pin the development linter to the same Go 1.25-compatible
#     release used by CI.
set -euo pipefail

# Only apt-based images have these packages; skip quietly elsewhere.
if command -v apt-get >/dev/null 2>&1; then
  SUDO=""
  [ "$(id -u)" -eq 0 ] || SUDO="sudo"
  echo "==> installing libopenblas0 (FAISS runtime BLAS backend)"
  $SUDO apt-get update -qq
  $SUDO apt-get install -y -qq libopenblas0
fi

GOLANGCI_LINT_VERSION="v2.12.2"
GOLANGCI_LINT_BIN_DIR="$(go env GOPATH)/bin"
GOLANGCI_LINT_BIN="$GOLANGCI_LINT_BIN_DIR/golangci-lint"

if [ ! -x "$GOLANGCI_LINT_BIN" ] ||
  ! "$GOLANGCI_LINT_BIN" version 2>/dev/null | grep -Fq "has version ${GOLANGCI_LINT_VERSION#v} "; then
  echo "==> installing golangci-lint $GOLANGCI_LINT_VERSION"
  mkdir -p "$GOLANGCI_LINT_BIN_DIR"
  curl -sSfL https://golangci-lint.run/install.sh |
    sh -s -- -b "$GOLANGCI_LINT_BIN_DIR" "$GOLANGCI_LINT_VERSION"
fi

echo "devcontainer post-create setup complete"
