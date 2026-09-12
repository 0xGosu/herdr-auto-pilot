package daemon

import (
	"context"
	"log/slog"
	"sort"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/daemonhealth"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// agyWorkspaceNote is one agy agent observed working outside the directory it
// was started in.
type agyWorkspaceNote struct {
	// terminalID is what makes this safe to remember. herdr recycles pane ids
	// and an agent id IS a pane id, so without it a new tenant of the same
	// pane inherits the previous agent's warning and the operator is told to
	// restart an agent that was launched correctly.
	terminalID string
	name       string
	root       string
	elsewhere  string
	at         time.Time
}

// noteAgyWorkspace records that this agy agent is asking permission for files
// outside its own workspace root.
//
// Called from the classify path with the capture already in hand, so it costs
// no pane read: paneCwd is a cache that refreshes off the main loop and returns
// "" while cold, which AgyWorkspaceMismatch reads as "unknown" rather than as a
// mismatch. That is the important direction — a fresh agent must never be
// reported as misconfigured because its cwd had not been read yet.
//
// Deliberately not gated on the situation TYPE beyond agy: the prompt is
// recognized structurally from the pane, and an approval hap is about to answer
// autonomously is exactly the one an operator would otherwise never see.
func (d *Daemon) noteAgyWorkspace(ctx context.Context, s domain.Situation, agentName, pane string) {
	if !domain.IsAgy(s.AgentType) {
		return
	}
	path, ok := domain.AgyOutsideWorkspacePath(pane)
	if !ok {
		return
	}
	root := d.paneCwd(ctx, s.PaneID)
	if !domain.AgyWorkspaceMismatch(root, path) {
		return
	}
	note := agyWorkspaceNote{
		terminalID: s.TerminalID,
		name:       agentName,
		root:       root,
		elsewhere:  domain.AgyWorkspaceRootOf(path),
		at:         d.opt.Clock.Now(),
	}
	d.mu.Lock()
	prev, had := d.agyOutsideWorkspace[s.AgentID]
	d.agyOutsideWorkspace[s.AgentID] = note
	d.mu.Unlock()
	// Say it once per (agent, terminal, directory). The prompt repeats per
	// FILE — that is the whole complaint — so logging each one would reproduce
	// the noise this exists to explain.
	if had && prev.terminalID == note.terminalID && prev.elsewhere == note.elsewhere {
		return
	}
	slog.Info("agy agent is working outside the directory it was started in",
		"agent", s.AgentID, "started_in", note.root, "working_in", note.elsewhere)
}

// agyWorkspaceHealth reports the mismatches for the heartbeat, or nil when
// there are none — the same "absent when there is nothing to say" contract the
// other optional health sections keep, so an older reader and a healthy daemon
// look identical.
//
// Sorted by agent id so the status page does not reorder between reads over a
// map whose iteration order is random.
func (d *Daemon) agyWorkspaceHealth() []daemonhealth.AgyWorkspaceMismatch {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if len(d.agyOutsideWorkspace) == 0 {
		return nil
	}
	out := make([]daemonhealth.AgyWorkspaceMismatch, 0, len(d.agyOutsideWorkspace))
	for agentID, n := range d.agyOutsideWorkspace {
		out = append(out, daemonhealth.AgyWorkspaceMismatch{
			AgentID: agentID, Name: n.name,
			Root: n.root, Elsewhere: n.elsewhere, SeenAt: n.at,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AgentID < out[j].AgentID })
	return out
}
