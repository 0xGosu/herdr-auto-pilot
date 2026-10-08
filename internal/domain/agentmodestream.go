package domain

// AutonomousModeFor returns the mode an orchestrator should rotate an agent
// into from current, and false when the agent must be left where it is.
//
// Only the RESTRICTIVE end of each cycle is promoted — claude's manual, agy's
// default (its most restrictive mode, unlike codex's) — to the most
// autonomous mode the Shift+Tab cycle can reach. Everything else answers
// false, each for a reason:
//
//   - plan is never left by anyone but the operator: it is a deliberate "think
//     before touching anything", and flipping it would start the agent editing
//     a plan nobody approved;
//   - acceptEdits, auto and bypassPermissions are already past the restrictive
//     end, and codex's default IS its unrestricted mode — codex has nothing to
//     promote;
//   - unknown is never a mode (see AgentMode).
//
// claude's auto is not offered by every session (some models' sessions
// cycle manual/acceptEdits/plan), so a caller whose rotation to auto is
// refused falls back to acceptEdits; SetAgentMode detects the closed cycle and
// rotates the agent back before refusing.
//
// Accepted: an agy launched with --dangerously-skip-permissions paints no
// indicator and reads as default (AgyAgentMode), so it is offered acceptEdits
// too — one needless press into an agent that asks nothing either way.
func AutonomousModeFor(agentType string, current AgentMode) (AgentMode, bool) {
	switch modeAgentKind(agentType) {
	case "claude":
		if current == AgentModeManual {
			return AgentModeAuto, true
		}
	case AgentTypeAgy:
		if current == AgentModeDefault {
			return AgentModeAcceptEdits, true
		}
	}
	return AgentModeUnknown, false
}

// AgentModeStreamScope is the dedupe scope holding an agent's last announced
// mode. Keyed by agent (pane) id, so a recycled pane must forget it — the new
// tenant's mode is news even when it matches the old one's.
func AgentModeStreamScope(agentID string) string {
	return "agent.mode:" + agentID + ":"
}

// AgentModeStreamEvent builds the agent.mode event for one positive reading of
// an agent's mode (Kind, Fields and the dedupe pair; the caller stamps author
// and time). agent is the name every surface shows.
//
// observed is true for the daemon's own reading of the pane, false for a mode
// a hap command SET. Only an observation carries promote=<mode>: a mode the
// operator chose through hap is theirs to hold, and the orchestrator must not
// read its own announcement of one as an instruction to change it. Both kinds
// share the scope, so a set recorded first makes the daemon's matching
// observation a duplicate — which is what keeps the operator's choice from
// ever being announced as promotable.
func AgentModeStreamEvent(agentID, agent, agentType string, mode AgentMode, observed bool) StreamEvent {
	fields := []StreamField{StreamStr("agent", agent), StreamStr("mode", string(mode))}
	if observed {
		if target, ok := AutonomousModeFor(agentType, mode); ok {
			fields = append(fields, StreamStr("promote", string(target)))
		}
	}
	scope := AgentModeStreamScope(agentID)
	return StreamEvent{Kind: StreamAgentMode, Fields: fields, Dedupe: scope + string(mode), DedupeScope: scope}
}
