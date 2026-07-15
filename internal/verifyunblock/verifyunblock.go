// Package verifyunblock is the post-action self-check: after the plugin
// delivers a reply to a blocked agent (an approval/choice/error prompt), it
// re-queries the agent's status a short delay later and, if the agent is STILL
// blocked, appends a diagnostic audit row so the operator can see the action
// did not unblock the agent.
//
// The check degrades safely: a herdr read failure or a vanished pane never
// writes a false failure. It is defined against minimal interfaces so both the
// daemon (autonomous sends) and, indirectly via the daemon's correction drain,
// operator sends reuse one implementation — and it unit-tests without the full
// store or herdr adapter.
//
// LIMITATION: the check looks only at the agent's reported status
// (agent_status == "blocked"); it cannot distinguish the original prompt from a
// NEW one the agent raised after answering. An agent that answers and then
// immediately blocks on a follow-up prompt within the fixed one-second delay
// can trip a benign false-positive delivery_failed row. Operators should read
// delivery_failed as "still blocked shortly after the action."
package verifyunblock

import (
	"context"
	"fmt"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// StatusFailed is the audit Status recorded when a delivered action left the
// agent still blocked. It is distinct from the decision-audit statuses
// ("auto"/"escalated"/"resolved"/"dismissed") so it shows plainly in
// `hap audit` without matching the pending-escalation filters.
const StatusFailed = "delivery_failed"

// reasonStillBlocked prefixes the failure Rationale (mirrors the daemon's
// bracketed reason convention).
const reasonStillBlocked = "[still_blocked]"

// StatusLister reports the current agent set; herdr's ListAgents satisfies it.
// PaneInfo does NOT carry agent_status, so ListAgents is the on-demand source.
type StatusLister interface {
	ListAgents(ctx context.Context) ([]domain.AgentTransition, error)
}

// Auditer appends a diagnostic audit row. Only the daemon's store implements
// this (audit writes are daemon-owned); the frontend routes its checks through
// the daemon instead of calling this directly.
type Auditer interface {
	AppendAudit(ctx context.Context, a domain.AuditRecord) (int64, error)
}

// Params describes one delivered action to verify.
type Params struct {
	PaneID        string
	AgentID       string
	AgentType     string
	Signature     string
	Input         string // the reply that was delivered
	Excerpt       string // pane content the action was decided from
	SituationType domain.SituationType
}

// Relevant reports whether a situation type is one that leaves an agent
// blocked/waiting for input, so the self-check applies. idle is excluded — an
// idle agent is not blocked.
func Relevant(t domain.SituationType) bool {
	switch t {
	case domain.SituationApproval, domain.SituationChoice, domain.SituationError:
		return true
	}
	return false
}

// stillBlocked reports whether the named pane is present in agents and its
// reported status is "blocked".
func stillBlocked(agents []domain.AgentTransition, paneID string) bool {
	for _, a := range agents {
		if a.PaneID == paneID {
			return a.Status == "blocked"
		}
	}
	return false
}

// Check re-queries the agent's status and, when it is still blocked, appends a
// delivery_failed audit row. It returns whether the agent was still blocked and
// the new audit id (0 when none was written). A herdr list error or a pane no
// longer present resolves to "not blocked" and writes nothing — the check never
// invents a failure. The audit-write error (if any) is returned so the caller
// can log it; the blocked verdict is still reported.
func Check(ctx context.Context, herdr StatusLister, store Auditer, p Params, now time.Time) (bool, int64, error) {
	agents, err := herdr.ListAgents(ctx)
	if err != nil {
		return false, 0, fmt.Errorf("verify unblock: list agents: %w", err)
	}
	if !stillBlocked(agents, p.PaneID) {
		return false, 0, nil
	}
	id, err := store.AppendAudit(ctx, domain.AuditRecord{
		AgentID:       p.AgentID,
		AgentType:     p.AgentType,
		Signature:     p.Signature,
		Trigger:       "self-check",
		SituationType: p.SituationType,
		Action:        "unblock_failed:" + p.Input,
		Input:         p.Input,
		Rationale:     reasonStillBlocked + " agent still blocked after action",
		Status:        StatusFailed,
		PaneExcerpt:   p.Excerpt,
		CreatedAt:     now,
	})
	if err != nil {
		return true, 0, fmt.Errorf("verify unblock: append audit: %w", err)
	}
	return true, id, nil
}
