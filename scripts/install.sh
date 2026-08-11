#!/usr/bin/env bash
# Fetch the prebuilt hap (Herd Auto Prompter) binary for this platform from GitHub
# Releases, verified against SHA256SUMS. Run by the herdr plugin [[build]]
# step with cwd = plugin root, so `herdr plugin install` needs no Go
# toolchain. Dev installs (herdr plugin link) build with Go themselves:
#   go build -o bin/hap ./cmd/hap
#
# When the requested version has no downloadable assets, this falls back to the
# newest EARLIER release that does — see "release fallback" below.
#
# macOS still ships bash 3.2 and `#!/usr/bin/env bash` resolves to it, so
# everything here must work there: no `mapfile`, no associative arrays.
set -euo pipefail

cd "$(dirname "$0")/.."
DEST="bin/hap"

fail() {
  echo "hap fetch failed: $1" >&2
  echo "to build from source instead: go build -o bin/hap ./cmd/hap" >&2
  exit 1
}

# A version passed in explicitly is a PIN: the operator named it, so a silent
# downgrade would defeat the point. Record that before applying the default.
# Set-but-EMPTY is not a pin — `HAP_VERSION=` falls through to the manifest
# below, so treating it as one would refuse a fallback in the name of a pin
# nobody made.
#
# `herdr plugin install --ref vX.Y.Z` is NOT visible here: it pins the git
# CLONE, and all this script ever sees is the manifest at whatever ref was
# checked out. In practice a --ref names an already-published release so the
# fallback never triggers, but if that release's assets are gone this WILL
# substitute an earlier one. HAP_NO_FALLBACK is the guaranteed refusal.
# (Detached HEAD is not usable as the signal: auto-release tags the bump commit
# on main, so an ordinary main install can sit exactly on a tag during the very
# window this feature exists for.)
VERSION_PINNED=""
[ -z "${HAP_VERSION:-}" ] || VERSION_PINNED="yes"

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
RELEASES="https://github.com/${SLUG}/releases/download"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
ROOT="$(pwd)"
ATTEMPT=0

# Right after a version bump lands, the release workflow may still be
# uploading assets for a couple of minutes; retry patiently (curl treats
# 404/5xx during that window as retryable with --retry-all-errors) instead
# of failing the install on the publish gap.
fetch() {
  curl -fsSL --retry 6 --retry-delay 10 --retry-all-errors -o "$1" "$2"
}

# Verify a downloaded asset against SHA256SUMS with whichever tool exists.
verify() {
  if command -v sha256sum >/dev/null 2>&1; then
    (cd "$1" && grep " $2\$" SHA256SUMS | sha256sum -c -)
  else
    (cd "$1" && grep " $2\$" SHA256SUMS | shasum -a 256 -c -)
  fi
}

# install_release <version> — download the required trio for that release,
# verify it, and only then touch the plugin directory. Returns non-zero when an
# asset could not be DOWNLOADED (the release is not published, or not published
# yet), which is the caller's cue to try an earlier release. A checksum
# mismatch is different in kind — corruption or tampering, never a publishing
# gap — so it fails hard here and is never answered with a downgrade.
#
# `set -e` does not apply inside a function called from an `if`, so every step
# whose failure must abort the attempt carries its own `|| return 1`. The two
# exceptions are deliberate: `verify` uses `|| fail` (a checksum mismatch is
# never answered with a downgrade) and `install_model` is optional and only
# warns.
install_release() {
  local ver="$1"
  local base="${RELEASES}/v${ver}"
  ATTEMPT=$((ATTEMPT + 1))
  # A fresh directory per attempt: a stale SHA256SUMS from a failed attempt
  # must never be the file a later attempt verifies against.
  local dir="${TMP}/attempt-${ATTEMPT}"
  mkdir -p "$dir" || return 1

  # Fetch and verify EVERYTHING required before installing anything. Installing
  # as we go would leave a half-swapped plugin (new binary, deleted lib/) when a
  # later asset turns out to be missing.
  echo "fetching ${base}/${ASSET}"
  fetch "${dir}/${ASSET}" "${base}/${ASSET}" || return 1
  fetch "${dir}/SHA256SUMS" "${base}/SHA256SUMS" || return 1
  echo "fetching ${base}/${NATIVE_ASSET}"
  fetch "${dir}/${NATIVE_ASSET}" "${base}/${NATIVE_ASSET}" || return 1

  verify "$dir" "${ASSET}" || fail "checksum verification failed for ${ASSET}"
  verify "$dir" "${NATIVE_ASSET}" || fail "checksum verification failed for ${NATIVE_ASSET}"

  # Native runtime libraries (FAISS + llama.cpp). The binary is dynamically
  # linked against these via an rpath of <plugin>/lib, so this is REQUIRED:
  # without it hap will not start.
  #
  # Unpack to the staging directory first. Extracting straight over lib/ means
  # `rm -rf lib` runs before tar can fail, so an out-of-space unpack would
  # leave an existing install with no libraries at all — and the error the
  # operator sees would blame the download.
  tar -xzf "${dir}/${NATIVE_ASSET}" -C "$dir" || return 1
  [ -d "${dir}/lib" ] || return 1

  mkdir -p "$(dirname "$DEST")" || return 1
  install -m 755 "${dir}/${ASSET}" "$DEST" || return 1
  echo "installed ${ASSET} v${ver} at ${DEST}"

  rm -rf lib
  mv "${dir}/lib" lib || return 1
  if [ "$OS" = "darwin" ] && command -v xattr >/dev/null 2>&1; then
    xattr -dr com.apple.quarantine lib 2>/dev/null || true
  fi
  echo "installed native libraries in lib/"

  install_model "$dir" "$base"
  return 0
}

# install_model <attempt-dir> <base-url> — the embedding model for semantic
# signature matching (25MB, shared across platforms). OPTIONAL: without it hap
# degrades to BM25 text matching, so a failed model download warns instead of
# failing the plugin install.
install_model() {
  local dir="$1"
  local base="$2"
  mkdir -p models
  if model_ok "$dir"; then
    echo "embedding model already present (checksum ok); skipping download"
  elif curl -fsSL --retry 3 --retry-delay 5 -o "${dir}/${MODEL_FILE}" "${base}/${MODEL_FILE}" &&
    verify "$dir" "${MODEL_FILE}"; then
    install -m 644 "${dir}/${MODEL_FILE}" "models/${MODEL_FILE}"
    echo "installed embedding model at models/${MODEL_FILE}"
  else
    echo "warning: embedding model download failed; semantic matching will fall back to text search" >&2
    echo "         retry later with: bash scripts/install.sh" >&2
  fi
}

model_ok() {
  [ -f "${ROOT}/models/${MODEL_FILE}" ] || return 1
  (cd "$1" && sed "s# ${MODEL_FILE}\$# ${ROOT}/models/${MODEL_FILE}#" SHA256SUMS |
    grep "models/${MODEL_FILE}\$" |
    if command -v sha256sum >/dev/null 2>&1; then sha256sum -c - >/dev/null 2>&1; else shasum -a 256 -c - >/dev/null 2>&1; fi)
}

# ---------------------------------------------------------------- release fallback
#
# The manifest version is supposed to TRAIL releases, but auto-release breaks
# that for a window: the bump PR writes the next version into
# herdr-plugin.toml and tags it, then release.yml spends ~15 minutes building on
# three native runners — and can fail outright. Until those assets exist, every
# `herdr plugin install` and `hap update` off main 404s. The curl retry above
# covers ~60s, which bridges the post-publish UPLOAD gap, not the build.
#
# So when the requested release has no assets, install the newest EARLIER
# release that does. This is deliberately automatic and never prompts: the
# script runs as herdr's [[build]] step and `hap update` runs it with no stdin,
# so there is nobody to ask. It self-heals — the TUI's release check sees the
# intended version once it publishes and offers `hap update`.

# fallback_candidates <version> — released versions strictly BELOW $1, newest
# first. Strictly-below is what excludes the failing tag itself.
#
# Tags, not the releases API: the API is rate-limited per IP, and
# /releases/latest can name the very release that is still building. A tag is
# also cheap and usually already local, since plugins are installed by clone.
fallback_candidates() {
  local want="$1"
  local tags
  tags="$(git tag --list 'v*' 2>/dev/null || true)"
  # A shallow or tagless clone has none locally; ask the remote.
  if [ -z "$tags" ]; then
    tags="$(git ls-remote --tags origin 'v*' 2>/dev/null |
      sed -n 's#.*refs/tags/\(v[0-9][^^]*\)$#\1#p' || true)"
  fi
  echo "$tags" |
    sed -n 's/^v\([0-9][0-9]*\.[0-9][0-9]*\.[0-9][0-9]*\)$/\1/p' |
    # Compare field by field rather than packing the parts into one number:
    # any packing picks a radix, and a component that outgrows it silently
    # reorders releases (with a 1000-radix, 0.9.1001 outranks 0.10.0).
    awk -v want="$want" '
      function vcmp(a, b,   x, y, i) {
        split(a, x, "."); split(b, y, ".")
        for (i = 1; i <= 3; i++) {
          if (x[i] + 0 != y[i] + 0) return (x[i] + 0 < y[i] + 0) ? -1 : 1
        }
        return 0
      }
      vcmp($0, want) < 0 { print }
    ' |
    # macOS `sort` predates `-V`, so compare the fields numerically instead.
    # The `r` must be ON EACH KEY: a key carrying its own ordering options
    # ignores the global `-r`, which silently yields OLDEST-first — the exact
    # opposite of what a fallback wants.
    sort -t. -k1,1nr -k2,2nr -k3,3nr |
    # Bounded so a dead network ends the walk quickly rather than probing
    # every tag this repo has ever had. Not tunable: an operator who wants a
    # specific older release names it with HAP_VERSION.
    head -5
}

# release_available <version> — is every REQUIRED asset downloadable? A HEAD is
# cheap, so this filters candidates without spending the retry budget on each.
# No -L: a 302 to the CDN already proves the asset exists.
#
# Only rc 22 (HTTP >=400) really means "not published". A timeout or a refused
# connection is also non-zero and will skip a candidate that does have assets —
# accepted: on a broken network every candidate skips and the walk ends in the
# same fail() as before, within the cap.
release_available() {
  local base="${RELEASES}/v$1"
  local name
  for name in "${ASSET}" "SHA256SUMS" "${NATIVE_ASSET}"; do
    curl -fsI --max-time 15 -o /dev/null "${base}/${name}" || return 1
  done
  return 0
}

# Note the asymmetry: the REQUESTED version is attempted with the full ~60s
# curl retry, never HEAD-probed first. Probing would make the common failure
# fast, but during the post-publish upload gap a probe 404s on an asset that is
# seconds from existing — and substituting an older release there would be
# worse than the wait. Only the CANDIDATES are probed, where a miss means the
# release is genuinely absent.
INSTALLED=""
if install_release "$VERSION"; then
  INSTALLED="$VERSION"
elif [ -n "$VERSION_PINNED" ]; then
  fail "download failed for the pinned version v${VERSION}
(HAP_VERSION names an exact release, so no earlier release was substituted)"
elif [ -n "${HAP_NO_FALLBACK:-}" ]; then
  # ANY non-empty value, not just "1": an escape hatch that quietly ignores
  # HAP_NO_FALLBACK=true would fail open, which is the wrong way for an opt-out
  # to break.
  fail "download failed for v${VERSION}
(HAP_NO_FALLBACK is set, so no earlier release was substituted)"
else
  echo "v${VERSION} is not downloadable yet; looking for an earlier release" >&2
  for candidate in $(fallback_candidates "$VERSION"); do
    release_available "$candidate" || continue
    if install_release "$candidate"; then
      INSTALLED="$candidate"
      break
    fi
  done
  [ -n "$INSTALLED" ] || fail "download failed for v${VERSION}, and no earlier release could be installed either
(if the v${VERSION} release was published in the last few minutes, its assets may still be uploading — retry shortly)"
  echo "" >&2
  echo "warning: installed hap v${INSTALLED}, NOT the v${VERSION} this plugin declares." >&2
  echo "         v${VERSION}'s release assets are not published yet — its build takes" >&2
  echo "         ~15 minutes after the version bump lands, and it may have failed." >&2
  echo "         Run 'hap update' once v${VERSION} publishes; the TUI header will say so." >&2
  echo "         Set HAP_NO_FALLBACK=1 to fail instead of substituting a release." >&2
  echo "" >&2
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
