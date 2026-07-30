// Command herd-auto-prompter is the single static binary for the Herd Auto
// Prompter Herdr plugin. Subcommands: daemon (monitor loop), tui (Herdr
// pane), mcp (stdio MCP server for the LLM fallback), and CLI verbs that
// mirror the TUI (FR-022).
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/buildinfo"
	"github.com/0xGosu/herdr-auto-pilot/internal/cli"
	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/crashguard"
	"github.com/0xGosu/herdr-auto-pilot/internal/daemon"
	"github.com/0xGosu/herdr-auto-pilot/internal/daemonhealth"
	"github.com/0xGosu/herdr-auto-pilot/internal/daemonlock"
	"github.com/0xGosu/herdr-auto-pilot/internal/embedder"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
	"github.com/0xGosu/herdr-auto-pilot/internal/herdr"
	"github.com/0xGosu/herdr-auto-pilot/internal/llm"
	"github.com/0xGosu/herdr-auto-pilot/internal/logging"
	"github.com/0xGosu/herdr-auto-pilot/internal/mcpserver"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
	"github.com/0xGosu/herdr-auto-pilot/internal/selfpath"
	"github.com/0xGosu/herdr-auto-pilot/internal/store"
	"github.com/0xGosu/herdr-auto-pilot/internal/tui"
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
		if err := cli.Run(context.Background(), nil, os.Stdout, "help", []string{verb}); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}
	if verb == "version" || verb == "--version" || verb == "-V" {
		fmt.Println("hap (herd-auto-prompter)", buildinfo.Version)
		return
	}

	if err := run(verb, args); err != nil {
		// `hap status` on an unhealthy daemon already printed the human detail;
		// exit non-zero for scripts without a redundant "error:" line.
		if !errors.Is(err, cli.ErrUnhealthy) {
			fmt.Fprintln(os.Stderr, "error:", err)
		}
		os.Exit(1)
	}
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
		return runDaemon(ctx, paths, args)
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
		if _, err := logging.Setup(paths.StateDir, false); err != nil {
			return err
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
	st, err := store.Open(paths.DBPath())
	if err != nil {
		return nil, nil, err
	}
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
	}
	// Desktop notifications only exist when herdr launched us as a managed
	// pane — it injects HERDR_ENV=1 and the control socket there. Outside
	// herdr the field stays nil and consumers fall back (the TUI beeps).
	if herdr.InHerdr() {
		app.Notifier = herdr.NewSocketNotifier(herdr.SocketPath())
	}
	return app, func() { st.Close() }, nil
}

func runDaemon(ctx context.Context, paths config.Paths, args []string) error {
	// Flags are parsed as a SET, not by position: `hap daemon --replace-only
	// --ensure` used to fall through to the foreground daemon, which acquires
	// the lock and blocks forever — inside scripts/install.sh that would hang
	// `herdr plugin install`. An unrecognized flag is an error for the same
	// reason: silently running a foreground daemon is the worst possible
	// interpretation of a typo.
	var ensure, replaceOnly bool
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
		default:
			return fmt.Errorf("unknown flag for `hap daemon`: %s (see: hap help daemon)", arg)
		}
	}
	if replaceOnly && !ensure {
		return fmt.Errorf("--replace-only only applies to `hap daemon --ensure`")
	}
	if ensure {
		return ensureDaemon(paths, replaceOnly)
	}

	if _, err := logging.Setup(paths.StateDir, os.Getenv("HAP_DEBUG") == "1"); err != nil {
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

	st, err := store.Open(paths.DBPath())
	if err != nil {
		return err
	}
	defer st.Close()

	cliAdapter := herdr.NewCLI()
	// The LLM adapter is rebuilt from config on every reload so that
	// llm.command/timeout edits apply without a daemon restart.
	llmFactory := func(cfg config.Config) ports.LLMPort {
		return &llm.Adapter{
			CommandTemplate:      cfg.LLM.Command,
			CommandStartTemplate: cfg.LLM.CommandStart,
			Timeout:              cfg.LLMTimeout(),
			DBPath:               paths.DBPath(),
			ControlPath:          paths.ControlSocketPath(),
			Store:                st,
			TaskGenTemplate:      cfg.LLM.GenerateTaskCommand,
			TaskGenStartTemplate: cfg.LLM.GenerateTaskCommandStart,
			TaskGenTimeout:       cfg.GenerateTaskTimeout(),
			// The `.env` files are never read here: the adapter reads them
			// when it spawns a CLI, so editing a file applies to the next
			// run, and changing the configured PATH applies on the next
			// config reload (which rebuilds this adapter).
			BaseEnv:         llm.EnvSpec{Vars: cfg.LLM.Env, File: cfg.LLM.EnvFile},
			CommandEnv:      llm.EnvSpec{Vars: cfg.LLM.CommandEnv, File: cfg.LLM.CommandEnvFile},
			CommandStartEnv: llm.EnvSpec{Vars: cfg.LLM.CommandStartEnv, File: cfg.LLM.CommandStartEnvFile},
			TaskGenEnv:      llm.EnvSpec{Vars: cfg.LLM.GenerateTaskEnv, File: cfg.LLM.GenerateTaskEnvFile},
			TaskGenStartEnv: llm.EnvSpec{Vars: cfg.LLM.GenerateTaskStartEnv, File: cfg.LLM.GenerateTaskStartEnvFile},
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

	d, err := daemon.New(daemon.Options{
		ConfigPath:        paths.File(),
		ControlSocketPath: paths.ControlSocketPath(),
		Store:             st,
		Herdr:             cliAdapter,
		Events:            herdr.NewSubscriber(socketPath),
		// Socket first, CLI as the transport backstop: `notification.show`
		// answers whether the toast was actually painted, which is the
		// difference between "the operator was told about this escalation" and
		// "nothing reached anyone" (IR-003). The CLI exits 0 either way, so it
		// can only ever be the fallback.
		Notify:          herdr.NewFallbackNotifier(socketPath, cliAdapter),
		LLMFactory:      llmFactory,
		EmbedderFactory: embedderFactory,
		MatchIndexDir:   filepath.Join(paths.StateDir, "match-index"),
		StateDir:        paths.StateDir,
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
	})
	if err != nil {
		return err
	}
	return d.Run(ctx)
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
	// Crash-loop hard stop: after we've given up (still looping even with the
	// embedder off), decline to respawn until the [embedding] config changes —
	// this is what actually ends the storm herdr's per-event --ensure would
	// otherwise sustain.
	if g, ok := crashguard.Read(paths.StateDir); ok {
		cfg, _ := config.Load(paths.File())
		blocked, cleared, reason := crashguard.SpawnBlocked(g, embeddingDigest(cfg))
		if blocked {
			slog.Warn("daemon respawn suppressed by crash-loop breaker", "reason", reason)
			return nil
		}
		if g.GaveUp && !cleared.GaveUp {
			// Config changed since we gave up: lift the latch so this start retries.
			_ = crashguard.Write(paths.StateDir, cleared)
		}
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
	st, err := store.Open(dbPath)
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
