// Command herd-auto-prompter is the single static binary for the Herd Auto
// Prompter Herdr plugin. Subcommands: daemon (monitor loop), tui (Herdr
// pane), mcp (stdio MCP server for the LLM fallback), and CLI verbs that
// mirror the TUI (FR-022).
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"github.com/0xGosu/herdr-auto-pilot/internal/cli"
	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/daemon"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
	"github.com/0xGosu/herdr-auto-pilot/internal/herdr"
	"github.com/0xGosu/herdr-auto-pilot/internal/llm"
	"github.com/0xGosu/herdr-auto-pilot/internal/logging"
	"github.com/0xGosu/herdr-auto-pilot/internal/mcpserver"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
	"github.com/0xGosu/herdr-auto-pilot/internal/store"
	"github.com/0xGosu/herdr-auto-pilot/internal/tui"
)

// version is stamped by the release build (-ldflags "-X main.version=...").
var version = "dev"

const usage = `herd-auto-prompter — keep your Herdr agents unblocked, hands-free

Core:
  daemon [--ensure]     run the monitoring daemon (--ensure: start only if not running)
  tui                   run the TUI control pane
  mcp                   run the stdio MCP server (used by the LLM fallback)

Operate:
  status                automation state, pending escalations, agents
  agents                list monitored agents
  escalations           list pending escalations
  confirm <id> [--send]         confirm an escalation's suggested action
  resolve <id> --action TEXT [--send]   record the correct action (post-hoc correction)
  audit [--limit N]     show the audit log
  pause | resume        global pause/kill switch
  kill-history          pause/kill event history

Configure:
  config [show|set-threshold <situation> <value>]
  rules [list|add <regex>]          never-auto allowlist
  task-source [--agent A] [--workspace W] <checklist.md>
  clear-data --yes      reset learned history + audit data

  version               print version
`

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(2)
	}
	verb := os.Args[1]
	args := os.Args[2:]

	if verb == "version" || verb == "--version" || verb == "-V" {
		fmt.Println("herd-auto-prompter", version)
		return
	}
	if verb == "help" || verb == "--help" || verb == "-h" {
		fmt.Print(usage)
		return
	}

	if err := run(verb, args); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(verb string, args []string) error {
	paths, err := config.ResolvePaths()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch verb {
	case "daemon":
		return runDaemon(ctx, paths, args)
	case "mcp":
		return runMCP(ctx, paths)
	case "tui":
		app, closeStore, err := buildApp(paths)
		if err != nil {
			return err
		}
		defer closeStore()
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
		return cli.Run(ctx, app, os.Stdout, verb, args)
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

	lock, err := acquireDaemonLock(paths)
	if err != nil {
		return err
	}
	defer lock.release()

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
			CommandTemplate: cfg.LLM.Command,
			Timeout:         cfg.LLMTimeout(),
			DBPath:          paths.DBPath(),
			ControlPath:     paths.ControlSocketPath(),
			Store:           st,
		}
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
	})
	if err != nil {
		return err
	}
	return d.Run(ctx)
}

// ensureDaemon starts a detached daemon if none is running (used by the
// Herdr event hook so hooks return promptly).
func ensureDaemon(paths config.Paths) error {
	if daemonRunning(paths) {
		return nil
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(self, "daemon")
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.Stdin = nil
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

func runMCP(ctx context.Context, paths config.Paths) error {
	st, err := store.Open(paths.DBPath())
	if err != nil {
		return err
	}
	defer st.Close()
	srv := &mcpserver.Server{
		Store:            st,
		ControlPath:      paths.ControlSocketPath(),
		DefaultRequestID: os.Getenv("HAP_REQUEST_ID"),
	}
	return srv.Run(ctx, os.Stdin, os.Stdout)
}
