package daemon

// The full-self-prompting orchestrator: an interactive claude session named
// "orchestrator" the daemon keeps alive while the mode is on
// (full_self_prompting.orchestrator_agent_command), briefed to watch
// `hap stream orchestrator` and keep the herd moving toward goals the operator
// types into it.
//
// Two halves, and they fail in opposite directions:
//
//   - IGNORING it is unconditional once its identity is known. A disabled
//     agent is still read, classified and audited on every event; the
//     orchestrator must not be, so every ingest point returns early on
//     isOrchestrator and every sweep pass runs over withoutOrchestrator. The
//     identity lives in <state>/orchestrator.json so the filter is up before
//     the first event after a restart.
//   - CREATING it fails closed: the mode must be on and not stood down, the
//     kill switch clear (a read error counts as paused), the command must name
//     claude, and herdr must offer the launch capability. The mode and kill
//     switch are re-asked before each step that acts (the start, the brief),
//     because `agent start` alone may wait two minutes. It runs off the select
//     loop, one pass at a time, at most orchestratorMaxSpawnsPerHour spawns an
//     hour, with a doubling backoff after a failure. hap never closes a live
//     session — only a pane it opened for a start that failed.
//
// "The mode is on" means the operator's switch (enabled, not stood down at a
// ceiling), deliberately NOT fspActive's preconditions: a herd whose graduated
// rules or llm.command lapsed is one where FSP answers LESS, which is when the
// orchestrator is most useful, and the operator can turn it off with the same
// key.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	skilldoc "github.com/0xGosu/herdr-auto-pilot"
	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/daemonhealth"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/logging"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
)

const (
	orchestratorStateFile        = "orchestrator.json"
	orchestratorDirName          = "orchestrator"
	orchestratorMaxSpawnsPerHour = 3
	orchestratorBackoffMax       = 30 * time.Minute
	// orchestratorBriefAttempts bounds SENDS of the brief that failed; a pass
	// that found the composer not ready typed nothing and costs no attempt.
	orchestratorBriefAttempts  = 3
	orchestratorBriefReadLines = 80
)

// orchestratorBrief is the built-in brief (full_self_prompting.
// orchestrator_agent_prompt replaces it). {self} is this hap binary, expanded
// at send time — named ONCE, as the fallback for a session whose PATH has no
// hap, so every command reads as the plain `hap …` the skill documents.
//
// {skills} is expanded the same way, and it is expanded rather than written out
// because the answer depends on WHERE the session runs: hap installs both
// skills into its own <state>/orchestrator, and deliberately installs nothing
// into an orchestrator_agent_cwd the operator chose. Telling a session in the
// operator's directory to load a skill that is not there would send it after a
// file it can never find — and `hap --skill` prints only the hap document, so
// there is no fallback route to the orchestrator one at all.
const orchestratorBrief = `You are hap's herd orchestrator on this machine. hap (Herd Auto Prompter) watches every coding agent in this herdr session, answers their prompts from rules it has learned, and runs in full self-prompting mode, so anything it cannot answer itself is left for a human. Your job is to be that human's deputy: keep every agent in the herd unblocked and moving toward the goals the operator gives you in this conversation. hap ignores this session completely — nothing here is classified, answered or handed work.

If the ` + "`hap`" + ` CLI is not on your PATH, use the binary at {self} in place of ` + "`hap`" + ` in every command below.

Start by setting yourself up:
1. {skills}
2. Run ` + "`hap status`" + `, ` + "`hap agents`" + ` and ` + "`hap escalations`" + ` to survey the herd.
3. Start the Monitor tool on ` + "`hap stream orchestrator`" + `. It prints a ` + "`# … head=N`" + ` line, then one line per event: ` + "`<seq> <time> <kind> key=value … by=<author>`" + `. Remember the last seq you handled; if the monitor stops, restart it with ` + "`hap stream orchestrator --resume <that seq>`" + `. A ` + "`# gap`" + ` or ` + "`# reset`" + ` line means events were lost: re-survey.
4. Schedule an hourly health check with the CronCreate tool — a recurring job every hour whose prompt tells you to run ` + "`hap status`" + ` and ` + "`hap agents`" + ` and rescue what you find. Check CronList first so there is only ever one. It runs whether or not the stream said anything, because a stopped hap daemon and a hung agent are both silent: if ` + "`hap status`" + ` shows no running daemon, start it with ` + "`hap daemon --ensure`" + `; if an agent has sat working or blocked with no progress, read its screen with herdr and unblock it or tell the operator here.

Events carry ids only; fetch details with the CLI (` + "`hap escalations`" + `, ` + "`hap audit`" + `, ` + "`hap task <source> list`" + `, ` + "`hap signatures`" + `, ` + "`hap config show`" + `). The kinds that need you most: ` + "`escalation`" + ` (an agent is waiting on something hap would not answer), ` + "`task.*`" + ` and ` + "`task_source.*`" + ` (work to hand out or re-plan), and ` + "`daemon.started`" + ` (re-survey).

How to act:
- Answer an escalation with ` + "`hap confirm <id> --send`" + ` or ` + "`hap resolve <id> --action TEXT --send`" + `, or drop it with ` + "`hap dismiss <id>`" + `. Read the agent's screen first (herdr) — the answer must fit what is on screen now.
- Hand out or re-plan work through ` + "`hap task`" + ` (add, edit, done, send). Prompt an agent directly with herdr only when the task list cannot express it.
- While ` + "`pause.on`" + ` is in effect, do nothing but watch.
- On ` + "`fsp.off`" + `, delete the hourly health check with the CronDelete tool and stand by until the operator tells you otherwise. On ` + "`fsp.on`" + `, re-create it with CronCreate unless CronList shows it is still there.
- Never type into your own pane, never act on an agent hap reports as disabled, and never approve destructive or irreversible work (deleting data, force-pushing, dropping databases, production deploys). When unsure, leave it for the operator and say so here.
- hap knows these commands come from you: it screens what they would send with the same safety rules as its own unattended answers, and refuses them while the herd is paused. A refusal is final for that item — leave it for the operator; never retype it into the agent with herdr.

When you are set up, report the herd's state in a few lines and ask the operator what the goals are.`

// The two answers {skills} expands to. The first is the default, where hap owns
// the working directory and has put both skills in it; the second is an
// operator's own orchestrator_agent_cwd, where hap installs nothing — so the
// brief must not claim otherwise, and must say plainly that nothing will hand
// these documents back after a compaction.
const (
	orchestratorSkillsInstalled = "Load the `hap-orchestrator` skill: hap has installed it, and the `hap` skill, " +
		"into this working directory, so both are yours to re-read at any time — do that whenever your context is " +
		"compacted and this brief is gone. `hap --skill` prints the hap one if it is somehow missing. Read " +
		"`herdr --skill` too: it documents herdr (workspaces, panes, agents, reading and prompting an agent), " +
		"which hap does not."
	orchestratorSkillsAbsent = "Run `hap --skill` and `herdr --skill` and read both: they document the hap CLI " +
		"(status, escalations, tasks, rules, config) and herdr (workspaces, panes, agents, reading and prompting " +
		"an agent). This working directory is the operator's own (full_self_prompting.orchestrator_agent_cwd), so " +
		"hap installed no skills in it — keep notes on what those two documents say, because nothing here will " +
		"hand them back to you after your context is compacted."
)

// orchestratorState is the daemon's in-memory view of the orchestrator,
// guarded by its own mutex: the ingest filter reads it on the select loop
// while an ensure pass writes it from a goroutine.
type orchestratorState struct {
	mu sync.Mutex
	id domain.OrchestratorIdentity
	// running is the one-pass-at-a-time latch.
	running bool
	// spawns are the creation times within the last hour (the rate cap).
	spawns []time.Time
	// failures and retryAt are the doubling backoff after a failed pass.
	failures int
	retryAt  time.Time
	// lastErr and lastErrAt are why the last attempt failed, and waiting that
	// the brief is held by a claude prompt — both surfaced on the heartbeat
	// (orchestratorHealth) so the TUI and `hap status` show them.
	lastErr   string
	lastErrAt time.Time
	waiting   bool
	// Once-only notices, so a condition that holds for hours logs once.
	waitingNoted    bool
	noLauncherNoted bool
	badCommandNoted string
	// skillsCwdNoted and skillsErrNoted are the skill bootstrap's own once-only
	// notices: the pass runs every minute while a session is missing, and both
	// conditions hold until an operator changes something.
	skillsCwdNoted bool
	skillsErrNoted string
}

func (d *Daemon) orchestratorStatePath() string {
	if d.opt.StateDir == "" {
		return ""
	}
	return filepath.Join(d.opt.StateDir, orchestratorStateFile)
}

// loadOrchestrator restores the identity written by an earlier run. A missing
// or unreadable file means no known orchestrator — the ensure pass then asks
// herdr by name, so a lost file costs a lookup, never a duplicate session.
func (d *Daemon) loadOrchestrator() {
	path := d.orchestratorStatePath()
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Warn("orchestrator: could not read its identity file", "path", path, "error", err)
		}
		return
	}
	var id domain.OrchestratorIdentity
	if err := json.Unmarshal(data, &id); err != nil {
		slog.Warn("orchestrator: ignoring an unreadable identity file", "path", path, "error", err)
		return
	}
	d.orch.mu.Lock()
	d.orch.id = id
	d.orch.mu.Unlock()
}

// saveOrchestratorLocked persists id (or removes the file for an unknown one).
// Caller holds d.orch.mu, which orders two writers' files as their updates.
func (d *Daemon) saveOrchestratorLocked(id domain.OrchestratorIdentity) {
	path := d.orchestratorStatePath()
	if path == "" {
		return
	}
	if !id.Known() {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			slog.Warn("orchestrator: could not remove its identity file", "error", err)
		}
		return
	}
	data, err := json.MarshalIndent(id, "", "  ")
	if err == nil {
		tmp := path + ".tmp"
		if err = os.WriteFile(tmp, data, 0o600); err == nil {
			err = os.Rename(tmp, path)
		}
	}
	if err != nil {
		// The in-memory identity still filters for this run; only a restart
		// would forget it, and that falls back to the lookup by name.
		slog.Warn("orchestrator: could not persist its identity", "error", err)
	}
}

func (d *Daemon) orchestratorIdentity() domain.OrchestratorIdentity {
	d.orch.mu.Lock()
	defer d.orch.mu.Unlock()
	return d.orch.id
}

// setOrchestratorIdentity installs a NEW identity and resets the once-per-
// session notices.
func (d *Daemon) setOrchestratorIdentity(id domain.OrchestratorIdentity) {
	d.orch.mu.Lock()
	defer d.orch.mu.Unlock()
	d.orch.id = id
	d.orch.waitingNoted = false
	d.orch.waiting = false
	d.saveOrchestratorLocked(id)
}

// updateOrchestratorIfCurrent records progress (the brief) on id, unless the
// identity was replaced or released while the caller worked.
func (d *Daemon) updateOrchestratorIfCurrent(id domain.OrchestratorIdentity) {
	d.orch.mu.Lock()
	defer d.orch.mu.Unlock()
	if d.orch.id.PaneID != id.PaneID || d.orch.id.TerminalID != id.TerminalID {
		return
	}
	d.orch.id = id
	d.saveOrchestratorLocked(id)
}

// isOrchestrator reports whether tr comes from the orchestrator session.
func (d *Daemon) isOrchestrator(tr domain.AgentTransition) bool {
	d.orch.mu.Lock()
	defer d.orch.mu.Unlock()
	return d.orch.id.Matches(tr)
}

// withoutOrchestrator drops the orchestrator from an agent listing, returning
// the listing itself when it is not there.
func (d *Daemon) withoutOrchestrator(agents []domain.AgentTransition) []domain.AgentTransition {
	id := d.orchestratorIdentity()
	if !id.Known() {
		return agents
	}
	for i, a := range agents {
		if !id.Matches(a) {
			continue
		}
		out := make([]domain.AgentTransition, 0, len(agents)-1)
		out = append(out, agents[:i]...)
		for _, b := range agents[i+1:] {
			if !id.Matches(b) {
				out = append(out, b)
			}
		}
		return out
	}
	return agents
}

// orchestratorAlive reports whether the identity's session is in the listing.
func orchestratorAlive(id domain.OrchestratorIdentity, agents []domain.AgentTransition) bool {
	if !id.Known() {
		return false
	}
	for _, a := range agents {
		if id.Matches(a) {
			return true
		}
	}
	return false
}

// observeOrchestrator releases the identity when the listing shows its pane
// held by a DIFFERENT terminal: herdr recycled the pane id, and without the
// release the new tenant would be ignored as the orchestrator forever — and
// inherit its name and disable through SyncAgentTerminalID.
func (d *Daemon) observeOrchestrator(ctx context.Context, agents []domain.AgentTransition) {
	id := d.orchestratorIdentity()
	for _, a := range agents {
		if !id.RecycledBy(a) {
			continue
		}
		d.orch.mu.Lock()
		// Compared on the session, not the whole struct: a brief pass may have
		// advanced Briefed/BriefAttempts meanwhile, which changes nothing about
		// which pane this is.
		swapped := d.orch.id.PaneID == id.PaneID && d.orch.id.TerminalID == id.TerminalID
		if swapped {
			d.orch.id = domain.OrchestratorIdentity{}
			d.saveOrchestratorLocked(d.orch.id)
		}
		d.orch.mu.Unlock()
		if swapped {
			slog.Info("orchestrator: its pane now holds another agent; forgetting it", "pane", id.PaneID)
			d.releaseOrchestratorName(ctx, id.PaneID)
		}
		return
	}
}

// releaseOrchestratorName hands a pane that no longer holds the orchestrator
// back to ordinary treatment: a generated name, and automation re-enabled —
// both of which hap itself set when it claimed the pane.
func (d *Daemon) releaseOrchestratorName(ctx context.Context, paneID string) {
	names, err := d.opt.Store.AgentNames(ctx)
	if err != nil || names[paneID] != domain.OrchestratorAgentName {
		return
	}
	taken := func(n string) bool {
		for _, v := range names {
			if v == n {
				return true
			}
		}
		return false
	}
	if err := d.opt.Store.AssignAgentName(ctx, paneID, domain.GenerateAgentName(paneID, taken)); err != nil {
		slog.Warn("orchestrator: could not rename a released pane", "pane", paneID, "error", err)
	}
	if err := d.opt.Store.SetAgentDisabled(ctx, paneID, false); err != nil {
		slog.Warn("orchestrator: could not re-enable a released pane", "pane", paneID, "error", err)
	}
}

// orchestratorModeOn reports whether cfg asks for an orchestrator right now:
// the key set, and full self-prompting on and not stood down at its ceiling.
func (d *Daemon) orchestratorModeOn(cfg config.Config) bool {
	fsp := cfg.FullSelfPrompting
	if !fsp.Enabled || !fsp.OrchestratorConfigured() {
		return false
	}
	d.mu.RLock()
	latched := d.fspCeilingLatched
	d.mu.RUnlock()
	return !latched
}

// orchestratorPermitted re-asks everything a pass must hold before it ACTS,
// against the LIVE config rather than the one the pass started with: the
// operator may have turned the mode off, or it may have stood down, during a
// start that took minutes.
func (d *Daemon) orchestratorPermitted(ctx context.Context) bool {
	cfg, _, _ := d.snapshot()
	return d.orchestratorModeOn(cfg) && !d.orchestratorPaused(ctx)
}

// startOrchestratorPass schedules one ensure pass when there is anything to
// do. agents is a fresh listing the caller already holds, or nil to have the
// pass list for itself (startup, a reload that turned it on). Cheap and
// non-blocking: every shell-out happens in the spawned pass.
func (d *Daemon) startOrchestratorPass(agents []domain.AgentTransition) {
	cfg, _, _ := d.snapshot()
	if !d.orchestratorModeOn(cfg) {
		return
	}
	launcher, ok := d.opt.Herdr.(ports.AgentLauncher)
	if !ok {
		d.orch.mu.Lock()
		noted := d.orch.noLauncherNoted
		d.orch.noLauncherNoted = true
		d.orch.lastErr, d.orch.lastErrAt = "this herdr adapter cannot start agents", d.opt.Clock.Now()
		d.orch.mu.Unlock()
		if !noted {
			slog.Warn("orchestrator: this herdr adapter cannot start agents; no orchestrator will run")
		}
		return
	}
	now := d.opt.Clock.Now()
	d.orch.mu.Lock()
	if d.orch.running {
		d.orch.mu.Unlock()
		return
	}
	id := d.orch.id
	if agents != nil && orchestratorAlive(id, agents) && (id.Briefed || id.BriefAttempts >= orchestratorBriefAttempts) {
		d.orch.mu.Unlock()
		return // healthy: nothing to do
	}
	if now.Before(d.orch.retryAt) {
		d.orch.mu.Unlock()
		return
	}
	d.orch.running = true
	d.orch.mu.Unlock()
	release := func() {
		d.orch.mu.Lock()
		d.orch.running = false
		d.orch.mu.Unlock()
	}
	if !d.spawn(func() {
		defer release()
		_ = logging.Guard("orchestrator", func() error {
			d.ensureOrchestrator(d.shutdownCtx, launcher, cfg, agents)
			return nil
		})
	}) {
		release()
	}
}

// ensureOrchestrator is one pass: make sure the session exists, then that it
// has been briefed.
func (d *Daemon) ensureOrchestrator(ctx context.Context, launcher ports.AgentLauncher, cfg config.Config,
	agents []domain.AgentTransition) {
	if agents == nil {
		listed, err := d.opt.Herdr.ListAgents(ctx)
		if err != nil {
			slog.Warn("orchestrator: listing agents failed", "error", err)
			return
		}
		agents = listed
	}
	// Against the OLD identity, before anything can replace it: a pane recycled
	// while the daemon was down still carries the previous orchestrator's name
	// and disable (the terminal-id sync carries both onto the new tenant), and
	// once a new orchestrator is recorded nothing compares that pane again — an
	// operator's agent would stay disabled, its escalations auto-dismissed.
	d.observeOrchestrator(ctx, agents)
	if !d.orchestratorPermitted(ctx) {
		return
	}
	argv := cfg.FullSelfPrompting.OrchestratorAgentCommand
	kind, args, err := domain.OrchestratorLaunch(argv)
	if err != nil {
		d.orch.mu.Lock()
		noted := d.orch.badCommandNoted == strings.Join(argv, "\x00")
		d.orch.badCommandNoted = strings.Join(argv, "\x00")
		d.orch.lastErr, d.orch.lastErrAt = err.Error(), d.opt.Clock.Now()
		d.orch.mu.Unlock()
		if !noted {
			slog.Warn("orchestrator: not starting one", "error", err)
		}
		return
	}
	// Before the alive/launch branch on purpose: a session that already exists
	// — adopted, or simply surviving a daemon restart — needs the skills just
	// as much as one being created, and an UPGRADE reaches them no other way.
	// Run's startup pass is what makes that free: it passes a nil listing, so
	// startOrchestratorPass's "healthy, nothing to do" early-out cannot skip it.
	d.bootstrapOrchestratorSkills(kind, cfg.FullSelfPrompting.OrchestratorAgentCwd)
	id := d.orchestratorIdentity()
	if !orchestratorAlive(id, agents) {
		var ok bool
		if id, ok = d.launchOrchestrator(ctx, launcher, kind, args, cfg.FullSelfPrompting.OrchestratorAgentCwd, agents); !ok {
			return
		}
	}
	if !id.Briefed && id.BriefAttempts < orchestratorBriefAttempts {
		d.briefOrchestrator(ctx, id, cfg)
	}
}

// launchOrchestrator adopts the agent herdr already calls "orchestrator", or
// creates one. An ADOPTED session is never briefed: hap did not start it, and
// typing into a session somebody else started is exactly what the rest of this
// daemon refuses to do without evidence.
func (d *Daemon) launchOrchestrator(ctx context.Context, launcher ports.AgentLauncher, kind string, args []string,
	cwd string, agents []domain.AgentTransition) (domain.OrchestratorIdentity, bool) {
	now := d.opt.Clock.Now()
	tr, found, err := launcher.AgentByName(ctx, domain.OrchestratorAgentName)
	if errors.Is(err, ports.ErrLaunchUnsupported) {
		d.orchestratorUnsupported(now, err)
		return domain.OrchestratorIdentity{}, false
	}
	if err != nil {
		d.orchestratorFailed(now, "looking it up", err)
		return domain.OrchestratorIdentity{}, false
	}
	adopted := found
	if !found {
		if !d.orchestratorSpawnAllowed(now) {
			return domain.OrchestratorIdentity{}, false
		}
		dir, err := d.orchestratorDir(cwd)
		if err != nil {
			d.orchestratorFailed(now, "preparing its working directory", err)
			return domain.OrchestratorIdentity{}, false
		}
		pane, err := launcher.NewPaneInWorkspace(ctx, domain.OrchestratorWorkspaceLabel, domain.OrchestratorAgentName, dir)
		if err != nil {
			d.orchestratorFailed(now, "opening its pane", err)
			return domain.OrchestratorIdentity{}, false
		}
		// Ignored from its first event: the start takes seconds to minutes, and
		// the new pane's agent_detected and status events arrive meanwhile.
		d.provisionalOrchestrator(pane)
		// abandon closes the pane this pass opened, so a start that keeps
		// failing does not add a shell tab on every backoff step.
		abandon := func(what string, err error) {
			d.forgetProvisionalOrchestrator(pane)
			if cerr := launcher.ClosePane(ctx, pane); cerr != nil {
				slog.Warn("orchestrator: could not close the pane of a failed start", "pane", pane, "error", cerr)
			}
			if err != nil {
				d.orchestratorFailed(now, what, err)
			}
		}
		if !d.orchestratorPermitted(ctx) {
			abandon("", nil)
			return domain.OrchestratorIdentity{}, false
		}
		startErr := launcher.StartAgent(ctx, domain.OrchestratorAgentName, kind, pane, args)
		if errors.Is(startErr, ports.ErrLaunchUnsupported) {
			abandon("", nil)
			d.orchestratorUnsupported(now, startErr)
			return domain.OrchestratorIdentity{}, false
		}
		// A start that timed out waiting for readiness (a first-run prompt on
		// screen, say) may still have started the session: ask rather than
		// assume, so a timeout never causes a second one.
		tr, found, err = launcher.AgentByName(ctx, domain.OrchestratorAgentName)
		if err != nil || !found {
			if err == nil {
				err = fmt.Errorf("no agent named %s after the start", domain.OrchestratorAgentName)
			}
			abandon("starting it", errors.Join(startErr, err))
			return domain.OrchestratorIdentity{}, false
		}
		if tr.PaneID != pane {
			// Another session took the name between the lookup and the start
			// (herdr keeps names unique, so ours was refused). Never record or
			// brief it from here: the next pass finds it by name and adopts it
			// the way it adopts any session hap did not start — unbriefed.
			abandon("starting it", errors.Join(startErr, fmt.Errorf(
				"the name %s was taken by another session (pane %s) during the start",
				domain.OrchestratorAgentName, tr.PaneID)))
			return domain.OrchestratorIdentity{}, false
		}
		if startErr != nil {
			slog.Info("orchestrator: herdr reported a start error, but the session is up", "error", startErr)
		}
	}
	id := domain.OrchestratorIdentity{
		PaneID: tr.PaneID, TerminalID: tr.TerminalID, WorkspaceID: tr.WorkspaceID,
		StartedAt: now, Briefed: adopted,
	}
	d.setOrchestratorIdentity(id)
	d.claimOrchestratorName(ctx, tr.PaneID, agents)
	d.orchestratorSucceeded()
	if adopted {
		slog.Info("orchestrator: adopted the existing agent of that name; hap will not brief it", "pane", tr.PaneID)
	} else {
		slog.Info("orchestrator: started", "pane", tr.PaneID, "workspace", domain.OrchestratorWorkspaceLabel)
	}
	return id, true
}

// claimOrchestratorName gives the pane the hap name "orchestrator" and
// disables automation on it — the operator-visible half of "ignored", which
// every send gate also honours. The name is taken back from a row whose agent
// is gone (a previous orchestrator), never from a LIVE agent an operator
// chose to call that: the filter does not need the name, the operator's
// choice stands.
func (d *Daemon) claimOrchestratorName(ctx context.Context, paneID string, agents []domain.AgentTransition) {
	names, err := d.opt.Store.AgentNames(ctx)
	if err != nil {
		slog.Warn("orchestrator: could not read agent names", "error", err)
	} else {
		live := make(map[string]bool, len(agents))
		for _, a := range agents {
			live[a.AgentID] = true
		}
		heldByALiveAgent := false
		for holder, name := range names {
			if name != domain.OrchestratorAgentName || holder == paneID {
				continue
			}
			if live[holder] {
				heldByALiveAgent = true
				slog.Warn("orchestrator: a live agent already has the name orchestrator; leaving it be",
					"agent", holder)
				continue
			}
			d.releaseOrchestratorName(ctx, holder)
		}
		// UNIQUE(node_id, name) would refuse it anyway; skipping says why.
		named := false
		if !heldByALiveAgent {
			if err := d.opt.Store.AssignAgentName(ctx, paneID, domain.OrchestratorAgentName); err != nil {
				slog.Warn("orchestrator: could not name its pane", "pane", paneID, "error", err)
			} else {
				named = true
			}
		}
		// The disable needs a row to land on, and the ingest filter kept the
		// pane from ever getting one the ordinary way.
		if !named {
			if _, err := d.opt.Store.EnsureAgentName(ctx, paneID); err != nil {
				slog.Warn("orchestrator: could not register its pane", "pane", paneID, "error", err)
			}
		}
	}
	if err := d.opt.Store.SetAgentDisabled(ctx, paneID, true); err != nil {
		// Not fatal: the ingest filter is what ignores the session.
		slog.Warn("orchestrator: could not disable automation on its pane", "pane", paneID, "error", err)
	}
}

// briefOrchestrator sends the brief once the session's composer is proven
// ready and empty. The daemon never types into a claude modal: a first-run
// prompt (trusting a new directory, say) defers the brief to a later pass and
// tells the operator once.
//
// Delivered WITHOUT WithAgentAutomation, deliberately — the agent is disabled
// on purpose, and this is its own bootstrap. The mode and the kill switch are
// still re-asked immediately before the send.
func (d *Daemon) briefOrchestrator(ctx context.Context, id domain.OrchestratorIdentity, cfg config.Config) {
	reader, ok := d.opt.Herdr.(ports.VisiblePaneReader)
	if !ok {
		return
	}
	pane, err := reader.ReadPaneVisible(ctx, id.PaneID, orchestratorBriefReadLines)
	if err != nil {
		slog.Warn("orchestrator: reading its pane failed", "error", err)
		return
	}
	if sess, ok := domain.ClaudeSessionFromPane(pane); !ok || !sess.ComposerEmpty {
		d.noteOrchestratorWaiting(ctx)
		return
	}
	if !d.orchestratorPermitted(ctx) {
		return
	}
	err = ports.SendToAgent(ctx, d.opt.Herdr, id.PaneID, domain.OrchestratorAgentKind, d.orchestratorPrompt(cfg))
	id.BriefAttempts++
	d.orch.mu.Lock()
	if err != nil {
		d.orch.lastErr = fmt.Sprintf("sending the brief failed (attempt %d of %d): %v",
			id.BriefAttempts, orchestratorBriefAttempts, err)
		d.orch.lastErrAt = d.opt.Clock.Now()
	} else {
		d.orch.lastErr, d.orch.waiting = "", false
	}
	d.orch.mu.Unlock()
	if err != nil {
		slog.Warn("orchestrator: sending the brief failed", "attempt", id.BriefAttempts, "error", err)
	} else {
		id.Briefed = true
		slog.Info("orchestrator: briefed", "pane", id.PaneID)
	}
	d.updateOrchestratorIfCurrent(id)
}

// orchestratorPrompt renders the brief at send time: {self} is the binary
// running NOW, since an upgrade may have replaced the one the daemon started
// as, and {skills} is whether this session's working directory is one hap put
// the skills in. Both are expanded in an operator's own brief too, so a custom
// prompt can use either.
func (d *Daemon) orchestratorPrompt(cfg config.Config) string {
	text := cfg.FullSelfPrompting.OrchestratorAgentPrompt
	if text == "" {
		text = orchestratorBrief
	}
	self := "hap"
	if d.opt.ResolveSelf != nil {
		if p, err := d.opt.ResolveSelf(); err == nil && p != "" {
			self = p
		}
	}
	// The same condition bootstrapOrchestratorSkills installs on: hap's own
	// directory gets the skills, an operator's gets nothing.
	skills := orchestratorSkillsInstalled
	if cfg.FullSelfPrompting.OrchestratorAgentCwd != "" {
		skills = orchestratorSkillsAbsent
	}
	return strings.NewReplacer("{self}", self, "{skills}", skills).Replace(text)
}

// noteOrchestratorWaiting tells the operator, once per session, that the
// orchestrator is waiting on something only they should answer.
func (d *Daemon) noteOrchestratorWaiting(ctx context.Context) {
	d.orch.mu.Lock()
	noted := d.orch.waitingNoted
	d.orch.waitingNoted = true
	d.orch.waiting = true
	d.orch.mu.Unlock()
	if noted {
		return
	}
	msg := "the orchestrator session is waiting on a claude prompt in workspace " +
		domain.OrchestratorWorkspaceLabel + " — answer it once and hap sends the brief"
	slog.Info("orchestrator: " + msg)
	if d.opt.Notify != nil {
		if err := d.opt.Notify.Notify(ctx, "hap orchestrator", msg); err != nil {
			slog.Debug("orchestrator: notification failed", "error", err)
		}
	}
}

// orchestratorPaused reports whether the kill switch stands the pass down. A
// read error counts as paused: this pass creates sessions and types into one.
func (d *Daemon) orchestratorPaused(ctx context.Context) bool {
	kill, err := d.opt.Store.LatestKillEvent(ctx)
	if err != nil {
		slog.Warn("orchestrator: kill-switch read failed; standing down this pass", "error", err)
		return true
	}
	return domain.KillStateActive(kill)
}

// orchestratorSpawnAllowed enforces the rolling-hour cap and, when it allows a
// spawn, counts it.
func (d *Daemon) orchestratorSpawnAllowed(now time.Time) bool {
	d.orch.mu.Lock()
	defer d.orch.mu.Unlock()
	kept := d.orch.spawns[:0]
	for _, t := range d.orch.spawns {
		if now.Sub(t) < time.Hour {
			kept = append(kept, t)
		}
	}
	d.orch.spawns = kept
	if len(kept) >= orchestratorMaxSpawnsPerHour {
		d.orch.retryAt = kept[0].Add(time.Hour)
		d.orch.lastErr = fmt.Sprintf("re-created %d times in the last hour; hap is waiting before the next start",
			len(kept))
		d.orch.lastErrAt = now
		slog.Warn("orchestrator: started too often in the last hour; waiting before the next",
			"spawns", len(kept), "retry_at", d.orch.retryAt.Format(time.RFC3339))
		return false
	}
	d.orch.spawns = append(d.orch.spawns, now)
	return true
}

func (d *Daemon) orchestratorFailed(now time.Time, what string, err error) {
	d.orch.mu.Lock()
	d.orch.failures++
	delay := time.Minute << min(d.orch.failures-1, 5)
	if delay > orchestratorBackoffMax {
		delay = orchestratorBackoffMax
	}
	d.orch.retryAt = now.Add(delay)
	d.orch.lastErr, d.orch.lastErrAt = what+" failed: "+err.Error(), now
	d.orch.mu.Unlock()
	slog.Warn("orchestrator: "+what+" failed; retrying later", "retry_in", delay.String(), "error", err)
}

// orchestratorUnsupported stands the feature down for as long as the backoff
// allows, saying why once: a herdr without the verbs will not grow them.
func (d *Daemon) orchestratorUnsupported(now time.Time, err error) {
	d.orch.mu.Lock()
	noted := d.orch.noLauncherNoted
	d.orch.noLauncherNoted = true
	d.orch.retryAt = now.Add(orchestratorBackoffMax)
	d.orch.lastErr, d.orch.lastErrAt = "herdr cannot start agents here (agent get / agent start); upgrade herdr", now
	d.orch.mu.Unlock()
	if !noted {
		slog.Warn("orchestrator: herdr cannot start one; upgrade herdr to use orchestrator_agent_command", "error", err)
	}
}

// provisionalOrchestrator filters a just-opened pane by its pane id alone while
// its session starts. In memory only: persisted, a crash mid-start would leave
// an identity with no terminal id, which a recycled pane can never contradict.
func (d *Daemon) provisionalOrchestrator(paneID string) {
	d.orch.mu.Lock()
	defer d.orch.mu.Unlock()
	d.orch.id = domain.OrchestratorIdentity{PaneID: paneID}
}

// forgetProvisionalOrchestrator undoes provisionalOrchestrator after a failed
// start, unless something else has been recorded since.
func (d *Daemon) forgetProvisionalOrchestrator(paneID string) {
	d.orch.mu.Lock()
	defer d.orch.mu.Unlock()
	if d.orch.id.PaneID == paneID && d.orch.id.TerminalID == "" {
		d.orch.id = domain.OrchestratorIdentity{}
		d.saveOrchestratorLocked(d.orch.id)
	}
}

func (d *Daemon) orchestratorSucceeded() {
	d.orch.mu.Lock()
	d.orch.failures = 0
	d.orch.retryAt = time.Time{}
	d.orch.lastErr = ""
	d.orch.mu.Unlock()
}

// CallerIsOrchestrator reports whether a hap command running in herdr pane
// paneID (its HERDR_PANE_ID) is running inside the orchestrator session, by
// the identity the daemon records in stateDir. Any failure answers false:
// the command then acts as the operator, exactly as before this existed.
func CallerIsOrchestrator(stateDir, paneID string) bool {
	if stateDir == "" || paneID == "" {
		return false
	}
	data, err := os.ReadFile(filepath.Join(stateDir, orchestratorStateFile))
	if err != nil {
		return false
	}
	var id domain.OrchestratorIdentity
	return json.Unmarshal(data, &id) == nil && id.Known() && id.PaneID == paneID
}

// actionScreen is the outbound screen a queued action's text must pass: none
// for an operator — they saw the text, and their confirm is the gate — and
// the daemon's own never-auto and irreversibility screen for the orchestrator,
// an LLM whose text no human has seen.
//
// A send_task hand-out is the one kind screened by POLICY only
// (screenOutboundStrict): the operator's never_auto_patterns and the strict
// seeds, without the suspected-irreversible heuristic and without the action
// rules. Its text is an existing checklist item somebody WROTE as a task — prose
// instructing an agent — and both of the other halves judge something else. The
// heuristic seeds corroborate a destructive verb against a data target across a
// line or two because they read a pending pane operation, so over prose they
// refuse a task for discussing the work: observed live refusing a task that
// explained how to fix a failing CI run, and again refusing one that used the
// word "irreversible" while describing this very rule. The action rules are
// already excluded from a generated task prompt for exactly this reason — see
// actionRefused's comment: "matching them against prose an agent is being asked
// to do would refuse work for containing a phrase" — and a hand-out is prose,
// not a menu option.
//
// This narrows the ONLY screen a hand-out's text ever gets: `hap task add`
// screens nothing. What makes that sound is that the whole of the operator's
// declared policy is strict-kind and survives here; what is dropped is two
// shipped heuristics written to read a screen.
//
// Nothing changes for accept_generated_task, whose text the task-generator LLM
// INVENTED rather than was asked for, and nothing for the daemon's own FSP
// sends (screenOutbound, unchanged).
func (d *Daemon) actionScreen(a domain.AgentAction, agentType string) func(string) error {
	if a.Author != domain.OrchestratorAuthor {
		return nil
	}
	if a.Kind == domain.AgentActionSendTask {
		return func(text string) error {
			if err := d.screenOutboundStrict(agentType, text); err != nil {
				return fmt.Errorf("%w: %v", errOutboundRefused, err)
			}
			return nil
		}
	}
	return func(text string) error {
		if err := d.screenOutbound(agentType, text); err != nil {
			return fmt.Errorf("%w: %v", errOutboundRefused, err)
		}
		// The action rules too: the orchestrator CHOOSES this text, so a
		// widening menu option it picked is exactly what they exist to refuse.
		if why := d.actionRefused(agentType, text); why != "" {
			return fmt.Errorf("%w: matched never-auto action %s", errOutboundRefused, why)
		}
		return nil
	}
}

// refuseOrchestratorWhilePaused holds the orchestrator's actions to the kill
// switch the daemon's own sends honour. An operator's are exempt: resolving
// by hand while the herd is paused is what the pause is for. A read error
// refuses (fail closed).
func (d *Daemon) refuseOrchestratorWhilePaused(ctx context.Context, a domain.AgentAction) error {
	if a.Author != domain.OrchestratorAuthor {
		return nil
	}
	if d.orchestratorPaused(ctx) {
		return errors.New("the herd is paused, so hap will not act for the orchestrator; resume it or act as the operator")
	}
	return nil
}

// orchestratorDir is the session's working directory. The default,
// <state>/orchestrator, is created (0700) so claude's project memory for it is
// its own. An operator's full_self_prompting.orchestrator_agent_cwd is used as
// given (after ~ and $VAR expansion) and never created: a typo must not
// silently make a directory and start the session somewhere unintended.
func (d *Daemon) orchestratorDir(cwd string) (string, error) {
	if cwd == "" {
		dir := filepath.Join(d.opt.StateDir, orchestratorDirName)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", err
		}
		return dir, nil
	}
	if err := config.OrchestratorCwdProblem(cwd); err != nil {
		return "", fmt.Errorf("orchestrator_agent_cwd: %w", err)
	}
	return config.ExpandPath(cwd), nil
}

// bootstrapOrchestratorSkills puts the bundled hap and hap-orchestrator skills
// on disk in the orchestrator's working directory, so the session can recall
// them at any time — including after an automatic compaction has dropped the
// brief that told it to run `hap --skill`.
//
// It writes ONLY into the default <state>/orchestrator, the directory hap
// creates and owns. An operator's orchestrator_agent_cwd gets nothing: that is
// the same seam orchestratorDir already draws — hap does not create that
// directory either — and here it also means an unattended write never lands in
// somebody's repository, where a .claude/skills/hap/SKILL.md could overwrite a
// project skill of their own and show up in their `git status`.
//
// The directory is created here, by the act of installing into it — this pass
// runs whether or not a session is about to be started, so do not move the
// MkdirAll back to the launch path: that is exactly what left an upgraded
// binary's skills unreachable to a session nothing re-creates.
//
// Accepted limit: an ADOPTED session — one hap found by name rather than
// started — runs in whatever directory the operator started it in, which hap
// neither chose nor may write to, so its skills land in <state>/orchestrator
// where that session will not look for them. Deliberate rather than fixed: the
// alternative is writing into a directory an operator owns, which is the one
// thing this refuses to do. `hap skill install` is their route.
//
// Never fatal: a herd with no skills on disk is worse off than one with them,
// but far better off than one with no orchestrator. Failures are logged once
// per distinct message and the pass carries on to the start and the brief.
func (d *Daemon) bootstrapOrchestratorSkills(kind, cwd string) {
	if cwd != "" {
		d.orch.mu.Lock()
		noted := d.orch.skillsCwdNoted
		d.orch.skillsCwdNoted = true
		d.orch.mu.Unlock()
		if !noted {
			slog.Info("orchestrator: orchestrator_agent_cwd is yours, not hap's, so the bundled skills are not "+
				"written there; run `hap skill install` if you want them", "cwd", cwd)
		}
		return
	}
	if d.opt.StateDir == "" {
		return
	}
	dir, err := d.orchestratorDir("")
	var written []string
	if err == nil {
		written, err = skilldoc.InstallAgentSkills(dir, kind)
	}
	// Reported even alongside an error: a partial refresh is what landed.
	if len(written) > 0 {
		slog.Info("orchestrator: refreshed the skills in its working directory", "dir", dir, "files", len(written))
	}
	if err == nil {
		return
	}
	msg := err.Error()
	d.orch.mu.Lock()
	noted := d.orch.skillsErrNoted == msg
	d.orch.skillsErrNoted = msg
	d.orch.mu.Unlock()
	if !noted {
		slog.Warn("orchestrator: could not install its skills; it will have to run `hap --skill` instead",
			"error", err)
	}
}

// orchestratorHealth is the orchestrator's trouble for the heartbeat, or nil
// when there is none to show — including whenever the feature is off, so a
// failure from before the operator turned it off does not linger.
func (d *Daemon) orchestratorHealth() *daemonhealth.OrchestratorHealth {
	cfg, _, _ := d.snapshot()
	if !d.orchestratorModeOn(cfg) {
		return nil
	}
	d.orch.mu.Lock()
	defer d.orch.mu.Unlock()
	if d.orch.lastErr == "" && !d.orch.waiting {
		return nil
	}
	return &daemonhealth.OrchestratorHealth{
		LastError: d.orch.lastErr, LastErrorAt: d.orch.lastErrAt,
		Failures: d.orch.failures, RetryAt: d.orch.retryAt,
		Waiting: d.orch.waiting, Workspace: domain.OrchestratorWorkspaceLabel,
	}
}
