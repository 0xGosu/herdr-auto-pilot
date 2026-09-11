// Command herd-auto-prompter is the single static binary for the Herd Auto
// Prompter Herdr plugin. Subcommands: daemon (monitor loop), tui (Herdr
// pane), mcp (stdio MCP server for the LLM fallback), and CLI verbs that
// mirror the TUI (FR-022).
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	skilldoc "github.com/0xGosu/herdr-auto-pilot"
	"github.com/0xGosu/herdr-auto-pilot/internal/buildinfo"
	"github.com/0xGosu/herdr-auto-pilot/internal/cli"
	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/control"
	"github.com/0xGosu/herdr-auto-pilot/internal/crashguard"
	"github.com/0xGosu/herdr-auto-pilot/internal/daemon"
	"github.com/0xGosu/herdr-auto-pilot/internal/daemonhealth"
	"github.com/0xGosu/herdr-auto-pilot/internal/daemonlock"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/embedder"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
	"github.com/0xGosu/herdr-auto-pilot/internal/herdr"
	"github.com/0xGosu/herdr-auto-pilot/internal/llm"
	"github.com/0xGosu/herdr-auto-pilot/internal/logging"
	"github.com/0xGosu/herdr-auto-pilot/internal/mcpserver"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
	"github.com/0xGosu/herdr-auto-pilot/internal/profiling"
	"github.com/0xGosu/herdr-auto-pilot/internal/selfpath"
	"github.com/0xGosu/herdr-auto-pilot/internal/store"
	"github.com/0xGosu/herdr-auto-pilot/internal/store/sqlbridge"
	"github.com/0xGosu/herdr-auto-pilot/internal/store/turso"
	"github.com/0xGosu/herdr-auto-pilot/internal/streamlog"
	"github.com/0xGosu/herdr-auto-pilot/internal/tui"
	"github.com/0xGosu/herdr-auto-pilot/internal/tuisession"
)

func main() {
	if len(os.Args) < 2 {
		// Same path as `hap help`, so the overview's own footer is never
		// subject to the switches that gate command output.
		_ = cli.Run(context.Background(), nil, os.Stdout, "help", nil)
		os.Exit(2)
	}
	verb := os.Args[1]
	args := os.Args[2:]

	// Help is served from the command registry (internal/cli), so the overview,
	// the per-command guides, and the dispatch table can never drift apart.
	// `hap help <command>` and `hap <command> --help` both land in cli.Run.
	if verb == "help" || verb == "--help" || verb == "-h" {
		if err := cli.Run(context.Background(), nil, os.Stdout, "help", args); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}
	// The commands main dispatches itself still document themselves through the
	// registry, so `hap daemon --help` works before any store is opened. This
	// runs BEFORE the version branch so `hap version --help` is a help request,
	// as the guides promise; it returns false for unknown verbs.
	if cli.WantsCommandHelp(verb, args) {
		// A `hap config` topic is a TWO-word command ("config rules"), so the
		// subcommand token is forwarded when — and only when — it names one.
		// Passing the verb alone would answer `hap config rules --help` with
		// the parent's page; passing the whole argument list would forward the
		// `--help` flag itself, which reads as a request for help's own guide.
		// The FIRST POSITIONAL argument is what may name a topic, not args[0]:
		// cli.Run strips --no-hints before its own two-word lookup, so probing
		// the raw args here would disagree with it and answer
		// `hap config --no-hints rules --help` with the parent's page.
		helpTarget := []string{verb}
		for _, a := range args {
			if strings.HasPrefix(a, "-") {
				continue
			}
			if _, ok := cli.Lookup(verb + " " + a); ok {
				helpTarget = append(helpTarget, a)
			}
			break
		}
		if err := cli.Run(context.Background(), nil, os.Stdout, "help", helpTarget); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}
	if verb == "version" || verb == "--version" || verb == "-V" {
		fmt.Println(buildinfo.VersionLine())
		return
	}
	// Like version, dispatched before run() so printing or installing the
	// bundled skill never resolves config paths or opens the store.
	if verb == "skill" || verb == "--skill" {
		if err := runSkill(os.Stdout, args); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}

	// Opt-in (HAP_PROFILE_DIR); a no-op otherwise. Stopped by hand rather than
	// deferred, because os.Exit below skips defers and the last window is the
	// one a short command is profiled for.
	stopProfile := profiling.FromEnv(verb)
	err := run(verb, args)
	stopProfile()
	if err != nil {
		// `hap status` on an unhealthy daemon already printed the human detail;
		// exit non-zero for scripts without a redundant "error:" line.
		if !errors.Is(err, cli.ErrUnhealthy) {
			fmt.Fprintln(os.Stderr, "error:", err)
		}
		os.Exit(1)
	}
}

// runSkill serves `hap skill` / `hap --skill`: with no arguments (or the
// explicit `show`) it dumps the bundled SKILL.md; `install <target>...`
// writes it into the named agents' skill directories. Any other subcommand
// is refused — a typo must not answer with the 77KB document.
func runSkill(w io.Writer, args []string) error {
	if len(args) == 0 || args[0] == "show" {
		_, err := io.WriteString(w, skilldoc.HapSkill)
		return err
	}
	if args[0] != "install" {
		return fmt.Errorf("unknown skill subcommand %q (did you mean show or install?)", args[0])
	}
	// A mid-list write failure still reports the targets that DID install,
	// so the operator knows ~/.claude was refreshed even when ~/.codex broke.
	written, err := skilldoc.Install(args[1:])
	for _, path := range written {
		fmt.Fprintln(w, "installed", path)
	}
	return err
}

// shutdownSignals are the signals that cancel the run context instead of
// killing the process where it stands.
//
// SIGHUP is the one that is easy to miss: it arrives when the terminal hosting
// us goes away — the herdr pane is closed, an ssh session drops — which is a
// routine way for `hap tui` to end, not an exceptional one. Unhandled, Go's
// default for it is immediate termination, so nothing deferred runs: the store
// is never closed and the TUI never restores the terminal, leaving it in raw
// mode with the alt screen still on.
//
// What this does NOT rescue is the submit-retry drain. Those workers run on
// the same context the signal just cancelled (herdr.CLI.spawnSubmitRetry), so
// they return without pressing Enter no matter how orderly the exit is; the
// drain only does its job on the ordinary quit path, where nothing is
// cancelled. Making retries survive a signal would mean detaching their
// context from shutdown, which is a delivery-path decision, not a
// signal-handling one.
var shutdownSignals = []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP}

// shutdownContext returns the run context and its release function. It is
// separate from run() so tests can exercise the real wiring — both halves of
// it, including the release below, which no assertion could otherwise reach.
//
// Catching a signal means it no longer kills us, so a process wedged in a call
// that does not observe ctx (a blocking flock, a stuck subprocess read) would
// become unkillable by anything short of SIGKILL. The graceful path therefore
// gets exactly one chance: the handler is released the moment the first signal
// arrives, so a second one terminates immediately. Without that, adding SIGHUP
// would turn an orphaned-and-wedged process — which the hangup used to reap —
// into one that lingers forever.
//
// The release happens BEFORE the cancellation, not in a goroutine watching
// ctx.Done(): that ordering is what makes the guarantee real rather than
// probabilistic. Watching for cancellation leaves the release racing every
// consumer of ctx, so a second signal arriving promptly — the operator hitting
// it again because nothing seemed to happen — could still find the handler
// installed and be swallowed.
func shutdownContext() (context.Context, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, shutdownSignals...)
	release := func() { signal.Stop(ch) }
	go func() {
		select {
		case <-ch:
			release()
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, func() {
		release()
		cancel()
	}
}

func run(verb string, args []string) error {
	ctx, stop := shutdownContext()
	defer stop()

	// Path-printing verbs are pure diagnostics: they resolve paths WITHOUT
	// creating directories, opening the store, or touching the daemon — so they
	// stay usable, and side-effect-free, in exactly the degraded states an
	// operator runs them to inspect (an unwritable parent dir, a corrupt DB:
	// "paste your `hap paths`"). Resolve before the creating ResolvePaths below
	// so none of that filesystem mutation happens on this path.
	if verb == "state-dir" || verb == "paths" ||
		(verb == "config" && len(args) > 0 && args[0] == "path") {
		paths, err := config.ResolvePathsNoCreate()
		if err != nil {
			return err
		}
		app := &frontend.App{ConfigPath: paths.File(), StateDir: paths.StateDir}
		return cli.Run(ctx, app, os.Stdout, verb, args)
	}

	paths, err := config.ResolvePaths()
	if err != nil {
		return err
	}

	switch verb {
	case "daemon":
		return runDaemon(ctx, paths, os.Stdout, args)
	case "embed-worker":
		// Internal subcommand: the short-lived child that the embedder Client
		// spawns to run llama.cpp out-of-process. It takes its config from the
		// environment the parent sets and speaks the framed stdin/stdout embed
		// protocol; not meant to be run by hand.
		return embedder.RunWorker(ctx, os.Stdin, os.Stdout)
	case "mcp":
		return runMCP(ctx, paths)
	case "tui":
		app, closeStore, err := buildApp(paths)
		if err != nil {
			return err
		}
		defer closeStore()
		defer drainSubmitRetries(app)
		// The TUI logs into the same file as the daemon, so it honours the same
		// configured level. It used to be pinned to Info regardless, which is
		// what made its 2s-tick warnings impossible to turn down.
		if _, err := logging.Setup(paths.StateDir, logOptions(paths)); err != nil {
			return err
		}
		// Join the TUI registry so this instance is counted, and so it can
		// close the older ones (`[tui] max_instances`; the first refresh
		// sweeps). Failing to register only costs the limit — never the TUI —
		// so it is logged, not returned.
		session, err := tuisession.Register(paths.StateDir)
		if err != nil {
			slog.Warn("TUI instance limit disabled for this run", "error", err)
		} else {
			defer session.Release()
			app.TUISessions = session
		}
		return tui.Run(ctx, app)
	default:
		app, closeStore, err := buildApp(paths)
		if err != nil {
			return err
		}
		defer closeStore()
		defer drainSubmitRetries(app)
		return cli.Run(ctx, app, os.Stdout, verb, args)
	}
}

// drainSubmitRetries waits for in-flight submit-retry Enter workers before a
// one-shot process (or a closing TUI) exits, so a prompt whose first Enter
// did not take still gets its retries.
func drainSubmitRetries(app *frontend.App) {
	if w, ok := app.Herdr.(ports.SubmitRetryWaiter); ok {
		w.WaitSubmitRetries()
	}
}

func buildApp(paths config.Paths) (*frontend.App, func(), error) {
	st, err := openProcessStore(paths)
	if err != nil {
		return nil, nil, err
	}
	// Opened lazily on the first event, so a command that emits nothing never
	// touches the file.
	events := streamlog.InStateDir(paths.StateDir)
	app := &frontend.App{
		Store:       st,
		Herdr:       herdr.NewCLI(),
		ConfigPath:  paths.File(),
		ControlPath: paths.ControlSocketPath(),
		Author:      "operator",
		StateDir:    paths.StateDir,
		DaemonInfo: func() (bool, int, string) {
			return daemonlock.Info(paths)
		},
		Stream: events,
	}
	// A command the orchestrator session runs is an LLM's, not the operator's:
	// its author makes the daemon screen what it sends and refuse it while
	// the herd is paused (daemon.actionScreen), and names it on the stream.
	if daemon.CallerIsOrchestrator(paths.StateDir, os.Getenv("HERDR_PANE_ID")) {
		app.Author = domain.OrchestratorAuthor
	}
	// Desktop notifications only exist when herdr launched us as a managed
	// pane — it injects HERDR_ENV=1 and the control socket there. Outside
	// herdr the field stays nil and consumers fall back (the TUI beeps).
	if herdr.InHerdr() {
		app.Notifier = herdr.NewSocketNotifier(herdr.SocketPath())
	}
	return app, func() { st.Close(); events.Close() }, nil
}

func runDaemon(ctx context.Context, paths config.Paths, out io.Writer, args []string) error {
	// Flags are parsed as a SET, not by position: `hap daemon --replace-only
	// --ensure` used to fall through to the foreground daemon, which acquires
	// the lock and blocks forever — inside scripts/install.sh that would hang
	// `herdr plugin install`. An unrecognized flag is an error for the same
	// reason: silently running a foreground daemon is the worst possible
	// interpretation of a typo.
	var ensure, replaceOnly, restart, reload bool
	for _, arg := range args {
		switch arg {
		case "--ensure":
			ensure = true
		case "--replace-only":
			// --replace-only swaps a running stale daemon but never starts
			// one. It is what scripts/install.sh calls after fetching a new
			// binary: an upgrade must hand the herd over to the new build
			// immediately, yet the plugin BUILD step must not bring a daemon
			// up on a fresh install (or in CI, where nothing asked for a
			// monitor at all).
			replaceOnly = true
		case "--restart":
			restart = true
		case "--reload":
			reload = true
		default:
			return fmt.Errorf("unknown flag for `hap daemon`: %s (see: hap help daemon)", arg)
		}
	}
	// Each mode replaces the whole run, so a combination has no honest
	// interpretation: refuse rather than silently pick one. --replace-only is
	// --ensure's modifier alone; a restart is unconditional by definition, and
	// pairing it with "never start one" describes a stop, which is not what
	// either flag is named for.
	if replaceOnly && !ensure {
		return fmt.Errorf("--replace-only only applies to `hap daemon --ensure`")
	}
	if n := boolsSet(ensure, restart, reload); n > 1 {
		return fmt.Errorf("--ensure, --restart and --reload are alternatives; pass one (see: hap help daemon)")
	}
	switch {
	case restart:
		return restartDaemon(paths, out)
	case reload:
		return reloadDaemon(ctx, paths, out)
	case ensure:
		return ensureDaemon(paths, replaceOnly)
	}

	if _, err := logging.Setup(paths.StateDir, logOptions(paths)); err != nil {
		return err
	}

	// The herdr event hook launches the daemon from arbitrary workspace
	// dirs that may later be deleted; a dead cwd kills child CLIs at spawn
	// (the Bun-built claude dies on getcwd), so run from the state dir.
	chdirStable(paths.StateDir)

	lock, err := daemonlock.Acquire(paths)
	if err != nil {
		return err
	}
	defer lock.Release()

	// Crash-loop breaker: record this boot and decide whether to degrade
	// BEFORE building the daemon — a native embedder abort kills us inside
	// daemon.New (model load), so the only lever is the persisted boot history.
	bootCfg, _ := config.Load(paths.File())
	guard, _ := crashguard.Read(paths.StateDir)
	guard, decision := crashguard.Evaluate(guard, time.Now(), embeddingDigest(bootCfg))
	if err := crashguard.Write(paths.StateDir, guard); err != nil {
		// A failed write means this boot is not recorded, so the breaker cannot
		// accumulate toward its threshold — it is effectively disarmed until the
		// disk recovers. Log loudly rather than swallow it; continuing is still
		// right (a guard-file write failure must not itself block the daemon).
		slog.Error("crashguard write failed; crash-loop breaker impaired this boot", "error", err)
	}
	if decision.GiveUp {
		// Looping even with the embedder disabled — degrading can't help.
		// Exit without running; ensureDaemon declines future respawns until
		// the [embedding] config changes.
		slog.Error("daemon not starting: unrecoverable crash-loop", "reason", decision.Reason)
		return nil
	}
	if decision.DisableEmbedding {
		slog.Warn("crash-loop mitigation: starting with the embedder disabled (BM25 fallback)", "reason", decision.Reason)
	}
	// Reset the boot history once we survive past the window (loop broken). If
	// we crash first this never fires, so the count keeps climbing toward the
	// mitigation threshold. This read-modify-write can race an embedder-reload
	// that clears the latch (both are in-process, un-serialized by the flock):
	// worst case it briefly resurrects a just-cleared latch, which the next
	// reload's digest check re-clears — bounded and self-healing, never an
	// incorrect give-up (that path creates no timer).
	survived := time.AfterFunc(crashguard.Window, func() {
		if g, ok := crashguard.Read(paths.StateDir); ok {
			if g2, changed := g.Survived(); changed {
				_ = crashguard.Write(paths.StateDir, g2)
			}
		}
	})
	defer survived.Stop()

	// The store, under the configured engine. turso: the daemon is the one
	// process that opens the sync database; it serves it to every other hap
	// process on this machine over the store socket, and syncs it with Turso
	// Cloud from the fleet sync loop.
	var st *store.Store
	var fleet ports.FleetSyncPort
	var fleetWrites chan struct{}
	var storeSocket string
	var nodeID string
	if bootCfg.Database.IsTurso() {
		if err := config.ValidateDatabase(bootCfg); err != nil {
			return err
		}
		var err error
		if nodeID, err = store.LoadNodeID(paths.StateDir); err != nil {
			return err
		}
		fleetWrites = make(chan struct{}, 1)
		tdb, err := openTurso(ctx, paths, bootCfg, nodeID, fleetWrites, time.Now())
		if err != nil {
			return fmt.Errorf("turso: %w", err)
		}
		defer tdb.Close()
		ids := store.NewTimeOrderedIDs(store.NodeBits(nodeID), nil)
		st, err = store.OpenDB(tdb.DB(), store.Options{
			NodeID:       nodeID,
			Engine:       store.EngineTurso,
			IDs:          ids,
			Migrate:      false,
			AgentLockDir: filepath.Join(paths.StateDir, "agent-automation-locks"),
		})
		if err != nil {
			return err
		}
		defer st.Close()
		// Pull, then migrate only as the schema lead: two nodes issuing the
		// same DDL wedge the loser (see turso.PrepareSharedSchema).
		if err := turso.PrepareSharedSchema(ctx, tdb, st, time.Now); err != nil {
			return fmt.Errorf("turso: prepare schema: %w", err)
		}
		// Ids carry 12 bits of the node id; two nodes sharing them would share
		// an id space. Refuse now, before this node writes anything, rather
		// than collide later — and say which file to regenerate, and where.
		if other, clash, err := st.NodeBitsCollision(ctx); err == nil && clash {
			label := other.Label
			if label == "" {
				label = other.ID
			}
			return fmt.Errorf("turso: this node's id bits collide with node %s (%s), so ids minted here could equal "+
				"that machine's — stop this daemon, move %s aside and start again on whichever of the two machines has "+
				"never written to the shared store (its rows would otherwise stay filed under the old id)",
				label, other.ID, filepath.Join(paths.StateDir, store.NodeIDFile))
		}
		if err := importLegacyStore(ctx, paths, st); err != nil {
			slog.Warn("turso: importing the local sqlite database failed; continuing without it", "error", err)
		}
		storeSocket = paths.StoreSocketPath()
		ln, err := control.ListenSocket(storeSocket)
		if err != nil {
			return fmt.Errorf("store socket: %w", err)
		}
		// The front ends draw their ids from this allocator too, so every
		// process on the node shares one sequence.
		srv := sqlbridge.Serve(ln, tdb.Executor(), sqlbridge.ServerOptions{NextID: ids.MustNext})
		defer srv.Close()
		fleet = tdb
	} else {
		var err error
		st, err = store.Open(paths.DBPath())
		if err != nil {
			return err
		}
		defer st.Close()
	}

	cliAdapter := herdr.NewCLI()
	// The LLM adapter is rebuilt from config on every reload so that
	// llm.command/timeout edits apply without a daemon restart.
	llmFactory := func(cfg config.Config) ports.LLMPort {
		return &llm.Adapter{
			CommandTemplate: cfg.LLM.Command,
			Timeout:         cfg.LLMTimeout(),
			DBPath:          paths.DBPath(),
			ControlPath:     paths.ControlSocketPath(),
			StoreSocketPath: storeSocket,
			NodeID:          nodeID,
			Store:           st,
			TaskGenTemplate: cfg.LLM.GenerateTaskCommand,
			TaskGenTimeout:  cfg.GenerateTaskTimeout(),
			LearnTemplate:   cfg.LLM.LearnFromUserCommand,
			LearnTimeout:    cfg.LearnFromUserTimeout(),
			RerankTemplate:  cfg.LLM.RerankingCommand,
			RerankTimeout:   cfg.RerankingTimeout(),
			RunInAgentCwd:   cfg.RunLLMInAgentCwd(),
			// The `.env` files are never read here: the adapter reads them
			// when it spawns a CLI, so editing a file applies to the next
			// run, and changing the configured PATH applies on the next
			// config reload (which rebuilds this adapter).
			BaseEnv:    llm.EnvSpec{Vars: cfg.LLM.Env, File: cfg.LLM.EnvFile},
			CommandEnv: llm.EnvSpec{Vars: cfg.LLM.CommandEnv, File: cfg.LLM.CommandEnvFile},
			TaskGenEnv: llm.EnvSpec{Vars: cfg.LLM.GenerateTaskEnv, File: cfg.LLM.GenerateTaskEnvFile},
			LearnEnv:   llm.EnvSpec{Vars: cfg.LLM.LearnFromUserEnv, File: cfg.LLM.LearnFromUserEnvFile},
			RerankEnv:  llm.EnvSpec{Vars: cfg.LLM.RerankingEnv, File: cfg.LLM.RerankingEnvFile},
		}
	}

	// The embedder is likewise rebuilt whenever the [embedding] section
	// changes; nil (disabled) leaves BM25/exact matching.
	//
	// The FIRST build honors the authoritative boot decision directly, rather
	// than re-deriving suppression from the crashguard file — if it re-derived,
	// any future divergence between how bootCfg and the factory's cfg normalize
	// the [embedding] section would make the mitigation boot rebuild the very
	// embedder that is aborting. Later builds (config reloads) consult the
	// persisted latch so that editing the [embedding] config re-enables
	// semantic matching live, without a restart.
	firstBuild := true
	embedderFactory := func(cfg config.Config) ports.EmbedderPort {
		if cfg.Embedding.Disabled {
			return nil
		}
		if firstBuild {
			firstBuild = false
			if decision.DisableEmbedding {
				return nil
			}
			return embedder.New(cfg.Embedding)
		}
		if g, ok := crashguard.Read(paths.StateDir); ok {
			suppressed, cleared, changed := crashguard.EmbeddingSuppressed(g, embeddingDigest(cfg))
			if changed {
				_ = crashguard.Write(paths.StateDir, cleared)
			}
			if suppressed {
				return nil
			}
		}
		return embedder.New(cfg.Embedding)
	}

	socketPath := herdr.SocketPath()

	// The front-end App, built here purely to lend the daemon two capabilities
	// that live on the operator surface: accepting an LLM-generated task
	// (checklist writes, task-source registration) and switching full
	// self-prompting off in config.toml. Both are optional seams — the daemon
	// degrades gracefully when they are nil — so this is the ONLY place the two
	// layers meet, and the daemon package still does not import the front end.
	//
	// Author "daemon" so the automation history says who switched the mode off;
	// an operator reading `hap kill-history` must not see their own name
	// against a machine's decision.
	// The orchestrator event log, shared by the daemon and its in-process App
	// so both write through one handle.
	events := streamlog.InStateDir(paths.StateDir)
	defer events.Close()
	fspApp := &frontend.App{
		Store:       st,
		Herdr:       cliAdapter,
		ConfigPath:  paths.File(),
		ControlPath: paths.ControlSocketPath(),
		Author:      "daemon",
		StateDir:    paths.StateDir,
		// This App runs INSIDE the daemon, on its select loop. Without the
		// flag it would refuse its own actions (no lock file to read) and
		// deadlock on any it queued.
		InDaemon: true,
		Stream:   events,
	}

	d, err := daemon.New(daemon.Options{
		ConfigPath:        paths.File(),
		ControlSocketPath: paths.ControlSocketPath(),
		Store:             st,
		Herdr:             cliAdapter,
		Events:            herdr.NewSubscriber(socketPath),
		Stream:            events,
		// Socket first, CLI as the transport backstop: `notification.show`
		// answers whether the toast was actually painted, which is the
		// difference between "the operator was told about this escalation" and
		// "nothing reached anyone" (IR-003). The CLI exits 0 either way, so it
		// can only ever be the fallback.
		Notify:          herdr.NewFallbackNotifier(socketPath, cliAdapter),
		LLMFactory:      llmFactory,
		EmbedderFactory: embedderFactory,
		// Full self-prompting's two opt-in behaviors. Both are gated on their
		// own config key inside the daemon; wiring them here only makes them
		// POSSIBLE.
		AcceptGeneratedTask: fspApp.AcceptGeneratedTaskAutomatically,
		DisableFSP:          fspApp.DisableFullSelfPromptingWithReason,
		// The OPERATOR's confirm of the same suggestion, reaching the daemon as
		// a queued accept_generated_task row so it can be answered from any
		// machine in the fleet. The pane access it needs is built by the DAEMON
		// and handed in (ports.TaskSendHost), which is why the front-end
		// packages hold no herdr adapter of their own.
		ConfirmGeneratedTask: fspApp.ConfirmGeneratedTaskForOperator,
		// And the operator's manual task hand-out, for the same reason: the
		// list, the agent's short name and the source's template are all
		// node-local, so only the owning daemon can render and deliver one.
		SendTask:      fspApp.SendTaskForOperator,
		MatchIndexDir: filepath.Join(paths.StateDir, "match-index"),
		StateDir:      paths.StateDir,
		// The shared database's sync engine and its write signal (turso only;
		// both nil under sqlite, and the loop never runs).
		FleetSync:         fleet,
		FleetSyncInterval: bootCfg.Database.SyncInterval(),
		FleetWrites:       fleetWrites,
		NodeLabel:         bootCfg.Database.NodeLabel,
		// Hand the herd to the binary that replaced ours (plugin upgrade)
		// instead of soldiering on with children we can no longer spawn.
		//
		// --ensure, not a bare `daemon`: we are still holding the lock when
		// the successor starts, and a bare daemon would fail its single
		// Acquire and die. --ensure sees a stale holder, waits for it to
		// release, and only then takes over — which is exactly the handover
		// we are performing.
		HandOff: func(exePath string) error {
			// The successor's own first act is this same crash-loop check, and
			// a latched breaker makes it log "respawn suppressed" and exit 0 —
			// after our cmd.Start() has already returned nil. The daemon would
			// then read a spawn that succeeded, step aside, and leave the herd
			// with nothing running (and `hap daemon --ensure` suppressed too).
			// Refusing here keeps this daemon up instead.
			if g, ok := crashguard.Read(paths.StateDir); ok {
				cfg, _ := config.Load(paths.File())
				if blocked, _, reason := crashguard.SpawnBlocked(g, embeddingDigest(cfg)); blocked {
					return fmt.Errorf("crash-loop breaker would suppress the replacement daemon: %s", reason)
				}
			}
			return spawnDaemon(paths, exePath, "daemon", "--ensure")
		},
		// Wired only under the turso engine, because that is the only place a
		// wedged sync engine can isolate this node — and leaving it nil under
		// sqlite means an install that never opted in can never self-restart.
		RestartSelf: restartSelfHook(paths, fleet != nil,
			func(exePath string, args ...string) error { return spawnDaemon(paths, exePath, args...) }),
	})
	if err != nil {
		return err
	}
	return d.Run(ctx)
}

// restartSelfHook builds the daemon's automatic sync-recovery seam, or nil
// when there is no shared database to be isolated from.
//
// It spawns `hap daemon --restart` — NOT the `--ensure` the upgrade handoff
// uses, and the difference is the whole reason this is a separate seam.
// `--ensure` is EnsureFresh, which returns doing nothing when the running
// holder is already the version and binary it would start (daemonlock's
// `current()` check). That is exactly this case: we are restarting as the SAME
// binary. So an `--ensure` successor would bow out, we would step aside behind
// it, and the herd would be left with no daemon at all — the one outcome worse
// than the isolation being repaired. `--restart` is `force`, which stops the
// holder, waits for the lock to release, starts a fresh daemon, and confirms a
// heartbeat before reporting.
//
// It follows that this daemon does NOT exit on its own: the command it spawns
// is what stops it, exactly as when an operator types the same words. The
// caller therefore latches instead of handing over — see checkFleetSyncWedged.
//
// The crash-loop breaker is consulted first for the reason the HandOff hook
// consults it: a latched breaker would refuse the start AFTER cmd.Start() had
// already returned nil, and the failure would be invisible here.
// spawn is injected so a test can drive the real hook — the breaker gate and
// the argv both — without forking a daemon.
func restartSelfHook(paths config.Paths, turso bool, spawn func(exePath string, args ...string) error) func(string) error {
	if !turso {
		return nil
	}
	return func(reason string) error {
		if blocked, why := spawnBlocked(paths); blocked {
			return fmt.Errorf("crash-loop breaker would suppress the replacement daemon: %s", why)
		}
		self, err := selfpath.Resolve()
		if err != nil {
			return fmt.Errorf("resolve the hap binary to restart as: %w", err)
		}
		slog.Warn("restarting the daemon to clear a wedged fleet sync engine", "reason", reason, "binary", self)
		return spawn(self, "daemon", "--restart")
	}
}

// ensureDaemon starts a detached daemon if none is running (used by the
// Herdr event hook so hooks return promptly). A daemon left over from a
// different binary version is stopped and replaced, so binary upgrades
// take effect without a manual kill.
// embeddingDigest fingerprints the [embedding] config so the crash-loop
// breaker can tell an operator config change (which lifts a latch) from a
// plain restart. Any change to the section produces a different string.
func embeddingDigest(cfg config.Config) string {
	return fmt.Sprintf("%+v", cfg.Embedding)
}

func ensureDaemon(paths config.Paths, replaceOnly bool) error {
	if blocked, reason := spawnBlocked(paths); blocked {
		slog.Warn("daemon respawn suppressed by crash-loop breaker", "reason", reason)
		return nil
	}
	if replaceOnlyBowsOut(replaceOnly, func() bool {
		running, _, _ := daemonlock.Info(paths)
		return running
	}) {
		return nil
	}
	self, err := selfpath.Resolve()
	if err != nil {
		return fmt.Errorf("resolve the hap binary to run as the daemon: %w", err)
	}
	return daemonlock.EnsureFresh(paths, buildinfo.Version, self, ensureWaitTimeout, daemonlock.Stop, func() error {
		return spawnDaemon(paths, self, "daemon")
	})
}

// spawnBlocked asks the crash-loop breaker whether a daemon may be started,
// lifting a give-up latch whose [embedding] config has since changed.
//
// Shared by --ensure and --restart on purpose. An operator typing --restart
// after the breaker gave up, with the config unchanged, is asking for exactly
// the storm the breaker exists to end — one start, one abort, and they type it
// again. What differs is only how the refusal is REPORTED: a hook wants it in
// the log, a person wants it on their terminal.
func spawnBlocked(paths config.Paths) (bool, string) {
	g, ok := crashguard.Read(paths.StateDir)
	if !ok {
		return false, ""
	}
	cfg, _ := config.Load(paths.File())
	blocked, cleared, reason := crashguard.SpawnBlocked(g, embeddingDigest(cfg))
	if blocked {
		return true, reason
	}
	if g.GaveUp && !cleared.GaveUp {
		// Config changed since we gave up: lift the latch so this start retries.
		_ = crashguard.Write(paths.StateDir, cleared)
	}
	return false, ""
}

// restartDaemon implements `hap daemon --restart`: stop the running daemon
// whatever binary it came from, and start a fresh one — the answer to a
// setting that a reload cannot reach, since [database] and [logging] are read
// once, when a process opens its store.
func restartDaemon(paths config.Paths, out io.Writer) error {
	if blocked, reason := spawnBlocked(paths); blocked {
		return fmt.Errorf("refusing to start a daemon: the crash-loop breaker gave up (%s).\n"+
			"It clears when the [embedding] section changes — `hap config set embedding.disabled true`\n"+
			"is the usual escape. `hap status --stderr` prints the crash output", reason)
	}
	self, err := selfpath.Resolve()
	if err != nil {
		return fmt.Errorf("resolve the hap binary to run as the daemon: %w", err)
	}
	return restartWith(paths, out, daemonlock.Stop,
		func() error { return spawnDaemon(paths, self, "daemon") },
		func(stoppedPID int, since time.Time) (int, bool) {
			return awaitDaemonHealth(paths, stoppedPID, since, restartConfirmWait)
		})
}

// restartConfirmWait bounds how long --restart waits for the daemon it started
// to publish its first heartbeat. It is generous because the wait spans the
// whole of daemon.New — under `engine = "turso"` that includes opening the sync
// database and bootstrapping from Turso Cloud, which is seconds on a cold start
// and is exactly the configuration this flag exists to switch into.
const restartConfirmWait = 20 * time.Second

// restartWith is restartDaemon's body with its three side effects injected, so
// a test can assert what it reports without forking a real daemon — or, for
// stop, without signalling whatever process on the machine happens to own the
// pid a fixture wrote into the lock file.
//
// The reporting is the point. spawnDaemon returns once the FORK succeeded, and
// every [database] failure — a bad URL, a rejected token, a node-bits collision
// — exits AFTER the new daemon takes the lock, so "started a fresh one" would
// be a claim about the herd that nothing observed, made by the one command that
// had just killed a working daemon to get there. Waiting for a heartbeat proves
// the successor opened its store; failing to see one is reported as the unknown
// it is, never as success.
func restartWith(paths config.Paths, out io.Writer, stop func(pid int) error, start func() error, confirm func(stoppedPID int, since time.Time) (int, bool)) error {
	forgetRestartBoots(paths)
	// Taken before the stop, so it can only ever be EARLIER than the successor
	// started — the outgoing daemon may beat once more inside this window, but
	// it is excluded by pid, and one state dir has only these two daemons.
	since := time.Now()
	outcome, err := daemonlock.Restart(paths, ensureWaitTimeout, stop, start)
	if err != nil {
		return err
	}
	if outcome.Replaced {
		fmt.Fprintf(out, "stopped the daemon (pid %d, %s)\n",
			outcome.StoppedPID, daemonlock.VersionLabel(outcome.StoppedVersion))
	} else {
		fmt.Fprintln(out, "no daemon was running")
	}
	pid, ok := confirm(outcome.StoppedPID, since)
	if !ok {
		fmt.Fprintf(out, "started a replacement, but it has not reported healthy within %s.\n", restartConfirmWait)
		fmt.Fprintln(out, "Run `hap status` (add --stderr if it died) — a [database] error exits after the")
		fmt.Fprintln(out, "daemon has taken the lock, so a failed switch looks exactly like a slow start.")
		// Non-zero, because `hap daemon --restart && <next step>` must not
		// proceed on an unknown: this command's contract is that it confirms,
		// and the herd may now have no monitor at all — it stopped one to get
		// here. ErrUnhealthy is the repo's sentinel for "the human detail is
		// already printed", so main exits 1 without an "error:" line restating
		// it.
		return cli.ErrUnhealthy
	}
	fmt.Fprintf(out, "started a fresh daemon (pid %d) and it is reporting healthy\n", pid)
	return nil
}

// awaitDaemonHealth polls for a heartbeat published AFTER since — the instant
// the successor was started — and returns the pid that published it.
//
// All three conditions are load-bearing, and each closes a way of reading a
// record that is NOT the successor's as evidence it came up — which would
// report "healthy" for a daemon that in fact died on the [database] error this
// whole wait exists to catch:
//
//   - The old daemon removes its health file on a clean shutdown, but only
//     best-effort, so the pid it was killed with is excluded outright.
//   - A pid check alone is not enough. A daemon killed hard — or simply one
//     that ran before this restart, when nothing was running and stoppedPID is
//     0 — leaves a record with a DIFFERENT pid. Age is the wrong test for it
//     (daemonhealth.StaleAfter is 35s, so a daemon that died seconds ago still
//     reads fresh); the honest question is whether the beat was written after
//     we started something, which no leftover can satisfy.
//   - And the beat must come from the process still HOLDING THE LOCK. The two
//     above are both satisfied by a successor that published one heartbeat and
//     then died — inside this very poll — and the flock is the only evidence
//     here that the kernel retracts when a process dies, rather than leaving
//     behind on disk.
func awaitDaemonHealth(paths config.Paths, stoppedPID int, since time.Time, timeout time.Duration) (int, bool) {
	deadline := time.Now().Add(timeout)
	for {
		if h, ok := daemonhealth.Read(paths.StateDir); ok && h.PID > 0 && h.PID != stoppedPID &&
			h.HeartbeatAt.After(since) && holdsTheLock(paths, h.PID) {
			return h.PID, true
		}
		if time.Now().After(deadline) {
			return 0, false
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// holdsTheLock reports whether pid is the daemon currently holding the state
// dir's lock. Info answers from the flock itself, so a pid that has exited
// reads as not running however recent its last heartbeat.
func holdsTheLock(paths config.Paths, pid int) bool {
	running, holder, _ := daemonlock.Info(paths)
	return running && holder == pid
}

// forgetRestartBoots drops the crash-loop breaker's BOOT HISTORY — never a
// latch — before an operator-typed restart.
//
// The breaker counts starts, and three inside 90s auto-disable semantic
// matching (two more after that stop the daemon entirely, for both --restart
// and herdr's per-event --ensure). Iterating on a setting is exactly what this
// flag is for, and `config set` + --restart three times is a normal minute's
// work: without this, hap answers a run of config edits by latching the
// embedder off and reporting the herd as crash-looping — a false diagnosis
// pointing away from the operator's real error, and one only an [embedding]
// edit clears.
//
// Clearing the evidence is safe where clearing a latch would not be:
// spawnBlocked still refuses to start under EmbeddingOff/GaveUp, and only a
// human types --restart (the hook runs --ensure), so a genuine crash-loop
// re-accumulates through the respawns the breaker was built for.
func forgetRestartBoots(paths config.Paths) {
	g, ok := crashguard.Read(paths.StateDir)
	if !ok || len(g.Starts) == 0 {
		return
	}
	g.Starts = nil
	if err := crashguard.Write(paths.StateDir, g); err != nil {
		// Not fatal: the worst case is the pre-existing behaviour.
		slog.Warn("could not clear the crash-loop boot history for this restart", "error", err)
	}
}

// reloadDaemon implements `hap daemon --reload`: ask the running daemon to
// re-read config.toml, the same nudge every `hap config` write sends.
//
// It REFUSES when no daemon is running rather than reporting success against a
// dead socket: control.Nudge's own contract is that a failed nudge is never
// fatal (a front end has already committed its change to the database), which
// is the opposite of what an operator asking for a reload needs to hear.
func reloadDaemon(ctx context.Context, paths config.Paths, out io.Writer) error {
	running, pid, _ := daemonlock.Info(paths)
	if !running {
		return errors.New("no daemon is running, so there is nothing to reload; start one with `hap daemon --restart`")
	}
	if err := control.Nudge(ctx, paths.ControlSocketPath(), control.KindReload); err != nil {
		return fmt.Errorf("nudge the daemon (pid %d) to reload: %w", pid, err)
	}
	fmt.Fprintf(out, "asked the daemon (pid %d) to reload config.toml\n", pid)
	fmt.Fprintln(out, "note: [database] and [logging] are read once, when a process opens its store —")
	fmt.Fprintln(out, "      changing an engine, URL, token or node label needs `hap daemon --restart`")
	return nil
}

// boolsSet counts how many of the given mode flags were passed.
func boolsSet(flags ...bool) int {
	var n int
	for _, f := range flags {
		if f {
			n++
		}
	}
	return n
}

// ensureWaitTimeout bounds how long --ensure waits for a stale daemon to
// release the lock after SIGTERM. It must cover a full teardown — the
// background drain, the FAISS matcher and SQLite closes — because the
// upgrade handoff spawns `--ensure` from a daemon that is doing exactly that,
// and a successor that gives up early leaves the herd unmonitored.
const ensureWaitTimeout = 10 * time.Second

// replaceOnlyBowsOut reports whether a --replace-only run should do nothing.
//
// The gate is here rather than in EnsureFresh's start callback because
// EnsureFresh also calls start to REPLACE a daemon it just stopped —
// suppressing that would leave the herd with no monitor at all, the opposite
// of the intent. It is deliberately a check-then-act: a daemon starting in
// the gap just means --replace-only ensures one that was already fresh, which
// is a no-op.
func replaceOnlyBowsOut(replaceOnly bool, running func() bool) bool {
	return replaceOnly && !running()
}

// logOptions resolves the log level and size cap for this process.
//
// Config is (re)loaded here rather than threaded in because Setup runs before
// the callers that hold a Config, and a load is cheap. An unreadable config
// yields the defaults — logging must come up whatever else is broken.
//
// HAP_DEBUG=1 still outranks the file, so an operator can raise verbosity for
// one run without editing it.
func logOptions(paths config.Paths) logging.Options {
	cfg, err := config.Load(paths.File())
	if err != nil {
		cfg = config.Default()
	}
	opt := logging.Options{
		Level:   cfg.Logging.SlogLevel(),
		MaxSize: int64(cfg.Logging.MaxSizeMB) << 20,
	}
	if os.Getenv("HAP_DEBUG") == "1" {
		opt.Level = slog.LevelDebug
	}
	return opt
}

// spawnDaemon launches a detached hap from exePath with the given args.
// Shared by --ensure and by a running daemon handing off to the binary that
// replaced it, so both get the same detachment and stderr capture.
func spawnDaemon(paths config.Paths, exePath string, args ...string) error {
	cmd := exec.Command(exePath, args...)
	cmd.Stdout = nil
	// Capture the detached daemon's stderr to a file. A native abort in
	// the embedder (llama.cpp GGML_ASSERT → SIGABRT) prints there and is
	// invisible to Go recovery; without this it went to /dev/null and the
	// only crash evidence vanished. Best-effort: a nil file means the
	// child inherits no stderr (today's behaviour), never a failed launch.
	if stderrLog := daemonhealth.OpenStderrLog(paths.StateDir); stderrLog != nil {
		cmd.Stderr = stderrLog
		defer stderrLog.Close() // the child dup'd the fd at Start
	}
	cmd.Stdin = nil
	daemonlock.Detach(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// chdirStable moves the daemon onto a directory that outlives it; failure
// is survivable (llm.Adapter.WorkDir still guards each spawn) so it only
// warns.
func chdirStable(stateDir string) {
	if err := os.Chdir(stateDir); err == nil {
		return
	}
	if home, err := os.UserHomeDir(); err == nil && os.Chdir(home) == nil {
		slog.Warn("state dir not usable as cwd; running from home", "state_dir", stateDir)
		return
	}
	slog.Warn("could not leave inherited cwd; child CLIs may fail if it is deleted", "state_dir", stateDir)
}

func runMCP(ctx context.Context, paths config.Paths) error {
	// Some agent CLIs (e.g. codex) launch MCP servers with a sanitized
	// environment that drops HERDR_PLUGIN_STATE_DIR, which would silently
	// point us at the wrong database. HAP_DB_PATH / HAP_CONTROL_PATH —
	// injectable via the {db} / {control} placeholders in the MCP server's
	// env map — take precedence over the path resolution.
	dbPath := os.Getenv("HAP_DB_PATH")
	if dbPath == "" {
		dbPath = paths.DBPath()
	}
	controlPath := os.Getenv("HAP_CONTROL_PATH")
	if controlPath == "" {
		controlPath = paths.ControlSocketPath()
	}
	// Under the turso engine the daemon hands its MCP children the store
	// socket and this node's id, the same way it hands them the paths above —
	// the launching CLI may have sanitized the environment that would let this
	// process work them out itself. A launch WITHOUT that hint (a direct
	// replay, an MCP registration that only carries HAP_DB_PATH) must still
	// never open a local SQLite file on a turso install: decisions staged there
	// would carry local rowids the daemon never sees. mcpStore decides the
	// engine from what this process can see — the config, or the turso state
	// dir beside the database — and a turso install without a reachable daemon
	// gets the same store-unavailable error every other front end gets.
	st, err := mcpStore(ctx, paths, dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	srv := &mcpserver.Server{
		Store:            st,
		ControlPath:      controlPath,
		DefaultRequestID: os.Getenv("HAP_REQUEST_ID"),
	}
	return srv.Run(ctx, os.Stdin, os.Stdout)
}
