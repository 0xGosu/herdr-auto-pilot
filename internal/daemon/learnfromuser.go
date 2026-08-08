package daemon

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/logging"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
)

// Learning from an operator correction (llm.learn_from_user_command). When the
// operator answers an escalation with something OTHER than what hap suggested,
// a one-shot CLI runs in the agent's own working directory and is asked to
// record the lesson in that project's memory file (CLAUDE.md / AGENTS.md).
//
// This sits deliberately outside every decision path. It reads no rule, writes
// no rule, sends nothing to the pane, and cannot escalate — a failure produces
// one audit row and nothing else. It runs only AFTER the correction it follows
// has been committed (see processCorrections), so it can never cost the
// operator their correction.

// maxLearnAuditOutput caps the transcript stored on an audit row, in RUNES (so
// a multibyte-heavy transcript can exceed this in bytes). The adapter already
// caps each captured stream; this is the tighter per-row bound that keeps the
// audit table small — audit_log is the second-largest consumer of hap's state
// directory, and llm_output is not covered by the excerpt retention prune —
// while leaving room for a real stack trace or usage banner.
const maxLearnAuditOutput = 4000

// retryHint is appended to a failed run's rationale so the operator learns the
// recovery from the row itself rather than from documentation.
const retryHint = " — press `l` on this row's detail view to retry"

// learnOutcome carries a finished learn-from-correction run back into the main
// loop, where the audit row is written — daemon-owned table writes stay on the
// main loop. The request rides along so the in-flight guard can be cleared for
// the right agent.
type learnOutcome struct {
	req domain.LearnRequest
	// output is the run's transcript (stdout and stderr, labelled). NOTHING
	// reads it — it is carried to the audit row for the operator. Empty is
	// normal: the CLI's product is the file it edited, not text.
	output string
	err    error
}

// learnableCorrectionStatus reports whether an audit row is still LIVE enough
// for a correction against it to teach the agent — i.e. the operator is
// answering a standing escalation, not annotating history.
//
// Only "escalated" and "auto_accepting" qualify. The second is the same state
// mid-flight: the auto-accept sweep flips a pending escalation to
// auto_accepting while it delivers, and an operator answering in that window is
// still answering a live question.
//
// Everything else — a resolved row, an auto row, a dismissed one — is a
// post-hoc correction, which the audit tab's `c` key can record against a row
// of any age. That path is unsafe for this feature specifically: the run spawns
// a file-editing CLI in the cwd of whatever pane now answers to the row's
// AgentID, herdr recycles pane ids, and an AuditRecord carries no terminal_id
// to detect it. The learning in applyCorrection is unaffected — a post-hoc
// correction still records its decision and re-scores the signature exactly as
// before; only the CLI run is withheld.
func learnableCorrectionStatus(status string) bool {
	return status == "escalated" || status == domain.AuditStatusAutoAccepting
}

// learnPort returns the LLM adapter as a LearnFromUserPort when a
// learn-from-user CLI is configured, else nil (the capability is optional).
func (d *Daemon) learnPort() ports.LearnFromUserPort {
	lp, ok := d.llmPort().(ports.LearnFromUserPort)
	if !ok || !lp.LearnFromUserConfigured() {
		return nil
	}
	return lp
}

// learnFromUser spawns the learn-from-correction CLI for one committed
// correction. Like consultLLM the subprocess runs in a goroutine — it shells
// out to herdr for the agent's cwd and then to the CLI itself — and the outcome
// funnels back through learnResults.
//
// It is called from the main loop and returns immediately; every gate below is
// evaluated there, before the goroutine, so the in-flight mark and the spawn
// cannot race a second correction for the same agent.
// The timeout and template come from the LLM adapter, which main rebuilds from
// config on every reload, so no config.Config is threaded through here.
//
// It reports whether a run was actually SPAWNED. A correction ignores that (a
// skipped run is simply no lesson), but a RETRY must not be marked processed
// when nothing ran — the operator pressed a key and would otherwise get silence.
func (d *Daemon) learnFromUser(ctx context.Context, req domain.LearnRequest) bool {
	lp := d.learnPort()
	if lp == nil {
		return false
	}

	// The kill switch covers this too. Learning from a correction is not a
	// pane action, but it IS hap spawning a subprocess that edits files in the
	// operator's project — which is exactly what an operator who just paused
	// hap does not want happening behind them.
	kill, err := d.opt.Store.LatestKillEvent(ctx)
	if err != nil || domain.KillStateActive(kill) {
		slog.Info("learn-from-user skipped: automation paused", "agent", req.AgentID)
		return false
	}

	// One run per agent at a time. Corrections are human-paced, so this only
	// ever guards a burst — a batch resolve, or a backlog drained at daemon
	// start. Dropping the extra is right: those corrections are about the same
	// agent, and a second CLI editing the same memory file concurrently would
	// race the first.
	d.mu.Lock()
	if d.learnInFlight[req.AgentID] {
		d.mu.Unlock()
		slog.Info("learn-from-user skipped: run already in flight", "agent", req.AgentID)
		return false
	}
	// Marked before the spawn so two corrections in one batch cannot both pass
	// this gate. A spawn dropped because shutdown has latched leaves the mark
	// set — the same tolerated skip spawn documents for sweepInFlight and
	// paneCwdRefreshing, and harmless for the same reason: the map dies with
	// the process, and no further deliveries run.
	d.learnInFlight[req.AgentID] = true
	d.mu.Unlock()

	req.SessionID = domain.NewSessionID()
	if req.PaneExcerpt == "" {
		// Nothing to show the CLI beyond the two actions. Still worth running:
		// "you answered X, the operator wanted Y" is the lesson, and a legacy
		// audit row carrying no excerpt should not silently skip learning.
		slog.Debug("learn-from-user: audit row carries no pane excerpt", "agent", req.AgentID)
	}

	scheduled := d.spawn(func() {
		outcome := learnOutcome{req: req}
		err := logging.Guard("llm-learn-from-user", func() error {
			agentName, nerr := d.opt.Store.EnsureAgentName(ctx, req.AgentID)
			if nerr != nil {
				agentName = ""
			}
			outcome.req.AgentName = agentName
			// cwd comes from `pane get`, resolved HERE rather than through the
			// main loop's paneCwd cache: that cache refreshes in the background
			// and returns "" for a cold pane, and this is the one caller for
			// which an empty cwd is not cosmetic — it decides which project's
			// memory file the CLI edits. An adapter with no inspector surface
			// leaves it empty, which the adapter REFUSES to run on rather than
			// falling back — a wrong directory is worse than no lesson.
			if insp, ok := d.opt.Herdr.(ports.InspectorPort); ok {
				if pi, perr := insp.PaneInfo(ctx, req.AgentID); perr == nil {
					outcome.req.Cwd = pi.ForegroundCwd
					if outcome.req.Cwd == "" {
						outcome.req.Cwd = pi.Cwd
					}
				}
			}
			out, sessionID, lerr := learnWithSession(ctx, lp, outcome.req)
			// Keep the id the run ACTUALLY used, on the failure path too — a
			// run that timed out still left a transcript.
			outcome.req.SessionID = sessionID
			outcome.output = out
			return lerr
		})
		outcome.err = err
		select {
		case d.learnResults <- outcome:
			// handleLearnOutcome clears the in-flight mark on the main loop.
		case <-ctx.Done():
			// The outcome will never be handled, so clear the mark here or this
			// agent could never learn again. Only reachable during shutdown
			// today, but the flag is a latch and a latch that leaks is a silent
			// permanent disable — not something to leave resting on the caller.
			d.mu.Lock()
			delete(d.learnInFlight, req.AgentID)
			d.mu.Unlock()
		}
	})
	if !scheduled {
		// Shutdown latched between the gate and the spawn, so nothing will run
		// and no outcome will ever clear the mark. Clear it here and report the
		// truth, so a queued retry survives to the next daemon instead of being
		// marked processed against a run that never happened.
		d.mu.Lock()
		delete(d.learnInFlight, req.AgentID)
		d.mu.Unlock()
		slog.Info("learn-from-user skipped: daemon is shutting down", "agent", req.AgentID)
		return false
	}
	return true
}

// applyLearnRetry re-runs a failed learn-from-correction CLI from its audit
// row. The row is self-sufficient by construction: handleLearnOutcome stores the
// pane excerpt, the suggestion and the correction precisely so a retry needs
// nothing else. (One erosion: the audit-excerpt retention prune blanks
// pane_excerpt on resolved rows, and a learn row is resolved from birth — so a
// retry after that window rebuilds with an empty excerpt. That still runs; the
// correction and the suggestion, which carry the lesson, are never pruned.)
//
// The working directory is deliberately NOT taken from the row — learnFromUser
// re-resolves it live, so a retry minutes later still edits the project the
// agent is in now, or refuses if it cannot tell.
//
// The return value follows applyLLMRetry's contract: true means the queue item
// reached a terminal outcome and may be marked processed. TERMINAL here means
// the retry can never succeed as queued — no command configured, the agent's
// pane is gone, the pane now hosts a different agent type, or automation is
// paused (see below for why that one is not held). TRANSIENT — returning false
// so the item survives to the next sweep — means it could not run YET: a
// ListAgents error, or another learn run still in flight for this agent.
func (d *Daemon) applyLearnRetry(ctx context.Context, audit *domain.AuditRecord) bool {
	// Unconfigured is TERMINAL, unlike the transient skips below: the operator
	// removed llm.learn_from_user_command since the failure, so no amount of
	// re-sweeping will ever run this. Consume the item instead of queueing it
	// against a command that no longer exists.
	if d.learnPort() == nil {
		slog.Info("learn-from-user retry dropped: no learn command configured", "audit", audit.ID)
		return true
	}

	// The agent must still be there. An audit row carries no terminal_id, so a
	// retry on a stale row would resolve the working directory of whatever pane
	// now answers to that id — and this CLI edits files. Requiring the agent to
	// be live closes the common stale case (its pane is simply gone) and mirrors
	// what applyLLMRetry already does for a consult retry. A pane RECYCLED to a
	// different agent within the window is still not detectable here; the
	// adapter's live-directory check is the remaining backstop.
	agents, err := d.opt.Herdr.ListAgents(ctx)
	if err != nil {
		// Transient: leave it queued for the next sweep rather than burning it.
		slog.Error("learn-from-user retry: listing agents failed", "agent", audit.AgentID, "error", err)
		return false
	}
	var live *domain.AgentTransition
	for i := range agents {
		if agents[i].AgentID == audit.AgentID {
			live = &agents[i]
			break
		}
	}
	if live == nil {
		slog.Info("learn-from-user retry: agent no longer present", "agent", audit.AgentID)
		d.notify(ctx, "Herd Auto Prompter: retry skipped",
			fmt.Sprintf("Agent %s is no longer present — cannot re-run the learn command.", audit.AgentID))
		return true
	}
	// The one recycle signal available here: an agent id now answering as a
	// DIFFERENT type is certainly not the agent the lesson was about. It is a
	// partial check — a claude recycled to another claude still passes — but it
	// is free, and the alternative is pointing a file-editing CLI at whatever
	// project the new occupant is in. Note the label deliberately does not go
	// through EnsureAgentName: that map is keyed by pane id, so on a recycled id
	// it would print the OLD agent's name and read as if nothing were wrong.
	//
	// DifferentAgentType, not `!=`: "unknown" is a real stored VALUE, not an
	// absence — the daemon writes the literal string whenever herdr reported no
	// type, and it travels onto decisions, signature state and audit rows. A
	// bare comparison would read an "unknown" row against a live claude as a
	// recycle and drop the retry TERMINALLY, so pressing `l` again could never
	// recover it. Absence of evidence is not evidence of a different agent.
	if domain.DifferentAgentType(audit.AgentType, live.AgentType) {
		slog.Warn("learn-from-user retry: pane now hosts a different agent type",
			"agent", audit.AgentID, "was", audit.AgentType, "now", live.AgentType)
		d.notify(ctx, "Herd Auto Prompter: retry skipped",
			fmt.Sprintf("Pane %s now runs %s, not %s — refusing to write a lesson into a different agent's project.",
				audit.AgentID, live.AgentType, audit.AgentType))
		return true
	}

	// A PAUSED daemon drops the retry rather than holding it. Nothing ages an
	// llm_retries row, so a held one would survive restarts and then spawn a
	// file-editing CLI the moment the operator resumes — possibly days after
	// they pressed a key, which is precisely the surprise `hap pause` exists to
	// prevent. It would also pin the row's pane excerpt against the retention
	// prune for the whole pause. Telling the operator to retry after resuming is
	// both honest and cheap.
	kill, kerr := d.opt.Store.LatestKillEvent(ctx)
	if kerr != nil || domain.KillStateActive(kill) {
		// A read error takes the SAME branch as an active pause, matching
		// learnFromUser's posture: unable to confirm we are not paused reads as
		// paused. Terminal rather than queued, because a persistently failing
		// store read would otherwise loop the item every sweep forever while
		// telling the operator nothing.
		why := "Automation is paused"
		if kerr != nil {
			why = "hap could not read the pause state"
			slog.Warn("learn-from-user retry dropped: pause state unreadable", "audit", audit.ID, "error", kerr)
		} else {
			slog.Info("learn-from-user retry dropped: automation paused", "audit", audit.ID, "agent", audit.AgentID)
		}
		d.notify(ctx, "Herd Auto Prompter: retry skipped",
			why+" — resolve it, then press `l` again to re-run the learn command.")
		return true
	}

	slog.Info("learn-from-user retry requested", "audit", audit.ID, "agent", audit.AgentID)
	// Only a run that actually SPAWNED consumes the queue item. The remaining
	// skip is a run already in flight for this agent, which clears in seconds —
	// leaving the item queued makes the retry land on the next sweep instead of
	// evaporating, which is what the operator expects from pressing a key.
	return d.learnFromUser(ctx, domain.LearnRequest{
		AgentType:     audit.AgentType,
		AgentID:       audit.AgentID,
		SituationType: audit.SituationType,
		PaneExcerpt:   audit.PaneExcerpt,
		Suggestion:    audit.Suggestion,
		Correction:    audit.Input,
	})
}

// learnWithSession runs the learn CLI through SessionReportingLearner when the
// adapter offers it, degrading to the plain call (and the id we passed in)
// otherwise — the same optional-capability shape as consultWithSession.
func learnWithSession(ctx context.Context, lp ports.LearnFromUserPort, req domain.LearnRequest) (string, string, error) {
	if sr, ok := lp.(ports.SessionReportingLearner); ok {
		return sr.LearnFromUserWithSession(ctx, req)
	}
	out, err := lp.LearnFromUser(ctx, req)
	return out, req.SessionID, err
}

// handleLearnOutcome records what one learn-from-correction run did. Every
// outcome gets an audit row — including the declines and the failures: the run
// edits a file in the operator's project, so "it ran and changed nothing" must
// be distinguishable from "it never ran".
//
// Nothing else happens here. There is no escalation on failure (the operator
// already answered; a failed lesson is not a question for them), and no
// learning state is touched (applyCorrection already did that).
func (d *Daemon) handleLearnOutcome(ctx context.Context, res learnOutcome) {
	d.mu.Lock()
	delete(d.learnInFlight, res.req.AgentID)
	d.mu.Unlock()

	now := d.opt.Clock.Now()
	// Name the directory: the row exists to answer "why did this project's
	// memory file change?", which it cannot do without saying WHICH one.
	action := domain.AuditActionLearnRecorded
	rationale := "recorded a lesson from the operator's correction in " + res.req.Cwd
	if res.err != nil {
		action = domain.AuditActionLearnFailed
		rationale = "learn-from-user run failed: " + res.err.Error() + retryHint
		slog.Warn("learn-from-user failed", "agent", res.req.AgentID, "error", res.err)
	}
	// The CLI's output is NOT inspected — no sentinel, no decision, nothing
	// parsed. It rides along verbatim so the operator can read what the run
	// said (and, on a failure, why) from the row's detail view.

	// Through d.audit, not a bare AppendAudit: this row is the ONLY record that
	// a CLI ran and edited a file in the operator's project, so a dropped write
	// must at least be logged rather than vanish.
	d.audit(ctx, domain.AuditRecord{
		AgentID:       res.req.AgentID,
		AgentType:     res.req.AgentType,
		Trigger:       domain.TriggerLLMLearnFromUser,
		SituationType: res.req.SituationType,
		Action:        action,
		Rationale:     rationale,
		// LLMOutput is where the detail view renders a run's output, so the
		// transcript goes there and is truncated to keep audit rows bounded —
		// the tail is kept, since a failing CLI says why at the end.
		LLMOutput: domain.TailRunes(res.output, maxLearnAuditOutput),
		// The corrected action is what the lesson is about; recording it keeps
		// the row self-explanatory next to the correction lineage row.
		Input: res.req.Correction,
		// PaneExcerpt and Suggestion are stored so a RETRY can rebuild the
		// request from this row alone (see applyLearnRetry) — and so the detail
		// view shows the operator the screen the lesson was about.
		PaneExcerpt:  res.req.PaneExcerpt,
		Suggestion:   res.req.Suggestion,
		LLMSessionID: res.req.SessionID,
		Status:       "resolved",
		CreatedAt:    now,
	})
}
