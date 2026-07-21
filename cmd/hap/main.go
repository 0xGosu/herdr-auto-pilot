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

func run(verb string, args []string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
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
	return app, func() { st.Close() }, nil
}

func runDaemon(ctx context.Context, paths config.Paths, args []string) error {
	ensure := len(args) > 0 && args[0] == "--ensure"
	if ensure {
		return ensureDaemon(paths)
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

	socketPath := os.Getenv("HERDR_SOCKET_PATH")
	if socketPath == "" {
		home, _ := os.UserHomeDir()
		socketPath = home + "/.config/herdr/herdr.sock"
	}

	d, err := daemon.New(daemon.Options{
		ConfigPath:        paths.File(),
		ControlSocketPath: paths.ControlSocketPath(),
		Store:             st,
		Herdr:             cliAdapter,
		Events:            herdr.NewSubscriber(socketPath),
		Notify:            cliAdapter,
		LLMFactory:        llmFactory,
		EmbedderFactory:   embedderFactory,
		MatchIndexDir:     filepath.Join(paths.StateDir, "match-index"),
		StateDir:          paths.StateDir,
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

func ensureDaemon(paths config.Paths) error {
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
	return daemonlock.EnsureFresh(paths, buildinfo.Version, 3*time.Second, daemonlock.Stop, func() error {
		self, err := os.Executable()
		if err != nil {
			return err
		}
		cmd := exec.Command(self, "daemon")
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
	})
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
