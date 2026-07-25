#!/usr/bin/env bash
# Fetch the prebuilt hap (Herd Auto Prompter) binary for this platform from GitHub
# Releases, verified against SHA256SUMS. Run by the herdr plugin [[build]]
# step with cwd = plugin root, so `herdr plugin install` needs no Go
# toolchain. Dev installs (herdr plugin link) build with Go themselves:
#   go build -o bin/hap ./cmd/hap
set -euo pipefail

cd "$(dirname "$0")/.."
DEST="bin/hap"

fail() {
  echo "hap fetch failed: $1" >&2
  echo "to build from source instead: go build -o bin/hap ./cmd/hap" >&2
  exit 1
}

VERSION="${HAP_VERSION:-$(sed -n 's/^version = "\(.*\)"/\1/p' herdr-plugin.toml | head -1)}"
[ -n "$VERSION" ] || fail "cannot read version from herdr-plugin.toml"

# owner/repo from the git remote (plugins are installed by git clone)
SLUG="$(git config --get remote.origin.url 2>/dev/null |
  sed -n 's#.*[:/]\([^/]*/[^/]*\)\.git$#\1#p; s#.*[:/]\([^/]*/[^/]*\)$#\1#p' | head -1)"
[ -n "$SLUG" ] || fail "cannot derive owner/repo from the git remote"

case "$(uname -s)" in
  Darwin) OS="darwin" ;;
  Linux) OS="linux" ;;
  *) fail "unsupported OS: $(uname -s)" ;;
esac
case "$(uname -m)" in
  arm64 | aarch64) ARCH="arm64" ;;
  x86_64 | amd64) ARCH="amd64" ;;
  *) fail "unsupported architecture: $(uname -m)" ;;
esac
# No Intel-macOS release assets are published: Apple Silicon only.
if [ "$OS" = "darwin" ] && [ "$ARCH" = "amd64" ]; then
  fail "Intel macOS (darwin-amd64) is not supported; hap ships Apple Silicon (arm64) builds only"
fi
ASSET="hap-${OS}-${ARCH}"
NATIVE_ASSET="hap-native-${OS}-${ARCH}.tar.gz"
MODEL_FILE="all-minilm-l6-v2-q8_0.gguf"
BASE="https://github.com/${SLUG}/releases/download/v${VERSION}"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# Right after a version bump lands, the release workflow may still be
# uploading assets for a couple of minutes; retry patiently (curl treats
# 404/5xx during that window as retryable with --retry-all-errors) instead
# of failing the install on the publish gap.
fetch() {
  curl -fsSL --retry 6 --retry-delay 10 --retry-all-errors -o "$1" "$2" ||
    fail "download failed: $2
(if the v${VERSION} release was published in the last few minutes, its assets may still be uploading — retry shortly)"
}
echo "fetching ${BASE}/${ASSET}"
fetch "${TMP}/${ASSET}" "${BASE}/${ASSET}"
fetch "${TMP}/SHA256SUMS" "${BASE}/SHA256SUMS"

# Verify a downloaded asset against SHA256SUMS with whichever tool exists.
verify() {
  if command -v sha256sum >/dev/null 2>&1; then
    (cd "$TMP" && grep " $1\$" SHA256SUMS | sha256sum -c -)
  else
    (cd "$TMP" && grep " $1\$" SHA256SUMS | shasum -a 256 -c -)
  fi
}
verify "${ASSET}" || fail "checksum verification failed for ${ASSET}"

mkdir -p "$(dirname "$DEST")"
install -m 755 "${TMP}/${ASSET}" "$DEST"
echo "installed ${ASSET} v${VERSION} at ${DEST}"

# Native runtime libraries (FAISS + llama.cpp). The binary is dynamically
# linked against these via an rpath of <plugin>/lib, so this is REQUIRED:
# without it hap will not start.
echo "fetching ${BASE}/${NATIVE_ASSET}"
fetch "${TMP}/${NATIVE_ASSET}" "${BASE}/${NATIVE_ASSET}"
verify "${NATIVE_ASSET}" || fail "checksum verification failed for ${NATIVE_ASSET}"
rm -rf lib
tar -xzf "${TMP}/${NATIVE_ASSET}"
if [ "$OS" = "darwin" ] && command -v xattr >/dev/null 2>&1; then
  xattr -dr com.apple.quarantine lib 2>/dev/null || true
fi
echo "installed native libraries in lib/"

# Embedding model for semantic signature matching (25MB, shared across
# platforms). OPTIONAL: without it hap degrades to BM25 text matching, so a
# failed model download warns instead of failing the plugin install.
mkdir -p models
ROOT="$(pwd)"
model_ok() {
  [ -f "${ROOT}/models/${MODEL_FILE}" ] || return 1
  (cd "$TMP" && sed "s# ${MODEL_FILE}\$# ${ROOT}/models/${MODEL_FILE}#" SHA256SUMS |
    grep "models/${MODEL_FILE}\$" |
    if command -v sha256sum >/dev/null 2>&1; then sha256sum -c - >/dev/null 2>&1; else shasum -a 256 -c - >/dev/null 2>&1; fi)
}
if model_ok; then
  echo "embedding model already present (checksum ok); skipping download"
elif curl -fsSL --retry 3 --retry-delay 5 -o "${TMP}/${MODEL_FILE}" "${BASE}/${MODEL_FILE}" &&
  verify "${MODEL_FILE}"; then
  install -m 644 "${TMP}/${MODEL_FILE}" "models/${MODEL_FILE}"
  echo "installed embedding model at models/${MODEL_FILE}"
else
  echo "warning: embedding model download failed; semantic matching will fall back to text search" >&2
  echo "         retry later with: bash scripts/install.sh" >&2
fi

# Hand a RUNNING daemon over to the binary we just installed. An upgrade puts
# the new release in a new directory and unlinks the old one, but the live
# daemon keeps running from the removed path — and every child it spawns from
# there (the MCP server the LLM CLI launches, the embed worker) then fails, so
# consults silently come back empty. Without this the swap waited for herdr's
# next pane.agent_detected / workspace.created event, which may be hours away.
#
# --replace-only never STARTS a daemon: a fresh install (and any CI run of this
# script) must not bring one up as a side effect of building the plugin.
#
# This runs inside herdr's [[build]] step, so herdr may not have registered the
# new plugin directory yet — which is fine: the daemon we start is spawned from
# THIS binary's own path (it exists, so hap's self-resolution never reaches the
# registry lookup). Best-effort — a plugin install must not fail because the
# handover did not take; `hap daemon --ensure` still fixes it.
if "./${DEST}" daemon --ensure --replace-only; then
  echo "handed a running daemon over to the new binary (if one was running)"
else
  echo "note: could not hand over a running daemon; run 'hap daemon --ensure' if hap was already running" >&2
fi
