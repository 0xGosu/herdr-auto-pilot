// Package daemon runs the monitor loop: subscribe → classify → signature →
// decide (gate + safety) → act | escalate → log. The decision core stays
// pure; this package owns all side effects behind ports and is the
// exclusive writer of hot-path rows (signatures, agent_rate, error_retries,
// decisions, daemon audit records).
package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/0xGosu/herdr-auto-pilot/internal/buildinfo"
	"github.com/0xGosu/herdr-auto-pilot/internal/classify"
	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/control"
	"github.com/0xGosu/herdr-auto-pilot/internal/daemonhealth"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/embedder"
	"github.com/0xGosu/herdr-auto-pilot/internal/logging"
	"github.com/0xGosu/herdr-auto-pilot/internal/match"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
	"github.com/0xGosu/herdr-auto-pilot/internal/taskfile"
	"github.com/0xGosu/herdr-auto-pilot/internal/verifyunblock"
)

// unblockCheckDelay is fixed rather than operator-configurable: post-action
// delivery verification should behave consistently across installations.
const unblockCheckDelay = time.Second

// Options configures a Daemon.
type Options struct {
	ConfigPath        string
	ControlSocketPath string
	Store             ports.StorePort
	Herdr             ports.HerdrPort
	Events            ports.EventPort
	Notify            ports.NotifyPort
	LLM               ports.LLMPort
	// LLMFactory, when set, rebuilds the LLM port from the freshly loaded
	// config on every reload so llm.command/timeout edits apply live. It
	// takes precedence over the static LLM field.
	LLMFactory func(cfg config.Config) ports.LLMPort
	// Embedder turns salient text into vectors for semantic signature
	// matching (nil = text/exact matching only).
	Embedder ports.EmbedderPort
	// EmbedderFactory, when set, rebuilds the embedder from freshly loaded
	// config whenever the [embedding] section changes. Takes precedence
	// over the static Embedder field.
	EmbedderFactory func(cfg config.Config) ports.EmbedderPort
	// MatchIndexDir is where the disposable bleve match index lives
	// (typically <state>/match-index). Empty disables semantic matching.
	MatchIndexDir string
	// StateDir is where the daemon writes its heartbeat/health record
	// (daemonhealth). Empty disables health writing (tests that don't care).
	StateDir string
	Clock    ports.Clock
	// ReadTaskFile reads a declared task-source file (os.ReadFile in prod).
	ReadTaskFile func(path string) ([]byte, error)
	// MutateTaskFile applies one locked read-modify-write to a declared
	// task-source file (taskfile.Mutate in prod). The daemon only writes a task
	// file on the auto-send-when-idle path, where the delivered item must be
	// reserved "[-]" so it is never handed to a second agent.
	MutateTaskFile func(path string, fn func(content string) (string, error)) error
	// PaneReadLines is how much recent pane content classification sees.
	PaneReadLines int
}

// Daemon is the monitor/decide/act loop.
type Daemon struct {
	opt Options
	// verifyUnblockDelay is initialized from unblockCheckDelay. Tests in this
	// package shorten it to keep asynchronous verification coverage fast.
	verifyUnblockDelay time.Duration

	mu         sync.RWMutex
	cfg        config.Config
	neverAuto  *domain.NeverAutoList
	classifier *classify.Classifier
	llm        ports.LLMPort
	embedder   ports.EmbedderPort

	// matcher is the semantic match index; semanticReady gates resolution
	// until the background initSemantic has populated it. semanticGen
	// invalidates in-flight initSemantic runs superseded by a newer reload,
	// so a slow old run can never rebuild the index from stale rows or
	// re-enable matching the operator just disabled.
	matcher       *match.Matcher
	semanticReady atomic.Bool
	semanticGen   atomic.Int64

	transitions         chan domain.AgentTransition
	nudges              chan control.Kind
	llmResults          chan llmOutcome
	actionReviewResults chan actionReviewOutcome
	taskGenResults      chan taskGenOutcome
	sweepResults        chan sweepOutcome
	// delayedTr re-enters attention transitions whose capture delay has
	// elapsed; the pane read happens back on the main loop, like every
	// other pipeline entry point.
	delayedTr chan domain.AgentTransition

	// pendingCapture coalesces attention-event bursts per pane: one timer
	// per pane, latest event wins, exactly one capture fires per burst.
	// captureStarted marks panes whose first capture has fired, so later
	// events use the shorter event delay. Both guarded by mu.
	pendingCapture map[string]*captureEntry
	captureStarted map[string]bool

	// episodeHandled dedupes the subscribe-time reconcile (#49): a pane whose
	// current parked episode (blocked/idle/done) has already been surfaced
	// through the pipeline is not re-driven on every 60s sweep. Cleared on the
	// agent's next real "working" transition (genuine progress = new episode).
	// Guarded by mu.
	episodeHandled map[string]bool

	// firstConsult marks agents whose first LLM consult has fired, so the
	// first interaction can use command_start and every later one uses the
	// base template. Keyed by agentID (== paneID in herdr).
	// NOT reset on "detected": that event also fires on every subscriber
	// reconnect (pane-topology change), which would re-fire the kickoff
	// prompt mid-session for long-running agents. A genuinely new agent
	// almost always arrives in a new pane (new key → first=true naturally);
	// the rare pane-id REUSE is caught by resetRecycledPaneState, which acts on
	// herdr's terminal_id — a signal that means "different agent" and, unlike
	// "detected", never fires on a mere reconnect. Guarded by mu.
	firstConsult map[string]bool
	// firstTaskGen marks agents whose first idle task generation has fired, so
	// the first one can use task_generate_command_start. Same keying/semantics
	// as firstConsult; guarded by mu.
	firstTaskGen map[string]bool

	// configured flips after the first successful reload so reloadEmbedder
	// can tell first load from a config change.
	configured bool

	// lastAutoSend tracks our own sends so a subsequent "working"
	// transition is attributed to automation, not the human. lastAutoNoop
	// does the same for noop decisions: nothing was sent, but a
	// self-flapping agent resuming right after a noop is not human
	// interaction — without this marker every flap would reset the runaway
	// counter and a noop rule could fire silently forever (D3).
	lastAutoSend map[string]time.Time
	lastAutoNoop map[string]time.Time

	// actionReviewInFlight tracks the one live outbound action review per
	// agent; the token lets the outcome handler drop superseded results.
	// Guarded by mu alongside actionReviewSeq.
	actionReviewInFlight map[string]actionReviewFlight
	actionReviewSeq      uint64

	// sweepInFlight dedupes the one live multi-tab form sweep per agent
	// (guarded by mu); outcomes return through sweepResults.
	sweepInFlight map[string]bool

	// toggleAttempt records, per agent, the signature of the multi-select form
	// this daemon last started answering — the evidence that lets a later
	// delivery accept a tab whose boxes are ALREADY ticked. Without it,
	// "checked ⊆ chosen" is only an inference: an operator halfway through
	// ticking a form, having chosen a subset of what the rule chose, would
	// pass it and have their form completed and submitted for them. With it,
	// hap widens the baseline only where it can point at its own abandoned
	// attempt. Cleared once a delivery completes, so the NEXT form on that
	// agent starts from the strict baseline again. Deliberately in-memory:
	// after a restart hap can no longer prove the ticks are its own, and
	// falling back to "escalate" is the safe direction. Guarded by mu.
	toggleAttempt map[string]string

	// idleSince tracks how long each agent has been continuously parked at a
	// non-busy status, for the auto-send-when-idle poll. Refreshed from the
	// sweep's agent listing: an agent that is still parked on the SAME pane
	// keeps its original timestamp, anything else (busy again, new pane, gone)
	// drops the entry. Deliberately in-memory only — after a daemon restart the
	// clock restarts, which delays an auto-send rather than firing one early.
	// Guarded by mu.
	idleSince map[string]idleMark

	// autoTaskClaim pairs an agent with the specific pending task the idle poll
	// picked for it, so two agents driven in the SAME sweep can never race for
	// the same "[ ]" item — the file itself is not touched until delivery.
	// Cleared when the agent's episode resolves, when it stops being idle, or
	// after autoTaskClaimTTL. Guarded by mu.
	autoTaskClaim map[string]taskClaim

	// snapshotSaved caches which signatures already have a provenance
	// snapshot this daemon lifetime (guarded by mu), so the hot path skips
	// the no-op INSERT OR IGNORE write transaction on repeat sightings.
	snapshotSaved map[string]bool

	// wsNames caches the workspace id→name listing for task-source
	// workspace matching; refreshed after workspaceCacheTTL.
	wsNames   map[string]string
	wsNamesAt time.Time

	// paneCwds caches per-pane working directory for the {cwd} placeholder in
	// next_task_template; declaredTask runs on the main loop and must not shell
	// out to `pane get` on every event. Entries expire after workspaceCacheTTL
	// and are refreshed OFF the main loop. paneCwdRefreshing dedupes concurrent
	// background refreshes per pane.
	paneCwds          map[string]paneCwdEntry
	paneCwdRefreshing map[string]bool

	// Background lifecycle. bg tracks every goroutine and AfterFunc callback
	// the daemon spawns via spawn/afterFunc; Run awaits it (shutdownBackground)
	// before its deferred matcher/embedder Close — and before the caller closes
	// the Store — so no background work can touch a store or matcher after it is
	// closed (the source of the "database is closed" warnings and capture
	// flakiness). lifeMu guards closing + timers. closing latches at shutdown so
	// no new background work is scheduled. timers holds pending (unfired)
	// AfterFunc timers so shutdown can Stop them. shutdownCtx is cancelled at
	// shutdown for background work that must outlive a single pipeline ctx yet
	// still stop on daemon teardown (verify-unblock and semantic-init, which
	// previously rooted at context.Background() and so ignored shutdown).
	lifeMu         sync.Mutex
	closing        bool
	timers         map[*time.Timer]struct{}
	bg             sync.WaitGroup
	shutdownCtx    context.Context
	cancelShutdown context.CancelFunc
}

// idleMark is one agent's parked-since timestamp, pinned to the terminal it
// was observed on. Herdr REUSES pane ids and reports the recreated terminal
// behind one via a new terminal_id — the same signal SyncAgentTerminalID
// treats as a lifecycle reset. Both are compared, so a fresh agent landing on
// a recycled pane starts its own idle clock instead of inheriting the previous
// occupant's age and being handed work before a full minute of idle.
// terminalID is empty on older herdr and on event-socket transitions, where
// the pane id alone remains the best available identity.
type idleMark struct {
	paneID     string
	terminalID string
	at         time.Time
}

// taskClaim is the pending task the idle poll assigned to one agent, plus the
// source it came from and when the claim was made (for the TTL sweep).
type taskClaim struct {
	sourcePath string
	taskText   string
	at         time.Time
}

// paneCwdEntry is one cached pane working directory with its capture time.
type paneCwdEntry struct {
	cwd string
	at  time.Time
}

type llmOutcome struct {
	situation domain.Situation
	sig       domain.SignatureResult
	request   domain.LLMRequest
	decision  *domain.LLMDecision
	err       error
}

// actionReviewFlight is the registry entry for one in-flight outbound action
// review. requestID names the staged llm_requests row so a cancelled flight
// can expire it (a lingering pending row would block other consults for this
// agent until expireStaleLLMWork reclaims it).
type actionReviewFlight struct {
	signature string
	requestID string
	token     uint64
	cancel    context.CancelFunc
}

// actionReviewOutcome carries a finished action review back into the main
// loop. The fallback template is snapshotted at handoff so a config reload
// mid-flight cannot change the failure behavior of an already-launched
// review.
type actionReviewOutcome struct {
	situation domain.Situation
	sig       domain.SignatureResult
	tr        domain.AgentTransition
	dec       domain.Decision
	learned   string // original learned form for RecordDecision
	fallback  string // snapshotted rewrite_action_fallback_template
	agentName string // agent short name, for {agent_name}
	request   domain.LLMRequest
	decision  *domain.LLMDecision
	err       error
	token     uint64
}

// taskGenOutcome carries a finished idle task generation back into the main
// loop: the suggested task text, or an error the daemon surfaces as a
// retryable escalation. The request rides along so the pending guard can be
// cleared exactly like a consult.
type taskGenOutcome struct {
	situation domain.Situation
	sig       domain.SignatureResult
	tr        domain.AgentTransition
	request   domain.LLMRequest
	task      string
	err       error
	// reason is the escalate reason that triggered this generation —
	// ReasonNoTaskSource (no source at all) or ReasonTaskSourceExhausted (a
	// declared source matched but ran out) — carried through so the eventual
	// success escalation is tagged with the reason that actually applies.
	reason domain.EscalateReason
}

// sweepOutcome carries a finished multi-tab form sweep back into the main
// loop: the situation with content/options aggregated across all tabs, or —
// degraded — the original single-frame situation plus the failure reason
// (a degraded capture escalates; it must never feed an LLM consult).
type sweepOutcome struct {
	situation domain.Situation
	tr        domain.AgentTransition
	agentName string
	degraded  bool
	reason    string
}

// New creates a daemon.
func New(opt Options) (*Daemon, error) {
	if opt.Clock == nil {
		opt.Clock = ports.SystemClock{}
	}
	if opt.ReadTaskFile == nil {
		opt.ReadTaskFile = os.ReadFile
	}
	if opt.MutateTaskFile == nil {
		opt.MutateTaskFile = func(path string, fn func(string) (string, error)) error {
			// Bounded: this runs on the main select loop, so waiting behind
			// another hap process's file lock must never stall every agent.
			_, err := taskfile.MutateWithin(path, taskLockWait, fn)
			return err
		}
	}
	if opt.PaneReadLines <= 0 {
		opt.PaneReadLines = 50
	}
	d := &Daemon{
		opt:                  opt,
		verifyUnblockDelay:   unblockCheckDelay,
		transitions:          make(chan domain.AgentTransition, 256),
		nudges:               make(chan control.Kind, 16),
		llmResults:           make(chan llmOutcome, 16),
		actionReviewResults:  make(chan actionReviewOutcome, 16),
		taskGenResults:       make(chan taskGenOutcome, 16),
		sweepResults:         make(chan sweepOutcome, 16),
		delayedTr:            make(chan domain.AgentTransition, 256),
		pendingCapture:       map[string]*captureEntry{},
		captureStarted:       map[string]bool{},
		episodeHandled:       map[string]bool{},
		firstConsult:         map[string]bool{},
		firstTaskGen:         map[string]bool{},
		lastAutoSend:         map[string]time.Time{},
		lastAutoNoop:         map[string]time.Time{},
		actionReviewInFlight: map[string]actionReviewFlight{},
		sweepInFlight:        map[string]bool{},
		toggleAttempt:        map[string]string{},
		idleSince:            map[string]idleMark{},
		autoTaskClaim:        map[string]taskClaim{},
		snapshotSaved:        map[string]bool{},
		paneCwds:             map[string]paneCwdEntry{},
		paneCwdRefreshing:    map[string]bool{},
		embedder:             opt.Embedder,
		timers:               map[*time.Timer]struct{}{},
	}
	// The shutdown context must exist before reload(): reload spawns the
	// semantic-init goroutine, which roots at shutdownCtx so daemon teardown
	// stops it. It is a child of Background (not a Run ctx, which does not yet
	// exist) and is cancelled by shutdownBackground.
	d.shutdownCtx, d.cancelShutdown = context.WithCancel(context.Background())
	if opt.MatchIndexDir != "" {
		d.matcher = match.New(opt.MatchIndexDir)
	}
	if err := d.reload(); err != nil {
		d.cancelShutdown()
		return nil, err
	}
	return d, nil
}

// spawn runs fn on a tracked background goroutine so shutdownBackground can
// await it. A spawn after shutdown has begun is dropped (fn never runs): the
// daemon is tearing down and nothing new may touch the store/matcher. Callers
// that reserve d.mu-guarded state before spawning and release it in fn's defer
// (sweepInFlight, paneCwdRefreshing) must tolerate that release being skipped
// on a dropped spawn — harmless here, since it only happens once closing has
// latched and no further deliveries run.
func (d *Daemon) spawn(fn func()) {
	d.lifeMu.Lock()
	if d.closing {
		d.lifeMu.Unlock()
		return
	}
	d.bg.Add(1)
	d.lifeMu.Unlock()
	go func() {
		defer d.bg.Done()
		fn()
	}()
}

// afterFunc schedules fn after delay on a tracked timer so shutdownBackground
// can Stop still-pending ones and await any already-firing callback. It returns
// the timer for callers that supersede it (stopTimer), or nil if the daemon is
// already shutting down (fn is never scheduled). The callback removes itself
// from the registry before running so a concurrent stopTimer/shutdown does not
// double-count its WaitGroup slot.
func (d *Daemon) afterFunc(delay time.Duration, fn func()) *time.Timer {
	d.lifeMu.Lock()
	if d.closing {
		d.lifeMu.Unlock()
		return nil
	}
	d.bg.Add(1)
	var t *time.Timer
	t = time.AfterFunc(delay, func() {
		d.lifeMu.Lock()
		delete(d.timers, t)
		d.lifeMu.Unlock()
		defer d.bg.Done()
		fn()
	})
	d.timers[t] = struct{}{}
	d.lifeMu.Unlock()
	return t
}

// stopTimer cancels a tracked timer scheduled by afterFunc (used to supersede a
// pending capture, and by shutdownBackground to drain unfired timers). If Stop
// wins the race — the callback had not started — it releases that timer's
// WaitGroup slot, since the callback will never run to release it itself. A
// callback already past its own registry-delete releases its own slot.
func (d *Daemon) stopTimer(t *time.Timer) {
	if t == nil {
		return
	}
	d.lifeMu.Lock()
	_, tracked := d.timers[t]
	if tracked {
		delete(d.timers, t)
	}
	d.lifeMu.Unlock()
	if tracked && t.Stop() {
		d.bg.Done()
	}
}

// shutdownBackground latches closing, cancels shutdownCtx, drains pending
// timers, and awaits every tracked goroutine/callback. Run calls it before its
// deferred matcher/embedder Close (and hence before the caller's Store.Close),
// so teardown never races live background work against a closed store or index.
func (d *Daemon) shutdownBackground() {
	d.lifeMu.Lock()
	d.closing = true
	pending := make([]*time.Timer, 0, len(d.timers))
	for t := range d.timers {
		pending = append(pending, t)
	}
	d.lifeMu.Unlock()

	d.cancelShutdown() // stop shutdownCtx-rooted work (verify-unblock, semantic-init)
	for _, t := range pending {
		d.stopTimer(t)
	}
	d.bg.Wait()
}

// reload re-reads TOML config and rebuilds derived state (classifier,
// never-auto list). Malformed config keeps the previous good state.
func (d *Daemon) reload() error { return d.reloadWith(false) }

// reloadWith(forceEmbedder=true) additionally rebuilds the embedder from a
// FRESH instance even when the [embedding] config is unchanged — a new
// instance starts with a clean degraded-failure latch, so a re-embed
// request can retry a model that previously failed. (With a static
// Options.Embedder — tests — there is nothing to swap; the pass still
// re-runs initSemantic.)
func (d *Daemon) reloadWith(forceEmbedder bool) error {
	cfg, err := config.Load(d.opt.ConfigPath)
	if err != nil {
		slog.Error("config reload failed; keeping previous config", "error", err)
		return err
	}
	// A reload also follows signature deletion: drop the snapshot cache so
	// a re-learned rule re-captures its situation.
	d.mu.Lock()
	if d.snapshotSaved != nil {
		d.snapshotSaved = map[string]bool{}
	}
	d.mu.Unlock()
	allow, errs := domain.NewNeverAutoList(!cfg.Safety.DisableNeverAutoSeedPatterns,
		cfg.Safety.NeverAutoPatterns, neverAutoRules(cfg.Safety))
	for _, e := range errs {
		slog.Warn("never-auto pattern rejected", "error", e)
	}
	cls := classify.New(cfg.Classifier)

	llmPort := d.opt.LLM
	if d.opt.LLMFactory != nil {
		llmPort = d.opt.LLMFactory(cfg)
	}

	d.mu.Lock()
	prev, first := d.cfg, !d.configured
	d.configured = true
	d.cfg = cfg
	d.neverAuto = allow
	d.classifier = cls
	d.llm = llmPort
	d.mu.Unlock()

	d.reloadEmbedder(prev, cfg, first || forceEmbedder)
	slog.Info("configuration loaded", "path", d.opt.ConfigPath)
	return nil
}

// readVisible returns the pane's current on-screen content when the adapter
// supports it (needed to see a standing menu, which the consuming "recent"
// read can miss), falling back to ReadPane otherwise.
func (d *Daemon) readVisible(ctx context.Context, paneID string, lines int) (string, error) {
	if vr, ok := d.opt.Herdr.(ports.VisiblePaneReader); ok {
		return vr.ReadPaneVisible(ctx, paneID, lines)
	}
	return d.opt.Herdr.ReadPane(ctx, paneID, lines)
}

// llmPort returns the current LLM port (rebuilt on reload when a factory is
// configured).
func (d *Daemon) llmPort() ports.LLMPort {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.llm
}

func (d *Daemon) snapshot() (config.Config, *domain.NeverAutoList, *classify.Classifier) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.cfg, d.neverAuto, d.classifier
}

// embedderState reports the current semantic-matching health for the heartbeat
// record. A hard native abort never reaches here (it kills the process first);
// this only distinguishes disabled / starting / soft-degraded / ready.
func (d *Daemon) embedderState() daemonhealth.EmbedderState {
	state, _ := d.embedderHealth()
	return state
}

// embedderHealth reports the semantic-matching state plus, when the engine
// offers it, the diagnostics explaining a degrade (timeout counts, effective
// budgets, last error). Diagnostics() is an optional accessor, type-asserted
// like Degraded() so alternate embedders and fakes keep compiling.
func (d *Daemon) embedderHealth() (daemonhealth.EmbedderState, *daemonhealth.EmbedderDiag) {
	d.mu.RLock()
	disabled := d.cfg.Embedding.Disabled
	d.mu.RUnlock()
	if disabled {
		return daemonhealth.EmbedderDisabled, nil
	}
	var diag *daemonhealth.EmbedderDiag
	degraded := false
	if emb := d.embedderPort(); emb != nil {
		if dg, ok := emb.(interface{ Diagnostics() embedder.Diagnostics }); ok {
			s := dg.Diagnostics()
			degraded = s.Degraded
			diag = &daemonhealth.EmbedderDiag{
				ConsecutiveFailures: s.ConsecutiveFailures,
				MaxFailures:         s.MaxFailures,
				Timeouts:            s.Timeouts,
				Failures:            s.Failures,
				LastError:           s.LastError,
				EmbedTimeoutMs:      int(s.EmbedTimeout / time.Millisecond),
				WarmTimeoutMs:       int(s.WarmTimeout / time.Millisecond),
				TimeoutBound:        s.TimeoutBound(),
			}
		} else if dg, ok := emb.(interface{ Degraded() bool }); ok {
			degraded = dg.Degraded()
		}
	}
	if degraded {
		return daemonhealth.EmbedderDegraded, diag
	}
	// Keep the diagnostics on a healthy embedder too: a run of timeouts that
	// has not yet latched is exactly the early warning an operator wants.
	if d.semanticReady.Load() {
		return daemonhealth.EmbedderReady, diag
	}
	return daemonhealth.EmbedderStarting, diag
}

// writeHealth refreshes the heartbeat file (best-effort; a failed write is
// logged once at debug and never disturbs the loop). No-op without a StateDir.
func (d *Daemon) writeHealth(startedAt time.Time) {
	if d.opt.StateDir == "" {
		return
	}
	state, diag := d.embedderHealth()
	h := daemonhealth.Health{
		PID:          os.Getpid(),
		Version:      buildinfo.Version,
		StartedAt:    startedAt,
		HeartbeatAt:  d.opt.Clock.Now(),
		Embedder:     state,
		EmbedderDiag: diag,
	}
	if err := daemonhealth.Write(d.opt.StateDir, h); err != nil {
		slog.Debug("heartbeat write failed", "error", err)
	}
}

// Run drives the daemon until ctx is done. It never panics: every handler
// runs under the fail-safe guard (NFR-004).
func (d *Daemon) Run(ctx context.Context) error {
	// Heartbeat first: publish a fresh health record (stamped with THIS pid)
	// as the very first action, before the slower socket/subscriber setup, so
	// a status check during startup sees this daemon's beat — not a dead
	// predecessor's stale file left by a hard abort. Refreshed on a ticker
	// below; removed on clean shutdown so a graceful stop reads as gone.
	startedAt := d.opt.Clock.Now()
	d.writeHealth(startedAt)
	if d.opt.StateDir != "" {
		defer func() { _ = daemonhealth.Remove(d.opt.StateDir) }()
	}

	// Background drain + native-resource release, registered BEFORE any fallible
	// setup so they run on EVERY Run return — including an early error return
	// below (e.g. control-socket setup). New() has already spawned initSemantic
	// via reloadEmbedder, so a bare early return would otherwise leave that
	// tracked goroutine touching the store/matcher while the caller closes them.
	// LIFO: shutdownBackground (drain) is registered last so it runs FIRST, then
	// the matcher/embedder Close.
	defer func() {
		if emb := d.embedderPort(); emb != nil {
			emb.Close()
		}
		if d.matcher != nil {
			d.matcher.Close()
		}
	}()
	defer d.shutdownBackground()

	// Control socket: reload/wake nudges from front-ends and mcp.
	var ctl *control.Server
	if d.opt.ControlSocketPath != "" {
		var err error
		ctl, err = control.NewServer(d.opt.ControlSocketPath, func(k control.Kind) {
			select {
			case d.nudges <- k:
			default:
			}
		})
		if err != nil {
			return fmt.Errorf("control socket: %w", err)
		}
		defer ctl.Close()
	}

	// Event subscription with reconnect/backoff lives in its own goroutine.
	d.spawn(func() {
		err := logging.Guard("event-subscriber", func() error {
			return d.opt.Events.Subscribe(ctx, d.transitions)
		})
		if err != nil && ctx.Err() == nil {
			slog.Error("event subscriber terminated", "error", err)
		}
	})

	// Consume corrections that accumulated while the daemon was down (a
	// failed front-end nudge is non-fatal by design), and keep a slow
	// periodic sweep as a safety net.
	logging.Guard("startup-corrections", func() error {
		d.processCorrections(ctx)
		d.processLLMRetries(ctx)
		d.expireStaleLLMWork(ctx)
		d.reconcileAttention(ctx)
		return nil
	})
	sweep := time.NewTicker(time.Minute)
	defer sweep.Stop()

	// Heartbeat ticker: refresh the health record (written once above at
	// startup) so out-of-process status/TUI can tell a live, progressing
	// daemon from a hung one.
	heartbeat := time.NewTicker(daemonhealth.HeartbeatInterval)
	defer heartbeat.Stop()

	slog.Info("daemon running", "version", buildinfo.Version)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-heartbeat.C:
			d.writeHealth(startedAt)
		case <-sweep.C:
			logging.Guard("periodic-sweep", func() error {
				d.processCorrections(ctx)
				d.processLLMRetries(ctx)
				d.expireStaleLLMWork(ctx)
				// One agent listing feeds both passes: the reconcile
				// re-drives parked episodes, then the idle poll hands a
				// pending task to agents that have been idle too long.
				agents, err := d.opt.Herdr.ListAgents(ctx)
				if err != nil {
					slog.Error("sweep: listing agents failed", "error", err)
					return nil
				}
				d.reconcileAttentionWith(ctx, agents)
				d.autoSendIdleTasks(ctx, agents)
				return nil
			})
		case tr := <-d.transitions:
			logging.Guard("pipeline", func() error {
				d.handleTransition(ctx, tr)
				return nil
			})
		case tr := <-d.delayedTr:
			logging.Guard("pipeline", func() error {
				d.handleAttention(ctx, tr)
				return nil
			})
		case kind := <-d.nudges:
			logging.Guard("nudge", func() error {
				if target, ok := control.CaptureTarget(kind); ok {
					d.captureLiveAgent(ctx, target)
				} else {
					switch kind {
					case control.KindReload:
						d.reload()
					case control.KindReembed:
						// Operator-requested re-compute: force a fresh embedder
						// (clean degraded latch) and re-run the semantic init.
						d.reloadWith(true)
					}
				}
				d.processCorrections(ctx)
				d.processLLMRetries(ctx)
				d.expireStaleLLMWork(ctx)
				d.reconcileAttention(ctx)
				return nil
			})
		case res := <-d.llmResults:
			logging.Guard("llm-result", func() error {
				d.handleLLMOutcome(ctx, res)
				return nil
			})
		case res := <-d.actionReviewResults:
			logging.Guard("action-review-result", func() error {
				d.handleActionReviewOutcome(ctx, res)
				return nil
			})
		case res := <-d.taskGenResults:
			logging.Guard("task-gen-result", func() error {
				d.handleTaskGenOutcome(ctx, res)
				return nil
			})
		case res := <-d.sweepResults:
			logging.Guard("sweep-result", func() error {
				d.handleSweepOutcome(ctx, res)
				return nil
			})
		}
	}
}

// captureLiveAgent resolves a targeted control request against the daemon's
// current live view, then feeds the current parked state into the same delayed
// capture pipeline as a real Herdr event. It intentionally bypasses only the
// reconcile episode/open-escalation guards; downstream duplicate and safety
// checks remain authoritative.
func (d *Daemon) captureLiveAgent(ctx context.Context, target string) {
	agents, err := d.opt.Herdr.ListAgents(ctx)
	if err != nil {
		slog.Error("manual capture: listing agents failed", "target", target, "error", err)
		return
	}
	for _, tr := range agents {
		if tr.AgentID != target && tr.PaneID != target {
			continue
		}
		switch tr.Status {
		case "blocked", "idle", "done":
			tr.ManualCapture = true
			// The nudge loop runs reconciliation immediately after this method.
			// Claim the parked episode first so reconcile cannot coalesce a
			// provenance-less transition over this explicit request.
			d.mu.Lock()
			d.episodeHandled[tr.PaneID] = true
			d.mu.Unlock()
			slog.Info("manual capture queued", "agent", tr.AgentID, "pane", tr.PaneID, "status", tr.Status)
			d.scheduleCapture(ctx, tr)
		default:
			slog.Warn("manual capture ignored: agent is not parked", "agent", tr.AgentID, "status", tr.Status)
		}
		return
	}
	slog.Warn("manual capture ignored: live agent not found", "target", target)
}

// handleTransition evaluates one agent-status transition end to end.
func (d *Daemon) handleTransition(ctx context.Context, tr domain.AgentTransition) {
	now := d.opt.Clock.Now()

	// Auto-generate a short friendly name on first sight — for EVERY
	// observed transition, including "detected" discovery events and
	// "working", so a brand-new agent is named the moment it appears, not
	// only when it first needs attention (insert-if-absent, so operator
	// renames are never clobbered).
	if _, err := d.opt.Store.EnsureAgentName(ctx, tr.AgentID); err != nil {
		slog.Warn("agent name generation failed", "agent", tr.AgentID, "error", err)
	}

	switch tr.Status {
	case "working":
		// A transition to working that we did not cause means the human
		// interacted: the runaway consecutive counter resets (FR-019).
		// While the agent is rate-paused nothing automated can have caused
		// the transition, so it always counts as human check-in.
		rate, err := d.opt.Store.GetAgentRate(ctx, tr.AgentID)
		paused := err == nil && rate.Paused
		d.mu.Lock()
		last, ours := d.lastAutoSend[tr.AgentID]
		// A noop decision sends nothing, but an agent resuming right after
		// one is self-flapping, not human-driven: without this the flap
		// would reset the runaway counter and a noop rule could fire
		// silently forever (D3).
		if ln, ok := d.lastAutoNoop[tr.AgentID]; ok && (!ours || ln.After(last)) {
			last, ours = ln, true
		}
		d.mu.Unlock()
		if paused || !ours || now.Sub(last) > 10*time.Second {
			d.registerHumanInteraction(ctx, tr.AgentID)
		}
		// The agent resumed: a pending delayed capture would read a pane
		// that moved on (the consuming recent-delta still holds the old
		// menu AND the answer) — activity supersedes it.
		d.cancelCapture(tr.PaneID)
		// Same for an in-flight action review: its situation no longer
		// stands, whoever caused the resume. registerHumanInteraction below
		// also cancels, but only when the resume counts as human — a
		// self-flap within 10s of our own last auto action would otherwise
		// leave the flight live, and a late "@noop" outcome (which skips the
		// staleness re-read) would stamp audit/rate side effects for a
		// pane that already moved on.
		d.cancelActionReviewExcept(ctx, tr.AgentID, "")
		// Genuine progress ends the pane's parked episode: re-arm the
		// subscribe-time reconcile so a fresh block/idle/done is surfaced (#49).
		// The agent is working again, so it is no longer idle and no longer
		// holds an auto-send claim (whether or not the claimed task is what set
		// it working — a claim only reserves the daemon's INTENT, never the
		// file, so dropping it strands nothing).
		d.mu.Lock()
		delete(d.episodeHandled, tr.PaneID)
		delete(d.idleSince, tr.AgentID)
		delete(d.autoTaskClaim, tr.AgentID)
		d.mu.Unlock()
		return
	case "idle", "done", "blocked":
		// Attention-requiring: the pane read is DELAYED so the agent TUI
		// has painted (an immediate read on the start event captures shell
		// scrollback, not the agent's screen). Bursts coalesce per pane —
		// the latest event wins and exactly one capture fires.
		d.scheduleCapture(ctx, tr)
		return
	default:
		// "detected" and unknown statuses: named above, nothing to act on.
		// "detected" is discovery (pane created / subscriber replay): a
		// fresh agent in this pane must get the full start settle again,
		// and any pending capture belongs to a previous tenant. A replay
		// on subscriber reconnect only costs one extra long settle.
		if tr.Status == "detected" {
			d.cancelCapture(tr.PaneID)
			d.mu.Lock()
			delete(d.captureStarted, tr.PaneID)
			d.mu.Unlock()
		}
		return
	}
}

// cancelCapture stops and forgets the pane's pending delayed capture —
// activity or discovery supersedes it. A closure that already started
// firing sees its map entry gone and bails (generation check).
func (d *Daemon) cancelCapture(paneID string) {
	d.mu.Lock()
	if p := d.pendingCapture[paneID]; p != nil {
		d.stopTimer(p.timer) // release the tracked bg slot, not a bare Stop
		delete(d.pendingCapture, paneID)
	}
	d.mu.Unlock()
}

// captureEntry is one pane's pending delayed capture; the pointer identity
// doubles as the generation token that closes the Timer.Stop race.
type captureEntry struct {
	timer        *time.Timer
	retryAuditID int64
}

// scheduleCapture (re)arms the pane's capture timer: a newer event cancels
// a pending one (latest wins). When the timer fires, the transition
// re-enters the main loop through delayedTr — nothing sleeps in Run.
func (d *Daemon) scheduleCapture(ctx context.Context, tr domain.AgentTransition) {
	cfg, _, _ := d.snapshot()
	d.mu.Lock()
	if p := d.pendingCapture[tr.PaneID]; p != nil {
		d.stopTimer(p.timer)
		// A regular attention event may arrive while an operator-requested
		// retry is settling. Coalescing must not erase the retry intent, or
		// the resulting high-confidence decision could be auto-promoted.
		if tr.RetryAuditID == 0 {
			tr.RetryAuditID = p.retryAuditID
		}
	}
	// The start delay applies until the pane's FIRST capture actually
	// fires — a burst of events during startup keeps the longer settle.
	delay := cfg.CaptureDelay(tr.AgentType, !d.captureStarted[tr.PaneID])
	entry := &captureEntry{retryAuditID: tr.RetryAuditID}
	entry.timer = d.afterFunc(delay, func() {
		d.mu.Lock()
		current := d.pendingCapture[tr.PaneID] == entry
		if current {
			delete(d.pendingCapture, tr.PaneID)
			d.captureStarted[tr.PaneID] = true
		}
		d.mu.Unlock()
		if !current {
			// Superseded: Stop() lost the race with this func starting,
			// but the map entry identity says a newer capture owns the
			// pane now. Only the newest delivers.
			return
		}
		select {
		case d.delayedTr <- tr:
		case <-ctx.Done():
		}
	})
	if entry.timer == nil {
		// Daemon shutting down: drop this capture rather than leak an entry
		// whose timer will never fire (afterFunc refused to schedule).
		d.mu.Unlock()
		return
	}
	d.pendingCapture[tr.PaneID] = entry
	d.mu.Unlock()
}

// reconcileAttention surfaces agents already parked in an attention state at
// (re)start/sweep time. herdr's pane.agent_status_changed subscription only
// delivers FUTURE transitions, so anything already blocked/idle/done before the
// daemon subscribed (restart, upgrade self-replace, kill→resume, resubscribe
// window) is otherwise invisible to the escalation path (#49). ListAgents reads
// live agent_status; each parked agent is re-driven through the normal
// capture→classify→escalate path (like applyLLMRetry) exactly once per parked
// episode. Runs on the sweep path, where ListAgents is already called.
func (d *Daemon) reconcileAttention(ctx context.Context) {
	agents, err := d.opt.Herdr.ListAgents(ctx)
	if err != nil {
		slog.Error("reconcile: listing agents failed", "error", err)
		return
	}
	d.reconcileAttentionWith(ctx, agents)
}

// reconcileAttentionWith is reconcileAttention over an agent listing the
// caller already has. The periodic sweep uses it so one `agent list` serves
// both the reconcile and the auto-send-when-idle poll.
func (d *Daemon) reconcileAttentionWith(ctx context.Context, agents []domain.AgentTransition) {
	// Before the parked-status filter: working agents must sync too, or a
	// busy agent on a recycled pane id keeps the stale AGE until it parks.
	d.syncTerminalIDs(ctx, agents)
	for _, a := range agents {
		switch a.Status {
		case "blocked", "idle", "done":
		default:
			continue
		}
		// Within-run guard: don't re-drive a still-parked episode every sweep.
		d.mu.Lock()
		handled := d.episodeHandled[a.PaneID]
		if !handled {
			d.episodeHandled[a.PaneID] = true
		}
		d.mu.Unlock()
		if handled {
			continue
		}
		// Durable guard: after a restart the in-memory set is empty, so an
		// escalation already open before the crash must not be re-raised.
		if d.hasOpenEscalation(ctx, a.AgentID) {
			continue
		}
		slog.Info("reconcile: re-driving parked agent", "agent", a.AgentID, "pane", a.PaneID, "status", a.Status)
		d.scheduleCapture(ctx, a)
	}
}

// syncTerminalIDs reconciles each live agent's herdr terminal id with its
// agent_names row, so an agent landing on a recycled pane id gets its
// created_at (the TUI AGE anchor) reset (issue #158). Piggybacks on the
// ListAgents call reconcileAttention already makes — no extra shell-outs.
// Best-effort: errors are logged and never abort the reconcile sweep.
func (d *Daemon) syncTerminalIDs(ctx context.Context, agents []domain.AgentTransition) {
	for _, a := range agents {
		if a.TerminalID == "" {
			continue // older herdr without terminal_id: keep today's behavior
		}
		reset, err := d.opt.Store.SyncAgentTerminalID(ctx, a.AgentID, a.TerminalID)
		if err != nil {
			slog.Warn("reconcile: terminal-id sync failed", "agent", a.AgentID, "error", err)
			continue
		}
		if reset {
			slog.Info("pane id recycled by a new terminal; agent age reset",
				"agent", a.AgentID, "terminal", a.TerminalID)
			d.resetRecycledPaneState(ctx, a)
		}
	}
}

// resetRecycledPaneState forgets everything the daemon remembered about the
// pane's PREVIOUS occupant, once herdr's terminal_id proves a different agent
// is behind it now. Pane ids are reused, and every one of these maps is keyed
// by pane or agent id — so without this a fresh agent inherits the dead one's
// bookkeeping:
//
//   - episodeHandled would still read "handled", so the new agent's first
//     parked episode is never reconciled — it simply sits there;
//   - captureStarted would still read "started", so its very first capture uses
//     the SHORT event delay instead of the long start delay, snapshotting a
//     half-painted TUI;
//   - a pending capture (and any in-flight action review) belongs to an agent
//     that no longer exists, and would deliver into the new one's pane;
//   - lastAutoSend/lastAutoNoop would misattribute the new agent's first
//     "working" transition to our own automation, suppressing the human
//     check-in that resets the runaway counter;
//   - a stale cwd would render the wrong {cwd} in its first task prompt.
//
// firstConsult/firstTaskGen are reset too: their doc notes a brand-new agent
// normally arrives on a NEW pane (so the key is absent and priming happens
// naturally) and that the rare pane-id reuse forgoes re-priming for want of a
// signal. terminal_id IS that signal, so a genuinely new agent primes properly.
// This is deliberately NOT done on a "detected" event, which also fires on
// every subscriber reconnect.
func (d *Daemon) resetRecycledPaneState(ctx context.Context, a domain.AgentTransition) {
	// Cancel first, outside the lock these helpers take themselves: a pending
	// capture or review for the dead agent must not fire against the new one.
	d.cancelCapture(a.PaneID)
	d.cancelActionReviewExcept(ctx, a.AgentID, "")

	d.mu.Lock()
	delete(d.episodeHandled, a.PaneID)
	delete(d.captureStarted, a.PaneID)
	delete(d.paneCwds, a.PaneID)
	delete(d.firstConsult, a.AgentID)
	delete(d.firstTaskGen, a.AgentID)
	delete(d.lastAutoSend, a.AgentID)
	delete(d.lastAutoNoop, a.AgentID)
	delete(d.idleSince, a.AgentID)
	delete(d.autoTaskClaim, a.AgentID)
	d.mu.Unlock()
	// sweepInFlight is deliberately left alone: it is a live-goroutine claim
	// released by its owner's defer, and clearing it here would license a
	// second concurrent pane interaction.
}

// hasOpenEscalation reports whether the agent already has an unresolved
// escalation (an audit_log row still in status 'escalated'). Fails safe: on a
// store error it returns true so reconcile skips rather than risk a duplicate.
func (d *Daemon) hasOpenEscalation(ctx context.Context, agentID string) bool {
	esc, err := d.opt.Store.PendingEscalations(ctx)
	if err != nil {
		slog.Warn("reconcile: pending-escalation check failed", "agent", agentID, "error", err)
		return true
	}
	for _, e := range esc {
		if e.AgentID == agentID {
			return true
		}
	}
	return false
}

// duplicatePendingEscalation reports whether the captured situation repeats an
// escalation the operator has not yet handled — so re-raising it would ask the
// same question twice. Herdr re-delivers an attention event for one agent after
// a delay (the agent flips done->idle when the operator reads the pane), and
// the exact-string check it replaces missed those re-fires because the pane's
// volatile chrome (a spinner tick, an elapsed/countdown counter) had moved on.
//
// The key is agent + agent type + the NORMALIZED pane content
// (domain.NormalizeForDedup elides that chrome, and deletes Claude's ※-led
// recap/tip blocks, which appear on their own on a settled screen). The agent
// status is deliberately excluded — it is the field that legitimately CHANGES
// between the duplicates — and so are trigger() (which embeds it) and
// situation_type (which the classifier DERIVES from it). A capture whose
// tail-anchored window merely shifted (an appearing note block pushed old
// text off the head) is also recognized, via a suffix compare gated on both
// captures filling at least half of snapshotMaxRunes. See
// domain.DuplicatesPendingEscalation.
//
// It runs only where escalations are raised (escalate() and the
// pane-read-failure path), NOT before the decision core: the rate guard, retry
// ceiling, and shadow-mode learning all work by re-processing repeated
// identical events, so suppressing those would bypass a safety control.
//
// It is the content-level sibling of hasOpenEscalation (which is
// pane/agent-level and used by the reconcile sweep): finer, so a genuinely new
// situation on a pane that already has an open escalation still gets processed.
// The pane excerpt is truncated exactly as the write paths store it so the
// comparison lines up.
//
// Fails OPEN (returns false) on a store error — the opposite of
// hasOpenEscalation. Dropping a real event silently is worse than
// re-processing one, so on doubt the event proceeds to escalate rather than
// being ignored (the "never a silent drop" architecture rule).
func (d *Daemon) duplicatePendingEscalation(ctx context.Context, s domain.Situation) bool {
	cfg, _, _ := d.snapshot()
	window := time.Duration(cfg.Limits.EscalationDedupWindowSeconds) * time.Second
	resolvedSince := d.opt.Clock.Now().Add(-window)
	pending, err := d.opt.Store.PendingEscalationExcerpts(ctx, s.AgentID, s.AgentType, resolvedSince)
	if err != nil {
		slog.Warn("duplicate-escalation check failed; processing event",
			"agent", s.AgentID, "pane", s.PaneID, "error", err)
		return false
	}
	return domain.DuplicatesPendingEscalation(s.Type,
		truncateTailRunes(s.Content, snapshotMaxRunes), snapshotMaxRunes,
		cfg.Limits.EscalationDedupJitterPercent, pending)
}

// ignoreDuplicate audits a no-op for an event whose situation already has a
// matching pending escalation awaiting the operator. Shared by escalate() and
// the pane-read-failure path so both routes record duplicates the same way.
func (d *Daemon) ignoreDuplicate(ctx context.Context, s domain.Situation,
	tr domain.AgentTransition, now time.Time) {
	d.audit(ctx, domain.AuditRecord{
		AgentID: s.AgentID, AgentType: s.AgentType, Trigger: trigger(tr),
		SituationType: s.Type, Action: "ignored", Status: domain.AuditStatusIgnored,
		Rationale:   "duplicated event",
		PaneExcerpt: truncateTailRunes(s.Content, snapshotMaxRunes),
		CreatedAt:   now,
	})
	slog.Info("ignored duplicate event: matching pending escalation exists",
		"agent", s.AgentID, "pane", s.PaneID, "situation", s.Type)
}

// handleAttention is the post-delay half of the pipeline: the classification
// pane read and everything after it. It runs on the main loop (via
// delayedTr) and re-derives its inputs at fire time, so every gate — kill
// switch, rate guard, retry ceiling — applies AFTER the delay.
func (d *Daemon) handleAttention(ctx context.Context, tr domain.AgentTransition) {
	_, _, cls := d.snapshot()
	now := d.opt.Clock.Now()
	// Insert-if-absent; the transition handler already named the agent,
	// this just re-reads (and covers rows racing a clear-data).
	agentName, err := d.opt.Store.EnsureAgentName(ctx, tr.AgentID)
	if err != nil {
		slog.Warn("agent name generation failed", "agent", tr.AgentID, "error", err)
	}

	pane, err := d.opt.Herdr.ReadPane(ctx, tr.PaneID, d.opt.PaneReadLines)
	if err != nil {
		// Herdr unreachable / pane read failure: no automated action, log,
		// notify (FR-023); the subscriber's backoff handles reconnection.
		slog.Warn("pane read failed; taking no action", "pane", tr.PaneID, "error", err)
		// Route even this unclassifiable failure through escalate() so its
		// lifecycle auto-dismiss and dedup rules are identical to every other
		// escalation. The unreadable pane has no excerpt, so the normalized
		// dedup's empty-content fallback keys on the situation type.
		failed := domain.Situation{
			AgentID: tr.AgentID, AgentType: tr.AgentType, PaneID: tr.PaneID,
			Type: domain.SituationUnclassifiable,
		}
		d.escalate(ctx, failed, domain.SignatureResult{}, domain.Decision{
			Action: domain.ActionEscalate, Reason: domain.ReasonHerdrUnreachable,
			Rationale: err.Error(),
		}, tr, now)
		return
	}

	situation := cls.Classify(tr.AgentType, tr.Status, pane)
	situation.AgentID = tr.AgentID
	situation.PaneID = tr.PaneID
	situation.TabID = tr.TabID
	situation.WorkspaceID = tr.WorkspaceID
	situation.RetryAuditID = tr.RetryAuditID
	// Carry the captured terminal identity so an unattended delivery can prove,
	// just before it sends, that the pane still hosts the agent it captured.
	situation.TerminalID = tr.TerminalID
	// Keep herdr's reported agent_status with the situation: downstream
	// sites (the async LLM path) must render the REAL status in triggers,
	// never a fabricated one.
	situation.Status = tr.Status
	if situation.AgentType == "" {
		situation.AgentType = "unknown"
	}

	// Multi-tab MCQ forms show one question at a time: sweep the remaining
	// tabs (Right-arrow protocol) so the signature, the escalation, and the
	// LLM consult all describe the WHOLE form, not question 1 of N. The
	// sweep is pane interaction and runs off the main loop; its outcome
	// re-enters decideAndAct. It is gated like any automation — kill
	// switch, rate pause, never-auto patterns — BEFORE the first keystroke, and
	// degrades to single-frame when the adapter cannot send keystrokes.
	if situation.Type == domain.SituationChoice && situation.EffectiveAnswerCount() > 1 {
		if ks, ok := d.opt.Herdr.(ports.KeystrokeSender); ok && d.sweepAllowed(ctx, situation) {
			d.startSweep(ctx, ks, situation, tr, agentName)
			return
		}
	}

	d.decideAndAct(ctx, situation, tr, agentName, now)
}

// snapshotMaxRunes caps stored Current/Original Situation pane snapshots.
// Captures keep the tail because shell/CLI results and prompts land at the
// bottom; older scrollback is discarded first.
const snapshotMaxRunes = 4000

// maxReviewOutput caps the accepted action-review replacement text (matches
// the LLM adapter's 16KB capture cap): the result is typed into a pane, so
// runaway output degrades to the fallback instead of being trimmed.
const maxReviewOutput = 16 * 1024

// decideAndAct is the decision tail shared by handleTransition and the
// multi-tab sweep outcome: signature, state reads, safety inputs, the pure
// decision core, and dispatch.
func (d *Daemon) decideAndAct(ctx context.Context, situation domain.Situation,
	tr domain.AgentTransition, agentName string, now time.Time) {

	cfg, allow, _ := d.snapshot()

	sig := domain.ComputeSignatureN(situation, cfg.Embedding.PaneSalientChars)
	// Semantic resolution may remap the key onto an existing learned
	// signature (embedding / BM25 match on the masked salient content);
	// sig.Raw always keeps the literal content hash.
	sig = d.resolveSignature(ctx, cfg, sig, situation)

	// Rule provenance: keep the pane snapshot the signature was FIRST seen
	// with (insert-or-ignore; an in-memory cache skips the no-op write on
	// repeat sightings), so the Rules views can show what situation a
	// learned action answers. Best-effort — never blocks the decision.
	d.mu.Lock()
	saved := d.snapshotSaved[sig.Signature]
	d.mu.Unlock()
	if sig.Signature != "" && !saved {
		if err := d.opt.Store.SaveSignatureSnapshot(ctx, sig.Signature,
			truncateTailRunes(situation.Content, snapshotMaxRunes), now); err != nil {
			slog.Warn("signature snapshot write failed", "error", err)
		} else {
			d.mu.Lock()
			d.snapshotSaved[sig.Signature] = true
			d.mu.Unlock()
		}
	}

	// Assemble decision inputs (all reads).
	state, history, rate, retries, killActive, readErr := d.readDecisionState(ctx, sig, situation)
	if readErr != nil {
		slog.Error("state read failed; escalating", "error", readErr)
		d.escalate(ctx, situation, sig, domain.Decision{
			Action: domain.ActionEscalate, Reason: domain.ReasonPersistenceFailed,
			Rationale: readErr.Error(),
		}, tr, now)
		return
	}
	disabled, err := d.opt.Store.AgentDisabled(ctx, situation.AgentID)
	if err != nil {
		slog.Error("disabled-state read failed; escalating", "error", err)
		d.escalate(ctx, situation, sig, domain.Decision{
			Action: domain.ActionEscalate, Reason: domain.ReasonPersistenceFailed,
			Rationale: "disabled-state read: " + err.Error(),
		}, tr, now)
		return
	}

	// Both the never-auto match and the suspected-irreversible heuristic scan
	// only the actionable region (pending dialog + outbound task text), not the
	// full scrollback: a destructive phrase anywhere in stale scrollback (old
	// commands, agent narration *about* destructive operations) must not veto a
	// benign pending action (FR-015/FR-016). A destructive command in the
	// pending dialog itself still matches.
	declared := d.declaredTask(ctx, cfg, tr, agentName)
	declaredPrompt := ""
	if declared != nil {
		declaredPrompt = declared.Prompt()
	}
	scan := domain.IrreversibleScanContent(situation, declaredPrompt)
	allowHit, allowMatched := allow.Match(situation.AgentType, scan)

	var irrevHit domain.IndicatorHit
	suspected := false
	if !allowMatched {
		irrevHit, suspected = allow.SuspectedIrreversible(situation.AgentType, scan)
	}

	// Pre-send LLM review of a determined declared task (opt-out per source via
	// llm_review=false): when an LLM command is configured, the LLM — not
	// shadow-mode graduation — decides whether this task should be sent to the
	// idle agent now, seeing the live pane through get_context. A decline
	// escalates to the operator; an approval is still re-gated in
	// handleLLMOutcome. The fail-safe pre-checks (killed / never-auto /
	// suspected-irreversible) escalate normally instead of spending a consult.
	if situation.Type == domain.SituationIdle && declared != nil &&
		declared.Task != domain.NoTaskContent && declared.LLMReview &&
		!disabled && !killActive && !allowMatched && !suspected &&
		d.llmPort() != nil && d.llmPort().Configured() {
		// A matching escalation already awaiting the operator (e.g. a prior
		// decline of this same task) means the situation is handled: don't
		// re-consult the LLM (a wasted, token-spending review) and don't fall
		// through to a non-reviewed send — ignore the duplicate event.
		if d.duplicatePendingEscalation(ctx, situation) {
			d.ignoreDuplicate(ctx, situation, tr, now)
			return
		}
		d.consultDeclaredTask(ctx, cfg, situation, sig, tr, declared, now)
		return
	}

	in := domain.DecideInput{
		Situation:            situation,
		Signature:            sig,
		State:                state,
		History:              history,
		ConfidenceThresholds: confidenceThresholds(cfg),
		ConfirmationWeight:   cfg.Learning.ConfirmationWeight,
		GraduationN:          cfg.Learning.GraduationN,
		KillActive:           killActive,
		Rate:                 rate,
		RateLimits: domain.RateLimits{
			MaxConsecutive: cfg.Limits.MaxConsecutiveAutoPrompts,
			MaxPerMinute:   cfg.Limits.MaxAutoPromptsPerMinute,
		},
		Now:                         now,
		RetryCount:                  retries,
		MaxRetries:                  cfg.Limits.MaxErrorRetries,
		DeclaredTask:                declared,
		LLMConfigured:               d.llmPort() != nil && d.llmPort().Configured(),
		GenerateTaskConfigured:      d.taskGenPort() != nil,
		GenerateTaskStartConfigured: len(cfg.LLM.GenerateTaskCommandStart) > 0,
		NeverAutoRuleHit:            allowHit,
		NeverAutoMatched:            allowMatched,
		SuspectedIrreversible:       suspected,
		IrreversibleHit:             irrevHit,
	}

	decision := domain.Decide(in)
	if disabled {
		if decision.Action == domain.ActionEscalate {
			d.escalate(ctx, situation, sig, decision, tr, now)
		} else {
			d.auditAgentDisabled(ctx, situation, sig, tr, decision.Input,
				decision.Confidence, nil, "", now)
		}
		return
	}

	// Any newer decision for this agent owns the pane: an in-flight action
	// review for a DIFFERENT situation must never deliver behind it. A same-
	// signature send is kept — startActionReview drops it as a duplicate.
	keepSig := ""
	if decision.Action == domain.ActionSend {
		keepSig = sig.Signature
	}
	d.cancelActionReviewExcept(ctx, situation.AgentID, keepSig)

	switch decision.Action {
	case domain.ActionSend:
		d.act(ctx, situation, sig, decision, tr, declared, now)
	case domain.ActionKindNoop:
		d.deliverNoop(ctx, situation, sig, decision, tr, now)
	case domain.ActionConsult:
		d.consultLLM(ctx, cfg, situation, sig, now)
	case domain.ActionGenerateTask:
		// ActionGenerateTask for an idle situation only fires when there's no
		// REAL pending task: declared != nil here means a task source matched
		// but its checklist is exhausted (Task == NoTaskContent), not that
		// none was ever declared.
		d.generateTask(ctx, cfg, situation, sig, tr, now, declared != nil)
	default:
		d.escalate(ctx, situation, sig, decision, tr, now)
	}
}

// truncateRunes shortens s to at most n runes, marking the cut with an
// ellipsis (rune-safe: never splits a UTF-8 sequence).
func truncateRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}

// truncateTailRunes shortens shell/CLI context to its final n runes. The
// actionable prompt or error is normally at the bottom, so stored situation
// snapshots must discard old scrollback from the top rather than the result.
func truncateTailRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return "…" + string(runes[len(runes)-n:])
}

// cancelActionReviewExcept invalidates the agent's in-flight action review
// unless it is for keepSig. The cancelled flight's outcome is dropped by the
// token check; its staged llm_requests row is expired best-effort so it does
// not block other consults for this agent until expireStaleLLMWork.
func (d *Daemon) cancelActionReviewExcept(ctx context.Context, agentID, keepSig string) {
	d.mu.Lock()
	fl, ok := d.actionReviewInFlight[agentID]
	if !ok || (keepSig != "" && fl.signature == keepSig) {
		d.mu.Unlock()
		return
	}
	delete(d.actionReviewInFlight, agentID)
	d.mu.Unlock()
	fl.cancel()
	if err := d.opt.Store.UpdateLLMRequestStatus(ctx, fl.requestID, "expired"); err != nil {
		slog.Error("expiring superseded action-review request failed",
			"request", fl.requestID, "error", err)
	}
	slog.Info("in-flight action review superseded", "agent", agentID)
}

// readDecisionState gathers all store reads for one decision. The latest
// kill event is read on every tick so a kill takes effect even before any
// reload nudge arrives (FR-017).
func (d *Daemon) readDecisionState(ctx context.Context, sig domain.SignatureResult, s domain.Situation) (
	state *domain.SignatureState, history []domain.DecisionRecord, rate domain.AgentRate, retries int, killActive bool, err error,
) {
	kill, err := d.opt.Store.LatestKillEvent(ctx)
	if err != nil {
		return nil, nil, rate, 0, true, err // fail closed: treat as killed
	}
	killActive = domain.KillStateActive(kill)

	if sig.Signature != "" {
		if state, err = d.opt.Store.GetSignature(ctx, sig.Signature); err != nil {
			return nil, nil, rate, 0, killActive, err
		}
		if history, err = d.opt.Store.DecisionsForSignature(ctx, sig.Signature, 50); err != nil {
			return nil, nil, rate, 0, killActive, err
		}
	}
	r, err := d.opt.Store.GetAgentRate(ctx, s.AgentID)
	if err != nil {
		return nil, nil, rate, 0, killActive, err
	}
	rate = *r
	if s.Type == domain.SituationError && sig.Signature != "" {
		er, err := d.opt.Store.GetErrorRetry(ctx, sig.Signature)
		if err != nil {
			return nil, nil, rate, 0, killActive, err
		}
		retries = er.RetryCount
	}
	return state, history, rate, retries, killActive, nil
}

// act performs a confirmed autonomous action with the pre-action audit
// guard: the audit record must be durably committed BEFORE any input is
// sent; a persistence failure blocks the action and notifies (FR-024).
// declared is the task source resolved for this situation (nil when none
// matched); it rides along so a send that IS the declared task can reserve the
// item as it goes — see reserveDeclaredTask.
func (d *Daemon) act(ctx context.Context, s domain.Situation, sig domain.SignatureResult,
	dec domain.Decision, tr domain.AgentTransition, declared *domain.DeclaredTask, now time.Time) {

	// The never-auto patterns also screen the OUTBOUND text: a next-task
	// line from a task file (or any learned action) naming an irreversible
	// operation must never be delivered automatically (FR-015).
	cfg, allow, _ := d.snapshot()
	if hit, matched := allow.Match(s.AgentType, dec.Input); matched {
		d.escalate(ctx, s, sig, domain.Decision{
			Action: domain.ActionEscalate, Reason: domain.ReasonNeverAutoMatch,
			Rationale:  "outbound: " + hit.Diagnostic(),
			Confidence: dec.Confidence,
		}, tr, now)
		return
	}

	// Multi-tab MCQ forms are answered with a digit series, one keystroke
	// per tab — never a single mapped label, never rewritten.
	if s.Type == domain.SituationChoice && s.EffectiveAnswerCount() > 1 {
		d.deliverSeries(ctx, s, sig, dec, tr, now)
		return
	}

	// Claude's "Select remote environment" picker commits per a per-build
	// protocol (the digit may only move the caret), so answer it adaptively
	// via verified keystrokes. Without the keystroke capability this FAILS
	// CLOSED to escalation: the plain text send below would type the literal
	// label + Enter, and under the caret binding Enter commits whatever
	// option the caret rests on — a rule learned on another project's
	// environment list must never launch the wrong cloud environment.
	if s.Type == domain.SituationApproval && strings.EqualFold(s.AgentType, "claude") {
		if _, isRemoteEnv := domain.ClaudeRemoteEnvForm(s.Content); isRemoteEnv {
			ks, ok := d.opt.Herdr.(ports.KeystrokeSender)
			if !ok {
				d.escalate(ctx, s, sig, domain.Decision{
					Action: domain.ActionEscalate, Reason: domain.ReasonHerdrUnreachable,
					Rationale:  "keystrokes unavailable; the remote-environment picker needs verified keystrokes",
					Confidence: dec.Confidence, Suggestion: "respond: " + dec.Input,
				}, tr, now)
				return
			}
			d.deliverRemoteEnv(ctx, ks, s, sig, dec, tr, now)
			return
		}
	}

	// Numbered menus (Claude approvals/choices) accept the option's digit,
	// not the label text; deliver the keystroke the menu expects. Free-text
	// situations deliver the literal reply. s.Content is the classification
	// snapshot, which carries the menu for the situation being acted on.
	outbound, menuMapped := domain.DeliverOutbound(s.Type, s.AgentType, s.Content, dec.Input)

	// Literal free text can be adapted to the live pane by the consult LLM
	// (llm.enable_rewrite_action); menu digits must reach the menu
	// untouched, and a declared task from a [[task_sources]] is never
	// reviewed here — the source's enable_llm_review gate owns that (an
	// opted-out or LLM-less source delivers its tasks verbatim). The send
	// completes asynchronously via handleActionReviewOutcome, so the learned
	// action is pinned NOW — situation state may drift before delivery.
	if cfg.LLM.EnableRewriteAction && !menuMapped && dec.Input != "" &&
		d.llmPort() != nil && d.llmPort().Configured() {
		if learned := d.learnedAction(ctx, s, dec); learned != domain.ActionNextDeclaredTask {
			d.startActionReview(ctx, s, sig, dec, tr, learned)
			return
		}
	}

	// Reserve the checklist item only when this send really is that declared
	// task (comparing the rendered prompt, which is exactly what Decide put in
	// dec.Input) — a learned free-text reply for the same agent must not
	// consume a task.
	del := delivery{sendText: outbound, input: dec.Input, rationale: dec.Rationale}
	if declared != nil && declared.Reserve && declared.Task != domain.NoTaskContent &&
		declared.Prompt() == dec.Input {
		del.declared, del.taskText = declared, declared.Task
	}

	// learned stays empty: deliverAutonomous computes it after the send,
	// exactly as the pre-review code did.
	d.deliverAutonomous(ctx, s, sig, dec, tr, del, now)
}

// learnedAction is the action recorded in decision history for a rule-path
// send (idle learns symbolically so signatures generalize across tasks).
func (d *Daemon) learnedAction(ctx context.Context, s domain.Situation, dec domain.Decision) string {
	switch {
	case dec.OptionID != "":
		return dec.OptionID
	case s.Type == domain.SituationIdle:
		if d.declaredTaskFor(ctx, s) != nil {
			return domain.ActionNextDeclaredTask
		}
		return domain.ActionNextInferredTask
	}
	return dec.Input
}

// llmLearnedAction is the action recorded in decision history for an LLM
// decision — the counterpart of learnedAction on the consult path.
//
// A task-review send learns SYMBOLICALLY, whatever the LLM actually sent: the
// rule is "send the next declared task", not the text of one particular task.
// Recording the literal text instead would bucket every task separately in
// domain.Confidence, which groups on the raw action string — the signature
// would never reach agreement, and a couple of @noop records could even win
// the plurality and stand the agent down (resolveSituation checks @noop before
// the declared task). A task review always has a declared task
// (consultDeclaredTask), so there is no inferred variant to distinguish here.
//
// Every other situation (approval / choice / error, and an idle consult with no
// task source, where the LLM authors free text) learns llmDec.Action — which is
// exactly what gets delivered below. Deliberately NOT llmDec.OptionID, even
// though learnedAction prefers dec.OptionID: that one comes from domain.Decide
// and agrees with the send by construction, whereas llmDec.OptionID is supplied
// by the model and can survive unresolved on the legacy option_id alias, which
// would learn an answer the daemon never sent.
func (d *Daemon) llmLearnedAction(llmDec *domain.LLMDecision, taskReviewSend bool) string {
	if taskReviewSend {
		return domain.ActionNextDeclaredTask
	}
	return llmDec.Action
}

// delivery describes one autonomous send: what to write to the pane, what
// to audit, and what to learn.
type delivery struct {
	sendText  string // exactly what is written to the pane
	input     string // audit Input and the "auto:" action label
	rationale string
	llmOutput string // LLM review diagnostics, when applicable
	learned   string // ChosenAction recorded for learning
	// llmConfidence is the review LLM's self-reported score (0-100) for the
	// audit row; nil when no LLM was involved or none was reported. Recorded
	// for observability only — the action-review path never gates on it.
	llmConfidence *int
	// declared/taskText name the checklist item this send delivers, when the
	// send IS a declared task from a source that reserves (see
	// reserveDeclaredTask). Both empty/nil for every other send.
	declared *domain.DeclaredTask
	taskText string
}

// deliverAutonomous is the shared tail of every autonomous rule-path send:
// pre-action audit guard (FR-024), delivery, and the daemon-owned learning
// and counter writes. It reports whether the input actually reached the
// pane — false covers the lifecycle-barrier refusal, a blocked audit write,
// and a failed send — so callers accounting for an LLM decision's fate
// (handleActionReviewOutcome) never record an undelivered action as applied.
func (d *Daemon) deliverAutonomous(ctx context.Context, s domain.Situation, sig domain.SignatureResult,
	dec domain.Decision, tr domain.AgentTransition, del delivery, now time.Time) bool {
	sent := false
	executed := d.withAgentAutomation(ctx, s, sig, tr, del.input, dec.Confidence, del.llmConfidence, del.llmOutput, now,
		func() { sent = d.deliverAutonomousClaimed(ctx, s, sig, dec, tr, del, now) })
	if !executed || !sent {
		// Nothing reached the pane — a refused reservation, a rolled-back send,
		// a blocked audit write, or the lifecycle barrier. The pairing must not
		// outlive the attempt: it would keep this agent out of the next sweep
		// AND withhold from every other agent a task that is pending again,
		// until the claim's TTL expired.
		d.dropAutoTaskClaim(s.AgentID)
	}
	return executed && sent
}

// deliverAutonomousClaimed runs while the cross-process per-agent lifecycle
// barrier is held, so SetAgentDisabled cannot commit between audit and send.
// Returns true once the input was sent to the pane (post-send bookkeeping
// failures do not retract that).
func (d *Daemon) deliverAutonomousClaimed(ctx context.Context, s domain.Situation,
	sig domain.SignatureResult, dec domain.Decision, tr domain.AgentTransition,
	del delivery, now time.Time) bool {
	// An unattended task hand-out must not land in a pane that has been recycled
	// since capture: the agent it was meant for is gone and a DIFFERENT agent
	// would receive it. Checked before the audit row, because nothing is
	// attempted — the claim is simply released so the next sweep re-pairs.
	if tr.AutoIdleSend || (del.declared != nil && del.declared.Reserve) {
		if recycled, why := d.paneRecycled(ctx, s); recycled {
			slog.Warn("pane was recycled since capture; abandoning the auto-send",
				"agent", s.AgentID, "reason", why)
			d.dropAutoTaskClaim(s.AgentID)
			return false
		}
	}

	auditID, err := d.opt.Store.AppendAudit(ctx, domain.AuditRecord{
		AgentID: s.AgentID, AgentType: s.AgentType, Signature: sig.Signature, Trigger: trigger(tr),
		SituationType: s.Type, Action: domain.AuditActionAutoPrefix + del.input, Input: del.input,
		Confidence: dec.Confidence, LLMConfidence: del.llmConfidence,
		Rationale: del.rationale, LLMOutput: del.llmOutput,
		Status: "auto", PaneExcerpt: truncateTailRunes(s.Content, snapshotMaxRunes), CreatedAt: now,
	})
	if err != nil {
		slog.Error("audit write failed; blocking autonomous action (FR-024)", "error", err)
		d.notify(ctx, "Herd Auto Prompter: persistence failure",
			"An automated action was blocked because its audit record could not be written.")
		return false
	}

	// Claim the checklist item BEFORE the send: marking it after delivery
	// would leave a window in which the very same "[ ]" line is handed to a
	// second idle agent. A refusal here means somebody already took it.
	rollback, err := d.reserveDeclaredTask(del.declared, del.taskText)
	if err != nil {
		slog.Warn("declared task could not be reserved; not sending", "agent", s.AgentID, "error", err)
		d.opt.Store.UpdateAuditStatus(ctx, auditID, "escalated")
		d.notify(ctx, "Herd Auto Prompter: action delivery skipped",
			fmt.Sprintf("Agent %s: the next task was claimed or edited before it could be sent (%v); please review the list.", s.AgentID, err))
		return false
	}

	if err := ports.SendToAgent(ctx, d.opt.Herdr, s.PaneID, s.AgentType, del.sendText); err != nil {
		slog.Error("agent send failed; escalating", "pane", s.PaneID, "error", err)
		rollback()
		d.opt.Store.UpdateAuditStatus(ctx, auditID, "escalated")
		d.notify(ctx, "Herd Auto Prompter: action delivery failed",
			fmt.Sprintf("Agent %s: could not deliver the decided input; please review.", s.AgentID))
		return false
	}

	d.mu.Lock()
	d.lastAutoSend[s.AgentID] = now
	d.mu.Unlock()

	// Learning + counters (daemon-owned hot-path rows). The action-review
	// path pins the learned action at decision time; the synchronous path
	// resolves it here, after the send, as it always has.
	learned := del.learned
	if learned == "" {
		learned = d.learnedAction(ctx, s, dec)
	}
	if _, err := d.opt.Store.RecordDecision(ctx, domain.DecisionRecord{
		Signature: sig.Signature, SituationType: s.Type, AgentType: s.AgentType,
		ChosenAction: learned, Source: dec.Source, Confidence: dec.Confidence, CreatedAt: now,
	}); err != nil {
		slog.Error("decision record write failed", "error", err)
	}

	rate, err := d.opt.Store.GetAgentRate(ctx, s.AgentID)
	if err == nil {
		updated := domain.RegisterAutoPrompt(*rate, now)
		updated.AgentID = s.AgentID
		if err := d.opt.Store.UpdateAgentRate(ctx, updated); err != nil {
			slog.Error("agent rate update failed", "error", err)
		}
	}

	if s.Type == domain.SituationError {
		er, err := d.opt.Store.GetErrorRetry(ctx, sig.Signature)
		if err != nil {
			slog.Error("error retry read failed; ceiling may undercount", "error", err)
		} else if err := d.opt.Store.UpsertErrorRetry(ctx, domain.ErrorRetry{
			ErrorSignature: sig.Signature, AgentID: s.AgentID,
			RetryCount: er.RetryCount + 1, UpdatedAt: now,
		}); err != nil {
			slog.Error("error retry update failed", "error", err)
		}
	}

	slog.Info("automated action delivered",
		"agent", s.AgentID, "situation", s.Type, "confidence", dec.Confidence,
		"rationale", del.rationale, "audit_id", auditID)

	d.scheduleUnblockCheck(verifyunblock.Params{
		PaneID: s.PaneID, AgentID: s.AgentID, AgentType: s.AgentType,
		Signature: sig.Signature, Input: del.input, Excerpt: s.Content, SituationType: s.Type,
	})
	return true
}

// scheduleUnblockCheck arms the post-action self-check: after the fixed
// one-second delay it re-queries the agent's status and, if the
// agent is STILL blocked, appends a delivery_failed audit row and notifies the
// operator. It is a no-op when the situation type does not block an agent
// (idle). The check runs in its own guarded goroutine via time.AfterFunc so it
// never stalls the monitor loop.
func (d *Daemon) scheduleUnblockCheck(p verifyunblock.Params) {
	if !verifyunblock.Relevant(p.SituationType) || p.PaneID == "" {
		return
	}
	// Keep the diagnostic row's excerpt the same size as the normal audit rows.
	p.Excerpt = truncateTailRunes(p.Excerpt, snapshotMaxRunes)
	delay := d.verifyUnblockDelay
	if delay <= 0 {
		delay = unblockCheckDelay
	}
	d.afterFunc(delay, func() {
		_ = logging.Guard("verify-unblock", func() error {
			// Root the check at shutdownCtx (not context.Background) so daemon
			// teardown cancels an in-flight self-check instead of letting it
			// read a store the caller is about to close.
			ctx, cancel := context.WithTimeout(d.shutdownCtx, 10*time.Second)
			defer cancel()
			blocked, _, err := verifyunblock.Check(ctx, d.opt.Herdr, d.opt.Store, p, d.opt.Clock.Now())
			if err != nil {
				slog.Warn("post-action unblock self-check failed", "agent", p.AgentID, "error", err)
				return nil
			}
			if blocked {
				// NOTE: this only checks status == blocked; it cannot tell the
				// original prompt from a NEW one the agent raised after
				// answering. A fast agent that answers and immediately blocks on
				// a follow-up may trip a benign false positive.
				slog.Warn("agent still blocked after delivered action",
					"agent", p.AgentID, "situation", p.SituationType, "input", p.Input)
				d.notify(ctx, "Herd Auto Prompter: agent still blocked after action",
					fmt.Sprintf("Agent %s is still blocked ~%dms after the delivered reply — it may not have landed (or the agent raised a new prompt). See hap audit.", p.AgentID, delay.Milliseconds()))
			}
			return nil
		})
	})
}

// deliverNoop applies a graduated "do nothing" rule autonomously: audit-first
// (FR-024), then the learning and rate writes — but nothing is sent. The
// runaway counter still advances (D3): a self-flapping agent must not
// consult-and-noop silently forever; the consecutive ceiling eventually
// escalates it to a human. lastAutoSend stays untouched (no send happened);
// lastAutoNoop is stamped instead so a self-resuming flap does not read as
// human interaction and reset that counter.
func (d *Daemon) deliverNoop(ctx context.Context, s domain.Situation, sig domain.SignatureResult,
	dec domain.Decision, tr domain.AgentTransition, now time.Time) {
	// Nothing is sent, so the pairing is spent — release it (see escalate).
	d.dropAutoTaskClaim(s.AgentID)
	d.withAgentAutomation(ctx, s, sig, tr, "", dec.Confidence, nil, "", now,
		func() { d.deliverNoopClaimed(ctx, s, sig, dec, tr, now) })
}

func (d *Daemon) deliverNoopClaimed(ctx context.Context, s domain.Situation,
	sig domain.SignatureResult, dec domain.Decision, tr domain.AgentTransition, now time.Time) {
	auditID, err := d.opt.Store.AppendAudit(ctx, domain.AuditRecord{
		AgentID: s.AgentID, AgentType: s.AgentType, Signature: sig.Signature, Trigger: trigger(tr),
		SituationType: s.Type, Action: "noop", Input: "",
		Confidence: dec.Confidence, Rationale: dec.Rationale,
		Status: "auto", PaneExcerpt: truncateTailRunes(s.Content, snapshotMaxRunes), CreatedAt: now,
	})
	if err != nil {
		slog.Error("audit write failed; blocking autonomous noop (FR-024)", "error", err)
		d.notify(ctx, "Herd Auto Prompter: persistence failure",
			"An automated no-op was blocked because its audit record could not be written.")
		return
	}

	if _, err := d.opt.Store.RecordDecision(ctx, domain.DecisionRecord{
		Signature: sig.Signature, SituationType: s.Type, AgentType: s.AgentType,
		ChosenAction: domain.ActionNoop, Source: dec.Source, Confidence: dec.Confidence, CreatedAt: now,
	}); err != nil {
		slog.Error("decision record write failed", "error", err)
	}

	if rate, err := d.opt.Store.GetAgentRate(ctx, s.AgentID); err == nil {
		updated := domain.RegisterAutoPrompt(*rate, now)
		updated.AgentID = s.AgentID
		if err := d.opt.Store.UpdateAgentRate(ctx, updated); err != nil {
			slog.Error("agent rate update failed", "error", err)
		}
	}

	d.mu.Lock()
	d.lastAutoNoop[s.AgentID] = now
	d.mu.Unlock()

	slog.Info("learned noop applied: no reply sent",
		"agent", s.AgentID, "situation", s.Type, "confidence", dec.Confidence, "audit_id", auditID)
}

// deliverActionReviewNoop stands the daemon down after the review LLM vetoed
// a send ("@noop"): audit-first (FR-024), then the rate write — nothing is
// sent. Unlike deliverNoop and the consult noop, NO decision is recorded:
// this is a delivery-time contextual veto of an already-learned action, not
// a decision about the signature — recorded @noop rows could win the
// plurality and permanently stand the learned rule down (see the
// llmLearnedAction warning) or trip the variance guard on later consults.
// The runaway counter still advances (D3) and lastAutoNoop is stamped, so
// a review-noop loop eventually escalates instead of spinning silently.
func (d *Daemon) deliverActionReviewNoop(ctx context.Context, res actionReviewOutcome,
	llmConf *int, now time.Time) {
	s := res.situation
	rationale := fmt.Sprintf("%s; llm review declined to send (@noop) (original: %q)",
		res.dec.Rationale, truncateRunes(res.dec.Input, 200))
	if res.decision != nil && strings.TrimSpace(res.decision.Rationale) != "" {
		rationale += "; LLM: " + strings.TrimSpace(res.decision.Rationale)
	}
	// applied flips once the veto's audit row is durably written — an audit
	// failure blocks the stand-down (FR-024), so the decision must not
	// resolve as accepted for it.
	applied := false
	executed := d.withAgentAutomation(ctx, s, res.sig, res.tr, "", res.dec.Confidence, llmConf,
		domain.ActionNoop, now, func() {
			auditID, err := d.opt.Store.AppendAudit(ctx, domain.AuditRecord{
				AgentID: s.AgentID, AgentType: s.AgentType, Signature: res.sig.Signature,
				Trigger: trigger(res.tr), SituationType: s.Type, Action: "noop", Input: "",
				Confidence: res.dec.Confidence, LLMConfidence: llmConf,
				Rationale: rationale,
				LLMOutput: domain.ActionNoop,
				Status:    "auto", PaneExcerpt: truncateTailRunes(s.Content, snapshotMaxRunes),
				CreatedAt: now,
			})
			if err != nil {
				slog.Error("audit write failed; blocking review noop (FR-024)", "error", err)
				d.notify(ctx, "Herd Auto Prompter: persistence failure",
					"A review-declined no-op was blocked because its audit record could not be written.")
				return
			}
			applied = true
			if rate, err := d.opt.Store.GetAgentRate(ctx, s.AgentID); err == nil {
				updated := domain.RegisterAutoPrompt(*rate, now)
				updated.AgentID = s.AgentID
				if err := d.opt.Store.UpdateAgentRate(ctx, updated); err != nil {
					slog.Error("agent rate update failed", "error", err)
				}
			}
			d.mu.Lock()
			d.lastAutoNoop[s.AgentID] = now
			d.mu.Unlock()
			slog.Info("action-review noop: no reply sent",
				"agent", s.AgentID, "situation", s.Type, "audit_id", auditID)
		})
	if res.decision != nil {
		status := "accepted"
		if !executed || !applied {
			status = "rejected"
		}
		if err := d.opt.Store.UpdateLLMDecisionStatus(ctx, res.decision.ID, status); err != nil {
			slog.Error("llm decision status update failed", "error", err)
		}
	}
}

// escalate records and surfaces an escalation: no input is sent (FR-018).
func (d *Daemon) escalate(ctx context.Context, s domain.Situation, sig domain.SignatureResult,
	dec domain.Decision, tr domain.AgentTransition, now time.Time) {
	// The episode ended without an autonomous send, so an auto-send pairing for
	// this agent is spent: the operator owns the situation now, and holding the
	// claim would withhold a still-pending task from every other agent until
	// its TTL. The file was never touched, so nothing is stranded.
	d.dropAutoTaskClaim(s.AgentID)
	autoDismissReason := d.escalationAutoDismissReason(ctx, s.AgentID)

	// Dedup: if this normalized situation is already awaiting the user in the
	// pending-escalation queue, re-raising it would just be a duplicate ask.
	// Ignore the event (no ops) and record why. Gating here rather than before
	// decideAndAct means only a would-be duplicate ESCALATION is suppressed — a
	// situation that can now auto-answer (e.g. after a kill-switch resume) still
	// acts, and the rate guard / retry ceiling still see every repeated event.
	// escalate() is where almost every escalation is born, so this also caps
	// escalation storms from the async LLM/action-review/taskgen rejection paths.
	// A proven-gone/disabled agent still gets an audit row below, so the
	// automatic dismissal remains visible in history. Do not let an older
	// pending row turn that lifecycle decision into an opaque "duplicate".
	if autoDismissReason == "" && d.duplicatePendingEscalation(ctx, s) {
		d.ignoreDuplicate(ctx, s, tr, now)
		return
	}

	// Tag-only when the reason self-explains: the escalation line's budget
	// belongs to the suggestion, not to prose repeating the tag.
	rationale := "[" + string(dec.Reason) + "]"
	if dec.Rationale != "" {
		rationale += " " + dec.Rationale
	}
	rec := domain.AuditRecord{
		AgentID: s.AgentID, AgentType: s.AgentType, Signature: sig.Signature, Trigger: trigger(tr),
		SituationType: s.Type, Action: domain.AuditActionEscalated, Confidence: dec.Confidence,
		LLMConfidence: dec.LLMConfidence,
		Rationale:     rationale,
		Status:        "escalated", Suggestion: dec.Suggestion,
		// The content THIS escalation was classified from — per entry,
		// unlike the signature's first-seen provenance snapshot.
		PaneExcerpt: truncateTailRunes(s.Content, snapshotMaxRunes),
		// How this situation resolved to its rule (cosine / BM25 / exact) and
		// any embedding failure for this event, so the operator can see WHY
		// the matched rule was chosen. Auto-send rows leave these empty.
		MatchMethod: sig.Match.Method,
		MatchScore:  sig.Match.Score,
		EmbedError:  sig.Match.EmbedError,
		CreatedAt:   now,
	}
	if autoDismissReason != "" {
		// Store the would-be escalation directly as dismissed. This is atomic
		// from the front-end's perspective (it can never flash in the pending
		// queue), while Action="escalated" plus Status="dismissed" preserves
		// the same append-only audit shape as an operator dismissal.
		rec.Status = "dismissed"
		rec.Rationale += " [" + autoDismissReason + "]"
		if _, err := d.opt.Store.AppendAudit(ctx, rec); err != nil {
			slog.Error("audit write failed for auto-dismissed escalation", "error", err)
			return
		}
		slog.Info("escalation auto-dismissed: agent unavailable",
			"agent", s.AgentID, "situation", s.Type, "reason", autoDismissReason)
		return
	}
	if _, err := d.opt.Store.AppendAudit(ctx, rec); err != nil {
		slog.Error("audit write failed for escalation", "error", err)
	}

	// Rate-limit escalations pause the agent until human check-in.
	if dec.Reason == domain.ReasonRateLimited {
		if rate, err := d.opt.Store.GetAgentRate(ctx, s.AgentID); err == nil {
			paused := domain.PauseAgent(*rate)
			paused.AgentID = s.AgentID
			d.opt.Store.UpdateAgentRate(ctx, paused)
		}
	}

	agentLabel := s.AgentID
	if name, err := d.opt.Store.EnsureAgentName(ctx, s.AgentID); err == nil && name != "" {
		agentLabel = fmt.Sprintf("%s (%s)", name, s.AgentID)
	}
	title := fmt.Sprintf("Agent %s needs attention", agentLabel)
	body := fmt.Sprintf("%s situation escalated (%s).", s.Type, dec.Reason)
	if dec.Suggestion != "" {
		body += " Suggestion: " + dec.Suggestion
	}
	d.notify(ctx, title, body)
	slog.Info("escalated", "agent", s.AgentID, "situation", s.Type,
		"reason", dec.Reason, "suggestion", dec.Suggestion,
		"version", buildinfo.Version)
}

const (
	autoDismissAgentNotLive  = "agent_not_live"
	autoDismissAgentDisabled = "agent_disabled"
)

// escalationAutoDismissReason returns a non-empty audit tag only when Herdr
// authoritatively reports that an escalation's target cannot receive operator
// attention: a successful live-agent snapshot omits it, or reports it disabled.
// List failures are inconclusive and deliberately keep the escalation visible.
func (d *Daemon) escalationAutoDismissReason(ctx context.Context, agentID string) string {
	if disabled, err := d.opt.Store.AgentDisabled(ctx, agentID); err == nil && disabled {
		return autoDismissAgentDisabled
	} else if err != nil {
		slog.Warn("disabled-state read failed while raising escalation",
			"agent", agentID, "error", err)
	}
	if agentID == "" || d.opt.Herdr == nil {
		return ""
	}
	agents, err := d.opt.Herdr.ListAgents(ctx)
	if err != nil {
		return ""
	}
	for _, agent := range agents {
		if agent.AgentID != agentID {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(agent.Status), "disabled") {
			return autoDismissAgentDisabled
		}
		return ""
	}
	return autoDismissAgentNotLive
}

// auditAgentDisabled records the autonomous action that would have happened,
// without sending, learning, or advancing the rate counter. The exact
// rationale tag is intentionally stable for audit consumers.
func (d *Daemon) auditAgentDisabled(ctx context.Context, s domain.Situation,
	sig domain.SignatureResult, tr domain.AgentTransition, input string,
	confidence float64, llmConfidence *int, llmOutput string, now time.Time) {
	d.audit(ctx, domain.AuditRecord{
		AgentID: s.AgentID, AgentType: s.AgentType, Signature: sig.Signature,
		Trigger: trigger(tr), SituationType: s.Type,
		Action: domain.AuditActionDenied, Input: input, Confidence: confidence,
		LLMConfidence: llmConfidence, LLMOutput: llmOutput,
		Rationale: "[agent_disabled]", Status: "denied",
		PaneExcerpt: truncateTailRunes(s.Content, snapshotMaxRunes), CreatedAt: now,
	})
	slog.Info("autonomous action denied: agent disabled",
		"agent", s.AgentID, "situation", s.Type, "input", input)
}

// withAgentAutomation is the daemon side of the persistent disable barrier.
// The store coordinates this callback with SetAgentDisabled across processes;
// all audit/send/learn/rate work in fn therefore happens wholly before a
// disable commits, or is denied wholly after it.
func (d *Daemon) withAgentAutomation(ctx context.Context, s domain.Situation,
	sig domain.SignatureResult, tr domain.AgentTransition, input string,
	confidence float64, llmConfidence *int, llmOutput string, now time.Time,
	fn func()) bool {
	disabled, err := d.opt.Store.WithAgentAutomation(ctx, s.AgentID, fn)
	if err != nil {
		d.escalate(ctx, s, sig, domain.Decision{
			Action: domain.ActionEscalate, Reason: domain.ReasonPersistenceFailed,
			Rationale: "agent lifecycle barrier: " + err.Error(),
		}, tr, now)
		return false
	}
	if disabled {
		d.auditAgentDisabled(ctx, s, sig, tr, input, confidence, llmConfidence, llmOutput, now)
		return false
	}
	return true
}

// consultLLM assembles the consult context, stages the request, and
// launches the operator's LLM CLI — all inside a goroutine, because the
// context assembly shells out to the herdr CLI (deep pane read + pane get)
// and must not stall the main loop; every failure funnels back through
// handleLLMOutcome (NFR-006 timeout handled by the adapter).
func (d *Daemon) consultLLM(ctx context.Context, cfg config.Config, s domain.Situation,
	sig domain.SignatureResult, now time.Time) {

	llm := d.llmPort()
	// The first consult for this agent selects command_start (when
	// configured); mark it consumed here on the main loop, before the goroutine.
	d.mu.Lock()
	first := !d.firstConsult[s.AgentID]
	d.firstConsult[s.AgentID] = true
	d.mu.Unlock()
	req := domain.LLMRequest{
		RequestID: fmt.Sprintf("req-%s-%d", s.AgentID, now.UnixNano()),
		Signature: sig.Signature, SituationType: s.Type, AgentType: s.AgentType,
		AgentID: s.AgentID, Status: "pending", CreatedAt: now, First: first,
		RetryAuditID: s.RetryAuditID,
	}
	d.spawn(func() {
		outcome := llmOutcome{situation: s, sig: sig, request: req}
		err := logging.Guard("llm-consult", func() error {
			// The short name rides on the request ({agent_name}) and the
			// consult context blob; degrade to "" when unresolvable.
			agentName, nerr := d.opt.Store.EnsureAgentName(ctx, s.AgentID)
			if nerr != nil {
				agentName = ""
			}
			req.AgentName = agentName
			req.ContextJSON = string(d.consultContext(ctx, cfg, s, agentName, nil, ""))
			if _, err := d.opt.Store.StageLLMRequest(ctx, req); err != nil {
				return fmt.Errorf("staging LLM request failed: %w", err)
			}
			outcome.request = req
			decision, err := llm.Consult(ctx, req)
			outcome.decision = decision
			return err
		})
		outcome.err = err
		select {
		case d.llmResults <- outcome:
		case <-ctx.Done():
		}
	})
}

// taskReviewContext carries the extra get_context fields for a pre-send
// declared-task review: the rendered prompt that would be sent (proposed_task),
// the task-list file path, the current (next unchecked) task, and every pending
// task in file order so the LLM can pick a different one when it judges the
// current task already done.
type taskReviewContext struct {
	proposedPrompt string
	listPath       string
	currentTask    string
	pending        []string
	inProgress     []string
}

// consultDeclaredTask reviews a determined declared task before sending: it
// consults the operator's LLM (the same command + get_context/submit_decision
// MCP round-trip as consultLLM) with the proposed task in context, and funnels
// the result through the shared handleLLMOutcome so every safety re-gate still
// applies. An approval is delivered (subject to the confidence gate); a decline
// (@noop) is escalated to the operator in handleLLMOutcome. Runs the context
// assembly + spawn in a goroutine so the herdr CLI reads never stall the loop.
func (d *Daemon) consultDeclaredTask(ctx context.Context, cfg config.Config, s domain.Situation,
	sig domain.SignatureResult, tr domain.AgentTransition, declared *domain.DeclaredTask, now time.Time) {

	llm := d.llmPort()
	// Don't stack a review onto a consult already in flight for this agent —
	// the same guard the generate-task path uses. A persistence error must
	// escalate + audit, never silently drop the task (fail-safe rule).
	if pending, err := d.opt.Store.HasPendingLLMConsult(ctx, s.AgentID); err != nil {
		d.escalate(ctx, s, sig, domain.Decision{
			Action: domain.ActionEscalate, Reason: domain.ReasonPersistenceFailed,
			Rationale: "task-review pending check failed: " + err.Error(),
		}, tr, now)
		return
	} else if pending {
		slog.Info("task-review skipped: consult already in flight", "agent", s.AgentID)
		return
	}

	proposed := declared.Prompt()
	// A review consults the operator's command, but the priming/first-consult
	// variant is meant for answering pane prompts, not task review — always use
	// the base command so First stays false here. SourcePath/ReviewedTask let
	// the delayed send revalidate the checklist (see handleLLMOutcome).
	req := domain.LLMRequest{
		RequestID: fmt.Sprintf("taskreview-%s-%d", s.AgentID, now.UnixNano()),
		Signature: sig.Signature, SituationType: s.Type, AgentType: s.AgentType,
		AgentID: s.AgentID, Status: "pending", CreatedAt: now,
		TaskReview: true, ProposedTask: proposed,
		SourcePath: declared.Path, ReviewedTask: declared.Task,
		ReserveTask:  declared.Reserve,
		RetryAuditID: s.RetryAuditID,
	}
	// Stage the pending row synchronously (context filled off-loop below) so a
	// second idle event cannot race past the guard before the goroutine
	// registers the flight. Mirrors generateTask.
	if _, err := d.opt.Store.StageLLMRequest(ctx, req); err != nil {
		d.escalate(ctx, s, sig, domain.Decision{
			Action: domain.ActionEscalate, Reason: domain.ReasonPersistenceFailed,
			Rationale: "staging task review failed: " + err.Error(),
		}, tr, now)
		return
	}

	d.spawn(func() {
		outcome := llmOutcome{situation: s, sig: sig, request: req}
		err := logging.Guard("llm-task-review", func() error {
			agentName, nerr := d.opt.Store.EnsureAgentName(ctx, s.AgentID)
			if nerr != nil {
				agentName = ""
			}
			req.AgentName = agentName
			// Give the review the full remaining list (re-read off the main
			// loop) so it can pick a different task when the current one is
			// already done; degrade to just the current task on a read error.
			review := &taskReviewContext{
				proposedPrompt: proposed, listPath: declared.Path, currentTask: declared.Task,
			}
			if data, rerr := d.opt.ReadTaskFile(declared.Path); rerr == nil {
				review.pending = domain.PendingDeclaredTasks(string(data))
				review.inProgress = domain.InProgressDeclaredTasks(string(data))
			} else {
				slog.Warn("task-review: re-reading task source for pending list failed",
					"path", declared.Path, "error", rerr)
			}
			// Fill the context on the already-staged row before the CLI reads
			// it via get_context.
			req.ContextJSON = string(d.consultContext(ctx, cfg, s, agentName, review, ""))
			if err := d.opt.Store.UpdateLLMRequestContext(ctx, req.RequestID, req.ContextJSON); err != nil {
				return fmt.Errorf("staging LLM request context failed: %w", err)
			}
			outcome.request = req
			decision, err := llm.Consult(ctx, req)
			outcome.decision = decision
			return err
		})
		outcome.err = err
		select {
		case d.llmResults <- outcome:
		case <-ctx.Done():
		}
	})
}

// taskGenPort returns the LLM adapter as a TaskGeneratorPort when a
// generate-task CLI is configured, else nil (the capability is optional).
func (d *Daemon) taskGenPort() ports.TaskGeneratorPort {
	tg, ok := d.llmPort().(ports.TaskGeneratorPort)
	if !ok || !tg.GenerateTaskConfigured() {
		return nil
	}
	return tg
}

// generateTask asks the operator LLM to SUGGEST a task for an idle agent that
// has no real pending task — either no task source at all, or a declared
// source whose checklist is exhausted (FR-011 relaxation, and its
// task-source-exhausted extension). Like consultLLM the subprocess runs in a
// goroutine — it shells out to herdr for the pane excerpt and cwd — and the
// suggestion funnels back through handleTaskGenOutcome as an escalation the
// operator confirms or dismisses; it is never auto-acted. A pending
// llm_requests row guards against concurrent generations from bursty idle
// events and lets `l`-retry reuse the same in-flight check. sourceExhausted
// reports whether a declared source currently matches (exhausted) rather
// than none existing at all — see the First computation below.
func (d *Daemon) generateTask(ctx context.Context, cfg config.Config, s domain.Situation,
	sig domain.SignatureResult, tr domain.AgentTransition, now time.Time, sourceExhausted bool) {

	tg := d.taskGenPort()
	if tg == nil {
		return
	}
	// Don't stack a generation onto one already in flight for this agent.
	if pending, err := d.opt.Store.HasPendingLLMConsult(ctx, s.AgentID); err != nil {
		slog.Error("generate-task: pending check failed", "agent", s.AgentID, "error", err)
		return
	} else if pending {
		slog.Info("generate-task skipped: request already in flight", "agent", s.AgentID)
		return
	}

	// A source that has grown past its max_tasks cap must not be topped up:
	// refilling an already-long list just buries the operator, so warn and
	// skip until they prune it. Only the exhausted branch carries a
	// [[task_sources]] entry (and thus a max_tasks); the no-source branch is
	// bootstrapping an empty list, which can never be over the cap. This must
	// stay above StageLLMRequest below — returning after staging would orphan
	// a pending-consult row that the in-flight guard then trips on forever.
	if sourceExhausted {
		agentName, _ := d.opt.Store.EnsureAgentName(ctx, s.AgentID)
		// At-cap counts as full (n >= limit, matching AddTask and the
		// confirm-time append): appending even one generated task to a list
		// already holding max_tasks items would exceed the cap, so generating
		// would only raise escalations every confirm must refuse.
		if m, ok := d.matchTaskSource(ctx, cfg, tr.AgentID, tr.AgentType, tr.WorkspaceID, agentName); ok {
			if n, limit := len(domain.ParseChecklist(string(m.data))), m.src.MaxTasksLimit(); n >= limit {
				name := agentName
				if name == "" {
					name = s.AgentID
				}
				slog.Warn("maximum number of tasks reached — clean up the task list to make room for new tasks; skipping task generation",
					"agent", name, "tasks", n, "max_tasks", limit, "path", m.src.Path)
				return
			}
		}
	}

	// task_generate_command_start is only for bootstrapping a list from
	// nothing: when a declared source already exists (even exhausted), a list
	// already exists, so this is never a "first" generation. That case must
	// NOT touch firstTaskGen — it tracks the no-source path's own "has
	// generation ever fired for this agent" independently, so a LATER no-
	// source bootstrap for the same agent (e.g. its exhausted source's file
	// is later deleted) still correctly sees first=true and selects
	// task_generate_command_start.
	d.mu.Lock()
	var first bool
	if sourceExhausted {
		first = false
	} else {
		first = !d.firstTaskGen[s.AgentID]
		d.firstTaskGen[s.AgentID] = true
	}
	d.mu.Unlock()

	reason := domain.ReasonNoTaskSource
	if sourceExhausted {
		reason = domain.ReasonTaskSourceExhausted
	}

	req := domain.LLMRequest{
		RequestID: fmt.Sprintf("gentask-%s-%d", s.AgentID, now.UnixNano()),
		Signature: sig.Signature, SituationType: s.Type, AgentType: s.AgentType,
		AgentID: s.AgentID, Status: "pending", CreatedAt: now, First: first,
	}
	// Stage the pending row synchronously so a second idle event cannot race
	// past the guard above before the goroutine registers the flight.
	if _, err := d.opt.Store.StageLLMRequest(ctx, req); err != nil {
		slog.Error("generate-task: staging request failed", "agent", s.AgentID, "error", err)
		d.escalate(ctx, s, sig, domain.Decision{
			Action: domain.ActionEscalate, Reason: domain.ReasonTaskGenFailed,
			Rationale: "staging request failed: " + err.Error(),
		}, tr, now)
		return
	}

	d.spawn(func() {
		outcome := taskGenOutcome{situation: s, sig: sig, tr: tr, request: req, reason: reason}
		err := logging.Guard("generate-task", func() error {
			agentName, nerr := d.opt.Store.EnsureAgentName(ctx, s.AgentID)
			if nerr != nil {
				agentName = ""
			}
			// cwd comes from `pane get`; degrade to empty when the adapter
			// cannot report it (ports.InspectorPort is optional).
			var info domain.PaneInfo
			if insp, ok := d.opt.Herdr.(ports.InspectorPort); ok {
				if pi, perr := insp.PaneInfo(ctx, s.PaneID); perr == nil {
					info = pi
				}
			}
			cwd := info.ForegroundCwd
			if cwd == "" {
				cwd = info.Cwd
			}
			task, gerr := tg.GenerateTask(ctx, domain.TaskGenRequest{
				AgentType:   s.AgentType,
				AgentName:   agentName,
				PaneExcerpt: d.paneExcerpt(ctx, cfg, s),
				Cwd:         cwd,
				First:       first,
			})
			outcome.task = task
			return gerr
		})
		outcome.err = err
		select {
		case d.taskGenResults <- outcome:
		case <-ctx.Done():
		}
	})
}

// handleTaskGenOutcome surfaces a finished idle task generation as an
// escalation: a suggestion the operator confirms (writing a per-agent tasks.md)
// or dismisses on success, or a retryable failure rationale. It never acts on
// the pane itself.
func (d *Daemon) handleTaskGenOutcome(ctx context.Context, res taskGenOutcome) {
	now := d.opt.Clock.Now()
	s := res.situation

	// Clear the pending guard for this agent; a failed write leaves it pending
	// until expireStaleLLMWork reclaims it.
	if err := d.opt.Store.UpdateLLMRequestStatus(ctx, res.request.RequestID, "done"); err != nil {
		slog.Error("marking generate-task request done failed", "request", res.request.RequestID, "error", err)
	}

	// Staleness: generation took up to its timeout — never surface a suggestion
	// for an agent that moved on. If the agent STARTED WORKING (its live herdr
	// status is no longer idle/done), or it now has a matching task source
	// with a REAL pending item, the suggestion is moot — drop it instead of
	// leaving a stale, confirmable escalation. A still-exhausted declared
	// source (the res.reason == ReasonTaskSourceExhausted trigger) must NOT
	// count as "now has a task source": it's the same exhausted source that
	// triggered this generation, and dropping here would silently discard
	// every exhausted-source suggestion. Unknown status (list error / agent
	// absent) falls through and still surfaces the outcome (fail-safe). The
	// confirm path re-checks too, since the operator may act minutes later.
	if d.agentNotCleanlyIdle(ctx, s.AgentID) {
		slog.Info("generate-task: agent no longer idle; dropping suggestion", "agent", s.AgentID)
		return
	}
	if dt := d.declaredTaskFor(ctx, s); dt != nil && dt.Task != domain.NoTaskContent {
		slog.Info("generate-task: agent now has a task source; dropping suggestion", "agent", s.AgentID)
		return
	}

	if res.err != nil || res.task == "" {
		rationale := "generate-task produced no task"
		if res.err != nil {
			rationale = res.err.Error()
		}
		d.escalate(ctx, s, res.sig, domain.Decision{
			Action: domain.ActionEscalate, Reason: domain.ReasonTaskGenFailed,
			Rationale: rationale,
		}, res.tr, now)
		return
	}

	// Drop the noop sentinel before anything else looks at the text, so it can
	// never be written into a checklist — and later typed into a pane — as if
	// it were work. What is left is still RAW: the confirm path normalizes, and
	// NormalizeGeneratedTasks is not idempotent, so pre-normalizing here would
	// make the two passes disagree and silently drop items (see
	// StripNoopGeneratedLines).
	task, declined := domain.StripNoopGeneratedLines(res.task)

	// Parse only to VALIDATE. Output that yields no task (a bare horizontal
	// rule, a punctuation-only reply) would otherwise become a confirmable
	// escalation that can ONLY fail when the operator acts on it — the confirm
	// path runs this same parse and refuses. Deciding it here keeps the two
	// verdicts ("is there a task" / "can this be confirmed") from disagreeing.
	if len(domain.NormalizeGeneratedTasks(task)) == 0 {
		// Nothing but sentinels: the model's explicit decline. Park the
		// situation by suggesting the human-readable noop (never the raw
		// sentinel), matching the exhausted-source escalation in domain.Decide
		// — confirming it learns a plain @noop rule through the existing
		// SuggestedAction round-trip. Deliberately NOT ReasonTaskGenFailed: the
		// CLI worked, so offering `l: retry LLM` would only re-ask a model that
		// already answered. Note this rides on res.reason, so a decline for an
		// exhausted DECLARED source learns an operator-backed noop over that
		// source — which takes precedence in Decide but still yields to real
		// pending items (ReasonNoopVsPendingTasks), so a later refill is not
		// parked by it.
		if declined {
			d.escalate(ctx, s, res.sig, domain.Decision{
				Action: domain.ActionEscalate, Reason: res.reason,
				Rationale:  "generate-task declined: no new task needed",
				Suggestion: domain.ActionNoopSuggestion,
			}, res.tr, now)
			return
		}
		// No sentinel and no task IS a broken CLI — keep it retryable so
		// `l: retry LLM` can re-ask.
		d.escalate(ctx, s, res.sig, domain.Decision{
			Action: domain.ActionEscalate, Reason: domain.ReasonTaskGenFailed,
			Rationale: "generate-task produced no usable task",
		}, res.tr, now)
		return
	}

	// Success: surface the suggestion for confirmation. The reason carries
	// what triggered this generation (no_task_source, or
	// task_source_exhausted for a declared source that ran out) so the
	// escalation stays NOT retryable — the operator confirms or dismisses it
	// — while the suggestion carries the generated task for the confirm path.
	d.escalate(ctx, s, res.sig, domain.Decision{
		Action: domain.ActionEscalate, Reason: res.reason,
		Suggestion: domain.SuggestTaskPrefix + task,
	}, res.tr, now)
}

// agentNotCleanlyIdle reports whether the agent's LIVE herdr status means it is
// no longer cleanly idle (see domain.AgentBusy: not idle/done/unknown). It
// fails closed to false — an unreadable agent list or an absent agent is
// inconclusive, so the caller does not drop on it. Used to invalidate an idle
// task suggestion the agent has since moved past.
func (d *Daemon) agentNotCleanlyIdle(ctx context.Context, agentID string) bool {
	agents, err := d.opt.Herdr.ListAgents(ctx)
	if err != nil {
		return false
	}
	for _, a := range agents {
		if a.AgentID == agentID {
			// Disabled is an unavailable lifecycle state, not merely a busy
			// agent. Let escalate() create the required dismissed audit row.
			if strings.EqualFold(strings.TrimSpace(a.Status), "disabled") {
				return false
			}
			return domain.AgentBusy(a.Status)
		}
	}
	return false
}

// consultContext builds the JSON context handed to the LLM CLI via the
// get_context MCP tool: the classified situation, a pane excerpt, the
// agent's herdr location, and the pane working directory. review is
// non-nil only for a pre-send declared-task review: it adds proposed_task (the
// rendered task under review), task_list_path, current_task, and pending_tasks,
// with an answer_format that frames submit_decision as send (recommend_action)
// vs. decline (@noop) — and lets the LLM pick a different pending task.
// proposedAction is non-empty only for a pre-delivery action review
// (llm.enable_rewrite_action): it adds proposed_action (the learned reply
// about to be typed into the pane) with an answer_format that frames
// submit_decision as adapt (literal text) vs. affirm
// (@proposed_action:send) vs. veto (@noop).
func (d *Daemon) consultContext(ctx context.Context, cfg config.Config, s domain.Situation, agentName string, review *taskReviewContext, proposedAction string) []byte {
	excerpt := d.paneExcerpt(ctx, cfg, s)

	// Pane location and cwd come from `pane get`; degrade to empty values
	// when the adapter cannot report them (ports.InspectorPort is optional).
	var info domain.PaneInfo
	if insp, ok := d.opt.Herdr.(ports.InspectorPort); ok {
		var err error
		if info, err = insp.PaneInfo(ctx, s.PaneID); err != nil {
			slog.Warn("pane info for LLM context failed", "pane", s.PaneID, "error", err)
			info = domain.PaneInfo{}
		}
	}
	tabID := s.TabID
	if tabID == "" {
		tabID = info.TabID
	}
	workspaceID := s.WorkspaceID
	if workspaceID == "" {
		workspaceID = info.WorkspaceID
	}

	// "options" and answer-count fields are a wire contract with the MCP server's
	// select_options resolver (mcpserver.consultContextFields) — keep the
	// key names in sync.
	fields := map[string]any{
		"situation_type":   s.Type,
		"agent_type":       s.AgentType,
		"options":          s.Options,
		"permission_verb":  s.PermissionVerb,
		"error_summary":    s.ErrorSummary,
		"pane_excerpt":     excerpt,
		"workspace_id":     workspaceID,
		"tab_id":           tabID,
		"pane_id":          s.PaneID,
		"agent_id":         s.AgentID,
		"agent_session_id": info.AgentSessionID,
		"agent_name":       agentName,
		"cwd":              info.Cwd,
		"foreground_cwd":   info.ForegroundCwd,
		"no_reply_option":  "if the agent needs no reply (it finished or is only reporting status), submit_decision with recommend_action \"@noop\" to explicitly do nothing",
	}
	answerCount := s.EffectiveAnswerCount()
	if answerCount > 1 {
		fields["mcq_kind"] = s.MCQKind
		fields["answer_count"] = answerCount
		var answer string
		if s.MCQKind == domain.MCQCodexQuestions {
			fields["question_count"] = answerCount
			answer = fmt.Sprintf("this is a Codex multi-question form with %d questions. The pane excerpt lists every question in order. submit_decision select_options must contain exactly %d entries, one choice per question; there is no Submit pseudo-option", answerCount, answerCount)
		} else {
			// Retain the established field for prompt/client compatibility.
			fields["tab_count"] = answerCount
			answer = fmt.Sprintf(
				"this is a multi-tab question form with %d tabs (the final tab is Submit); the pane excerpt lists every question in order. submit_decision select_options is a list of exactly %d entries, one per tab in order including Submit, e.g. [1, 2, 3, 2, 1]",
				answerCount, answerCount)
		}
		if kinds, anyMulti := tabSelectKinds(s.EffectiveAnswerMultiSelect()); anyMulti {
			fields["tab_select_kinds"] = kinds
			answer += ". Some tabs are MULTI-SELECT (tab_select_kinds marks each tab \"single\" or \"multi\"; a multi-select question shows `[ ]` checkboxes on its options): for a multi-select tab pass an ARRAY of the option numbers to toggle instead of a single integer, e.g. [1, [1, 3], 2] chooses option 1 on tab 1, toggles options 1 and 3 on tab 2, and option 2 on tab 3" +
				". An option already rendered `[✔]` is ALREADY ticked (a previous, unfinished delivery): still list every option you want selected — the complete desired set for that tab, not the difference — because the keys are pressed only for the boxes that are not already ticked, and any box you leave out is never cleared"
		}
		fields["answer_format"] = answer
	} else if len(s.Options) > 0 {
		fields["answer_format"] = "answer with submit_decision select_options: a one-element list with the 1-based number of the chosen option, e.g. [2]"
	} else if s.Type == domain.SituationApproval || s.Type == domain.SituationChoice {
		fields["answer_format"] = "no numbered options were detected on the pane: answer with submit_decision recommend_action — the literal text the prompt expects (e.g. \"y\" for a y/n confirmation)"
	}
	// A matched [[task_sources]] entry is surfaced on every consult, not just
	// the pre-send review below — the LLM should know the agent's backlog
	// state regardless of what situation triggered this consult. The review
	// path below already has a fresh read of the same file (review.pending/
	// review.inProgress), so reuse it there instead of reading the file a
	// third time.
	if review != nil {
		fields["task_list_path"] = review.listPath
		fields["pending_task_count"] = len(review.pending)
		if p := taskPreview(review.pending); p != "" {
			fields["next_pending_task"] = p
		}
		fields["in_progress_task_count"] = len(review.inProgress)
		if p := taskPreview(review.inProgress); p != "" {
			fields["first_in_progress_task"] = p
		}
	} else if summary, ok := d.taskSourceSummary(ctx, cfg, s, workspaceID, agentName); ok {
		fields["task_list_path"] = summary.path
		fields["pending_task_count"] = summary.pendingCount
		if summary.nextPending != "" {
			fields["next_pending_task"] = summary.nextPending
		}
		fields["in_progress_task_count"] = summary.inProgressCount
		if summary.firstInProgress != "" {
			fields["first_in_progress_task"] = summary.firstInProgress
		}
	}
	// Pre-send declared-task review: the agent is idle with a next task queued.
	// Ask the LLM to judge, from the pane, whether sending it now is right —
	// send it (recommend_action, edited if warranted), pick a different pending
	// task when the current one is already done, or decline (@noop).
	if review != nil {
		fields["proposed_task"] = review.proposedPrompt
		fields["current_task"] = review.currentTask
		fields["pending_tasks"] = review.pending
		fields["answer_format"] = "this idle agent has a next task ready to send: proposed_task is the exact instruction that would be sent, current_task is that task's text, task_list_path is the checklist file, and pending_tasks lists every remaining task in order. Decide from the pane what to send. To send the queued task unchanged, submit_decision recommend_action \"@next_task:declared\" — the daemon sends proposed_task verbatim, so you never need to copy it. Only put literal text in recommend_action when you are editing the task or, if the pane shows current_task is ALREADY DONE, sending a different still-unfinished item from pending_tasks. To decline — the agent is still busy, every task is done, or nothing should run now — submit_decision recommend_action \"@noop\" with a one-sentence rationale. Always include confident_score: a confident decision is applied automatically (the chosen task is sent, or skipped on a decline), while a low-confidence one is surfaced to the operator"
	}
	// Pre-delivery action review: a learned rule already resolved the reply;
	// ask the LLM to adapt it to the live pane, affirm it, or veto the send.
	// A review failure or veto never surfaces here — the daemon falls back to
	// sending the original (handleActionReviewOutcome), so the instruction
	// frames the review as advisory, not gating.
	if proposedAction != "" {
		fields["proposed_action"] = proposedAction
		fields["answer_format"] = "this agent's automation resolved a learned reply that is about to be typed into the pane: proposed_action is the exact text. Review it against the live pane. To send it unchanged, submit_decision recommend_action \"@proposed_action:send\". To adapt it to what the pane currently shows, put the full replacement text in recommend_action — it is sent verbatim, so never submit commentary or partial edits. If nothing should be sent at all (the pane resolved itself, or a reply would do harm), submit_decision recommend_action \"@noop\" with a one-sentence rationale. If the review cannot decide, the daemon sends the original text unchanged"
	}
	contextJSON, _ := json.Marshal(fields)
	return contextJSON
}

// tabSelectKinds renders per-tab select kinds ("single"/"multi") for the LLM
// consult context and reports whether any tab is multi-select. Returns
// (nil, false) when no per-tab metadata is present (e.g. a form that was not
// swept), so the consult falls back to the single-select-only phrasing.
func tabSelectKinds(multi []bool) ([]string, bool) {
	if len(multi) == 0 {
		return nil, false
	}
	kinds := make([]string, len(multi))
	any := false
	for i, m := range multi {
		if m {
			kinds[i] = "multi"
			any = true
		} else {
			kinds[i] = "single"
		}
	}
	return kinds, any
}

// paneExcerpt reads a deep pane excerpt (last llm.pane_excerpt_chars) for
// LLM-facing context. It reads the VISIBLE screen: the consuming "recent"
// delta was already drained by this transition's classification read, so a
// ReadPane here would often return just the cursor line. A failed or empty
// read keeps the classification snapshot (~10 chars/line is a conservative
// floor for the line count).
func (d *Daemon) paneExcerpt(ctx context.Context, cfg config.Config, s domain.Situation) string {
	chars := cfg.LLM.PaneExcerptChars
	if chars <= 0 {
		chars = config.Default().LLM.PaneExcerptChars
	}
	// A multi-tab situation carries the swept aggregate (every question in
	// order); a fresh read would see only the currently focused tab. Take
	// the HEAD when it exceeds the excerpt cap — the consult contract says
	// the questions appear in order, so question 1 must never be cut.
	if s.EffectiveAnswerCount() > 1 {
		return truncateRunes(s.Content, chars)
	}
	excerpt := s.Content
	lines := chars / 10
	if lines < d.opt.PaneReadLines {
		lines = d.opt.PaneReadLines
	}
	if deep, err := d.readVisible(ctx, s.PaneID, lines); err == nil && strings.TrimSpace(deep) != "" {
		excerpt = deep
	} else if err != nil {
		slog.Warn("deep pane read for LLM context failed; using classification snapshot",
			"pane", s.PaneID, "error", err)
	}
	if strings.EqualFold(s.AgentType, "codex") {
		excerpt = domain.StripCodexComposer(excerpt)
	}
	return tail(excerpt, chars)
}

// startActionReview hands a literal outbound text to the consult LLM
// (llm.enable_rewrite_action) for a pre-delivery review. The subprocess runs
// in a goroutine — it must never stall the main loop — and the send completes
// in handleActionReviewOutcome. One flight per agent: a duplicate transition
// for the same signature is dropped, a new situation cancels and supersedes
// the old flight. The review must never block the send: a pending consult
// already in flight for this agent, or a staging failure, delivers the
// original text directly instead of stacking or dropping.
func (d *Daemon) startActionReview(ctx context.Context, s domain.Situation,
	sig domain.SignatureResult, dec domain.Decision, tr domain.AgentTransition, learned string) {

	cfg, allow, _ := d.snapshot()
	now := d.opt.Clock.Now()
	llm := d.llmPort()

	// A review that never runs is a review failure: deliver the original
	// through the same fallback template as handleActionReviewOutcome's
	// degrade path, so the operator's failure framing is consistent
	// wherever the review died (passthrough by default). The wrapped form
	// is re-screened like the handler's — act() only vetted the raw
	// original, and a template's framing could complete a pattern it
	// did not (SC-5).
	deliverOriginal := func(why string) {
		slog.Warn("action review skipped; sending original", "agent", s.AgentID, "reason", why)
		agentName, err := d.opt.Store.EnsureAgentName(ctx, s.AgentID)
		if err != nil {
			agentName = ""
		}
		outbound := domain.ApplyRewriteFallback(cfg.LLM.RewriteActionFallbackTemplate, dec.Input, agentName)
		escalateFallback := func(reason domain.EscalateReason, why string) {
			d.escalate(ctx, s, sig, domain.Decision{
				Action: domain.ActionEscalate, Reason: reason,
				Rationale:  "action-review fallback: " + why,
				Confidence: dec.Confidence,
				Suggestion: actionReviewSuggestion(s.Type, learned, dec.Input),
			}, tr, now)
		}
		if hit, matched := allow.Match(s.AgentType, outbound); matched {
			escalateFallback(domain.ReasonNeverAutoMatch, hit.Diagnostic())
			return
		}
		if hit, sus := allow.SuspectedIrreversible(s.AgentType, outbound); sus {
			escalateFallback(domain.ReasonSuspectedIrrevers, hit.Diagnostic())
			return
		}
		d.deliverAutonomous(ctx, s, sig, dec, tr, delivery{
			sendText: outbound, input: outbound,
			rationale: dec.Rationale + "; action review skipped (" + why + "); fallback template applied",
			learned:   learned,
		}, now)
	}

	// One flight per agent: a duplicate transition for the same signature is
	// dropped (its review is already running). A DIFFERENT-signature flight
	// cannot exist here — decideAndAct cancels (and expires) it before act()
	// dispatches — so any other pending consult row below is a foreign one.
	d.mu.Lock()
	if fl, ok := d.actionReviewInFlight[s.AgentID]; ok && fl.signature == sig.Signature {
		d.mu.Unlock()
		slog.Info("action review already in flight for this situation; dropping duplicate",
			"agent", s.AgentID)
		return
	}
	d.mu.Unlock()

	// Don't stack onto a consult already in flight for this agent (same
	// guard as task review) — but unlike a review-gated flow, degrade to
	// the direct send rather than dropping the learned action.
	if pending, err := d.opt.Store.HasPendingLLMConsult(ctx, s.AgentID); err != nil {
		deliverOriginal("pending-consult check failed: " + err.Error())
		return
	} else if pending {
		deliverOriginal("consult already in flight")
		return
	}

	// A review consults the operator's command, but the priming/first-consult
	// variant is meant for answering pane prompts, not reviewing outbound
	// text — always use the base command (First stays false, same rationale
	// as consultDeclaredTask).
	req := domain.LLMRequest{
		RequestID: fmt.Sprintf("actreview-%s-%d", s.AgentID, now.UnixNano()),
		Signature: sig.Signature, SituationType: s.Type, AgentType: s.AgentType,
		AgentID: s.AgentID, Status: "pending", CreatedAt: now,
		ActionReview: true, ProposedAction: dec.Input,
	}
	// Stage the pending row synchronously (context filled off-loop below) so
	// a second transition cannot race past the pending-consult guard before
	// the goroutine registers anything. Mirrors consultDeclaredTask.
	if _, err := d.opt.Store.StageLLMRequest(ctx, req); err != nil {
		deliverOriginal("staging failed: " + err.Error())
		return
	}

	rctx, cancel := context.WithCancel(ctx)
	d.mu.Lock()
	d.actionReviewSeq++
	token := d.actionReviewSeq
	d.actionReviewInFlight[s.AgentID] = actionReviewFlight{
		signature: sig.Signature, requestID: req.RequestID, token: token, cancel: cancel,
	}
	d.mu.Unlock()

	d.spawn(func() {
		// The short name rides on the request ({agent_name}) and is reused
		// for the fallback template if the review degrades; degrade to "".
		agentName, err := d.opt.Store.EnsureAgentName(rctx, s.AgentID)
		if err != nil {
			agentName = ""
		}
		outcome := actionReviewOutcome{
			situation: s, sig: sig, tr: tr, dec: dec, learned: learned,
			fallback: cfg.LLM.RewriteActionFallbackTemplate, agentName: agentName,
			token: token,
		}
		outcome.err = logging.Guard("llm-action-review", func() error {
			req.AgentName = agentName
			req.ContextJSON = string(d.consultContext(rctx, cfg, s, agentName, nil, dec.Input))
			if err := d.opt.Store.UpdateLLMRequestContext(rctx, req.RequestID, req.ContextJSON); err != nil {
				return fmt.Errorf("staging LLM request context failed: %w", err)
			}
			decision, err := llm.Consult(rctx, req)
			outcome.decision = decision
			return err
		})
		outcome.request = req
		select {
		case d.actionReviewResults <- outcome:
		case <-ctx.Done():
		}
	})
}

// actionReviewSuggestion formats the original action as an escalation
// suggestion the front-ends' Confirm flow can replay (same prefixes
// SuggestedAction parses), for the rare case a review outcome must escalate.
func actionReviewSuggestion(sitType domain.SituationType, learned, original string) string {
	switch sitType {
	case domain.SituationApproval:
		return "respond: " + original
	case domain.SituationChoice:
		return "choose: " + original
	case domain.SituationError:
		return "on error: " + original
	case domain.SituationIdle:
		if learned == domain.ActionNextInferredTask {
			return "send inferred next task: " + original
		}
		return "send next declared task: " + original
	}
	return original
}

// handleActionReviewOutcome finalizes an async outbound action review: the
// reviewed text is re-gated through every safety control (the reviewer is an
// LLM authoring outbound text — FR-015 applies) and delivered. A review
// failure never blocks the send — it degrades to the original as-is (or the
// configured rewrite_action_fallback_template); only safety trips on that
// degraded form escalate. The consult confidence gate deliberately does NOT
// apply here (the review is advisory; a learned rule already earned the
// send), though the LLM's self-reported score still lands on the audit row.
// Two sentinels short-circuit the review: "@proposed_action:send" sends the
// original verbatim (bypassing any fallback template), and "@noop" sends
// nothing at all — the LLM judged that no reply is better than this send.
func (d *Daemon) handleActionReviewOutcome(ctx context.Context, res actionReviewOutcome) {
	s := res.situation

	// A superseded flight must never send: a newer situation owns the pane.
	// Its staged row was already expired by cancelActionReviewExcept, so
	// only the live flight's row is resolved to "done" below — a late
	// outcome must not repaint cancelled work as completed.
	d.mu.Lock()
	fl, ok := d.actionReviewInFlight[s.AgentID]
	if !ok || fl.token != res.token {
		d.mu.Unlock()
		slog.Info("action-review outcome superseded; dropping", "agent", s.AgentID)
		if res.decision != nil {
			if err := d.opt.Store.UpdateLLMDecisionStatus(ctx, res.decision.ID, "expired"); err != nil {
				slog.Error("llm decision status update failed", "error", err)
			}
		}
		return
	}
	delete(d.actionReviewInFlight, s.AgentID)
	d.mu.Unlock()
	fl.cancel()

	// Resolving the request keeps llm_requests hygienic on every later exit
	// path (mirrors handleLLMOutcome); a failed write is reclaimed by
	// expireStaleLLMWork.
	if err := d.opt.Store.UpdateLLMRequestStatus(ctx, res.request.RequestID, "done"); err != nil {
		slog.Error("marking action-review request done failed",
			"request", res.request.RequestID, "error", err)
	}

	// The submitted action, defensively re-normalized like handleLLMOutcome
	// (a row staged by an older binary must not slip a noop spelling into
	// the pane as literal text). The self-reported score rides on the audit
	// row for observability; it never gates the review.
	reviewed := ""
	var llmConf *int
	if res.decision != nil {
		reviewed = domain.NormalizeNoopAction(res.decision.Action)
		if res.decision.ConfidentScore >= 0 {
			score := res.decision.ConfidentScore
			llmConf = &score
		}
	}

	cfg, allow, cls := d.snapshot()
	now := d.opt.Clock.Now()
	if disabled, err := d.opt.Store.AgentDisabled(ctx, s.AgentID); err == nil && disabled {
		d.auditAgentDisabled(ctx, s, res.sig, res.tr, res.dec.Input,
			res.dec.Confidence, llmConf, reviewed, now)
		if res.decision != nil {
			d.opt.Store.UpdateLLMDecisionStatus(ctx, res.decision.ID, "rejected")
		}
		return
	} else if err != nil {
		d.escalate(ctx, s, res.sig, domain.Decision{
			Action: domain.ActionEscalate, Reason: domain.ReasonPersistenceFailed,
			Rationale: "disabled-state read after action review: " + err.Error(),
		}, res.tr, now)
		return
	}

	isNoop := false
	escalateWith := func(reason domain.EscalateReason, why string) {
		suggestion := actionReviewSuggestion(s.Type, res.learned, res.dec.Input)
		if isNoop {
			// The LLM advised silence — suggesting the original send would
			// invert that. The suggestion text round-trips to @noop on a
			// confirm (suggestionAction); raw "@noop" is never shown.
			suggestion = domain.ActionNoopSuggestion
		}
		if res.decision != nil {
			d.opt.Store.UpdateLLMDecisionStatus(ctx, res.decision.ID, "rejected")
		}
		d.escalate(ctx, s, res.sig, domain.Decision{
			Action: domain.ActionEscalate, Reason: reason, Rationale: why,
			Confidence: res.dec.Confidence, LLMConfidence: llmConf,
			Suggestion: suggestion,
		}, res.tr, now)
	}

	// Final text: the reviewed action, or — on any failure, including safety
	// trips on the reviewed form — the original via the fallback template
	// (passthrough by default).
	final := strings.TrimSpace(reviewed)
	note := "rewritten by llm.command (rewrite action)"
	llmOutput := ""
	// discarded marks a degraded review, so the decision row is resolved as
	// rejected below — the model's output was NOT what got delivered.
	discarded := false
	degrade := func(why string) {
		discarded = true
		final = domain.ApplyRewriteFallback(res.fallback, res.dec.Input, res.agentName)
		note = "action review " + why + "; fallback template applied"
	}
	switch {
	case res.err != nil:
		degrade(fmt.Sprintf("failed (%v)", res.err))
		llmOutput = res.err.Error()
	case res.decision == nil:
		degrade("returned no decision")
	case domain.IsNoopAction(final):
		// Handled after the kill-switch check below: nothing will be sent.
		isNoop = true
	case strings.EqualFold(final, domain.ActionSendProposedAction):
		// The LLM affirmed the original — send it verbatim, bypassing even
		// a custom fallback template (that frames failures, not agreements).
		final = res.dec.Input
		note = "review affirmed original (" + domain.ActionSendProposedAction + ")"
		llmOutput = domain.ActionSendProposedAction
	case final == "":
		degrade("produced empty output")
	case final == domain.ActionSendProposed:
		// The task-review sentinel is meaningless here and must never reach
		// the pane as literal text.
		llmOutput = "discarded review: " + final
		degrade("submitted the task-review sentinel outside a task review")
	case len(final) > maxReviewOutput:
		// The result is SENT to a pane — a truncated half-instruction is
		// worse than the safe fallback, so oversized output degrades.
		llmOutput = fmt.Sprintf("discarded review: oversized output (%d bytes > %d cap)",
			len(final), maxReviewOutput)
		degrade("produced oversized output")
	default:
		// The LLM's actual output always lands on the audit row (LLMOutput)
		// — on a clean review it is also the delivered text.
		llmOutput = final
		if hit, matched := allow.Match(s.AgentType, final); matched {
			llmOutput = "discarded review: " + truncateRunes(final, 500)
			degrade("output matched never-auto " + hit.Diagnostic())
		} else if hit, sus := allow.SuspectedIrreversible(s.AgentType, final); sus {
			llmOutput = "discarded review: " + truncateRunes(final, 500)
			degrade("output tripped irreversible " + hit.Diagnostic())
		}
	}

	// Safety controls are never bypassed (SC-5): the final text — even the
	// fallback-wrapped original, whose framing could complete a pattern the
	// raw original did not — is screened once more, and the world may have
	// changed since Decide ran (kill switch, rate, the pane itself).
	kill, err := d.opt.Store.LatestKillEvent(ctx)
	if err != nil || domain.KillStateActive(kill) {
		escalateWith(domain.ReasonDaemonPaused, "at action review")
		return
	}
	if isNoop {
		// The review LLM vetoed the send ("@noop"): nothing goes to the
		// pane. The rate guard still runs (D3 — a noop-forever loop must
		// eventually hit the consecutive ceiling and surface to a human),
		// but the never-auto screens and the staleness re-read are skipped:
		// they vet outbound text and stale injections, and there is no send.
		rate, err := d.opt.Store.GetAgentRate(ctx, s.AgentID)
		if err != nil {
			// Fail closed: an unreadable rate row must not skip the guard.
			escalateWith(domain.ReasonPersistenceFailed, "rate read failed at action review: "+err.Error())
			return
		}
		if ok, reason := domain.CheckRate(*rate, now, domain.RateLimits{
			MaxConsecutive: cfg.Limits.MaxConsecutiveAutoPrompts,
			MaxPerMinute:   cfg.Limits.MaxAutoPromptsPerMinute,
		}); !ok {
			escalateWith(reason, "at action review")
			return
		}
		d.deliverActionReviewNoop(ctx, res, llmConf, now)
		return
	}
	if hit, matched := allow.Match(s.AgentType, final); matched {
		escalateWith(domain.ReasonNeverAutoMatch, "action review: "+hit.Diagnostic())
		return
	}
	if hit, sus := allow.SuspectedIrreversible(s.AgentType, final); sus {
		escalateWith(domain.ReasonSuspectedIrrevers, "action review: "+hit.Diagnostic())
		return
	}
	rate, err := d.opt.Store.GetAgentRate(ctx, s.AgentID)
	if err != nil {
		// Fail closed: an unreadable rate row must not skip the guard.
		escalateWith(domain.ReasonPersistenceFailed, "rate read failed at action review: "+err.Error())
		return
	}
	if ok, reason := domain.CheckRate(*rate, now, domain.RateLimits{
		MaxConsecutive: cfg.Limits.MaxConsecutiveAutoPrompts,
		MaxPerMinute:   cfg.Limits.MaxAutoPromptsPerMinute,
	}); !ok {
		escalateWith(reason, "at action review")
		return
	}

	// Staleness: the review took up to its timeout — never inject into a
	// pane that moved on. Re-classify the visible screen with the original
	// transition status. Signature equality is required only for
	// approval/choice/error: idle signatures hash a masked content head
	// that legitimately differs between the visible re-read and the
	// original consuming "recent" read, so idle matches on type alone.
	pane, err := d.readVisible(ctx, s.PaneID, d.opt.PaneReadLines)
	if err != nil {
		escalateWith(domain.ReasonHerdrUnreachable, "pane re-read failed: "+err.Error())
		return
	}
	current := cls.Classify(s.AgentType, res.tr.Status, pane)
	current.AgentID, current.PaneID, current.WorkspaceID = s.AgentID, s.PaneID, s.WorkspaceID
	current.Status = res.tr.Status
	if current.Type != s.Type {
		slog.Info("situation changed during action review; dropping send",
			"agent", s.AgentID, "was", s.Type, "now", current.Type)
		if res.decision != nil {
			d.opt.Store.UpdateLLMDecisionStatus(ctx, res.decision.ID, "expired")
		}
		return
	}
	if s.Type != domain.SituationIdle {
		// Compare raw content hashes: the staged signature may have been
		// semantically remapped onto another key, but Raw always reflects
		// the pane content as read, so equal Raw means the pane held still.
		if freshSig := domain.ComputeSignatureN(current, cfg.Embedding.PaneSalientChars); freshSig.Raw != res.sig.Raw {
			slog.Info("signature changed during action review; dropping send", "agent", s.AgentID)
			if res.decision != nil {
				d.opt.Store.UpdateLLMDecisionStatus(ctx, res.decision.ID, "expired")
			}
			return
		}
	}
	// The idle policy tolerates changed content, so the FRESH pane must be
	// re-screened the way handleTransition screened the original: Decide's
	// veto ran against content that may no longer be what's on screen.
	if hit, matched := allow.Match(s.AgentType, domain.IrreversibleScanContent(current, "")); matched {
		escalateWith(domain.ReasonNeverAutoMatch,
			hit.Diagnostic()+" (at action review)")
		return
	}
	if hit, sus := allow.SuspectedIrreversible(s.AgentType,
		domain.IrreversibleScanContent(current, "")); sus {
		escalateWith(domain.ReasonSuspectedIrrevers,
			hit.Diagnostic()+" (at action review)")
		return
	}

	original := truncateRunes(res.dec.Input, 200)
	delivered := d.deliverAutonomous(ctx, s, res.sig, res.dec, res.tr, delivery{
		sendText:      final,
		input:         final,
		rationale:     fmt.Sprintf("%s; %s (original: %q)", res.dec.Rationale, note, original),
		llmOutput:     llmOutput,
		learned:       res.learned,
		llmConfidence: llmConf,
	}, now)
	if res.decision != nil {
		// Resolve the decision AFTER delivery so the trail stays honest: a
		// degraded review delivered the fallback (its output was discarded),
		// and a blocked audit write or failed send delivered nothing — both
		// are rejected, never accepted.
		status := "accepted"
		if discarded || !delivered {
			status = "rejected"
		}
		if err := d.opt.Store.UpdateLLMDecisionStatus(ctx, res.decision.ID, status); err != nil {
			slog.Error("llm decision status update failed", "error", err)
		}
	}
}

// handleLLMOutcome re-gates a staged LLM submission through the same safety
// controls before acting; every failure path escalates (FR-010, SC-5).
func (d *Daemon) handleLLMOutcome(ctx context.Context, res llmOutcome) {
	cfg, allow, _ := d.snapshot()
	now := d.opt.Clock.Now()
	s := res.situation
	// The reconstructed transition must carry the status herdr actually
	// reported (kept on the situation at classify time): the escalation
	// trigger renders it, and a fabricated "blocked" misled operators
	// whenever the consulted pane was really idle/done.
	tr := domain.AgentTransition{AgentID: s.AgentID, PaneID: s.PaneID, Status: s.Status}

	// Resolving the request clears the retry guard for this agent; a failed
	// write leaves it pending until expireStaleLLMWork reclaims it.
	if err := d.opt.Store.UpdateLLMRequestStatus(ctx, res.request.RequestID, "done"); err != nil {
		slog.Error("marking LLM request done failed", "request", res.request.RequestID, "error", err)
	}

	if res.err != nil || res.decision == nil {
		reason := domain.ReasonLLMNoSubmit
		if res.err != nil && strings.Contains(res.err.Error(), "timeout") {
			reason = domain.ReasonLLMTimeout
		}
		if res.err != nil && strings.Contains(res.err.Error(), "staging LLM request") {
			reason = domain.ReasonPersistenceFailed
		}
		d.escalate(ctx, s, res.sig, domain.Decision{
			Action: domain.ActionEscalate, Reason: reason,
			Rationale: fmt.Sprintf("%v", res.err),
		}, tr, now)
		return
	}

	llmDec := res.decision
	// Defensive re-normalization: the MCP server already normalizes, but a
	// row staged by an older binary (or written directly) must not slip a
	// noop spelling into the pane as literal text.
	llmDec.Action = domain.NormalizeNoopAction(llmDec.Action)
	// A task-review send — the LLM approved the queued task, edited it, or
	// picked another pending item. Only a decline (@noop) is not one, so
	// classify AFTER the noop normalization above. The action is symbolic for
	// learning (llmLearnedAction) but literal for the gates and the send below.
	taskReviewSend := res.request.TaskReview && res.request.ProposedTask != "" &&
		!domain.IsNoopAction(llmDec.Action)
	// Task-review shorthand: the LLM may approve the queued task verbatim with
	// the send-proposed sentinel instead of re-typing it. Expand it to the
	// reviewed task BEFORE the re-gates/send so every safety scan and the
	// escalation suggestion operate on the real instruction, never the sentinel.
	if taskReviewSend && llmDec.Action == domain.ActionSendProposed {
		llmDec.Action = res.request.ProposedTask
	}
	// The sentinels only mean something on the consult that carries their
	// referent: "send the reviewed task" on a task review, "send the
	// proposed action" on an action review (which never enters this
	// function — see handleActionReviewOutcome). Anywhere else there is
	// nothing to expand them to, so they must never reach the pane as
	// literal text. Escalate WITHOUT a suggestion rather than via reject(),
	// which would surface the raw sentinel as confirmable — an operator
	// confirm --send would then type it into the pane.
	if llmDec.Action == domain.ActionSendProposed || llmDec.Action == domain.ActionSendProposedAction {
		d.opt.Store.UpdateLLMDecisionStatus(ctx, llmDec.ID, "rejected")
		d.escalate(ctx, s, res.sig, domain.Decision{
			Action: domain.ActionEscalate, Reason: domain.ReasonLLMNoSubmit,
			Rationale: fmt.Sprintf("LLM submitted the %q sentinel outside the review that defines it", llmDec.Action),
		}, tr, now)
		return
	}
	isNoop := domain.IsNoopAction(llmDec.Action)
	if isNoop {
		llmDec.OptionID = ""
	}
	reject := func(reason domain.EscalateReason, why string) {
		d.opt.Store.UpdateLLMDecisionStatus(ctx, llmDec.ID, "rejected")
		suggested := llmDec.Action
		if isNoop {
			// Raw "@noop" is never surfaced to humans.
			suggested = domain.ActionNoopSuggestion
		} else if taskReviewSend {
			// Carry the task-send prefix so a confirm round-trips to the
			// symbolic action (suggestionAction / frontend.SuggestedAction)
			// while the operator still sees the real instruction, and
			// materializeForSend can recover it for a confirm --send.
			suggested = "send next declared task: " + suggested
		}
		// Surface the agent's self-reported confidence on the escalation so
		// the operator can weigh the suggestion (-1 = not reported).
		if llmDec.ConfidentScore >= 0 {
			conf := fmt.Sprintf("llm confidence %d/100", llmDec.ConfidentScore)
			if why == "" {
				why = conf
			} else {
				why += "; " + conf
			}
		}
		// A retry result is itself the thing the operator asked to inspect, so
		// preserve the model's fresh rationale even for ordinary pane consults.
		// Task reviews append it in the richer block below.
		if res.request.RetryAuditID != 0 && !res.request.TaskReview {
			if r := strings.TrimSpace(llmDec.Rationale); r != "" {
				if why != "" {
					why += "; "
				}
				why += "LLM: " + r
			}
		}
		// A task review's suggestion is the LLM's EXACT recommendation (the
		// possibly-rewritten task, or "[no reply]" when it declined). Ride the
		// LLM's reasoning and the ORIGINAL queued task on the rationale so the
		// detail view still shows what was proposed even when the LLM rewrote or
		// dismissed it.
		if res.request.TaskReview {
			if r := strings.TrimSpace(llmDec.Rationale); r != "" {
				if why != "" {
					why += "; "
				}
				why += "LLM: " + r
			}
			if res.request.ProposedTask != "" {
				if why != "" {
					why += "; "
				}
				why += "proposed task: " + res.request.ProposedTask
			}
		}
		// Carry the LLM's self-reported score onto the escalation's audit row
		// (0-100); -1 means the agent reported none, so leave it nil.
		var llmConf *int
		if llmDec.ConfidentScore >= 0 {
			score := llmDec.ConfidentScore
			llmConf = &score
		}
		d.escalate(ctx, s, res.sig, domain.Decision{
			Action: domain.ActionEscalate, Reason: reason, Rationale: why,
			LLMConfidence: llmConf,
			Suggestion:    "LLM suggested: " + suggested,
		}, tr, now)
	}

	// A task review follows the SAME confidence gate as any consult (below): a
	// confident decline (@noop, score ≥ threshold) promotes a silent noop —
	// nothing is sent — while a sub-threshold outcome (approve or decline) is
	// surfaced by reject() above. No task-review special-casing here.

	// Re-gate: kill switch, never-auto patterns, heuristic, rate — the LLM can never
	// bypass safety controls.
	kill, err := d.opt.Store.LatestKillEvent(ctx)
	if err != nil || domain.KillStateActive(kill) {
		reject(domain.ReasonDaemonPaused, "at LLM promotion")
		return
	}
	// The never-auto match and the heuristic both scan the situation's
	// actionable region (pending dialog + outbound task text), not the full
	// scrollback, so stale narration doesn't veto a benign action (FR-015/FR-016).
	declaredPrompt := ""
	if dt := d.declaredTaskFor(ctx, s); dt != nil {
		declaredPrompt = dt.Prompt()
	}
	scan := domain.IrreversibleScanContent(s, declaredPrompt)
	if hit, matched := allow.Match(s.AgentType, scan); matched {
		reject(domain.ReasonNeverAutoMatch, hit.Diagnostic())
		return
	}
	// The LLM authors the outbound text, so the never-auto patterns screen the
	// submitted action too — the LLM can never smuggle an irreversible
	// operation past the never-auto screen (FR-015). A noop has no outbound text to
	// screen.
	if !isNoop {
		if hit, matched := allow.Match(s.AgentType, llmDec.Action); matched {
			reject(domain.ReasonNeverAutoMatch, "LLM action: "+hit.Diagnostic())
			return
		}
	}
	// The heuristic screens the situation's actionable region plus the
	// outbound text the LLM authored (which is what would actually be sent).
	if hit, sus := allow.SuspectedIrreversible(s.AgentType, scan); sus {
		reject(domain.ReasonSuspectedIrrevers, hit.Diagnostic())
		return
	}
	if !isNoop {
		if hit, sus := allow.SuspectedIrreversible(s.AgentType, llmDec.Action); sus {
			reject(domain.ReasonSuspectedIrrevers, "LLM action: "+hit.Diagnostic())
			return
		}
	}
	rate, err := d.opt.Store.GetAgentRate(ctx, s.AgentID)
	if err != nil {
		reject(domain.ReasonPersistenceFailed, err.Error())
		return
	}
	if ok, reason := domain.CheckRate(*rate, now, domain.RateLimits{
		MaxConsecutive: cfg.Limits.MaxConsecutiveAutoPrompts,
		MaxPerMinute:   cfg.Limits.MaxAutoPromptsPerMinute,
	}); !ok {
		reject(reason, "at LLM promotion")
		return
	}
	// Choice sanity. Multi-question forms expect one token per answer step —
	// a mismatched length must never be partially
	// delivered. Single menus require the chosen option to exist in the
	// offered set. A noop is never an offered option; it deliberately
	// bypasses both checks.
	answerCount := s.EffectiveAnswerCount()
	if s.Type == domain.SituationChoice && answerCount > 1 && !isNoop {
		if seq, ok := domain.ParseDigitSeries(llmDec.Action); !ok || len(seq) != answerCount {
			detail := fmt.Sprintf("multi-question form expects a series of %d answer tokens, got %q", answerCount, llmDec.Action)
			if s.MCQKind != domain.MCQCodexQuestions {
				detail = fmt.Sprintf("multi-tab form expects a series of %d digits (e.g. \"1 2 3 2 1\"), got %q", answerCount, llmDec.Action)
			}
			reject(domain.ReasonUnfamiliarOptions,
				detail)
			return
		}
	} else if s.Type == domain.SituationChoice && !isNoop && len(s.Options) > 0 {
		found := false
		for _, o := range s.Options {
			if strings.EqualFold(strings.TrimSpace(o), strings.TrimSpace(llmDec.Action)) ||
				strings.EqualFold(strings.TrimSpace(o), strings.TrimSpace(llmDec.OptionID)) {
				found = true
				break
			}
		}
		if !found {
			reject(domain.ReasonUnfamiliarOptions, "LLM option not offered")
			return
		}
	}
	// A retry is an explicit request for another LLM opinion, not permission
	// to auto-act. Once all safety and option-validity checks pass, surface
	// that opinion as a fresh escalation regardless of confidence. This also
	// turns a high-confidence @noop into a reviewable "[no reply]" suggestion.
	if res.request.RetryAuditID != 0 {
		reject(domain.ReasonLLMRetry,
			fmt.Sprintf("operator-requested retry of audit #%d", res.request.RetryAuditID))
		return
	}
	// Confidence gate: auto-act only when the LLM's self-reported confidence
	// meets the operator's threshold. A missing score (-1) is below any
	// threshold >= 0, so it always escalates; the default threshold (99) auto-
	// acts only on a near-certain score, and a threshold above 100 (e.g. 999)
	// never auto-acts. The reject closure surfaces the score ("llm confidence
	// N/100") on the escalation.
	if llmDec.ConfidentScore < cfg.LLM.AutoActConfidenceThreshold {
		reject(domain.ReasonLLMLowConfidence, "")
		return
	}
	history, err := d.opt.Store.DecisionsForSignature(ctx, res.sig.Signature, 50)
	if err != nil {
		reject(domain.ReasonPersistenceFailed, err.Error())
		return
	}
	// Exclude pre-reset decisions from the confidence the variance guard checks
	// and the score written to the audit row (id > the signature's floor).
	// Best-effort: a missing/failed state read just leaves full history in play.
	if res.sig.Signature != "" {
		if st, err := d.opt.Store.GetSignature(ctx, res.sig.Signature); err == nil && st != nil {
			history = domain.DecisionsSince(history, st.DecisionFloorID)
		}
	}
	// The variance guard compares the LLM's action to the signature's dominant
	// learned action; it does not apply to a declared-task review. A review's
	// history is symbolic ("@next_task:declared") while llmDec.Action here is
	// the expanded task text, so the two never compare equal — the guard would
	// reject every review. The review has its own gate: the LLM judged the live
	// pane, and the re-gates below still apply.
	if !res.request.TaskReview {
		if conf := domain.Confidence(history, cfg.Learning.ConfirmationWeight); conf.TopAction != "" && conf.TopAction != llmDec.Action {
			reject(domain.ReasonVarianceGuard, "LLM contradicts history")
			return
		}
	}

	// Both scores land on the auto-acted audit row: the computed 0-1 agreement
	// over this signature's history AND the LLM's self-reported 0-100 (>= 0
	// here — a lower score would have escalated at the gate above).
	computedConf := domain.Confidence(history, cfg.Learning.ConfirmationWeight).Score
	llmConf := llmDec.ConfidentScore

	if isNoop {
		// NoOp promotion: record and stand down — nothing is sent. The
		// staleness re-read is skipped on purpose: it exists solely to
		// prevent stale *injections*, a stale noop is harmless, and
		// rejecting it would recreate the very escalation noise the noop
		// resolves. Audit-before-act still applies (FR-024), and the final
		// lifecycle barrier prevents learning or rate advancement after disable.
		// A declined review sends nothing, so an auto-send pairing for this
		// agent is spent: holding it would withhold a still-pending task from
		// every other agent until the claim's TTL. The file was never touched,
		// so nothing is stranded.
		d.dropAutoTaskClaim(s.AgentID)
		executed := d.withAgentAutomation(ctx, s, res.sig, tr, "",
			computedConf, &llmConf, llmDec.CapturedOutput, now, func() {
				if _, err := d.opt.Store.AppendAudit(ctx, domain.AuditRecord{
					AgentID: s.AgentID, AgentType: s.AgentType, Signature: res.sig.Signature, Trigger: "llm-fallback",
					SituationType: s.Type, Action: "noop", Input: "",
					Confidence: computedConf, LLMConfidence: &llmConf,
					Rationale: "LLM: " + llmDec.Rationale, LLMOutput: llmDec.CapturedOutput,
					Status: "auto", PaneExcerpt: truncateTailRunes(s.Content, snapshotMaxRunes), CreatedAt: now,
				}); err != nil {
					slog.Error("audit write failed; blocking LLM noop (FR-024)", "error", err)
					d.notify(ctx, "Herd Auto Prompter: persistence failure",
						"An LLM-derived no-op was blocked because its audit record could not be written.")
					return
				}
				if err := d.opt.Store.UpdateLLMDecisionStatus(ctx, llmDec.ID, "accepted"); err != nil {
					slog.Error("llm decision status update failed", "error", err)
				}
				if _, err := d.opt.Store.RecordDecision(ctx, domain.DecisionRecord{
					Signature: res.sig.Signature, SituationType: s.Type, AgentType: s.AgentType,
					ChosenAction: domain.ActionNoop, Source: domain.SourceLLM, CreatedAt: now,
				}); err != nil {
					slog.Error("decision record write failed", "error", err)
				}
				d.ensureSignatureRow(ctx, res.sig.Signature, s.Type, s.AgentType, now)
				// The runaway counter still advances (D3): a self-flapping agent
				// must not consult-and-noop silently forever. lastAutoSend stays
				// untouched (nothing was sent); lastAutoNoop is stamped so a
				// self-resuming flap does not reset that counter.
				if rate2, err := d.opt.Store.GetAgentRate(ctx, s.AgentID); err == nil {
					updated := domain.RegisterAutoPrompt(*rate2, now)
					updated.AgentID = s.AgentID
					if err := d.opt.Store.UpdateAgentRate(ctx, updated); err != nil {
						slog.Error("agent rate update failed", "error", err)
					}
				}
				d.mu.Lock()
				d.lastAutoNoop[s.AgentID] = now
				d.mu.Unlock()
				slog.Info("LLM noop accepted: no reply sent", "agent", s.AgentID)
			})
		if !executed {
			if err := d.opt.Store.UpdateLLMDecisionStatus(ctx, llmDec.ID, "rejected"); err != nil {
				slog.Error("llm decision status update failed", "error", err)
			}
		}
		return
	}

	// Staleness re-check: the consultation took up to the LLM timeout, so
	// re-read the pane and verify the same situation is still showing —
	// never inject a stale answer into a pane that moved on. Use the visible
	// screen: the consuming "recent" delta was already drained by the
	// classification read, so it would read empty and falsely reject.
	_, _, cls := d.snapshot()
	pane, err := d.readVisible(ctx, s.PaneID, d.opt.PaneReadLines)
	if err != nil {
		reject(domain.ReasonHerdrUnreachable, "pane re-read failed: "+err.Error())
		return
	}
	current := cls.Classify(s.AgentType, s.Status, pane)
	current.AgentID, current.PaneID, current.WorkspaceID = s.AgentID, s.PaneID, s.WorkspaceID
	current.Status = s.Status
	if s.EffectiveAnswerCount() > 1 {
		// Multi-tab situations carry the swept AGGREGATE as content, which
		// never hashes equal to any single frame: staleness here means the
		// form is gone, reshaped, or REPLACED — a different form with the
		// same tab count (consults take minutes) must never receive this
		// series, so the first question is compared verbatim too.
		form, ok := domain.ParseMCQForm(s.AgentType, pane)
		if !ok || form.Kind != s.MCQKind || form.AnswerCount != s.EffectiveAnswerCount() ||
			domain.ExtractAgentMCQForm(s.MCQKind, pane) != domain.FirstMCQQuestion(s.Content) {
			reject(domain.ReasonLLMNoSubmit, "stale: form changed during consult")
			return
		}
	} else if freshSig := domain.ComputeSignatureN(current, cfg.Embedding.PaneSalientChars); freshSig.Raw != res.sig.Raw {
		// Compare raw content hashes: the staged signature may have been
		// semantically remapped onto another key, but Raw always reflects
		// the pane content as read, so equal Raw means the pane did not
		// move on.
		reject(domain.ReasonLLMNoSubmit, "stale: situation changed during consult")
		return
	}

	// Task-review send: the review took up to the LLM timeout, so besides the
	// pane the SOURCE can have moved on (item checked off, list edited). Re-read
	// it and refuse to inject a task whose next item changed since review —
	// escalate for the operator instead of sending a stale task.
	var taskReserve *domain.DeclaredTask
	reserveText := ""
	if res.request.TaskReview && res.request.SourcePath != "" {
		data, rerr := d.opt.ReadTaskFile(res.request.SourcePath)
		if rerr != nil {
			reject(domain.ReasonHerdrUnreachable, "task source unreadable before send: "+rerr.Error())
			return
		}
		pendingTasks := domain.PendingDeclaredTasks(string(data))
		// The idle poll can pair an agent with a task that is NOT the list's
		// first pending item (a sibling agent took that one), so a claimed task
		// only has to be still pending; every other review keeps the strict
		// "still the next task" check.
		claim, claimed := d.autoTaskClaimFor(s.AgentID)
		claimed = claimed && claim.sourcePath == canonicalTaskPath(res.request.SourcePath) &&
			claim.taskText == res.request.ReviewedTask
		fresh := domain.NextDeclaredTask(string(data)) == res.request.ReviewedTask
		if claimed {
			fresh = slices.Contains(pendingTasks, res.request.ReviewedTask)
		}
		if !fresh {
			reject(domain.ReasonLLMNoSubmit, "task list changed during review")
			return
		}
		// A reserving source must consume a checklist item on EVERY send, or
		// the same line is handed out again. The flag is the one pinned on the
		// request at consult time — re-reading the config here would let a
		// reload mid-review silently downgrade this to an unreserved send.
		if res.request.ReserveTask {
			// The review may have swapped to a different pending item, so
			// reserve the item the outbound text actually consumes, not the one
			// that was proposed. An edit/adaptation of the reviewed task still
			// resolves to the reviewed task.
			reserveText = reservedByAction(llmDec.Action, res.request.ReviewedTask, pendingTasks)
			taskReserve = &domain.DeclaredTask{Path: res.request.SourcePath, Reserve: true}
		}
	}

	// Same numbered-menu mapping as the learned act path: deliver the digit
	// for approval/choice, the literal reply otherwise. Multi-tab forms take
	// the validated digit series, one keystroke per tab — off the main loop
	// (the keystrokes take seconds) and mutually exclusive with any sweep.
	// `pane` is the visible re-read verified current just above.
	if s.Type == domain.SituationChoice && s.EffectiveAnswerCount() > 1 {
		ks, ok := d.opt.Herdr.(ports.KeystrokeSender)
		if !ok {
			reject(domain.ReasonHerdrUnreachable,
				"herdr adapter cannot send keystrokes; multi-tab answer needs them")
			return
		}
		if !d.acquirePane(s.AgentID) {
			reject(domain.ReasonRateLimited,
				"another pane interaction is in flight for this agent; not delivering concurrently")
			return
		}
		groups, _ := domain.ParseTabSelections(llmDec.Action)
		d.deliverSeriesLLM(ctx, ks, s, res.sig, tr, llmDec, groups,
			computedConf, &llmConf, now)
		return
	}
	// Claude's remote-environment picker takes the adaptive keystroke
	// deliverer for the same reason as the act path: its commit protocol is
	// per-build, so a blind digit + Enter send could no-op or double-commit.
	// The freshness gate above only proves SOME picker is standing (the
	// approval salient is verb-only); the deliverer's live label→digit
	// mapping is what rejects a swapped environment list. Without keystroke
	// support this fails closed to escalation, like the multi-tab branch —
	// the generic send below could commit whatever the caret rests on.
	if s.Type == domain.SituationApproval && strings.EqualFold(s.AgentType, "claude") {
		if _, isRemoteEnv := domain.ClaudeRemoteEnvForm(pane); isRemoteEnv {
			ks, ok := d.opt.Herdr.(ports.KeystrokeSender)
			if !ok {
				reject(domain.ReasonHerdrUnreachable,
					"herdr adapter cannot send keystrokes; the remote-environment picker needs them")
				return
			}
			if !d.acquirePane(s.AgentID) {
				reject(domain.ReasonRateLimited,
					"another pane interaction is in flight for this agent; not delivering concurrently")
				return
			}
			d.deliverRemoteEnvLLM(ctx, ks, s, res.sig, tr, llmDec, computedConf, &llmConf, now)
			return
		}
	}
	executed := d.withAgentAutomation(ctx, s, res.sig, tr, llmDec.Action,
		computedConf, &llmConf, llmDec.CapturedOutput, now, func() {
			auditID, err := d.opt.Store.AppendAudit(ctx, domain.AuditRecord{
				AgentID: s.AgentID, AgentType: s.AgentType, Signature: res.sig.Signature, Trigger: "llm-fallback",
				SituationType: s.Type, Action: domain.AuditActionAutoPrefix + llmDec.Action, Input: llmDec.Action,
				Confidence: computedConf, LLMConfidence: &llmConf,
				Rationale: "LLM: " + llmDec.Rationale, LLMOutput: llmDec.CapturedOutput,
				Status: "auto", PaneExcerpt: truncateTailRunes(s.Content, snapshotMaxRunes), CreatedAt: now,
			})
			if err != nil {
				slog.Error("audit write failed; blocking LLM action (FR-024)", "error", err)
				d.notify(ctx, "Herd Auto Prompter: persistence failure",
					"An LLM-derived action was blocked because its audit record could not be written.")
				return
			}
			// Same recycled-pane guard as the rule path — and it matters more
			// here, because an LLM review can add seconds of drift between the
			// capture and this send.
			if taskReserve != nil {
				if recycled, why := d.paneRecycled(ctx, s); recycled {
					slog.Warn("pane was recycled during the task review; not sending",
						"agent", s.AgentID, "reason", why)
					d.dropAutoTaskClaim(s.AgentID)
					d.opt.Store.UpdateAuditStatus(ctx, auditID, "escalated")
					d.notify(ctx, "Herd Auto Prompter: action delivery skipped",
						fmt.Sprintf("Agent %s: %s; the task was not sent.", s.AgentID, why))
					return
				}
			}
			// Same claim-before-send rule as the rule path: an auto-send source's
			// item is marked "[-]" before the text reaches the pane.
			rollback, rerr := d.reserveDeclaredTask(taskReserve, reserveText)
			if rerr != nil {
				d.dropAutoTaskClaim(s.AgentID)
				slog.Warn("reviewed task could not be reserved; not sending", "agent", s.AgentID, "error", rerr)
				d.opt.Store.UpdateAuditStatus(ctx, auditID, "escalated")
				d.notify(ctx, "Herd Auto Prompter: action delivery skipped",
					fmt.Sprintf("Agent %s: the reviewed task was claimed or edited before it could be sent (%v); please review the list.", s.AgentID, rerr))
				return
			}
			if err := ports.SendToAgent(ctx, d.opt.Herdr, s.PaneID, s.AgentType,
				domain.DeliverKeystroke(s.Type, s.AgentType, pane, llmDec.Action)); err != nil {
				rollback()
				// The task is pending again; release the pairing with it so no
				// other agent is denied the item until the claim's TTL (the
				// rule path does this centrally in deliverAutonomous).
				d.dropAutoTaskClaim(s.AgentID)
				d.opt.Store.UpdateAuditStatus(ctx, auditID, "escalated")
				d.notify(ctx, "Herd Auto Prompter: action delivery failed", err.Error())
				return
			}
			d.opt.Store.UpdateLLMDecisionStatus(ctx, llmDec.ID, "accepted")
			d.opt.Store.RecordDecision(ctx, domain.DecisionRecord{
				Signature: res.sig.Signature, SituationType: s.Type, AgentType: s.AgentType,
				ChosenAction: d.llmLearnedAction(llmDec, taskReviewSend),
				Source:       domain.SourceLLM, CreatedAt: now,
			})
			d.ensureSignatureRow(ctx, res.sig.Signature, s.Type, s.AgentType, now)
			if rate2, err := d.opt.Store.GetAgentRate(ctx, s.AgentID); err == nil {
				updated := domain.RegisterAutoPrompt(*rate2, now)
				updated.AgentID = s.AgentID
				d.opt.Store.UpdateAgentRate(ctx, updated)
			}
			d.mu.Lock()
			d.lastAutoSend[s.AgentID] = now
			d.mu.Unlock()
			slog.Info("LLM decision promoted and delivered", "agent", s.AgentID, "action", llmDec.Action)
			d.scheduleUnblockCheck(verifyunblock.Params{
				PaneID: s.PaneID, AgentID: s.AgentID, AgentType: s.AgentType,
				Signature: res.sig.Signature, Input: llmDec.Action, Excerpt: s.Content, SituationType: s.Type,
			})
		})
	if !executed {
		// The lifecycle barrier refused (the agent was disabled between the
		// decision and its execution): nothing reached the pane, so release the
		// pairing — as deliverAutonomous does on the rule path — or the task
		// stays promised to an agent that never got it.
		d.dropAutoTaskClaim(s.AgentID)
		d.opt.Store.UpdateLLMDecisionStatus(ctx, llmDec.ID, "rejected")
	}
}

// ensureSignatureRow makes an LLM-learned signature addressable: every
// recorded decision gets a signatures state row, so `hap signatures
// list/delete/reset` and the escalation rule line can reach it (#175).
// EnsureSignature is a single atomic INSERT OR IGNORE, so an existing row is
// never touched (a below-threshold consult on an already learned rule lands
// here too — its mode/confirmations/floor must survive, including against a
// concurrent correction writer). DecisionFloorID starts 0 so LLM decisions
// keep counting toward the plurality until the operator first speaks;
// applyCorrection floors them out at that point. Errors only log: the
// decision and audit are already durable, and a failed visibility write must
// not reject an accepted LLM decision (fail-safe daemon path).
func (d *Daemon) ensureSignatureRow(ctx context.Context, signature string,
	situationType domain.SituationType, agentType string, now time.Time) {
	if err := d.opt.Store.EnsureSignature(ctx, domain.SignatureState{
		Signature: signature, SituationType: situationType,
		AgentType: agentType, Mode: domain.ModeShadow, UpdatedAt: now,
	}); err != nil {
		slog.Error("signature row write failed", "signature", signature, "error", err)
	}
}

// processCorrections consumes front-end-written correction records and
// re-derives the affected signature's learning state — front-ends never
// write hot-path rows (FR-007, Solution §Concurrency).
func (d *Daemon) processCorrections(ctx context.Context) {
	cfg, _, _ := d.snapshot()
	corrections, err := d.opt.Store.UnprocessedCorrections(ctx)
	if err != nil {
		slog.Error("reading corrections failed", "error", err)
		return
	}
	for _, c := range corrections {
		arm, err := d.applyCorrection(ctx, cfg, c)
		if err != nil {
			slog.Error("applying correction failed", "correction", c.ID, "error", err)
			continue
		}
		if err := d.opt.Store.MarkCorrectionProcessed(ctx, c.ID); err != nil {
			// Stop the batch: re-applying this correction on the next sweep
			// would double-record decisions and inflate confidence.
			slog.Error("marking correction processed failed; aborting batch", "correction", c.ID, "error", err)
			return
		}
		// Arm the post-action self-check only after the correction is committed,
		// so a correction retried on a transient error never arms it twice.
		if arm != nil {
			d.scheduleUnblockCheck(*arm)
		}
	}
}

// processLLMRetries drains operator-queued LLM-retry requests: for each, it
// re-drives a fresh consult on the escalation's agent by re-injecting an
// attention transition for the agent's LIVE pane (the normal
// read→classify→decide→consult pipeline). Terminal outcomes are marked
// processed; transient store/Herdr failures stay queued for the next sweep.
func (d *Daemon) processLLMRetries(ctx context.Context) {
	retries, err := d.opt.Store.UnprocessedLLMRetries(ctx)
	if err != nil {
		slog.Error("reading llm retries failed", "error", err)
		return
	}
	for _, r := range retries {
		if !d.applyLLMRetry(ctx, r) {
			continue
		}
		if err := d.opt.Store.MarkLLMRetryProcessed(ctx, r.ID); err != nil {
			// A duplicate on the next sweep only re-injects a coalesced
			// capture and re-hits the pending-consult guard — harmless, so
			// log and keep draining rather than aborting the batch.
			slog.Error("marking llm retry processed failed", "retry", r.ID, "error", err)
		}
	}
}

// applyLLMRetry re-injects one retry request. It resolves the escalation's
// agent to its live pane and schedules an attention capture, guarding against
// stacking a retry onto a consult that is still running. The return value says
// whether the queue item reached a terminal outcome and may be marked
// processed; false preserves transient failures for the next sweep.
func (d *Daemon) applyLLMRetry(ctx context.Context, r domain.LLMRetry) bool {
	audit, err := d.opt.Store.GetAudit(ctx, r.AuditID)
	if err != nil {
		slog.Error("llm retry: reading audit failed", "retry", r.ID, "audit", r.AuditID, "error", err)
		return false
	}
	if audit == nil || audit.AgentID == "" {
		slog.Warn("llm retry: audit missing or has no agent", "retry", r.ID, "audit", r.AuditID)
		return true
	}
	agentID := audit.AgentID

	// Guard: never stack a retry onto a consult that is still in flight for
	// this agent (a pending llm_requests row). The operator re-queues once it
	// resolves; expireStaleLLMWork clears an abandoned one after 2×timeout.
	if pending, err := d.opt.Store.HasPendingLLMConsult(ctx, agentID); err != nil {
		slog.Error("llm retry: pending-consult check failed", "agent", agentID, "error", err)
		return false
	} else if pending {
		slog.Info("llm retry skipped: consult already in flight", "agent", agentID)
		return true
	}

	// Resolve the agent to its current pane AND its live herdr state — retry
	// re-reads the LIVE screen, so the pane may differ from where the
	// escalation first fired. ListAgents reports the real agent_status, type,
	// and location; carry the WHOLE transition forward (like reconcileAttention)
	// so the recapture reflects herdr's current snapshot, never a fabricated one.
	agents, err := d.opt.Herdr.ListAgents(ctx)
	if err != nil {
		slog.Error("llm retry: listing agents failed", "agent", agentID, "error", err)
		return false
	}
	var live domain.AgentTransition
	var found bool
	for _, a := range agents {
		if a.AgentID == agentID {
			live, found = a, true
			break
		}
	}
	if !found {
		slog.Info("llm retry: agent no longer present", "agent", agentID)
		label := agentID
		if name, nerr := d.opt.Store.EnsureAgentName(ctx, agentID); nerr == nil && name != "" {
			label = fmt.Sprintf("%s (%s)", name, agentID)
		}
		d.notify(ctx, "Herd Auto Prompter: retry skipped",
			fmt.Sprintf("Agent %s is no longer present — cannot re-invoke the LLM.", label))
		return true
	}

	// The retry is now accepted: its consult is not competing with an
	// in-flight one and its agent still has a live pane. Retire the source
	// escalation before scheduling the recapture. This removes the stale
	// failure from the pending list and prevents the duplicate check from
	// mistaking the explicitly requested retry for a duplicate of itself.
	// A retry failure writes a fresh retryable escalation; a successful LLM
	// result writes a fresh llm_retry escalation for operator review.
	retired, err := d.opt.Store.RetireEscalationForRetry(ctx, r.AuditID)
	if err != nil {
		slog.Error("llm retry: retiring source escalation failed",
			"retry", r.ID, "audit", r.AuditID, "agent", agentID, "error", err)
		return false
	}
	if !retired {
		slog.Info("llm retry skipped: source escalation no longer pending",
			"retry", r.ID, "audit", r.AuditID, "agent", agentID)
		return true
	}
	live.RetryAuditID = r.AuditID

	// Re-drive the attention pipeline with the live transition. scheduleCapture
	// fires unconditionally (it re-enters via delayedTr → handleAttention, not
	// handleTransition, so the status does not gate the capture), re-reads the
	// pane, and re-derives every gate at fire time — re-consulting when the live
	// pane still needs help. Forwarding herdr's real status/type/location (vs a
	// fabricated "blocked") keeps the trigger honest and lets the Claude
	// structural detectors and signature lookup see the correct agent type. This
	// also carries an idle task-generation retry correctly: the live status is
	// idle, so the pane re-classifies as idle and re-enters the generate-task
	// path (and if the agent has since started working, it is no longer idle and
	// no stale suggestion is regenerated). scheduleCapture coalesces per pane, so
	// rapid double-retries collapse into one capture.
	slog.Info("llm retry: source escalation retired; re-driving consult",
		"retry", r.ID, "audit", r.AuditID, "agent", agentID,
		"pane", live.PaneID, "status", live.Status)
	d.scheduleCapture(ctx, live)
	return true
}

// applyCorrection re-derives the affected signature's learning state from one
// operator correction. When the correction was DELIVERED to the agent
// (c.Sent), it returns the params to arm the post-action unblock self-check;
// the caller arms it only after the correction is committed (see
// processCorrections), so a correction retried on a transient error never arms
// the check twice.
func (d *Daemon) applyCorrection(ctx context.Context, cfg config.Config, c domain.CorrectionRecord) (*verifyunblock.Params, error) {
	audit, err := d.opt.Store.GetAudit(ctx, c.AuditID)
	if err != nil {
		return nil, err
	}
	if audit == nil {
		return nil, fmt.Errorf("correction %d references missing audit %d", c.ID, c.AuditID)
	}
	now := d.opt.Clock.Now()

	// The operator responded: this is human interaction for the runaway
	// guard (FR-019) regardless of confirm/correct semantics.
	if audit.AgentID != "" {
		d.registerHumanInteraction(ctx, audit.AgentID)
	}

	// A delivered correction (c.Sent) arms the post-action self-check so a
	// still-blocked agent surfaces a delivery_failed audit row, exactly as the
	// daemon's own autonomous sends do. (AgentID is the pane id.) The params
	// are returned rather than armed here so the caller arms once, after commit.
	var arm *verifyunblock.Params
	if c.Sent && audit.AgentID != "" {
		arm = &verifyunblock.Params{
			PaneID: audit.AgentID, AgentID: audit.AgentID, AgentType: audit.AgentType,
			Signature: audit.Signature, Input: c.CorrectedAction, Excerpt: audit.PaneExcerpt,
			SituationType: audit.SituationType,
		}
	}

	if audit.Signature == "" {
		// Nothing learnable (e.g. herdr-unreachable escalation).
		return arm, d.opt.Store.UpdateAuditStatus(ctx, c.AuditID, "resolved")
	}

	history, err := d.opt.Store.DecisionsForSignature(ctx, audit.Signature, 50)
	if err != nil {
		return nil, err
	}

	state, err := d.opt.Store.GetSignature(ctx, audit.Signature)
	if err != nil {
		return nil, err
	}
	if state == nil {
		state = &domain.SignatureState{
			Signature: audit.Signature, SituationType: audit.SituationType,
			AgentType: agentTypeOf(history, audit), Mode: domain.ModeShadow,
		}
	} else if state.AgentType == "" || state.AgentType == "unknown" {
		// Heal rules learned before the audit carried an agent type.
		if at := agentTypeOf(history, audit); at != "unknown" {
			state.AgentType = at
		}
	}

	// A rule begins when the OPERATOR first speaks, so it starts on a clean
	// slate: floor out every decision that predates this one. The trigger is
	// "no operator/rule evidence since the rule's last (re)birth" — NOT state
	// row absence, because LLM decisions now create their own shadow row for
	// CLI addressability (#175), so the row can exist while every decision is
	// still SourceLLM. The evidence is checked over the POST-FLOOR slice of
	// the same capped window that confidence and graduation consume, on
	// purpose, so the floor decision and the score it protects always see
	// the same rows:
	//   - post-floor, not full-window: a reset rule's pre-reset operator rows
	//     are "no evidence yet" by definition (the reset floored them), so
	//     post-reset LLM guesses still get floored when the operator first
	//     speaks again instead of polluting the re-earning score;
	//   - capped, not unbounded: operator rows older than the read window
	//     contribute nothing to confidence/graduation anywhere (every reader
	//     caps at 50, and recency decay zeroes them long before that), so
	//     when 50+ LLM guesses have buried old operator rows, advancing the
	//     floor discards exactly the in-window guesses — an unbounded check
	//     would instead keep that contradictory noise in the live window and
	//     pin the rule under the variance guard.
	// Flooring only a purely-LLM slice means this can only ever discard the
	// LLM's own guesses — never operator evidence that still counts.
	// (Modulo one narrow window: the decision below and the state row are
	// written without a shared transaction, so a crash between them orphans
	// an operator decision that the next correction floors out. Harmless —
	// that rule restarts at 1.00, which is what this wants anyway.)
	//
	// Those guesses are not agreement about anything: an LLM that answered
	// the same situation six different ways would otherwise hand its brand
	// new rule a contradictory history it never earned, scoring it below the
	// variance-guard floor and pinning it there — visibly inconsistent, since
	// the operator has agreed with it exactly once and never disagreed. The
	// rows are KEPT (the floor only hides them from confidence/graduation),
	// so history and audit stay intact.
	//
	// history is read BEFORE the operator's decision is recorded below, so
	// history[0] is the newest PRE-EXISTING decision: the new one lands above
	// the floor and counts, giving the fresh rule 1.00 over its single
	// operator decision. (Contrast ResetGraduation, which floors the newest
	// decision INCLUDING itself — a reset has no evidence yet and reads "-".)
	if len(history) > 0 &&
		!domain.HasOperatorEvidence(domain.DecisionsSince(history, state.DecisionFloorID)) &&
		history[0].ID > state.DecisionFloorID {
		state.DecisionFloorID = history[0].ID
	}

	// Confidence/graduation see only post-reset decisions (id > the floor); the
	// full history above still feeds agent-type healing.
	prior := domain.Confidence(domain.DecisionsSince(history, state.DecisionFloorID), cfg.Learning.ConfirmationWeight)

	// Was this a confirmation of the suggested/learned action, or a
	// correction to something else?
	suggested := suggestionAction(audit)
	isConfirmation := suggested != "" && c.CorrectedAction == suggested

	// Record the operator's decision (corrections count in the recency
	// window; FR-007).
	if _, err := d.opt.Store.RecordDecision(ctx, domain.DecisionRecord{
		Signature: audit.Signature, SituationType: audit.SituationType,
		AgentType: state.AgentType, ChosenAction: c.CorrectedAction,
		Source: domain.SourceOperator, IsCorrection: !isConfirmation, CreatedAt: now,
	}); err != nil {
		return nil, err
	}

	// Permanent graduation (revised FR-007) is enforced inside
	// ObserveConfirmation, which freezes a graduated rule's count outright — so
	// both paths below can call it unconditionally. Do NOT reintroduce an
	// `audit.Status == "auto"` guard here: that proxy does not mean "a graduated
	// rule acted". It also matches an LLM auto-act on a signature with no rule
	// at all, and an auto row from before a reset — in both cases skipping
	// ObserveConfirmation stranded the streak at 0, so the rule could never
	// (re-)graduate no matter how often the operator confirmed it.
	newState := *state
	if isConfirmation {
		consistent := prior.TopAction == "" || prior.TopAction == c.CorrectedAction
		newState = domain.ObserveConfirmation(newState, consistent)
	} else {
		// Correcting a shadow suggestion: the corrected action starts its
		// own streak.
		newState = domain.ObserveConfirmation(newState, prior.TopAction == c.CorrectedAction)
	}

	refreshed, err := d.opt.Store.DecisionsForSignature(ctx, audit.Signature, 50)
	if err != nil {
		return nil, err
	}
	conf := domain.Confidence(domain.DecisionsSince(refreshed, newState.DecisionFloorID), cfg.Learning.ConfirmationWeight)
	newState.CachedConfidence = conf.Score
	newState.UpdatedAt = now
	newState = domain.MaybeGraduate(newState, conf.Score,
		confidenceThresholds(cfg).ForType(audit.SituationType), cfg.Learning.GraduationN)

	if err := d.opt.Store.UpsertSignature(ctx, newState); err != nil {
		return nil, err
	}

	// Error corrections clear the retry counter (FR-014).
	if audit.SituationType == domain.SituationError {
		d.opt.Store.ResetErrorRetry(ctx, audit.Signature)
	}

	// Correction lineage in the audit trail (DR-005).
	d.opt.Store.AppendAudit(ctx, domain.AuditRecord{
		AgentID: audit.AgentID, AgentType: state.AgentType, Signature: audit.Signature,
		Trigger:       domain.TriggerOperatorCorrection,
		SituationType: audit.SituationType, Action: "corrected:" + c.CorrectedAction,
		Input: c.CorrectedAction, Rationale: map[bool]string{true: domain.RationaleOperatorConfirmed, false: domain.RationaleOperatorCorrected}[isConfirmation],
		CorrectsAuditID: c.AuditID, Status: "resolved", CreatedAt: now,
	})
	return arm, d.opt.Store.UpdateAuditStatus(ctx, c.AuditID, "resolved")
}

// expireStaleLLMWork marks dangling pending LLM decisions AND pending consult
// requests expired. Reclaiming stale requests is load-bearing for the retry
// guard: a consult whose outcome was never delivered (daemon restart/upgrade,
// cancelled context) would otherwise leave a "pending" llm_requests row that
// blocks every future retry for that agent forever.
func (d *Daemon) expireStaleLLMWork(ctx context.Context) {
	cfg, _, _ := d.snapshot()
	// The reclaim window must exceed the LONGEST work a pending llm_requests
	// row can represent: a consult (LLMTimeout) or an idle task generation
	// (GenerateTaskTimeout, which can be configured longer). Using only the
	// consult timeout would expire a still-running generation early, letting a
	// second idle event launch a concurrent generation (defeats the in-flight
	// guard in generateTask).
	longest := cfg.LLMTimeout()
	if gt := cfg.GenerateTaskTimeout(); gt > longest {
		longest = gt
	}
	cutoff := d.opt.Clock.Now().Add(-2 * longest)

	if _, err := d.opt.Store.ExpireStalePendingLLMRequests(ctx, cutoff); err != nil {
		slog.Error("expiring stale LLM requests failed", "error", err)
	}

	pending, err := d.opt.Store.PendingLLMDecisions(ctx)
	if err != nil {
		return
	}
	for _, p := range pending {
		if p.CreatedAt.Before(cutoff) {
			d.opt.Store.UpdateLLMDecisionStatus(ctx, p.ID, "expired")
		}
	}
}

func (d *Daemon) registerHumanInteraction(ctx context.Context, agentID string) {
	// The human owns the pane now: a pending reviewed send is moot.
	d.cancelActionReviewExcept(ctx, agentID, "")
	rate, err := d.opt.Store.GetAgentRate(ctx, agentID)
	if err != nil {
		return
	}
	updated := domain.RegisterHumanInteraction(*rate)
	updated.AgentID = agentID
	if err := d.opt.Store.UpdateAgentRate(ctx, updated); err != nil {
		slog.Error("resetting rate on human interaction failed", "error", err)
	}
}

// taskSourceMatch is the source-selection result shared by declaredTask
// (which builds the outbound prompt) and taskSourceSummary (which only needs
// the winning path + pending items for get_context).
type taskSourceMatch struct {
	src  config.TaskSource
	data []byte
}

// matchTaskSource walks cfg.TaskSources for a source matching the given
// agent/workspace selectors, preferring a source with real remaining work
// over one whose checklist is fully completed (a matched source with a fully
// completed list still resolves so a templated "all done" prompt or a
// completed-summary can be built; sources with a real remaining task take
// precedence over one that is done).
// sourceSelectsAgent reports whether BOTH of a task source's selectors — the
// agent selector (id / type / short name, "" = any) and the workspace selector
// (herdr workspace label with "*" wildcards, falling back to the raw id when
// no name resolves) — match this agent. It is the single definition shared by
// matchTaskSource and the auto-send-when-idle poll, so the two can never
// disagree about which agents a source owns.
func (d *Daemon) sourceSelectsAgent(ctx context.Context, src config.TaskSource,
	agentID, agentType, workspaceID, agentName string) bool {

	if !src.MatchesAgent(agentID, agentType, agentName) {
		return false
	}
	if src.Workspace == "" || src.Workspace == "*" {
		return true
	}
	// workspaceName caches the listing for workspaceCacheTTL, so resolving it
	// per source costs at most one herdr round-trip per sweep.
	target := d.workspaceName(ctx, workspaceID)
	if target == "" {
		target = workspaceID
	}
	return domain.MatchWorkspace(src.Workspace, target)
}

func (d *Daemon) matchTaskSource(ctx context.Context, cfg config.Config, agentID, agentType, workspaceID, agentName string) (taskSourceMatch, bool) {
	var completed *taskSourceMatch
	for _, src := range cfg.TaskSources {
		if !d.sourceSelectsAgent(ctx, src, agentID, agentType, workspaceID, agentName) {
			continue
		}
		data, err := d.opt.ReadTaskFile(src.Path)
		if err != nil {
			slog.Warn("task source unreadable", "path", src.Path, "error", err)
			continue
		}
		if domain.NextDeclaredTask(string(data)) != "" {
			return taskSourceMatch{src: src, data: data}, true
		}
		// Only a real checklist with every item checked counts as completed;
		// an empty or non-checklist file must not suppress tier-2 inference.
		if completed == nil && domain.HasChecklistItems(string(data)) {
			completed = &taskSourceMatch{src: src, data: data}
		}
	}
	if completed != nil {
		return *completed, true
	}
	return taskSourceMatch{}, false
}

// taskSummaryMaxRunes bounds the get_context preview of the next pending or
// in-progress task (short-field truncation, unlike the pane-excerpt-sized
// limits).
const taskSummaryMaxRunes = 200

// taskSourceSummaryFields is the get_context task_source surface: present
// whenever a [[task_sources]] entry matches, independent of situation type.
type taskSourceSummaryFields struct {
	path            string
	pendingCount    int
	nextPending     string
	inProgressCount int
	firstInProgress string
}

// taskPreview returns a truncated preview of the first item, or "" when
// items is empty — the shared rule for whether a get_context preview field
// is included at all.
func taskPreview(items []string) string {
	if len(items) == 0 {
		return ""
	}
	return truncateRunes(items[0], taskSummaryMaxRunes)
}

// taskSourceSummary reports the get_context task_source surface for the
// given agent/workspace.
func (d *Daemon) taskSourceSummary(ctx context.Context, cfg config.Config, s domain.Situation, workspaceID, agentName string) (taskSourceSummaryFields, bool) {
	m, ok := d.matchTaskSource(ctx, cfg, s.AgentID, s.AgentType, workspaceID, agentName)
	if !ok {
		return taskSourceSummaryFields{}, false
	}
	pending := domain.PendingDeclaredTasks(string(m.data))
	inProgress := domain.InProgressDeclaredTasks(string(m.data))
	return taskSourceSummaryFields{
		path:            m.src.Path,
		pendingCount:    len(pending),
		nextPending:     taskPreview(pending),
		inProgressCount: len(inProgress),
		firstInProgress: taskPreview(inProgress),
	}, true
}

// declaredTask resolves the operator-declared next task for a transition.
// A task source's agent selector matches the agent/pane id, the agent type,
// or the agent's short name; the workspace selector matches the workspace's
// herdr name (label) with "*" wildcards, falling back to the raw workspace
// id when no name is resolvable. A matched source with a fully completed
// list still resolves (task content NoTaskContent, "none") so callers can
// tell "matched but exhausted" from "nothing matched" — the decision core
// (domain.Decide) never sends that templated "none" prompt, escalating or
// generating more tasks instead; sources with a real remaining task take
// precedence.
//
// One side effect, safe from every caller: when the agent holds an auto-send
// claim whose task is no longer pending, that claim is dead — it can never be
// honored again — so it is dropped here rather than lingering until its TTL and
// keeping the agent out of the next pairing. A LIVE claim is only read.
func (d *Daemon) declaredTask(ctx context.Context, cfg config.Config, tr domain.AgentTransition, agentName string) *domain.DeclaredTask {
	m, ok := d.matchTaskSource(ctx, cfg, tr.AgentID, tr.AgentType, tr.WorkspaceID, agentName)
	if !ok {
		return nil
	}
	task := domain.NextDeclaredTask(string(m.data))
	// The idle poll may have assigned this agent a task further down the list
	// (a sibling agent was given the first one in the same sweep). Honor that
	// pairing when the claimed item is still pending in the matched source;
	// otherwise the claim is stale — drop it and resolve normally.
	if claim, ok := d.autoTaskClaimFor(tr.AgentID); ok && claim.sourcePath == canonicalTaskPath(m.src.Path) {
		if slices.Contains(domain.PendingDeclaredTasks(string(m.data)), claim.taskText) {
			task = claim.taskText
		} else {
			d.dropAutoTaskClaim(tr.AgentID)
		}
	}
	if task == "" {
		task = domain.NoTaskContent
	}
	// Resolve cwd only when the template references it (the common case does
	// not), keeping the main-loop `pane get` shell-out off the hot path.
	// Via TemplateOrDefault, so this asks the same question the frontend's
	// manual send does — the template that will actually render, default
	// included, not just the source's own field.
	cwd := ""
	if strings.Contains(domain.TemplateOrDefault(m.src.NextTaskTemplate), "{cwd}") {
		cwd = d.paneCwd(ctx, tr.PaneID)
	}
	return &domain.DeclaredTask{
		Task: task, Path: m.src.Path, Template: m.src.NextTaskTemplate,
		AgentName: agentName, Cwd: cwd,
		// A source opts out of the pre-send LLM review with llm_review=false
		// (nil = the default, on).
		LLMReview: m.src.EnableLLMReview == nil || *m.src.EnableLLMReview,
		// An auto-send source hands tasks out unattended, so the delivered
		// item must be marked "[-]" as it goes — otherwise the next idle agent
		// is handed the very same "[ ]" line.
		Reserve: m.src.EnableAutoSendTaskWhenIdle,
	}
}

func (d *Daemon) declaredTaskFor(ctx context.Context, s domain.Situation) *domain.DeclaredTask {
	cfg, _, _ := d.snapshot()
	name, err := d.opt.Store.EnsureAgentName(ctx, s.AgentID)
	if err != nil {
		name = ""
	}
	return d.declaredTask(ctx, cfg, domain.AgentTransition{
		AgentID: s.AgentID, AgentType: s.AgentType, WorkspaceID: s.WorkspaceID,
	}, name)
}

// workspaceCacheTTL bounds how long the workspace id→name listing is reused;
// declaredTask runs on every event and must not spawn the herdr CLI each time.
const workspaceCacheTTL = 5 * time.Second

// workspaceName resolves a workspace id to its herdr display name (label).
// It returns "" when no name is resolvable — the Herdr port has no locator
// surface, the listing fails, or the id is unknown.
func (d *Daemon) workspaceName(ctx context.Context, workspaceID string) string {
	if workspaceID == "" {
		return ""
	}
	loc, ok := d.opt.Herdr.(ports.LocatorPort)
	if !ok {
		return ""
	}
	now := d.opt.Clock.Now()
	d.mu.RLock()
	names, at := d.wsNames, d.wsNamesAt
	d.mu.RUnlock()
	if names == nil || now.Sub(at) > workspaceCacheTTL {
		names = map[string]string{}
		if wss, err := loc.ListWorkspaces(ctx); err == nil {
			for _, w := range wss {
				names[w.ID] = w.Label
			}
		} else {
			slog.Warn("workspace listing failed; task-source workspace match falls back to ids", "error", err)
		}
		d.mu.Lock()
		d.wsNames, d.wsNamesAt = names, now
		d.mu.Unlock()
	}
	return names[workspaceID]
}

// paneCwd resolves a pane's working directory (foreground cwd, falling back to
// the shell cwd) for the {cwd} placeholder in next_task_template. It caches per
// pane for workspaceCacheTTL so declaredTask — which runs on the main loop —
// never shells out to `pane get` on every event; a deleted-directory suffix
// ("/path (deleted)") is left as herdr reports it. Returns "" when the Herdr
// port has no inspector surface or the read fails.
func (d *Daemon) paneCwd(ctx context.Context, paneID string) string {
	if paneID == "" {
		return ""
	}
	now := d.opt.Clock.Now()
	d.mu.RLock()
	entry, ok := d.paneCwds[paneID]
	d.mu.RUnlock()
	if !ok || now.Sub(entry.at) > workspaceCacheTTL {
		// NEVER shell out on the main loop: `pane get` can block for its CLI
		// timeout. Refresh in the background and return the cached (or empty)
		// value now — the first {cwd} render for a cold pane is empty and
		// self-heals once the refresh lands (the review path still gets the
		// real cwd via get_context's cwd field, resolved off-loop).
		d.refreshPaneCwd(ctx, paneID)
	}
	return entry.cwd
}

// refreshPaneCwd resolves a pane's working directory off the main loop and
// caches it (foreground cwd, falling back to the shell cwd), deduping
// concurrent refreshes per pane. A failed read caches "" for the TTL so a
// broken `pane get` is not hammered every event.
func (d *Daemon) refreshPaneCwd(ctx context.Context, paneID string) {
	insp, ok := d.opt.Herdr.(ports.InspectorPort)
	if !ok {
		return
	}
	d.mu.Lock()
	if d.paneCwdRefreshing[paneID] {
		d.mu.Unlock()
		return
	}
	d.paneCwdRefreshing[paneID] = true
	d.mu.Unlock()
	d.spawn(func() {
		defer func() {
			d.mu.Lock()
			delete(d.paneCwdRefreshing, paneID)
			d.mu.Unlock()
		}()
		cwd := ""
		if pi, err := insp.PaneInfo(ctx, paneID); err == nil {
			cwd = pi.ForegroundCwd
			if cwd == "" {
				cwd = pi.Cwd
			}
		} else {
			slog.Warn("pane cwd refresh for next-task template failed", "pane", paneID, "error", err)
		}
		d.mu.Lock()
		d.paneCwds[paneID] = paneCwdEntry{cwd: cwd, at: d.opt.Clock.Now()}
		d.mu.Unlock()
	})
}

func (d *Daemon) audit(ctx context.Context, rec domain.AuditRecord) {
	if _, err := d.opt.Store.AppendAudit(ctx, rec); err != nil {
		slog.Error("audit write failed", "error", err)
	}
}

func (d *Daemon) notify(ctx context.Context, title, body string) {
	if d.opt.Notify == nil {
		return
	}
	if err := d.opt.Notify.Notify(ctx, title, body); err != nil {
		slog.Warn("notification failed", "error", err)
	}
}

// neverAutoRules maps agent-scoped operator configuration into the unified
// domain matcher. Flat never_auto_patterns are passed separately.
func neverAutoRules(s config.Safety) []domain.NeverAutoRule {
	rules := make([]domain.NeverAutoRule, 0, len(s.NeverAutoRules))
	for _, r := range s.NeverAutoRules {
		rules = append(rules, domain.NeverAutoRule{
			Pattern: r.Pattern, AgentTypes: r.AgentTypes,
			Kind: domain.NeverAutoStrict, Source: domain.NeverAutoOperator,
		})
	}
	return rules
}

func confidenceThresholds(cfg config.Config) domain.ConfidenceThresholds {
	return domain.ConfidenceThresholds{
		Minimum:  cfg.ConfidenceThresholds.Minimum,
		Idle:     cfg.ConfidenceThresholds.Idle,
		Approval: cfg.ConfidenceThresholds.Approval,
		Choice:   cfg.ConfidenceThresholds.Choice,
		Error:    cfg.ConfidenceThresholds.Error,
	}
}

func trigger(tr domain.AgentTransition) string {
	if tr.ManualCapture {
		return fmt.Sprintf("manual-capture: %s", tr.Status)
	}
	if tr.AutoIdleSend {
		return fmt.Sprintf("auto-idle-send: %s", tr.Status)
	}
	return fmt.Sprintf("agent-status: %s", tr.Status)
}

// suggestionAction extracts the actionable text from an escalation's
// suggestion ("respond: y" → "y"). Keep in sync with
// frontend.SuggestedAction, which does the same for confirm flows.
func suggestionAction(audit *domain.AuditRecord) string {
	sug := audit.Suggestion
	for _, prefix := range []string{"respond: ", "choose: ", "answer series: ", "on error: ", "LLM suggested: "} {
		if rest, ok := strings.CutPrefix(sug, prefix); ok {
			sug = rest
			break
		}
	}
	if strings.HasPrefix(sug, "send next declared task: ") {
		return domain.ActionNextDeclaredTask
	}
	if strings.HasPrefix(sug, "send inferred next task: ") {
		return domain.ActionNextInferredTask
	}
	// The human-readable "do nothing" suggestion round-trips to the sentinel
	// so a confirmed noop is learned as @noop, never sent as literal text.
	if sug == domain.ActionNoopSuggestion {
		return domain.ActionNoop
	}
	return sug
}

// agentTypeOf resolves the agent type for a signature learned via an
// operator correction: decision history first, then the audit row (which
// records the type observed at escalation time), else "unknown".
func agentTypeOf(history []domain.DecisionRecord, audit *domain.AuditRecord) string {
	for _, h := range history {
		if h.AgentType != "" && h.AgentType != "unknown" {
			return h.AgentType
		}
	}
	if audit != nil && audit.AgentType != "" {
		return audit.AgentType
	}
	return "unknown"
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := s[len(s)-n:]
	// Never start mid-rune: skip leading UTF-8 continuation bytes.
	for i := 0; i < len(cut) && i < utf8.UTFMax; i++ {
		if !utf8.RuneStart(cut[i]) {
			continue
		}
		return cut[i:]
	}
	return cut
}
