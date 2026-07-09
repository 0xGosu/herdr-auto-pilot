# Constitution — Herd Auto Prompter

## Project Vision

**Problem.** Operators who run coding agents inside [Herdr](https://herdr.dev) must babysit each pane: an agent finishes a step and goes idle, blocks on an approval or permission prompt, asks a multiple-choice question, or stalls on an error — and nothing progresses until a human notices and responds. Across a herd of parallel agents this manual attention is the bottleneck, and long-running work stops the moment the operator looks away.

**Target users.** Individual engineers and solution architects who drive multiple coding agents concurrently in Herdr and want those agents to keep making progress hands-free, while retaining full control and auditability over what was automated on their behalf.

**Long-term outcome.** Herd Auto Prompter is a Herdr plugin that monitors every agent session in the herd, detects when an agent needs input, and automatically supplies the next prompt or the correct response — choosing actions the way *this operator* would, learned from their own past decisions. The plugin advances agents unattended when it is confident, and escalates to the human when it is not, so the operator's judgment scales across the whole fleet without being replaced by it.

## Core Principles

1. **Learned, not guessed.** Every automated choice must be traceable to the operator's own observed decisions or explicitly declared preferences. The plugin encodes the user's judgment; it does not invent policy.
2. **Confidence-gated autonomy.** The plugin acts automatically only when its learned confidence for a situation exceeds a configured threshold. Below the threshold it escalates to the human rather than guessing.
3. **Safety over throughput.** Advancing agents faster must never override the safety controls. When a safety rule and an automation opportunity conflict, safety wins.
4. **Total auditability.** Every automatic decision is recorded with its trigger, chosen action, confidence, and rationale. The operator can always reconstruct and correct what the plugin did.
5. **Fail-safe by construction.** A monitoring tool must never destabilize the herd. The daemon must degrade to "do nothing and escalate" on any uncertainty or error — never crash a pane, never take an action it cannot explain.
6. **Local and private by default.** Learning data, history, and audit logs stay on the operator's machine. No telemetry. Data leaves the machine only through an integration the user explicitly configures.
7. **Reversible and interruptible.** A global pause/kill switch halts all automation instantly, and irreversible operations are never automated.
8. **Host-respecting integration.** Herdr owns the host surface; the plugin uses the documented plugin/CLI/socket API and does not depend on Herdr internals beyond the pinned `min_herdr_version`.

## Technology Constraints

**Language & runtime.**
- The plugin (monitoring daemon, TUI, and CLI) MUST be implemented in **Go**, shipped as a single self-contained static binary with no external runtime dependency.

**Herdr integration surface.**
- The daemon MUST consume Herdr's **raw socket API** for long-lived event subscriptions (`events.subscribe`) and agent-status transitions.
- One-shot control actions (e.g. `agent send`, `pane read`, `agent list`, `wait agent-status`) MUST be issued through the Herdr **CLI via `HERDR_BIN_PATH`** to stay portable across Unix sockets and Windows named pipes.
- The plugin MUST NOT depend on undocumented Herdr internals; it targets the documented plugin API, CLI reference, and socket API only.

**Persistence.**
- Learned decision history and the audit log MUST be stored in **embedded SQLite** (queryable, transactional).
- User-facing rules and configuration MUST be stored as **TOML** in the plugin's config folder so the operator can inspect and hand-edit them.

**LLM fallback.**
- The hybrid decision engine's LLM fallback MUST be provided by **delegating to a local LLM/coding-agent CLI already installed on the operator's machine** (shell-out). No cloud LLM SDK is a hard dependency of the MVP core.

**Platform.**
- The MVP targets **Linux and macOS first**; Windows is a follow-up. Code MUST avoid gratuitous platform lock-in so Windows support remains achievable.

## Architecture Constraints

- **Single binary, shared-state subcommands.** One Go binary exposes subcommands: `daemon` runs the monitor loop; `tui` and CLI subcommands read and write the **same SQLite + TOML state directly**. No bespoke IPC layer between front-end and daemon.
- **The TUI and CLI expose identical functionality.** The TUI is the primary control surface (declared as a Herdr pane command); every TUI capability is also reachable as a CLI subcommand, and both operate on the shared config/state.
- **Event-driven pipeline with a pure decision core.** The runtime flow is: `subscribe → classify situation → match rule / gate confidence → (act | escalate) → log`. The decision and learning logic MUST be a **pure, deterministic, Herdr-agnostic core**; all side effects (Herdr calls, LLM shell-out, persistence, notifications) live at the edges behind adapter boundaries.
- **Supervised shadow-mode learning.** The plugin acquires "past actions" by observing the operator during a shadow-training phase — it suggests, the human confirms or corrects — and only transitions to autonomous action once confidence is established. Learning is a defined subsystem feeding the pure decision core.
- **Situation taxonomy.** The classifier MUST distinguish, at minimum: agent **idle/finished**, **blocked on approval/permission**, **blocked on multiple-choice question**, and **error/retry** — the four situations in MVP scope.
- **Safety controls are first-class components**, not afterthoughts: the never-auto allowlist, the global pause/kill switch, and the confidence gate each sit on the action path and can veto any automated action.

## Testing Approaches

- **Unit tests on the pure decision core are mandatory** — deterministic rule matching, confidence gating, and situation-to-action resolution, using table-driven Go tests.
- **Golden/snapshot tests for situation classification** — recorded pane transcripts are classified into idle/approval/choice/error, with expected outputs pinned as golden files.
- **Safety-invariant tests are mandatory and non-negotiable** — proving the never-auto allowlist blocks listed operations, the kill switch halts all automated action, and the confidence gate prevents low-confidence auto-acts.
- **Integration tests against a faked/mocked Herdr** (CLI + socket) exercise the full monitor → decide → act loop without requiring a live Herdr instance.

## Coding Standards

- **Formatting & linting enforced in CI:** `gofmt`/`goimports`, `go vet`, and `golangci-lint` must pass; idiomatic Go with small, well-defined interfaces at adapter boundaries.
- **Structured logging for auditability:** use structured logging (`slog`); every automated decision emits a structured, machine-readable audit record (trigger, action, confidence, rationale).
- **Release discipline:** Conventional Commits and SemVer; the manifest's `min_herdr_version` is pinned and bumped only deliberately when new Herdr APIs are adopted.
- **Fail-safe error handling:** no panics on the daemon path. All errors are handled and surfaced to the audit log and/or Herdr notifications — the daemon must never crash the herd or leave an agent in an unrecoverable state.

## Security Constraints

- **Fully local, no telemetry.** All learning data, decision history, and audit logs remain on the operator's machine. The plugin performs no external calls except to a user-configured local LLM CLI.
- **No unattended irreversible actions.** A hard allowlist of never-auto operations (e.g. force-push, destructive filesystem operations, deploys) is always escalated to the human and can never be automated regardless of confidence.
- **Explicit interruptibility.** A global pause/kill switch instantly halts all auto-prompting across the entire herd.
- **Trust-boundary awareness.** The plugin runs as the operator's user with full CLI access (per Herdr's plugin trust model); it MUST NOT broaden that surface — no network listeners beyond the documented Herdr socket, no exfiltration of pane contents.

## Performance Targets

These are committed baseline budgets for the MVP; the requirements and solution stages may tighten them but must not weaken them without an explicit decision.

- **Decision latency.** From an agent-status transition to a decision (act or escalate), the daemon MUST achieve **p95 ≤ 1 second** on the **rules-only decision path**. LLM-fallback cases are explicitly **excluded** from this budget, since they shell out to an external CLI and are inherently slower; they carry their own configured timeout after which the daemon fails safe and escalates.
- **Supported herd size.** The daemon MUST sustain the decision-latency budget while monitoring **up to ~25 concurrently active agents** on a typical operator workstation.
- **Runaway-loop guard.** Automated prompting MUST be bounded per agent: **no more than 5 consecutive automated prompts to the same agent without intervening human interaction**, and a **per-minute auto-prompt rate limit per agent**. On reaching either ceiling, the daemon pauses automation for that agent and escalates for a human check-in. The exact per-minute figure is finalized in requirements.
- **Low idle overhead.** While no agent needs attention, the monitoring daemon MUST impose negligible CPU/memory cost so it can run continuously alongside the herd without competing with the agents themselves.

## Integration Points

- **Herdr socket API** — `events.subscribe` for agent-status transitions and long-lived monitoring (raw JSON socket).
- **Herdr CLI via `HERDR_BIN_PATH`** — `agent send`, `pane read`, `agent list`, `wait agent-status`, `notification show`, and related one-shot control commands.
- **Herdr plugin manifest (`herdr-plugin.toml`)** — declares the plugin id/version, pinned `min_herdr_version`, the pane command (TUI), event hooks, and Go build steps.
- **Local LLM / coding-agent CLI** — shelled out to for the hybrid decision engine's low-confidence fallback (user-configured; optional).
- **Local filesystem** — plugin config folder (TOML rules/config) and SQLite database (history + audit log).
