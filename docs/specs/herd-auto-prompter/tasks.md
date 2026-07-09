# Tasks — Herd Auto Prompter

Implementation plan derived from the constitution, requirements, and solution specs.

**Sequencing.** Domain core first (pure decision/learning/signature, fully tested) → adapters (store, Herdr, LLM/MCP) → front-ends (TUI/CLI) → packaging/release. Tasks are grouped by the solution's system modules and arranged into milestones that respect that order.

**Conventions.**
- Testing is **TDD-forward**: each behavioral task authors its mandated tests alongside the implementation (unit for the decision core, golden for classification/signature, safety-invariant, concurrency, integration against a faked Herdr).
- Every task cites the **Requirements** ids (FR/NFR/DR/IR) it implements and the **Solution** module/section it maps to.
- Complexity per the constitution's sizing: Small < 1h, Medium 1–2h, Large 2–4h; larger work is split.

---

## Milestone 1 — Infrastructure & Scaffolding

- [x] 1\. Initialize Go module and single-binary subcommand skeleton
  - Create the Go module, `main` dispatch for subcommands `daemon` / `tui` / `mcp` / CLI verbs, and a layered package layout `cmd → domain → adapters` with the domain importing nothing from Herdr/SQLite/LLM.
  - Acceptance Criteria:
    - `go build` produces one static binary; each subcommand is reachable and prints usage.
    - The `domain` package compiles with no imports of Herdr, SQLite, or LLM packages (enforced by an import-lint check).
  - _Dependencies: none_
  - _Requirements: Constitution (Go single binary, pure-core), FR-022_
  - _Complexity: Medium_

- [x] 2\. Define port interfaces (HerdrPort, StorePort, LLMPort, NotifyPort)
  - Declare the adapter boundary interfaces the domain depends on; add fakes/mocks for each.
  - Acceptance Criteria:
    - Domain code references only port interfaces; fakes satisfy each port for unit tests.
  - _Dependencies: 1_
  - _Requirements: Constitution (ports/adapters), Solution §System Modules_
  - _Complexity: Medium_

- [x] 3\. Structured logging (slog) and fail-safe error scaffolding
  - Wire `slog` structured logging and a top-level recover/guard so the daemon path never panics.
  - Acceptance Criteria:
    - All modules log via slog; an induced error on the daemon path is handled (escalate/log), not a panic.
  - _Dependencies: 1_
  - _Requirements: NFR-004, Coding Standards (structured logging, no panics)_
  - _Complexity: Small_

- [x] 4\. TOML config loader with defaults and reload
  - Load per-situation thresholds (incl. inferred-task bar), graduation N, error-retry ceiling, allowlist patterns + seed, classifier manifests, next-task sources, LLM argv/timeout, rate ceilings; provide safe defaults and a reload entrypoint.
  - Acceptance Criteria:
    - Missing/partial config falls back to documented defaults; a reload re-reads without restart.
    - Malformed TOML is rejected with a clear error and does not crash the process.
  - _Dependencies: 1_
  - _Requirements: DR-003, FR-009, FR-016, NFR-006_
  - _Complexity: Medium_

---

## Milestone 2 — Persistence (StorePort / SQLite + WAL)

- [x] 5\. SQLite schema + migrations (WAL, busy_timeout)
  - Create tables `signatures`, `decisions`, `audit_log`, `agent_rate`, `error_retries`, `corrections`, `kill_events`, `llm_decisions` (+ operator anchor); enable WAL and `busy_timeout`; add migrations.
  - Acceptance Criteria:
    - Fresh DB opens in WAL mode with busy_timeout set; all tables/indexes created; `audit_log` and `kill_events` are append-only by convention.
  - _Dependencies: 2_
  - _Requirements: DR-001, DR-002, Solution §Data Model_
  - _Complexity: Medium_

- [x] 6\. StorePort implementation with partitioned write ownership
  - Implement reads/writes enforcing the write-ownership split: daemon-exclusive writers for `signatures`/`agent_rate`/`error_retries`/daemon `audit_log`/`decisions`; front-end writers for `corrections`/`kill_events`/TOML; `mcp` writer for `llm_decisions` only.
  - Acceptance Criteria:
    - Store API exposes the correct writer surface per caller class; hot-path counter writes are confined to the daemon path.
    - All mutations run inside transactions honoring busy_timeout.
  - _Dependencies: 5_
  - _Requirements: Solution §Concurrency & Durability Model, DR-005_
  - _Complexity: Large_

- [x] 7\. Concurrency test suite for shared-state safety
  - Author concurrency tests: concurrent daemon + front-end + `mcp` writes show no lost updates on `signatures`/`agent_rate`/`error_retries`; append-only integrity for `audit_log`/`kill_events`.
  - Acceptance Criteria:
    - Test proves no lost updates on hot-path rows under contention (SC-7); pause/kill history preserved across toggles.
  - _Dependencies: 6_
  - _Requirements: SC-7, NFR-004_
  - _Complexity: Medium_

- [x] 8\. Audit log write + query + post-hoc correction lineage + persistence-failure fail-safe
  - Implement audit record writes (trigger, situation type, action/escalation, confidence, rationale, llm_output, timestamp) and correction records amending an audit entry with preserved lineage. Implement the pre-action guard: an autonomous action is only permitted after its audit record is durably committed, and a persistence write failure blocks the action and raises a notification (FR-024).
  - Acceptance Criteria:
    - Every action/escalation produces a queryable audit record; a correction links to its original via lineage (DR-005).
    - No autonomous action can be recorded without a corresponding audit row (enforced pre-action).
    - **Safety-invariant test:** a simulated audit/history write failure (e.g. injected StorePort error) blocks the autonomous action — no input is sent to any agent — and raises an operator-visible notification (FR-024 acceptance clause).
  - _Dependencies: 6_
  - _Requirements: FR-020, FR-021, FR-024, DR-002, DR-005, NFR-005_
  - _Complexity: Medium_

---

## Milestone 3 — Domain Core (pure, fully tested)

- [x] 9\. Situation signature normalizer + guards
  - Implement signature generation: retain situation type + agent type + salient content (option set / permission verb), mask volatile tokens (paths/hashes/line numbers/timestamps/UUIDs), scope per agent type; add variance guard and over-masking floor.
  - Acceptance Criteria (golden + unit):
    - Prompts differing only in volatile tokens collapse to one signature; differing type/options/verb/agent-type diverge.
    - Contradictory-history signature forces escalation; over-masked prompt → `unclassifiable`.
  - _Dependencies: 2_
  - _Requirements: FR-003, FR-003a_
  - _Complexity: Large_

- [x] 10\. Confidence model (recency-weighted agreement ratio)
  - Implement recency-weighted agreement ratio over a signature's decision history.
  - Acceptance Criteria (unit, table-driven):
    - 9/10 consistent → high confidence; even split → low; recent decisions shift the score more than older ones.
  - _Dependencies: 2_
  - _Requirements: FR-005, DR-001_
  - _Complexity: Medium_

- [x] 11\. Graduation / demotion state machine
  - Implement shadow→autonomous graduation (N consecutive consistent confirmations AND above threshold) and demotion on correction (reset consecutive count to zero; ratio not additionally discounted).
  - Acceptance Criteria (unit):
    - < N confirmations stays shadow even at high confidence; correction resets count and requires N fresh confirmations before re-graduation.
  - _Dependencies: 10_
  - _Requirements: FR-006, FR-007_
  - _Complexity: Medium_

- [x] 12\. Per-situation thresholds + confidence gate
  - Implement independent per-situation-type thresholds and the gate that decides act vs escalate vs consult-LLM.
  - Acceptance Criteria (unit):
    - Above threshold → act; at/below → escalate (or consult-LLM per config); thresholds independently configurable.
  - _Dependencies: 10_
  - _Requirements: FR-008, FR-009_
  - _Complexity: Medium_

- [x] 13\. Situation resolvers — approval & multiple-choice
  - Implement approval (learned yes/no) and choice (learned option match; unfamiliar option set → escalate) resolvers.
  - Acceptance Criteria (unit):
    - Confident familiar approval/choice auto-resolves; unfamiliar option set escalates.
  - _Dependencies: 12_
  - _Requirements: FR-012, FR-013_
  - _Complexity: Medium_

- [x] 14\. Situation resolver — idle / next-task (two-tier)
  - Implement two-tier next-task resolution: operator-declared task source first (next unchecked item); else pane-history inference **only** from an explicit structured signal, held to the higher inferred-task bar; else escalate — never synthesize an arbitrary "continue".
  - Acceptance Criteria (unit + golden):
    - Declared list → next item; structured todo clearing the higher bar → inferred task; free-form/ambiguous/sub-threshold → escalate.
  - _Dependencies: 12_
  - _Requirements: FR-011_
  - _Complexity: Large_

- [x] 15\. Situation resolver — error / retry with per-signature ceiling
  - Implement learned retry/skip/escalate bounded by the daemon-owned `error_retries` per-error-signature ceiling (default 2), forcing escalation on exhaustion.
  - Acceptance Criteria (unit):
    - ≤ 2 automated retries per error signature; the 3rd occurrence escalates regardless of confidence; counter reset on resolution/correction.
  - _Dependencies: 12, 6_
  - _Requirements: FR-014_
  - _Complexity: Medium_

---

## Milestone 4 — Safety Controls & Audit Foundation

- [x] 16\. Never-auto allowlist matcher + suspected-irreversible heuristic
  - Implement regex/keyword allowlist matching against prompt content with a seed set, plus the "suspected-irreversible-but-unmatched" heuristic biasing to escalation.
  - Acceptance Criteria (safety-invariant):
    - Allowlist-matched op always escalates regardless of confidence; destructive-looking unmatched prompt escalates via heuristic.
  - _Dependencies: 4, 12_
  - _Requirements: FR-015, FR-016, FR-018_
  - _Complexity: Large_

- [x] 17\. Allowlist irreversible-op corpus + CI regression gate
  - Build the maintained corpus of known irreversible-op prompts; add a CI gate asserting seed patterns catch 100% of it.
  - Acceptance Criteria:
    - CI fails if any corpus entry is unmatched by seed patterns (NFR-005a).
  - _Dependencies: 16_
  - _Requirements: NFR-005a, SC-3_
  - _Complexity: Medium_

- [x] 18\. Global pause/kill switch (append-only kill_events, read every tick)
  - Implement pause/kill/resume as append-only `kill_events` inserts (author/scope/timestamp) with the daemon reading the latest row each pipeline tick.
  - Acceptance Criteria (safety-invariant):
    - Setting kill halts all automation across the herd on the next tick even without a reload nudge; full pause/kill history retained.
  - _Dependencies: 6_
  - _Requirements: FR-017, SC-2_
  - _Complexity: Medium_

- [x] 19\. Runaway-loop guard (agent_rate: consecutive + per-minute)
  - Implement per-agent counters: ≤ 5 consecutive auto-prompts without human interaction and ≤ 10 auto-prompts/agent/minute, then pause+escalate.
  - Acceptance Criteria (safety-invariant):
    - 6th consecutive or 11th-in-a-minute auto-prompt is blocked and escalated; resumes only after human interaction.
  - _Dependencies: 6, 12_
  - _Requirements: FR-019_
  - _Complexity: Medium_

- [x] 20\. Escalation-on-uncertainty consolidation
  - Route every uncertain path (unclassifiable, below-threshold, variance/over-masking guard, suspected-irreversible, rate/retry-exhausted, killed) to escalate + audit + notify.
  - Acceptance Criteria:
    - Each error-code path takes no agent action and produces an audit record + escalation surface.
  - _Dependencies: 8, 12, 16, 18, 19_
  - _Requirements: FR-018, Solution §Error Codes_
  - _Complexity: Small_

---

## Milestone 5 — Herdr Adapters & Pipeline

- [x] 21\. Fake Herdr harness + recorded-transcript fixtures
  - Build a faked Herdr (socket events + CLI) and a fixtures library of recorded pane transcripts for idle/approval/choice/error/unclassifiable.
  - Acceptance Criteria:
    - Golden + integration tests can drive transitions and pane reads without a live Herdr; fixtures cover all situation types.
  - _Dependencies: 2_
  - _Requirements: Testing Strategy (faked Herdr, golden), IR-001, IR-002_
  - _Complexity: Large_

- [x] 22\. Event Subscriber (HerdrPort inbound) with reconnect/backoff
  - Implement raw-socket `events.subscribe`, agent-transition decoding, monitored-agent set tracking, and reconnect with exponential backoff; no actions while disconnected.
  - Acceptance Criteria (integration):
    - Transitions arrive without polling; on socket loss the daemon halts actions, logs, and reconnects with backoff (FR-023).
  - _Dependencies: 21_
  - _Requirements: FR-001, FR-023, IR-001_
  - _Complexity: Large_

- [x] 23\. Classifier (TOML per-agent-type manifests)
  - Implement manifest-driven classification of pane snapshots into the four types or `unclassifiable`.
  - Acceptance Criteria (golden):
    - Recorded transcripts classify correctly; no match → `unclassifiable`; manifest parse error fails safe to unclassifiable.
  - _Dependencies: 4, 21_
  - _Requirements: FR-002, IR-002_
  - _Complexity: Large_

- [x] 24\. Action Executor (HerdrPort outbound) + notifications
  - Implement `agent send`, `pane read`, and `notification show` via `HERDR_BIN_PATH`; guarantee no action without a preceding durably-committed audit record (delegates the pre-action audit guard from Task 8).
  - Acceptance Criteria (integration):
    - Decided input reaches the target pane; escalations/failures raise Herdr notifications; CLI failure → log + escalate.
    - **The pre-action audit guard is honored end-to-end:** when the audit write fails, the executor sends nothing and raises a notification (FR-024), verified against the Task 8 fail-safe.
  - _Dependencies: 8, 21_
  - _Requirements: FR-024, IR-002, IR-003_
  - _Complexity: Medium_

- [x] 25\. Daemon pipeline wiring (subscribe → classify → signature → decide → gate/safety → act/escalate → log)
  - Compose the modules into the event pipeline. Scope is functional wiring + end-to-end idle→act and blocked→resolve loops against the fake Herdr; the p95/25-agent latency assertion (SC-1) is verified separately in Task 36.
  - Acceptance Criteria (integration):
    - End-to-end idle→act and blocked→resolve loops pass against the fake Herdr for all four situations; every branch routes to act or escalate+audit.
  - _Dependencies: 9, 12, 13, 14, 15, 16, 18, 19, 20, 22, 23, 24_
  - _Requirements: FR-001, NFR-001, NFR-002_
  - _Complexity: Large_

---

## Milestone 6 — Learning Subsystem

- [x] 26\. Shadow-mode observation & decision recording
  - Record confirmations/corrections as learning events feeding confidence/graduation. (Reload-triggered re-derivation of signature counters is verified in Task 27, once the control-socket reload path exists.)
  - Acceptance Criteria (unit + integration):
    - Confirm stores the chosen action; correct stores the corrected action; graduation/demotion state transitions behave per FR-006/007 given a direct in-process apply.
  - _Dependencies: 11, 6_
  - _Requirements: FR-004, FR-006, FR-007, DR-001, DR-005_
  - _Complexity: Medium_

- [x] 27\. Correction-driven re-derivation on reload
  - On reload nudge, consume new front-end-written correction records and re-derive affected signature mode/counters (no front-end writes to hot-path rows).
  - Acceptance Criteria:
    - A correction submitted from TUI/CLI demotes the signature after the daemon reloads; hot-path rows only ever written by the daemon.
  - _Dependencies: 26, 30_
  - _Requirements: FR-007, Solution §Concurrency & Durability Model_
  - _Complexity: Medium_

---

## Milestone 7 — LLM Fallback (MCP)

- [x] 28\. `mcp` subcommand stdio server (get_context, submit_decision)
  - Implement the per-invocation stdio MCP server exposing `get_context` and `submit_decision`; `submit_decision` writes a staged `llm_decisions` row and nudges the daemon.
  - Acceptance Criteria (integration):
    - A test MCP client can pull context and submit a decision that lands as a `pending` `llm_decisions` row + nudge.
  - _Dependencies: 6, 30_
  - _Requirements: FR-010, IR-005, Solution §MCP tool surface_
  - _Complexity: Large_

- [x] 29\. LLM adapter: launch, timeout, staging → re-gate → promotion
  - Implement launching the operator's LLM CLI via the argv template with attached MCP; consume staged decisions, re-gate (confidence + never-auto allowlist), promote accepted ones into `decisions` (source=LLM) + audit; capture stdout for audit only.
  - Acceptance Criteria (integration + safety-invariant):
    - No `submit_decision`/timeout/unparseable → escalate within the configured bound (SC-5); submitted decision re-gated by safety controls before acting; LLM never bypasses allowlist/confidence gate.
  - _Dependencies: 28, 16, 12_
  - _Requirements: FR-010, NFR-006, SC-5_
  - _Complexity: Large_

---

## Milestone 8 — Control Channel & Front-ends

- [x] 30\. Unix-domain control socket (named pipe on Windows) + reload/wake nudge
  - Implement the daemon control listener and client; front-ends/`mcp` write directly then nudge; reload re-reads TOML + operator/staged rows.
  - Acceptance Criteria (integration):
    - A config/correction/kill write + nudge is reflected in daemon behavior ≤ 1s (SC-2); kill still honored on next tick if the nudge is delayed; socket owner-only permissions.
  - _Dependencies: 4, 6, 18_
  - _Requirements: FR-017, FR-022, NFR-009, NFR-003, SC-2_
  - _Complexity: Large_

- [x] 31\. Shared view/command layer for TUI and CLI
  - Build the shared read queries + write/nudge command layer both front-ends use, ensuring functional parity.
  - Acceptance Criteria:
    - Every operation is reachable from both TUI and CLI against the same state (FR-022).
  - _Dependencies: 30, 8_
  - _Requirements: FR-022_
  - _Complexity: Medium_

- [x] 32\. TUI (Herdr pane): monitored agents, escalations, audit, rules, pause/kill + history
  - Implement the TUI surfacing monitored agents, pending escalations, audit log + corrections, threshold/rule editing, and pause/kill with history.
  - Acceptance Criteria:
    - Operator can view state and perform every control action; pause/kill history is visible.
  - _Dependencies: 31_
  - _Requirements: FR-022, FR-017, FR-021, Observability_
  - _Complexity: Large_

- [x] 33\. CLI verbs mirroring the TUI
  - Implement CLI subcommands for view/pause/resume/review/correct/config with output suitable for scripting.
  - Acceptance Criteria:
    - CLI achieves parity with the TUI on the shared command layer.
  - _Dependencies: 31_
  - _Requirements: FR-022_
  - _Complexity: Medium_

---

## Milestone 9 — Integration Testing & Hardening

- [x] 34\. End-to-end integration suite against the fake Herdr (incl. per-adapter panic injection)
  - Cover all four situations, LLM fallback staging→promotion, escalation paths, and reconnect behavior end-to-end. Inject panic-inducing faults at each adapter boundary (event subscriber, classifier, action executor, MCP/LLM) and assert the recover/guard resolves each to escalate+log.
  - Acceptance Criteria:
    - Full monitor→decide→act loop green for every situation; every error path resolves to escalate/log.
    - **Panic-injection matrix:** a fault injected at each adapter boundary (subscriber decode, classifier, executor CLI invocation, MCP/LLM) is caught by the daemon guard with **zero unhandled panics**, giving SC-4 end-to-end coverage rather than a single induced error.
  - _Dependencies: 25, 27, 29, 33_
  - _Requirements: SC-1, SC-4, SC-5, NFR-004, Testing Strategy_
  - _Complexity: Large_

- [x] 35\. Privacy / no-telemetry verification
  - Add a test asserting no outbound network calls beyond the Herdr socket and the configured local LLM CLI.
  - Acceptance Criteria:
    - No unexpected egress observed in the suite (SC-6).
  - _Dependencies: 34_
  - _Requirements: NFR-007, SC-6_
  - _Complexity: Small_

- [x] 36\. Latency & idle-overhead benchmark
  - Benchmark rules-only p95 decision latency at 25 agents and near-zero idle CPU; pin the deferred idle-memory ceiling. This is the sole owner of the SC-1 latency assertion.
  - Acceptance Criteria:
    - p95 ≤ 1s at 25 agents (SC-1); idle CPU near zero; documented idle-memory ceiling met (NFR-003).
  - _Dependencies: 25_
  - _Requirements: NFR-001, NFR-002, NFR-003, SC-1, SC-2_
  - _Complexity: Medium_

---

## Milestone 10 — Open-Source Packaging, CI & Release

- [x] 37\. CI pipeline: gofmt/goimports, go vet, golangci-lint, tests, corpus gate
  - Wire a GitHub Actions CI workflow gating formatting/vet/lint, the full test suite, and the allowlist-corpus regression gate on every push/PR.
  - Acceptance Criteria:
    - CI fails on format/vet/lint violations, test failures, or a corpus miss.
  - _Dependencies: 17, 34_
  - _Requirements: Coding Standards, NFR-005a_
  - _Complexity: Medium_

- [x] 38\. Cross-platform (Linux/macOS) build + portability check
  - Build and run the suite on Linux and macOS in CI; abstract the control channel (Unix socket vs named pipe) so no platform-locked API sits on the daemon/action path.
  - Acceptance Criteria:
    - Green build + suite on Linux and macOS; no platform lock precluding a Windows build (SC-8).
  - _Dependencies: 30, 34_
  - _Requirements: NFR-008, SC-8_
  - _Complexity: Medium_

- [x] 39\. `herdr-plugin.toml` manifest + build steps + min_herdr_version pin
  - Author the manifest declaring id/version, pinned `min_herdr_version`, the daemon entrypoint (event hook), the TUI pane, CLI actions (pause/resume/review/correct), and Go build steps that run from a clean checkout exactly as `herdr plugin install` would execute them.
  - Acceptance Criteria:
    - Herdr validates the manifest, builds the plugin from a clean checkout, and launches the TUI pane + daemon entrypoint.
  - _Dependencies: 32, 33_
  - _Requirements: IR-004, Constitution (semver, pinned min_herdr_version)_
  - _Complexity: Medium_

- [x] 40\. Open-source repository setup (MIT LICENSE, README quickstart, CONTRIBUTING)
  - Make the repo a proper installable open-source Herdr plugin: add an **MIT `LICENSE`**; a **README** whose quickstart is `herdr plugin install <github-org/repo>` (with a pinned `--ref` example and `--yes` note), covering config TOML (thresholds, task sources), allowlist authoring + the irreversible-op corpus, local LLM CLI/MCP setup, shadow-mode training/onboarding, and the pause/kill + audit workflow; and a **CONTRIBUTING** guide (Conventional Commits, build/test, PR flow). Minimal community-health set — no CODE_OF_CONDUCT/SECURITY/templates for MVP.
  - Acceptance Criteria:
    - Repo has MIT LICENSE, README, and CONTRIBUTING; README documents the `herdr plugin install <org/repo>` flow end to end.
    - A new operator can install, configure, train in shadow mode, and safely enable autonomy from the README alone.
  - _Dependencies: 39_
  - _Requirements: IR-004, FR-022, Constitution (auditability, coding standards), DR-004 (Data Protection)_
  - _Complexity: Medium_

- [ ] 41\. Tag-driven GitHub Actions release with artifacts
  - Add a release workflow: on a SemVer tag, run the full CI gate, build Linux/macOS binaries, and publish a GitHub Release with artifacts so `herdr plugin install` can resolve a pinned `--ref`.
  - Acceptance Criteria:
    - Pushing a semver tag produces a GitHub Release with Linux/macOS artifacts after CI passes; releases are versioned per SemVer.
  - _Dependencies: 37, 38, 39_
  - _Requirements: Constitution (SemVer, release discipline), IR-004_
  - _Complexity: Medium_

- [ ] 42\. End-to-end install-from-GitHub verification
  - Add a verification job that installs the plugin from GitHub via `herdr plugin install <org/repo>` (including a pinned `--ref`) on a clean machine/CI and confirms manifest validation, build, and daemon + TUI launch succeed.
  - Acceptance Criteria:
    - `herdr plugin install <org/repo> --ref <tag>` succeeds from a clean environment; manifest validates, the plugin builds, and the daemon + TUI pane start.
  - _Dependencies: 41_
  - _Requirements: IR-004, Constitution (host-respecting integration)_
  - _Complexity: Medium_

---

## Parallelization Notes

After the Milestone 1 scaffolding and the port interfaces (Tasks 1–4), three tracks are largely independent and can proceed concurrently to shorten the critical path:

- **Track A — Pure domain core:** Tasks 9–15 (signature, confidence, graduation, gate, resolvers) depend only on the ports/fakes, not on any adapter.
- **Track B — Persistence + Herdr adapters:** Tasks 5–8 and the fake-Herdr harness + adapters (21–24) can be built alongside Track A.
- **Track C — Control channel + front-ends:** Task 30 and Tasks 31–33 gate the front-end and MCP milestones; because Task 30 is a bottleneck for Tasks 27, 28, and 31, prioritize it once Tasks 4/6/18 land.

Tasks 25 (pipeline wiring) and 34 (integration) are the convergence points where the tracks join.

## Coverage Summary

- **Monitoring/detection:** FR-001 (22, 25), FR-002 (23), FR-003/003a (9), FR-023 (22).
- **Learning:** FR-004 (26), FR-005 (10), FR-006/007 (11, 26, 27).
- **Decision/action:** FR-008/009 (12), FR-010 (28, 29), FR-011 (14), FR-012/013 (13), FR-014 (15).
- **Safety:** FR-015/016 (16, 17), FR-017 (18, 30, 32), FR-018 (20), FR-019 (19).
- **Auditability/control:** FR-020 (8), FR-021 (8, 32), FR-022 (31, 32, 33), FR-024 (8, 24).
- **NFRs:** NFR-001/002 (25, 36), NFR-003 (30, 36), NFR-004 (3, 34), NFR-005 (8), NFR-005a (17, 37), NFR-006 (29), NFR-007 (35), NFR-008 (38), NFR-009 (30).
- **Data:** DR-001 (5, 10, 26), DR-002 (5, 8), DR-003 (4), DR-004 (40), DR-005 (8, 26).
- **Integration:** IR-001 (22), IR-002 (23, 24), IR-003 (24), IR-004 (39, 40, 41, 42), IR-005 (28, 29).
- **Success Criteria:** SC-1 (36), SC-2 (18, 30), SC-3 (17), SC-4 (34), SC-5 (29), SC-6 (35), SC-7 (7), SC-8 (38).
- **Open-source / installable plugin:** MIT LICENSE + README quickstart + CONTRIBUTING (40), tag-driven release with artifacts (41), `herdr plugin install org/repo` end-to-end verification (42).
