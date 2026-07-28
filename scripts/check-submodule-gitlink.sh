#!/usr/bin/env bash
# Guard: this repository's submodules must be recorded as GITLINKS (mode
# 160000) — never as a symlink (120000), never as their contents committed
# as ordinary files, and never missing.
#
# Why this exists: a worktree that wants the prebuilt native archives
# symlinks submodule/github.com/seed-hypermedia/llama-go at a sibling
# checkout. `git add` on that path rewrites the tree entry from a gitlink to
# a symlink. Everything still builds for whoever made it — their symlink
# resolves — but a fresh clone gets a dangling link, `git submodule update`
# has nothing to check out, and every CI job dies deep inside
# scripts/setup-native.sh with a linker error that names neither git nor
# submodules. That broke all of CI twice in #265. This check takes seconds
# and runs first, so the failure names itself.
#
# Usage: bash scripts/check-submodule-gitlink.sh [ref]   (ref defaults to HEAD)
#
# Checks BOTH the ref's tree and the index. The index leg is what a local
# `make check` can still catch — before the bad commit exists.
set -euo pipefail

REF="${1:-HEAD}"
GITLINK_MODE="160000"
TAB="$(printf '\t')"

# Submodules the build cannot do without. Hardcoded deliberately: deriving
# the list from .gitmodules alone would let a commit that DELETES the
# .gitmodules entry pass with "nothing to check" — the same broken clone,
# with no warning at all.
REQUIRED_PATHS="submodule/github.com/seed-hypermedia/llama-go"

# Every tracked entry under these roots must be a gitlink. This catches a
# submodule this script has never been told about, which is why adding one
# needs no edit here — the repo keeps them all under
# submodule/<host>/<org>/<repo>.
SWEEP_ROOTS="submodule"

root="$(git rev-parse --show-toplevel)" || {
  echo "check-submodule-gitlink: not inside a git working tree" >&2
  exit 2
}
cd "$root"

# core.quotePath=false keeps a non-ASCII path raw instead of C-quoted, so
# the prefix tests below still match it.
ls_tree() { git -c core.quotePath=false ls-tree -r --full-tree "$@"; }
ls_index() { git -c core.quotePath=false ls-files --stage "$@"; }

# `git ls-tree` and `git ls-files` exit 0 on a pathspec that matches
# nothing, so an absent entry reads as an empty mode rather than aborting.
tree_mode() {
  git -c core.quotePath=false ls-tree --full-tree "${REF}" -- "$1" | awk '{print $1}'
}

tree_sha() {
  git -c core.quotePath=false ls-tree --full-tree "${REF}" -- "$1" | awk 'NR==1 {print $3}'
}

index_mode() {
  # A submodule whose contents were committed as ordinary files stages many
  # entries; collapse to the distinct modes so that case is reported too.
  ls_index -- "$1" | awk '{print $1}' | sort -u | tr '\n' ' ' | sed 's/ *$//'
}

index_sha() {
  ls_index -- "$1" | awk 'NR==1 {print $2}'
}

describe_mode() {
  case "$1" in
    160000) echo "a gitlink (submodule)" ;;
    120000) echo "a symlink" ;;
    100644 | 100755) echo "a regular file" ;;
    040000 | 40000) echo "an ordinary directory of files" ;;
    "") echo "absent" ;;
    *) echo "mode $1" ;;
  esac
}

# `submodule.<KEY>.ignore` is keyed by the submodule NAME, not its path.
# They coincide in this repo, but the printed advice must be correct even
# when they don't.
submodule_name_for_path() {
  local path="$1" line
  [ -f .gitmodules ] || { printf '%s' "$path"; return; }
  line="$(git config -f .gitmodules --get-regexp '^submodule\..*\.path$' 2>/dev/null |
    awk -v p="$path" '$2 == p {print $1; exit}')" || true
  if [ -n "$line" ]; then
    # submodule.<name>.path -> <name>
    line="${line#submodule.}"
    printf '%s' "${line%.path}"
  else
    printf '%s' "$path"
  fi
}

reported=" "

fail() {
  local path="$1" where="$2" mode="$3" cause remove name

  case "$mode" in
    120000)
      cause="The submodule directory was replaced by a SYMLINK and committed — the
  local worktree trick for reusing another checkout's prebuilt native archives."
      remove='git rm --cached -- "PATH" && rm -f -- "PATH"'
      ;;
    "")
      cause="The submodule has no entry here at all, so nothing checks it out."
      remove='# nothing tracked to remove'
      ;;
    *)
      cause="The submodule's contents were committed as ordinary files instead of a
  gitlink, so git tracks a stale copy that no submodule update can refresh."
      remove='git rm -r --cached -- "PATH" && rm -rf -- "PATH"'
      ;;
  esac
  remove="${remove//PATH/${path}}"
  name="$(submodule_name_for_path "${path}")"

  cat >&2 <<EOF

✖ submodule gitlink is broken: ${path}
  ${where} records it as $(describe_mode "${mode}"), expected $(describe_mode "${GITLINK_MODE}") (${GITLINK_MODE}).

  ${cause}
  A fresh clone cannot materialize it, so scripts/setup-native.sh fails to
  link and every CI job dies with an unrelated-looking error (#265).

  Fix it:
    1. ${remove}
    2. Restore the gitlink from a commit that still has one. VERIFY the
       source first — a ref that carries the bug names a blob, not a commit:
         git ls-tree <good-commit> -- "${path}"   # must print: ${GITLINK_MODE} commit <sha>
         git checkout <good-commit> -- "${path}"
    3. git submodule update --init --recursive -- "${path}"

  Do NOT repair this with 'git update-index --cacheinfo ${GITLINK_MODE},<sha>,<path>':
  it accepts any object id without checking its type, so a sha copied from a
  broken ref yields a ${GITLINK_MODE} entry pointing at the symlink blob. The clone
  stays just as broken (this guard rejects that shape too).

  Verify the repair:
    git ls-tree ${REF} -- "${path}"   # must start with ${GITLINK_MODE}

  Then stop it recurring in that worktree:
    git config submodule."${name}".ignore all
  (that keeps 'git add -A' off it; an explicit 'git add ${path}' still
  records the symlink, so never run one)
EOF
}

# A ${GITLINK_MODE} entry proves nothing about the OBJECT it names: cacheinfo
# accepts any id without checking its type, so "repairing" a symlink by
# re-staging the blob sha as a gitlink yields an entry that looks right and
# still cannot be checked out. This is the one shape the mode comparison
# cannot see.
fail_object() {
  local path="$1" where="$2" sha="$3" type="$4" name
  name="$(submodule_name_for_path "${path}")"
  cat >&2 <<EOF

✖ submodule gitlink names the wrong kind of object: ${path}
  ${where} has mode ${GITLINK_MODE}, but ${sha} is a ${type}, not a commit.

  That is what 'git update-index --cacheinfo ${GITLINK_MODE},<sha>,<path>' produces when
  the sha came from a broken ref (the symlink's blob). The entry passes a
  mode check while 'git submodule update' still fails with
  "reference is not a tree".

  Fix it by taking the gitlink from a commit that has a real one:
    git ls-tree <good-commit> -- "${path}"   # must print: ${GITLINK_MODE} commit <sha>
    git checkout <good-commit> -- "${path}"
    git submodule update --init --recursive -- "${path}"

  Then stop it recurring in that worktree:
    git config submodule."${name}".ignore all
EOF
}

# A gitlink with no .gitmodules mapping cannot be fetched: the checkout
# leaves an empty directory and setup-native.sh has nothing to build.
fail_mapping() {
  local path="$1" name="$2"
  cat >&2 <<EOF

✖ submodule has no .gitmodules mapping: ${path}
  The gitlink is intact, but .gitmodules declares no url for it, so
  'git submodule update --init' cannot fetch it and a fresh clone leaves the
  directory empty — scripts/setup-native.sh then fails with a linker error.

  Fix it by restoring the block in .gitmodules:
    [submodule "${name}"]
        path = ${path}
        url = <upstream url>
  then: git submodule update --init --recursive -- "${path}"
EOF
}

status=0

# Report each path once PER PLACE: an operator who broke both HEAD and the
# index should learn about both in one run, but a required path that the
# sweep also sees must not print twice.
report() {
  local path="$1" where="$2"
  case "${reported}" in
    *" ${path}|${where} "*) return ;;
  esac
  reported="${reported}${path}|${where} "
  fail "$@"
  status=1
}

report_object() {
  local path="$1" where="$2"
  case "${reported}" in
    *" ${path}|${where} "*) return ;;
  esac
  reported="${reported}${path}|${where} "
  fail_object "$@"
  status=1
}

have_ref=1
git rev-parse --verify --quiet "${REF}^{commit}" >/dev/null || have_ref=0
if [ "${have_ref}" = 0 ]; then
  echo "check-submodule-gitlink: ${REF} is not a commit — checking the index only" >&2
fi

# The submodule paths this run knows about: the required ones plus whatever
# .gitmodules declares. Required paths must EXIST as gitlinks; a merely
# declared one is only checked if it is tracked, so pointing this script at
# an older ref does not fail over a submodule added later.
declared_paths=""
if [ -f .gitmodules ]; then
  # `--get-regexp` prints "<key> <value>" space-separated, so a path (or a
  # submodule name) containing whitespace cannot be parsed here. Rather than
  # silently skipping it — leaving that submodule unguarded — say so and fail.
  declared_paths="$(git config -f .gitmodules --get-regexp '^submodule\..*\.path$' 2>/dev/null |
    awk 'NF != 2 { print "UNPARSEABLE"; exit } { print $2 }')" || true
  case "${declared_paths}" in
    *UNPARSEABLE*)
      echo "check-submodule-gitlink: .gitmodules has a submodule name or path containing" >&2
      echo "  whitespace, which this guard cannot parse. Rename it, or add the path to" >&2
      echo "  REQUIRED_PATHS in $0 so it is still checked." >&2
      exit 1
      ;;
  esac
fi

# A 160000 entry can still name a blob (see fail_object). Only a KNOWN object
# of the wrong type is a failure: a healthy submodule commit normally lives in
# the submodule's own object store, so `cat-file -t` not resolving it here is
# the expected case and must pass.
check_gitlink_object() {
  local path="$1" where="$2" sha="$3" type
  [ -n "${sha}" ] || return 0
  type="$(git cat-file -t "${sha}" 2>/dev/null)" || return 0
  if [ "${type}" != commit ]; then
    report_object "${path}" "${where}" "${sha}" "${type}"
  fi
}

check_path() {
  local path="$1" required="$2" mode
  path_ok=1
  if [ "${have_ref}" = 1 ]; then
    mode="$(tree_mode "${path}")"
    if [ -z "${mode}" ] && [ "${required}" = no ]; then
      : # declared but not tracked at this ref — not this guard's business
    elif [ "${mode}" != "${GITLINK_MODE}" ]; then
      report "${path}" "${REF}" "${mode}"
      path_ok=0
    else
      check_gitlink_object "${path}" "${REF}" "$(tree_sha "${path}")"
    fi
  fi
  mode="$(index_mode "${path}")"
  if [ -z "${mode}" ] && [ "${required}" = no ]; then
    return
  fi
  if [ "${mode}" != "${GITLINK_MODE}" ]; then
    report "${path}" "the index" "${mode}"
    path_ok=0
  else
    check_gitlink_object "${path}" "the index" "$(index_sha "${path}")"
  fi
}

# A required submodule also needs its .gitmodules mapping: without a url,
# `git submodule update --init` cannot fetch it and the clone is just as
# unbuildable as one with a broken gitlink.
check_mapping() {
  local path="$1" name url
  name="$(submodule_name_for_path "${path}")"
  url=""
  if [ -f .gitmodules ]; then
    url="$(git config -f .gitmodules --get "submodule.${name}.url" 2>/dev/null)" || true
  fi
  if [ -z "${url}" ]; then
    fail_mapping "${path}" "${name}"
    status=1
  fi
}

# 1. Every known submodule path is a gitlink.
path_ok=1
for path in ${REQUIRED_PATHS}; do
  check_path "${path}" yes
  if [ "${path_ok}" = 1 ]; then
    check_mapping "${path}"
  fi
done
for path in ${declared_paths}; do
  case " ${REQUIRED_PATHS} " in
    *" ${path} "*) continue ;; # already checked, and required
  esac
  check_path "${path}" no
done

# 2. Sweep the submodule roots for the two shapes rule 1 cannot see:
#    - a SYMLINK anywhere under them (the #265 shape, including a submodule
#      nobody declared or that a bad commit dropped from .gitmodules), and
#    - files tracked strictly INSIDE a known submodule path, which means its
#      contents were committed instead of the gitlink.
# Ordinary files that live under the roots but outside any submodule (the
# submodule/github.com/.gitkeep placeholder) are left alone.
sweep_entry() {
  local mode="$1" path="$2" where="$3" known
  if [ "${mode}" = "120000" ]; then
    report "${path}" "${where}" "${mode}"
    return
  fi
  for known in ${REQUIRED_PATHS} ${declared_paths}; do
    case "${path}" in
      "${known}"/*)
        report "${known}" "${where}" "${mode}"
        return
        ;;
    esac
  done
}

# Capture to a variable first so a git failure aborts (set -e) instead of
# being swallowed by a pipeline, and feed each loop from a heredoc so
# `status` survives (a pipe would run the loop in a subshell).
for sweep_root in ${SWEEP_ROOTS}; do
  if [ "${have_ref}" = 1 ]; then
    # `ls-tree -r` does not descend INTO a gitlink, so a healthy tree lists
    # the gitlink rows themselves and nothing from inside them.
    entries="$(ls_tree "${REF}" -- "${sweep_root}")"
    while IFS= read -r line; do
      [ -n "${line}" ] || continue
      sweep_entry "${line%% *}" "${line#*"${TAB}"}" "${REF}"
    done <<EOF
${entries}
EOF
  fi

  entries="$(ls_index -- "${sweep_root}")"
  while IFS= read -r line; do
    [ -n "${line}" ] || continue
    sweep_entry "${line%% *}" "${line#*"${TAB}"}" "the index"
  done <<EOF
${entries}
EOF
done

if [ "${status}" = 0 ]; then
  echo "check-submodule-gitlink: OK — submodules are gitlinks (${GITLINK_MODE})"
fi
exit "${status}"
