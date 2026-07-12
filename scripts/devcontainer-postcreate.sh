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
set -euo pipefail

# Only apt-based images have these packages; skip quietly elsewhere.
if command -v apt-get >/dev/null 2>&1; then
  SUDO=""
  [ "$(id -u)" -eq 0 ] || SUDO="sudo"
  echo "==> installing libopenblas0 (FAISS runtime BLAS backend)"
  $SUDO apt-get update -qq
  $SUDO apt-get install -y -qq libopenblas0
fi

echo "devcontainer post-create setup complete"
