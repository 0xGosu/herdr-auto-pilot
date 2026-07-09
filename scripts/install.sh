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
ASSET="hap-${OS}-${ARCH}"
BASE="https://github.com/${SLUG}/releases/download/v${VERSION}"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
echo "fetching ${BASE}/${ASSET}"
curl -fsSL --retry 2 -o "${TMP}/${ASSET}" "${BASE}/${ASSET}" || fail "download failed: ${BASE}/${ASSET}"
curl -fsSL --retry 2 -o "${TMP}/SHA256SUMS" "${BASE}/SHA256SUMS" || fail "download failed: ${BASE}/SHA256SUMS"

# Verify the checksum with whichever tool this platform has.
if command -v sha256sum >/dev/null 2>&1; then
  (cd "$TMP" && grep " ${ASSET}\$" SHA256SUMS | sha256sum -c -) || fail "checksum verification failed for ${ASSET}"
else
  (cd "$TMP" && grep " ${ASSET}\$" SHA256SUMS | shasum -a 256 -c -) || fail "checksum verification failed for ${ASSET}"
fi

mkdir -p "$(dirname "$DEST")"
install -m 755 "${TMP}/${ASSET}" "$DEST"
echo "installed ${ASSET} v${VERSION} at ${DEST}"
