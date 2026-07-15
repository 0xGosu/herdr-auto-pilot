# Requirements — Herd Auto Prompter

## Overview

Herd Auto Prompter is a Herdr plugin that monitors every coding-agent session in the herd, detects when an agent needs input, and automatically supplies the next prompt or the correct response — choosing actions the way the operator would, learned from their own past decisions. It advances agents unattended when confident and escalates to the human when not.

This document defines the functional and non-functional requirements for the MVP. It is technology-neutral (what, not how); architecture and stack decisions live in the solution specification. All requirements are consistent with, and bounded by, the project constitution — in particular its principles of learned-not-guessed automation, confidence-gated autonomy, safety-over-throughput, total auditability, fail-safe operation, and local-and-private-by-default operation.

### In-scope situations (MVP)

The plugin auto-responds to four agent situations:

1. **Idle / finished** — the agent completed a step and is idle.
2. **Blocked on approval / permission** — the agent awaits a yes/no or permission confirmation.
3. **Blocked on a multiple-choice question** — the agent asks the operator to pick among options.
4. **Error / retry** — the agent stalled on an error and awaits a retry/skip/abort decision.

### Definitions

- **Situation signature** — a normalized fingerprint of a detected situation (situation type + salient prompt/pane content) used to match against past decisions.
- **Confidence** — a score in `[0,1]` expressing how consistently the operator has historically resolved a matching situation signature.
- **Shadow mode** — a per-situation-signature state in which the plugin suggests an action and the operator confirms or corrects it, without acting autonomously.
- **Autonomous mode** — a per-situation-signature state in which the plugin acts automatically when confidence exceeds the applicable threshold.
- **Escalation** — the plugin declines to auto-act and hands the decision to the human (via the TUI and/or a Herdr notification), leaving the agent untouched.

## User Roles

The MVP defines a single role. Multi-user or role-separated access is explicitly out of scope.

- **Operator** — the person running the herd. The Operator installs and configures the plugin, participates in shadow-mode training, tunes thresholds and rules, reviews and corrects the audit log, and controls the global pause/kill switch. The Operator has full read/write access to all plugin configuration, learned data, and audit records via both the TUI and the equivalent CLI subcommands.

## Functional Requirements

### Monitoring & detection

**FR-001 — Continuous herd monitoring.**
The plugin SHALL continuously monitor all agent sessions in the running Herdr instance and react to agent-status transitions without operator polling.
*Acceptance:* Given N agents running, when any agent transitions status (e.g. to idle/blocked), the plugin registers the transition and begins evaluation without manual action; stopping/starting an agent updates the monitored set automatically.

**FR-002 — Situation classification.**
The plugin SHALL classify each attention-requiring transition into exactly one of the four in-scope situations, or `unclassifiable`.
*Acceptance:* Given a recorded pane transcript for a known situation type, the classifier assigns the correct situation type; given content it cannot categorize, it assigns `unclassifiable` and triggers escalation (see FR-018).

**FR-003 — Situation signature generation.**
For each classified situation, the plugin SHALL derive a stable situation signature usable for matching against historical decisions.
The signature SHALL:
- Retain the **situation type**, the **agent type**, and the **salient decision content** — the normalized option set (for multiple-choice) or the permission verb/action (for approval/permission).
- **Mask volatile tokens** before hashing — absolute paths, hashes, line numbers, timestamps, UUIDs, and similar variable spans are replaced with typed placeholders so equivalent prompts collapse to one signature.
- Be **scoped per agent type**, so one agent type's learned habits do not leak into another's signatures.
*Acceptance:* Two prompts differing only in volatile tokens (paths, hashes, timestamps) produce the same signature; prompts differing in situation type, option set, permission verb, or agent type produce different signatures.

**FR-003a — Signature collision / over-generalization guard.**
The plugin SHALL guard against both over-general and over-masked signatures:
- **Variance guard.** If decisions accumulating under a single signature show high disagreement/variance (i.e. the signature is matching materially different situations), the plugin SHALL treat that signature as low-confidence and escalate until the operator disambiguates.
- **Over-masking floor.** If normalization masks so much of the prompt that little salient content remains, the plugin SHALL treat the situation as `unclassifiable` and escalate rather than match on a degenerate signature.
*Acceptance:* A signature with an even split of contradictory historical decisions forces escalation rather than auto-acting; a prompt reduced almost entirely to placeholders is classified `unclassifiable` and escalated.

### Learning (supervised shadow mode)

**FR-004 — Shadow-mode observation.**
In shadow mode for a given signature, the plugin SHALL present a suggested action and record the operator's confirmation or correction as a learning event.
*Acceptance:* When the operator confirms a suggestion, a decision record is stored linking the signature to the chosen action; when the operator corrects it, the corrected action is stored instead.

**FR-005 — Confidence computation.**
The plugin SHALL compute confidence for a signature as a **recency-weighted agreement ratio** over past decisions for that signature (more recent decisions weighted more heavily). An explicit **operator confirmation** (accepted confirm/send) SHALL weigh more than a passive auto-send or an aging vote, by a configurable factor (`learning.confirmation_weight`, default 3×), so that operator-blessed rules build confidence faster; corrections are not boosted.
*Acceptance:* A signature the operator resolved the same way 9 of the last 10 times yields high confidence; an evenly split history yields low confidence; recent decisions shift the score more than older ones; an operator confirmation raises the confidence of a contested signature more than the same vote from an auto-send would.

**FR-006 — Shadow-to-autonomous graduation.**
A signature SHALL graduate from shadow mode to autonomous mode only when BOTH (a) the operator has provided **N consecutive consistent confirmations** (N configurable; default 5) AND (b) confidence exceeds the applicable threshold.
*Acceptance:* A signature with fewer than N consecutive consistent confirmations remains in shadow mode even at high confidence; once both conditions hold, subsequent matching situations are eligible for autonomous action.

**FR-007 — Permanent graduation; operator-initiated reset.**
Graduation SHALL be permanent: once a signature is autonomous, its consecutive-confirmation count is **frozen** and a subsequent correction of an autonomous decision SHALL NOT demote it. The correction is still recorded as a decision, so the recency-weighted agreement ratio (FR-005) reflects it and the confidence gate (FR-008) still applies — a run of corrections can drop confidence below threshold and force escalation — but the mode never auto-reverts. The ONLY path back to shadow is an **explicit operator reset** via the CLI/TUI, which returns the signature to shadow mode with a consecutive-confirmation count of zero AND clears its confidence: the reset stamps a **decision-id floor** so that all decisions recorded before the reset are **kept as history but excluded from confidence and graduation** (FR-005) — the score reads a fresh 1.0 and the rule behaves confidence-new. The rule still **suggests its learned answer** (drawn from full history), so re-earning trust is just re-confirming it; a reset signature MUST re-earn N consecutive consistent confirmations (FR-006), computed over post-reset decisions only, before it can act autonomously again.
*Acceptance:* After a correction to an autonomous decision, the signature remains autonomous with an unchanged consecutive-confirmation count, and the correction is recorded as a decision in the recency window; after an operator reset (`hap signatures reset`, or the Rules-tab reset key), the same signature is in shadow mode with a count of zero, all decision rows are retained but pre-reset decisions no longer affect confidence/graduation, and the rule still surfaces its learned answer as the escalation suggestion until re-confirmed.

### Decision & action

**FR-008 — Confidence-gated autonomous action.**
For a signature in autonomous mode not blocked by a safety control, the plugin SHALL auto-execute the learned action only when confidence exceeds the applicable per-situation threshold; otherwise it SHALL escalate.
*Acceptance:* Confidence above threshold → action executed and logged; at/below threshold → escalation with no action taken.

**FR-009 — Per-situation-type thresholds.**
The plugin SHALL support a distinct confidence threshold per situation type (idle, approval, choice, error), each operator-configurable in TOML.
*Acceptance:* Setting the approval threshold higher than the idle threshold causes approval situations to require more confidence to auto-act; each threshold is independently editable and takes effect after reload.

**FR-010 — Hybrid decision with LLM fallback.**
When no confident learned rule applies, the plugin SHALL optionally consult a configured local LLM/agent CLI for a suggested action; any LLM-derived action is subject to the same confidence gate and safety controls, and on LLM unavailability the plugin SHALL escalate.
*Acceptance:* With a rule match above threshold, the LLM is not consulted; with no confident rule and an LLM configured, the LLM's suggestion is offered/acted per gating; with no LLM configured or on LLM failure, the situation escalates (see FR-020).

**FR-011 — Idle / finished behavior.**
On an idle/finished situation, the plugin SHALL prompt the agent toward its next task using next-task context resolved in the following priority order:
1. **Operator-declared task source** — a task list / backlog / plan file the operator points the plugin at, per agent or workspace; the plugin prompts the next unchecked item.
2. **Agent pane-history inference (fallback)** — when no declared source is configured, the plugin MAY infer the next task from the agent's own recent transcript **only when the transcript contains an explicit, structured signal** — a todo/checklist or numbered plan the agent itself emitted with an unambiguous "next" item. Free-form prose that merely discusses possible work does NOT qualify as an inferable next task.
Pane-history-inferred next tasks are subject to the confidence gate and SHALL be held to a **higher confidence bar than operator-declared-source tasks**, because inference is riskier than reading a declared list.
If neither a declared task source nor a qualifying, sufficiently-confident inferable next task is available, the plugin SHALL **escalate** and SHALL NOT synthesize an arbitrary "continue" prompt.
*Acceptance:* With a declared task list, the agent receives the next unchecked item; with no declared source but an explicit structured todo/checklist in pane history clearing the higher inferred-task bar, the inferred next task is used; with only free-form prose, ambiguous history, or sub-threshold confidence, the situation escalates and no arbitrary prompt is sent.

**FR-012 — Approval / permission behavior.**
On an approval/permission situation, the plugin SHALL select the learned yes/no response when confident and not blocked by the never-auto allowlist.
*Acceptance:* A repeatedly-approved permission shape is auto-approved when confident; an allowlist-matched operation is escalated regardless of confidence (see FR-015).

**FR-013 — Multiple-choice behavior.**
On a multiple-choice situation, the plugin SHALL select the learned option matching the choice signature; if the option set is unfamiliar, it SHALL escalate.
*Acceptance:* A familiar choice with a confident learned option is auto-selected; an unfamiliar option set escalates.

**FR-014 — Error / retry behavior.**
On an error situation, the plugin SHALL choose the learned retry / skip / escalate action, bounded by a max of **2 automated retries per error signature**, after which it SHALL force escalation.
*Acceptance:* Up to 2 automated retries occur for a matching error signature; a 3rd occurrence escalates to the human regardless of confidence.

### Safety controls

**FR-015 — Never-auto allowlist.**
The plugin SHALL maintain a never-auto allowlist of irreversible operations; any situation whose prompt content matches an allowlist entry SHALL be escalated and never auto-executed regardless of confidence or mode.
*Acceptance:* A prompt matching an allowlist entry (e.g. force-push, destructive filesystem op, deploy/publish, credential change) is always escalated; no autonomous action is taken on it.

**FR-016 — Allowlist seeding, extension, and coverage safety.**
The plugin SHALL ship a default seed allowlist and SHALL allow the operator to extend/override it via user-editable TOML **regex/keyword patterns** matched against prompt/pane content.
To ensure allowlist coverage degrades safely:
- The shipped seed patterns SHALL be validated against a **maintained test corpus of known irreversible-operation prompts**, which the seed patterns MUST catch; this corpus is regression-tested (see NFR-005a).
- The plugin SHALL apply a **"suspected-irreversible-but-unmatched" heuristic**: when a prompt exhibits destructive-operation indicators but matches no allowlist pattern, the plugin SHALL bias toward **escalation** rather than autonomous action.
*Acceptance:* The default seed set is present on first run and passes the irreversible-op corpus; operator-added patterns are honored after reload; a destructive-looking prompt that matches no pattern is escalated rather than auto-acted. (Per operator decision, only definite pattern matches escalate via the allowlist proper; ambiguity is handled by this heuristic, the confidence gate, and FR-018.)

**FR-017 — Global pause / kill switch.**
The plugin SHALL provide a global pause/kill switch that instantly halts all automated prompting across the entire herd, controllable from both the TUI and CLI. A toggle issued from the TUI or CLI SHALL take effect across the herd within a small bounded delay (see NFR-009).
*Acceptance:* Activating the switch causes all subsequent situations to be held/escalated with no automated action, across every monitored agent, within the bounded propagation delay, until resumed; the state is visible in the TUI.

**FR-018 — Escalation on uncertainty.**
The plugin SHALL escalate (take no action, notify the operator) whenever a situation is `unclassifiable`, below the confidence threshold, tripped by the variance/over-masking guard (FR-003a), flagged by the suspected-irreversible heuristic (FR-016), or otherwise ineligible for autonomous action.
*Acceptance:* Each such case results in no input sent to the agent and a surfaced escalation the operator can resolve.

**FR-019 — Runaway-loop guard.**
The plugin SHALL bound automated prompting per agent to no more than **5 consecutive automated prompts without intervening human interaction** AND no more than **10 automated prompts per agent per minute**; on reaching either ceiling it SHALL pause automation for that agent and escalate for a human check-in.
*Acceptance:* A 6th consecutive auto-prompt to one agent, or the 11th within a minute, is blocked and escalated; automation for that agent resumes only after human interaction.

### Auditability & control surface

**FR-020 — Full audit log.**
The plugin SHALL record every automated decision and escalation with its trigger, situation type, chosen action (or escalation), confidence, rationale (rule match or LLM), and timestamp.
*Acceptance:* Every action/escalation produces a queryable audit record containing all listed fields; no autonomous action occurs without a corresponding audit record (see FR-024).

**FR-021 — Post-hoc correction.**
The operator SHALL be able to review the audit log and correct any past automated decision, feeding the correction back into learning (per FR-007).
*Acceptance:* Correcting a logged decision updates the learning history for that signature and is itself recorded in the audit trail.

**FR-022 — Equivalent TUI and CLI.**
The plugin SHALL expose a TUI (as a Herdr pane) and a CLI that provide identical functionality: viewing monitored agents, pending escalations, decisions/audit log, thresholds and rules, and the pause/kill switch. Mutations issued from either surface (rule edits, threshold changes, pause/kill) SHALL propagate promptly to the running daemon (see NFR-009).
*Acceptance:* Every operation available in the TUI is achievable via a CLI subcommand operating on the same underlying state, and vice versa; a mutation from either surface is reflected in daemon behavior within the bounded propagation delay.

### Edge-case handling

**FR-023 — Herdr unreachable.**
If the Herdr socket/CLI is unreachable or a pane read fails, the plugin SHALL take no automated action, log the condition, and attempt to reconnect with backoff.
*Acceptance:* While Herdr is unreachable, no input is sent to any agent; the plugin retries connection with increasing backoff and resumes monitoring on recovery.

**FR-024 — Persistence failure.**
If writing the audit/history/config store fails, the plugin SHALL NOT proceed to auto-act and SHALL surface the failure via notification.
*Acceptance:* A simulated persistence write failure blocks autonomous action (no action without a recorded audit entry) and produces an operator-visible notification.

## Non-Functional Requirements

**NFR-001 — Decision latency.**
On the rules-only decision path, the plugin SHALL reach a decision (act or escalate) within **p95 ≤ 1 second** of an agent-status transition. LLM-fallback cases are excluded from this budget and are governed by their own timeout (NFR-006).
*Target:* p95 ≤ 1000 ms, rules-only path.

**NFR-002 — Supported herd size.**
The plugin SHALL sustain NFR-001 while monitoring **up to 25 concurrently active agents** on a typical operator workstation.
*Target:* ≥ 25 concurrent monitored agents within latency budget.

**NFR-003 — Low idle overhead.**
While no agent needs attention, the monitoring daemon SHALL impose negligible CPU/memory overhead so it can run continuously alongside the herd.
*Target:* Near-zero CPU when idle; steady-state memory small enough not to compete with agents (specific figure finalized in solution).

**NFR-004 — Fail-safe reliability.**
The daemon SHALL never crash a pane or the herd; all errors SHALL be handled and surfaced (audit log/notification) without panics on the daemon path.
*Target:* Zero unhandled daemon panics; every error path results in escalate/log, not agent disruption.

**NFR-005 — Auditability completeness.**
100% of automated decisions and escalations SHALL be represented in the audit log; no autonomous action SHALL occur without a corresponding audit record.
*Target:* 1:1 automated-action-to-audit-record ratio.

**NFR-005a — Allowlist corpus regression.**
The irreversible-operation test corpus backing FR-016 SHALL be maintained and regression-tested so shipped seed patterns continue to catch every corpus entry.
*Target:* 100% of the irreversible-op corpus matched by seed patterns in CI; a corpus miss fails the build.

**NFR-006 — LLM fallback timeout.**
LLM-fallback consultation SHALL be bounded by a configurable timeout, after which the plugin SHALL fail safe and escalate.
*Target:* Bounded timeout (default finalized in solution); on timeout/missing/unparseable output → escalate.

**NFR-007 — Privacy / no telemetry.**
All learning data, history, and audit logs SHALL remain local; the plugin SHALL make no external calls except to a user-configured local LLM CLI, and SHALL emit no telemetry.
*Target:* No outbound network calls beyond the documented Herdr socket and the configured local LLM CLI.

**NFR-008 — Portability.**
The plugin SHALL run on Linux and macOS for the MVP, avoiding gratuitous platform lock-in so Windows support remains achievable.
*Target:* Linux + macOS supported; no design decisions that preclude Windows.

**NFR-009 — Control-mutation propagation.**
A control mutation issued from the TUI or CLI — in particular the pause/kill switch — SHALL take effect in the running daemon within a **small bounded delay (target ≤ 1 second)**. The concurrency mechanism that achieves this (e.g. a command channel, shared-state signalling) is a solution-stage decision, but the observable propagation guarantee is required here.
*Target:* Pause/kill and config/rule edits reflected in daemon behavior within ≤ 1 s of issuance.

## Data Requirements

**DR-001 — Learned decision history.**
The plugin SHALL persist decision records: situation signature, situation type, agent type, chosen/corrected action, source (operator/rule/LLM), confidence at decision time, consecutive-confirmation count, and timestamp. Used to compute confidence (FR-005) and drive graduation/demotion (FR-006, FR-007).

**DR-002 — Audit log.**
The plugin SHALL persist an append-only audit trail of every automated decision and escalation with the fields listed in FR-020, supporting review and post-hoc correction (FR-021).

**DR-003 — Rules & configuration.**
The plugin SHALL persist operator-editable configuration: per-situation confidence thresholds (including the higher inferred-task bar of FR-011), graduation N, never-auto allowlist patterns, per-agent/workspace next-task source references, LLM CLI configuration and timeout, rate/consecutive ceilings, and pause/kill state — in operator-inspectable/hand-editable form.

**DR-004 — Data locality & retention.**
All persisted data SHALL remain on the operator's machine. The plugin SHOULD allow the operator to clear/reset learned history and audit data. No pane content leaves the machine except, when explicitly configured, to the local LLM CLI.

**DR-005 — Correction lineage.**
Corrections (FR-021) SHALL be recorded such that the relationship between an original automated decision and its correction is preserved for audit.

## Integration Requirements

**IR-001 — Herdr event subscription.**
The plugin SHALL subscribe to Herdr agent-status transition events to drive monitoring (constitution: raw socket `events.subscribe`).
*Acceptance:* Agent-status transitions in Herdr are received by the plugin as events without polling.

**IR-002 — Herdr control actions.**
The plugin SHALL send prompts/responses to agents and read pane content via Herdr's documented control commands (constitution: CLI via `HERDR_BIN_PATH` — `agent send`, `pane read`, `agent list`, `wait agent-status`).
*Acceptance:* A decided action results in the correct input delivered to the target agent's pane; pane content can be read for classification and pane-history next-task inference (FR-011).

**IR-003 — Herdr notifications.**
The plugin SHALL surface escalations and critical failures via Herdr notifications in addition to the TUI.
*Acceptance:* An escalation or persistence/connectivity failure produces a Herdr notification visible to the operator.

**IR-004 — Herdr plugin manifest.**
The plugin SHALL be packaged as a Herdr plugin declaring its id/version, pinned `min_herdr_version`, the TUI pane command, event hooks, and build steps in `herdr-plugin.toml`.
*Acceptance:* Herdr can validate the manifest, build the plugin, and launch the declared TUI pane and monitoring entrypoint.

**IR-005 — Local LLM CLI.**
The plugin SHALL integrate an optional, operator-configured local LLM/agent CLI for the hybrid decision fallback, treating its absence or failure as an escalation trigger (FR-010, NFR-006).
*Acceptance:* With an LLM CLI configured, low-confidence situations may consult it within the timeout; without one, those situations escalate.
