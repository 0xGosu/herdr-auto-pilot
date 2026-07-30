# changelog.d — one fragment per PR

Every change gets a changelog entry (see `CLAUDE.md`). Instead of editing
`CHANGELOG.md` directly, **add a new file here**:

```
changelog.d/<your-branch-name>.md
```

Name it after your branch (or anything unique to your PR). The name never
appears in the output — it exists only so two PRs can never touch the same
file.

## What goes in it

Just the bullets, no version heading — the version is added when the release
is cut:

```markdown
- Fixed the daemon assuming an escalation reached you when herdr had silently
  dropped the toast
- Added `notified=` to the `escalated` log line, so the log can say whether
  anyone was actually interrupted
```

Style is unchanged from `CHANGELOG.md`: a flat list of verb-first one-liners
(`Added …`, `Fixed …`, `Changed …`, `Removed …`), no sub-sections. Write what
it MEANS for the reader, not what the diff did — GitHub already generates a
list of PR titles per release; this file is for what a title cannot carry.
Mark a breaking change **Breaking.** at the start of its line.

## Why not just edit CHANGELOG.md

Because every PR inserted its section at the same place — the top of one
shared file — so two open PRs always conflicted on the same lines even when
the changes were unrelated. Worse, each PR had to *guess* its version number,
which is only knowable when the release is actually cut.

One file per PR makes the conflict structurally impossible, and nobody writes
a version number at all.

## What happens to it

On merge to main, the auto-release workflow computes the next version, runs
`scripts/assemble-changelog.sh <version>`, which folds every fragment here
into `CHANGELOG.md` under a `## <version>` heading and deletes them — all
inside the same commit that bumps `herdr-plugin.toml`.

Fragments are concatenated in filename order. If two entries need a specific
order relative to each other, put them in one fragment.

**Minor/major releases are the manual exception**: you already hand-write the
version into `herdr-plugin.toml` inside your PR, so run
`bash scripts/assemble-changelog.sh X.Y.0` in that same PR and commit the
result. The workflow tags your merge commit directly and never opens a bump
commit to assemble into.
