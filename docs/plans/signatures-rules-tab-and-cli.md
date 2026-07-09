# Plan: Learned-Signature Management (new Rules tab + `signatures` CLI)

Date: 2026-07-09
Status: implemented (v0.1.9)

## Goal

Learned signatures (the `signatures` table: mode, confirmation streak, cached
confidence) are written by the daemon but invisible to the operator — no TUI
tab or CLI command lists them. This plan adds:

1. TUI: rename the current **Rules** tab to **Config** (it shows config
   fields, allowlist patterns, and task sources — all config-file data).
2. TUI: a new **Rules** tab that lists all learned signatures and lets the
   operator inspect and delete them (delete requires confirmation).
3. CLI: a new `signatures` command with the same list / filter / delete
   functionality.

## Background (current code)

- `internal/store/store.go` — `signatures` table (signature PK,
  situation_type, agent_type, mode, consecutive_confirmations,
  cached_confidence, guard_state, updated_at). Only `GetSignature` (exact
  key) and `UpsertSignature` exist; there is no list or single-row delete.
  `ClearLearnedData` (store.go:389) is the existing precedent for a
  front-end-initiated destructive write to daemon-owned tables.
- `internal/ports/ports.go` — `ReadStore` / `FrontendStore` interfaces that
  new store methods must be added to.
- `internal/frontend/frontend.go` — shared view/command layer for both
  front-ends (FR-022): every new operation goes here once, both UIs call it.
  Mutations nudge the daemon (`control.KindReload`) afterwards.
- `internal/tui/tui.go` — tab constants + `tabNames` (line 30–35), `ruleItem`
  rows, `prompt` inline input (used by `clearDataPrompt` for type-`yes`
  confirmation — reuse for signature delete), `detailView` overlay.
- `internal/cli/cli.go` — verb dispatch in `Run`; `rules` currently manages
  allowlist patterns.
- The signature's *salient content is not persisted* — only the hash. The
  operator needs context to know what a signature means (see §Display
  context below).

## Design decisions

### Naming

- TUI tabs become: `Agents | Escalations | Audit | Rules | Config | Pause/Kill`.
  The renamed **Config** tab keeps all current behavior (edit fields,
  add/remove patterns and task sources, clear-data).
- CLI: add a new `signatures` verb (alias `sigs`). The existing `rules` CLI
  command (allowlist patterns) is **left unchanged** in this change to avoid
  breaking operators' scripts; its help text gains a note that the TUI
  equivalent now lives on the Config tab. Renaming `rules` → `patterns` can
  be a follow-up if the mismatch proves confusing.

### Delete semantics

Deleting a signature must remove, in one transaction:

- the `signatures` row (mode/streak state),
- all `decisions` rows for that signature (otherwise the next encounter
  recomputes confidence from the old history and the deletion is cosmetic),
- the `error_retries` row keyed by the signature, if any.

Audit rows are **kept** (DR-005 lineage; `clear-data` remains the way to
wipe those).

Concurrency: front-ends normally never write hot-path rows, but
`ClearLearnedData` already establishes the pattern of a guarded destructive
write + reload nudge. A race window exists (the daemon may upsert the same
signature between listing and deletion, or a pending correction may recreate
it in shadow mode). That is acceptable: the recreated state starts from
zero, which is exactly what deletion means. Document this in the method
comment.

### Signature addressing (CLI)

Full hashes (`approval:9f2c…`, 30+ chars) are hostile to typing. The CLI
accepts a unique prefix, git-style: `hap signatures delete approval:9f2c`.
A new store helper resolves prefixes and errors on ambiguity.

### Display context

A hash alone doesn't tell the operator what the signature matches. Without a
schema change, each list row is enriched with:

- **top action** + decision count — from `Confidence()` over
  `DecisionsForSignature(sig, 50)` (already exposed on `ReadStore`),
- **last audit rationale/action** — latest `audit_log` row for the
  signature (new query), shown in the TUI detail view and `signatures show`.

Follow-up (out of scope here): persist `SignatureResult.Salient` in a new
nullable `signatures.salient` column so rows are self-describing. Requires
an additive `ALTER TABLE` migration and threading the salient text to the
upsert path; not needed for v1.

## Implementation steps

### 1. Store layer (`internal/store/store.go`, `internal/ports/ports.go`)

New methods (added to `ReadStore` / `FrontendStore` as appropriate):

```go
// ListSignatures returns learning state rows, newest-updated first.
// Zero-valued filter fields are ignored.
func (s *Store) ListSignatures(ctx context.Context, f domain.SignatureFilter) ([]domain.SignatureState, error)

// ResolveSignature expands a unique signature prefix to the full key;
// errors on no match or >1 match (listing the candidates).
func (s *Store) ResolveSignature(ctx context.Context, prefix string) (string, error)

// DeleteSignature removes the signature row, its decision history, and its
// error-retry row in one transaction. Returns the number of decision rows
// removed for operator feedback.
func (s *Store) DeleteSignature(ctx context.Context, signature string) (int64, error)

// LatestAuditForSignature returns the newest audit row for a signature
// (nil when none) — display context for list/detail views.
func (s *Store) LatestAuditForSignature(ctx context.Context, signature string) (*domain.AuditRecord, error)
```

`domain.SignatureFilter` (new, in `internal/domain/types.go`):

```go
type SignatureFilter struct {
    SituationType SituationType // "" = any
    AgentType     string        // "" = any
    Mode          Mode          // "" = any (shadow | autonomous)
    MinConfidence float64       // 0 = any
}
```

### 2. Frontend layer (`internal/frontend/frontend.go`)

```go
// SignatureRow is a SignatureState enriched for display.
type SignatureRow struct {
    domain.SignatureState
    TopAction  string
    Decisions  int
    LastAudit  *domain.AuditRecord
}

func (a *App) Signatures(ctx context.Context, f domain.SignatureFilter) ([]SignatureRow, error)
func (a *App) SignatureDetail(ctx context.Context, prefix string) (SignatureRow, []domain.DecisionRecord, error)
// DeleteSignature resolves the prefix, deletes, and nudges KindReload.
func (a *App) DeleteSignature(ctx context.Context, prefix string) (deleted string, decisions int64, err error)
```

`Signatures` computes `TopAction`/`Decisions` via `domain.Confidence` over
`DecisionsForSignature(sig, 50)` per row (N+1 reads; fine at operator scale —
note a SQL aggregate as a future optimization if lists grow).

### 3. CLI (`internal/cli/cli.go`)

New verb wired into `Run`:

```
hap signatures                       # same as list
hap signatures list [--type approval|choice|error|idle] [--mode shadow|autonomous]
                    [--agent-type X] [--min-conf 0.8]
hap signatures show <sig-or-prefix>  # state + top action + recent decisions + last audit
hap signatures delete <sig-or-prefix> [--yes]
```

- `list` output: one row per signature — short hash (first 16 chars),
  situation type, agent type, mode, streak (`3/5` using the live
  `graduation_n`), confidence, top action, updated-at.
- `delete` without `--yes` prints the row and asks for interactive
  confirmation (`type 'yes' to delete:` read from stdin); `--yes` skips it
  for scripting. Non-TTY stdin without `--yes` fails with a message telling
  the operator to pass `--yes`.
- Flag parsing follows the existing `flag.NewFlagSet` pattern used by
  `task-source`.

### 4. TUI (`internal/tui/tui.go`)

Rename (mechanical):

- `tabRules` → `tabConfig`; `tabNames` entry `"Rules"` → `"Config"`. All
  `case tabRules:` sites (edit/add/remove/clear-data key handlers,
  `rowCount`, view rendering) follow the rename.

New Rules tab (inserted between Audit and Config in the `tab` iota order and
`tabNames`):

- `refreshMsg` gains `signatures []frontend.SignatureRow`, loaded in
  `refreshData` via `app.Signatures(ctx, domain.SignatureFilter{})` — the
  2-second tick keeps it live while the daemon learns.
- Row rendering mirrors the CLI list columns; autonomous rows highlighted
  (e.g. `okStyle`), shadow rows plain.
- Keys on the new tab:
  - `v` / `enter` — `detailView` overlay: full hash, mode, streak vs
    `graduation_n`, cached confidence, top action, recent decisions
    (action/source/is-correction/time), last audit rationale.
  - `x` / `delete` — delete the selected signature via the `prompt`
    mechanism, mirroring `clearDataPrompt`: label
    `type 'yes' to delete <short-hash> and its N decisions`, any other
    input aborts. On success show `actionResultMsg` with the deleted count.
  - `f` — cycle a mode filter (all → shadow → autonomous) applied to the
    displayed rows; keep it display-side (no re-query) for v1.
- Empty state: `no learned signatures yet — confirm suggestions to teach hap`.

### 5. Docs

- README: document the `signatures` CLI, the renamed Config tab, and the
  new Rules tab key bindings.
- `docs/specs/` — if a spec covers the TUI/CLI surface (FR-022), update it
  alongside the code.

## Tests (core deliverables, not optional)

- `internal/store/store_test.go`
  - `ListSignatures`: each filter field, combined filters, ordering.
  - `ResolveSignature`: unique prefix, ambiguous prefix (error lists
    candidates), no match.
  - `DeleteSignature`: removes signature + decisions + error_retries in one
    tx; other signatures' rows untouched; returns decision count; deleting
    a nonexistent signature errors cleanly.
  - `LatestAuditForSignature`: newest row wins; nil when none.
- `internal/frontend/frontend_test.go`
  - `Signatures` enrichment (top action, decision count) and filter
    pass-through; `DeleteSignature` nudges reload (existing fake control
    server pattern); prefix resolution errors surface.
- `internal/cli/cli_test.go`
  - `signatures list` output incl. filters; `show`; `delete --yes`;
    `delete` refusal without `--yes` on non-TTY stdin.
- `internal/tui/tui_test.go`
  - Tab rename (Config tab keeps edit/remove behavior — existing tests
    updated); new Rules tab: rows render, `x` opens the confirm prompt,
    non-`yes` input aborts, `yes` deletes; detail view content.
- Per repo practice: run tests via the test-runner subagent; note the macOS
  socket-path length cap when tests create sockets in temp dirs.

## Rollout / sequencing

1. domain filter type + store methods + tests (`internal/domain`,
   `internal/store`, `internal/ports`).
2. frontend methods + tests.
3. CLI verb + tests.
4. TUI rename, then new tab + tests (rename lands in the same commit as the
   new tab so "Rules" never means two things across commits).
5. README/docs update, plugin version bump in `herdr-plugin.toml`.

Commits follow the repo hook convention (ticket-id prefix, e.g.
`#1 feat: ...`).

## Open questions / accepted risks

- **Race with the daemon**: deletion can be undone by an in-flight
  correction recreating the signature in shadow mode — accepted (equivalent
  to "start over"), documented on `DeleteSignature`.
- **CLI `rules` naming mismatch** (CLI `rules` = allowlist, TUI Rules =
  signatures): accepted for back-compat; revisit after operator feedback.
- **Salient content column**: deferred (schema migration + upsert-path
  threading); v1 uses top action + last audit rationale for context.
