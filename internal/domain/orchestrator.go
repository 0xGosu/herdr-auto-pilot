package domain

import (
	"fmt"
	"path/filepath"
	"time"
)

// The orchestrator is a dedicated interactive agent session the daemon keeps
// alive under full self-prompting (full_self_prompting.orchestrator_agent_command):
// it watches `hap stream orchestrator` and keeps the herd moving toward goals
// the operator types into it. hap itself never classifies, answers or hands
// work to it.
const (
	// OrchestratorAgentName is the herdr agent name — and the hap short name —
	// the session runs under. Exact: never generated, never suffixed.
	OrchestratorAgentName = "orchestrator"
	// OrchestratorWorkspaceLabel labels the herdr workspace the session is
	// created in, and is how an existing one is found again.
	OrchestratorWorkspaceLabel = "hap-orchestrator"
	// OrchestratorAuthor is the author a hap command carries when the
	// orchestrator session runs it. Its actions are an LLM's, so the daemon
	// screens them like its own unattended sends and refuses them while the
	// herd is paused, where an operator's are neither.
	OrchestratorAuthor = "orchestrator"
	// OrchestratorAgentKind is the only agent kind supported so far.
	OrchestratorAgentKind = "claude"
)

// OrchestratorLaunch splits the configured command into the herdr agent kind
// and the arguments passed to it.
//
// argv[0] is a KIND ASSERTION, not an executable: `herdr agent start --kind
// claude` always runs claude's canonical executable, so a path such as
// /usr/local/bin/claude contributes only its base name, which must be claude.
// Saying so here is what keeps an operator from believing a wrapper script
// named in argv[0] is what runs.
func OrchestratorLaunch(argv []string) (kind string, args []string, err error) {
	if len(argv) == 0 {
		return "", nil, fmt.Errorf("no orchestrator command configured")
	}
	if base := filepath.Base(argv[0]); base != OrchestratorAgentKind {
		return "", nil, fmt.Errorf("the orchestrator command must start with %q (got %q): herdr starts the agent "+
			"by KIND and always runs its own executable, so the first word names the kind and only the words after "+
			"it are passed on", OrchestratorAgentKind, argv[0])
	}
	return OrchestratorAgentKind, append([]string(nil), argv[1:]...), nil
}

// OrchestratorIdentity is the herdr session the daemon started (or adopted) as
// the orchestrator, persisted in the state directory so the daemon recognizes
// it from its very first event after a restart.
//
// It is NOT agent_names.terminal_id: that column is rewritten whenever a pane
// id is recycled, and carries the name and the disable onto the new tenant —
// exactly the record that cannot be trusted to say which terminal was ours.
type OrchestratorIdentity struct {
	PaneID      string    `json:"pane_id"`
	TerminalID  string    `json:"terminal_id"`
	WorkspaceID string    `json:"workspace_id,omitempty"`
	StartedAt   time.Time `json:"started_at"`
	// Briefed is set once the brief was delivered; BriefAttempts bounds the
	// retries of a delivery that keeps failing.
	Briefed       bool `json:"briefed"`
	BriefAttempts int  `json:"brief_attempts,omitempty"`
}

// Known reports whether the identity names a session at all.
func (id OrchestratorIdentity) Known() bool { return id.PaneID != "" }

// Matches reports whether tr comes from the orchestrator session. The terminal
// id decides when both sides carry one; otherwise the pane id does, because
// event-socket transitions carry no terminal id and a strict compare would
// let every one of them through.
func (id OrchestratorIdentity) Matches(tr AgentTransition) bool {
	if !id.Known() {
		return false
	}
	pane := tr.PaneID
	if pane == "" {
		pane = tr.AgentID
	}
	if pane != id.PaneID {
		return false
	}
	return tr.TerminalID == "" || id.TerminalID == "" || tr.TerminalID == id.TerminalID
}

// RecycledBy reports whether tr shows the orchestrator's pane now held by a
// DIFFERENT terminal — the orchestrator is gone and a new agent took its pane
// id. Only positive evidence counts: an unknown terminal on either side is not
// proof of anything.
func (id OrchestratorIdentity) RecycledBy(tr AgentTransition) bool {
	return id.Known() && tr.PaneID == id.PaneID &&
		tr.TerminalID != "" && id.TerminalID != "" && tr.TerminalID != id.TerminalID
}
