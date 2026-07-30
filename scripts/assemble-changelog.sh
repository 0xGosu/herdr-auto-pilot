#!/usr/bin/env bash
# Fold the changelog fragments in changelog.d/ into CHANGELOG.md under a
# version heading, then delete them.
#
# Why fragments exist: every PR used to insert its section at the TOP of one
# shared CHANGELOG.md, so two PRs always collided on the same lines even when
# the changes were unrelated — and each had to GUESS its version number,
# because the real one is only known when the release is cut. One file per PR
# makes the conflict structurally impossible, and this script supplies the
# version at the only moment it is a fact rather than a guess.
#
# Usage: scripts/assemble-changelog.sh <version>          # e.g. 0.5.16
#
# Idempotent-ish: run it once per release. With no fragments it leaves
# CHANGELOG.md untouched and exits 0, so a release with nothing to say (a
# docs-only merge) never fails the pipeline.
set -euo pipefail

cd "$(dirname "$0")/.."

VERSION="${1:-}"
if [ -z "${VERSION}" ]; then
  echo "usage: $0 <version>   (e.g. $0 0.5.16)" >&2
  exit 2
fi
if ! printf '%s' "${VERSION}" | grep -qE '^[0-9]+\.[0-9]+\.[0-9]+$'; then
  echo "::error::version must be X.Y.Z, got '${VERSION}'" >&2
  exit 2
fi

FRAGMENT_DIR="changelog.d"
CHANGELOG="CHANGELOG.md"

# README.md documents the directory itself and is never a fragment.
#
# A read loop, not `mapfile`: that is a bash 4.0 builtin and macOS still ships
# bash 3.2, where `#!/usr/bin/env bash` resolves to it — and CONTRIBUTORS run
# this by hand on the minor/major path (see CLAUDE.md), not just CI. The
# explicit `FRAGMENTS=()` also keeps `${#FRAGMENTS[@]}` legal under `set -u` on
# 3.2. LC_ALL=C pins the sort so fragment order cannot differ between a
# contributor's locale and the runner's.
FRAGMENTS=()
while IFS= read -r f; do
  FRAGMENTS+=("${f}")
done < <(
  find "${FRAGMENT_DIR}" -maxdepth 1 -name '*.md' ! -name 'README.md' -type f 2>/dev/null | LC_ALL=C sort
)

if [ "${#FRAGMENTS[@]}" -eq 0 ]; then
  echo "no changelog fragments in ${FRAGMENT_DIR}/; leaving ${CHANGELOG} unchanged"
  exit 0
fi

if grep -qE "^## ${VERSION//./\\.}\$" "${CHANGELOG}"; then
  echo "::error::${CHANGELOG} already has a '## ${VERSION}' section" >&2
  exit 1
fi

echo "==> assembling ${#FRAGMENTS[@]} fragment(s) into ${CHANGELOG} as ${VERSION}"

SECTION="$(mktemp)"
trap 'rm -f "${SECTION}" "${SECTION}.new"' EXIT
{
  printf '## %s\n\n' "${VERSION}"
  for f in "${FRAGMENTS[@]}"; do
    echo "    + ${f}" >&2
    # Strip leading/trailing blank lines so fragment files can be written
    # with or without a trailing newline without changing the output.
    sed -e '/./,$!d' "${f}" | sed -e ':a' -e '/^\n*$/{$d;N;ba' -e '}'
  done
  printf '\n'
} >"${SECTION}"

# Insert immediately before the FIRST existing version heading, which keeps
# the file's explanatory preamble on top and the releases reverse-chronological.
# A CHANGELOG with no '## ' heading yet gets the section appended.
awk -v section_file="${SECTION}" '
  BEGIN { inserted = 0 }
  /^## / && !inserted {
    while ((getline line < section_file) > 0) print line
    close(section_file)
    inserted = 1
  }
  { print }
  END {
    if (!inserted) {
      while ((getline line < section_file) > 0) print line
      close(section_file)
    }
  }
' "${CHANGELOG}" >"${SECTION}.new"

mv "${SECTION}.new" "${CHANGELOG}"

# Post-condition before anything is deleted. awk's `getline < file` returns -1
# (not 0) when the file cannot be read, so a failed read skips the insert loop,
# still sets inserted=1, and exits 0 — the CHANGELOG would come through
# unchanged and we would then git rm every fragment, losing the entries from
# both places. Prove the section landed instead of trusting the exit status.
grep -qE "^## ${VERSION//./\\.}\$" "${CHANGELOG}" || {
  echo "::error::assembly produced no '## ${VERSION}' section; refusing to delete fragments" >&2
  exit 1
}

# git rm when the fragments are tracked (the release path), plain rm otherwise
# (a local dry run on files not yet committed).
#
# -f is required, not cosmetic: plain `git rm` REFUSES a file whose staged
# content differs from HEAD, which is every freshly-added fragment. Without it
# the script dies here having ALREADY rewritten CHANGELOG.md — a half-applied
# state with the entry duplicated in both places. We are deleting the file on
# purpose, so any staged state is irrelevant.
for f in "${FRAGMENTS[@]}"; do
  if git ls-files --error-unmatch "${f}" >/dev/null 2>&1; then
    git rm -q -f "${f}"
  else
    rm -f "${f}"
  fi
done

echo "==> ${CHANGELOG} now carries ${VERSION}; fragments removed"
