package daemon

import (
	"context"
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

// learnOutcome carries a finished learn-from-correction run back into the main
// loop, where the audit row is written — daemon-owned table writes stay on the
// main loop. The request rides along so the in-flight guard can be cleared for
// the right agent.
type learnOutcome struct {
	req domain.LearnRequest
	// output is the CLI's trimmed stdout, used only to recognize the @noop
	// decline. Empty is normal: the CLI's product is the file it edited.
	output string
	err    error
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
func (d *Daemon) learnFromUser(ctx context.Context, req domain.LearnRequest) {
	lp := d.learnPort()
	if lp == nil {
		return
	}

	// The kill switch covers this too. Learning from a correction is not a
	// pane action, but it IS hap spawning a subprocess that edits files in the
	// operator's project — which is exactly what an operator who just paused
	// hap does not want happening behind them.
	kill, err := d.opt.Store.LatestKillEvent(ctx)
	if err != nil || domain.KillStateActive(kill) {
		slog.Info("learn-from-user skipped: automation paused", "agent", req.AgentID)
		return
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
		return
	}
	d.learnInFlight[req.AgentID] = true
	d.mu.Unlock()

	req.SessionID = domain.NewSessionID()
	if req.PaneExcerpt == "" {
		// Nothing to show the CLI beyond the two actions. Still worth running:
		// "you answered X, the operator wanted Y" is the lesson, and a legacy
		// audit row carrying no excerpt should not silently skip learning.
		slog.Debug("learn-from-user: audit row carries no pane excerpt", "agent", req.AgentID)
	}

	d.spawn(func() {
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
			// memory file the CLI edits. Degrade to empty when the adapter has
			// no inspector surface (the adapter then falls back to its own
			// WorkDir).
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
	action := domain.AuditActionLearnRecorded
	rationale := "recorded a lesson from the operator's correction"
	switch {
	case res.err != nil:
		action = domain.AuditActionLearnFailed
		rationale = "learn-from-user run failed: " + res.err.Error()
		slog.Warn("learn-from-user failed", "agent", res.req.AgentID, "error", res.err)
	default:
		// The @noop sentinel is the CLI's explicit "no durable lesson here".
		// Reuse the shared line-wise stripper so the sentinel is recognized in
		// every spelling and shape the other paths accept ("@noop", "- noop",
		// "`NO-OP`"), rather than defining a second dialect.
		if _, declined := domain.StripNoopGeneratedLines(res.output); declined {
			action = domain.AuditActionLearnNoop
			rationale = "the agent judged the correction to carry no durable lesson"
		}
	}

	d.opt.Store.AppendAudit(ctx, domain.AuditRecord{
		AgentID:       res.req.AgentID,
		AgentType:     res.req.AgentType,
		Trigger:       domain.TriggerLLMLearnFromUser,
		SituationType: res.req.SituationType,
		Action:        action,
		Rationale:     rationale,
		// The corrected action is what the lesson is about; recording it keeps
		// the row self-explanatory next to the correction lineage row.
		Input:        res.req.Correction,
		LLMSessionID: res.req.SessionID,
		Status:       "resolved",
		CreatedAt:    now,
	})
}
