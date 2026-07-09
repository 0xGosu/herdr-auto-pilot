// Package cli implements the CLI verbs mirroring the TUI (FR-022), with
// output suitable for scripting.
package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
)

// Run dispatches one CLI verb against the shared front-end layer.
func Run(ctx context.Context, app *frontend.App, out io.Writer, verb string, args []string) error {
	switch verb {
	case "status":
		return status(ctx, app, out)
	case "agents":
		return agents(ctx, app, out)
	case "escalations":
		return escalations(ctx, app, out)
	case "audit":
		return audit(ctx, app, out, args)
	case "confirm":
		return confirm(ctx, app, out, args)
	case "resolve", "correct":
		return resolve(ctx, app, out, args)
	case "pause":
		if err := app.Pause(ctx); err != nil {
			return err
		}
		fmt.Fprintln(out, "automation paused (kill switch active)")
		return nil
	case "resume":
		if err := app.Resume(ctx); err != nil {
			return err
		}
		fmt.Fprintln(out, "automation resumed")
		return nil
	case "kill-history":
		return killHistory(ctx, app, out)
	case "config":
		return configCmd(ctx, app, out, args)
	case "rules":
		return rules(ctx, app, out, args)
	case "task-source":
		return taskSource(ctx, app, out, args)
	case "clear-data":
		return clearData(ctx, app, out, args)
	}
	return fmt.Errorf("unknown command %q", verb)
}

func status(ctx context.Context, app *frontend.App, out io.Writer) error {
	st, err := app.GetStatus(ctx)
	if err != nil {
		return err
	}
	state := "running"
	if st.Paused {
		state = "PAUSED (kill switch active)"
	}
	fmt.Fprintf(out, "automation:          %s\n", state)
	fmt.Fprintf(out, "pending escalations: %d\n", st.PendingEscalations)
	fmt.Fprintf(out, "monitored agents:    %d\n", len(st.MonitoredAgents))
	if st.LatestKill != nil {
		fmt.Fprintf(out, "last kill event:     %s by %s at %s\n",
			st.LatestKill.State, st.LatestKill.Author, st.LatestKill.CreatedAt.Format(time.RFC3339))
	}
	return nil
}

func agents(ctx context.Context, app *frontend.App, out io.Writer) error {
	st, err := app.GetStatus(ctx)
	if err != nil {
		return err
	}
	if len(st.MonitoredAgents) == 0 {
		fmt.Fprintln(out, "no agents detected (is herdr running?)")
		return nil
	}
	for _, a := range st.MonitoredAgents {
		fmt.Fprintf(out, "%s\t%s\t%s\n", a.AgentID, a.AgentType, a.Status)
	}
	return nil
}

func escalations(ctx context.Context, app *frontend.App, out io.Writer) error {
	esc, err := app.Escalations(ctx)
	if err != nil {
		return err
	}
	if len(esc) == 0 {
		fmt.Fprintln(out, "no pending escalations")
		return nil
	}
	for _, e := range esc {
		fmt.Fprintf(out, "#%d\t%s\t%s\t%s\tagent=%s\tsuggestion=%q\n",
			e.ID, e.CreatedAt.Format("15:04:05"), e.SituationType, e.Rationale, e.AgentID, e.Suggestion)
	}
	fmt.Fprintf(out, "\n%d pending; respond with: confirm <id> | resolve <id> --action TEXT [--send]\n", len(esc))
	return nil
}

func audit(ctx context.Context, app *frontend.App, out io.Writer, args []string) error {
	fs := flag.NewFlagSet("audit", flag.ContinueOnError)
	limit := fs.Int("limit", 30, "number of records")
	fs.SetOutput(out)
	if err := fs.Parse(args); err != nil {
		return err
	}
	recs, err := app.Audit(ctx, *limit)
	if err != nil {
		return err
	}
	for _, r := range recs {
		fmt.Fprintf(out, "#%d\t%s\t%s\t%s\t%s\tconf=%.2f\t%s\n",
			r.ID, r.CreatedAt.Format("01-02 15:04:05"), r.Status, r.SituationType,
			r.Action, r.Confidence, r.Rationale)
	}
	return nil
}

func confirm(ctx context.Context, app *frontend.App, out io.Writer, args []string) error {
	idArg, rest := splitLeadingID(args)
	fs := flag.NewFlagSet("confirm", flag.ContinueOnError)
	send := fs.Bool("send", false, "also deliver the confirmed action to the agent pane")
	fs.SetOutput(out)
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if idArg == "" && fs.NArg() > 0 {
		idArg = fs.Arg(0)
	}
	if idArg == "" {
		return fmt.Errorf("usage: confirm <audit-id> [--send]")
	}
	id, err := strconv.ParseInt(idArg, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid audit id %q", idArg)
	}
	if err := app.Confirm(ctx, id, *send); err != nil {
		return err
	}
	fmt.Fprintf(out, "confirmed escalation #%d (recorded as a learning event)\n", id)
	return nil
}

func resolve(ctx context.Context, app *frontend.App, out io.Writer, args []string) error {
	idArg, rest := splitLeadingID(args)
	fs := flag.NewFlagSet("resolve", flag.ContinueOnError)
	action := fs.String("action", "", "the response the agent should have received")
	send := fs.Bool("send", false, "also deliver the action to the agent pane")
	fs.SetOutput(out)
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if idArg == "" && fs.NArg() > 0 {
		idArg = fs.Arg(0)
	}
	if idArg == "" || *action == "" {
		return fmt.Errorf("usage: resolve <audit-id> --action TEXT [--send]")
	}
	id, err := strconv.ParseInt(idArg, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid audit id %q", idArg)
	}
	if err := app.Resolve(ctx, id, *action, *send); err != nil {
		return err
	}
	fmt.Fprintf(out, "recorded correction for audit #%d: %q\n", id, *action)
	return nil
}

// splitLeadingID lets verbs accept `<id>` before flags (Go's flag package
// stops parsing at the first positional argument).
func splitLeadingID(args []string) (id string, rest []string) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return args[0], args[1:]
	}
	return "", args
}

func killHistory(ctx context.Context, app *frontend.App, out io.Writer) error {
	events, err := app.KillHistory(ctx, 100)
	if err != nil {
		return err
	}
	if len(events) == 0 {
		fmt.Fprintln(out, "no pause/kill events recorded")
		return nil
	}
	for _, e := range events {
		fmt.Fprintf(out, "#%d\t%s\t%s\tby %s\t%s\n",
			e.ID, e.CreatedAt.Format(time.RFC3339), e.State, e.Author, e.Scope)
	}
	return nil
}

func configCmd(ctx context.Context, app *frontend.App, out io.Writer, args []string) error {
	if len(args) == 0 || args[0] == "show" {
		cfg, err := app.Config()
		if err != nil {
			return err
		}
		printConfig(out, cfg)
		return nil
	}
	if args[0] == "set-threshold" {
		if len(args) != 3 {
			return fmt.Errorf("usage: config set-threshold <idle|approval|choice|error|inferred_task_bar> <value>")
		}
		v, err := strconv.ParseFloat(args[2], 64)
		if err != nil {
			return fmt.Errorf("invalid threshold %q", args[2])
		}
		if err := app.SetThreshold(ctx, args[1], v); err != nil {
			return err
		}
		fmt.Fprintf(out, "threshold %s set to %.2f (daemon reloaded)\n", args[1], v)
		return nil
	}
	return fmt.Errorf("usage: config [show|set-threshold <situation> <value>]")
}

func printConfig(out io.Writer, cfg config.Config) {
	fmt.Fprintf(out, "thresholds: idle=%.2f approval=%.2f choice=%.2f error=%.2f inferred_task_bar=%.2f\n",
		cfg.Thresholds.Idle, cfg.Thresholds.Approval, cfg.Thresholds.Choice,
		cfg.Thresholds.Error, cfg.Thresholds.InferredTaskBar)
	fmt.Fprintf(out, "learning:   graduation_n=%d\n", cfg.Learning.GraduationN)
	fmt.Fprintf(out, "limits:     consecutive=%d per_minute=%d error_retries=%d\n",
		cfg.Limits.MaxConsecutiveAutoPrompts, cfg.Limits.MaxAutoPromptsPerMinute, cfg.Limits.MaxErrorRetries)
	fmt.Fprintf(out, "llm:        configured=%v timeout=%ds auto_act=%v\n",
		len(cfg.LLM.Command) > 0, cfg.LLM.TimeoutSeconds, cfg.LLM.AutoAct)
	fmt.Fprintf(out, "task sources: %d, operator allowlist patterns: %d (+%d seed)\n",
		len(cfg.TaskSources), len(cfg.Safety.AllowlistPatterns), len(domain.SeedAllowlistPatterns))
}

func rules(ctx context.Context, app *frontend.App, out io.Writer, args []string) error {
	if len(args) == 0 || args[0] == "list" {
		cfg, err := app.Config()
		if err != nil {
			return err
		}
		fmt.Fprintln(out, "# seed never-auto allowlist (always active unless disable_seed=true)")
		for _, p := range domain.SeedAllowlistPatterns {
			fmt.Fprintf(out, "seed\t%s\n", p)
		}
		for _, p := range cfg.Safety.AllowlistPatterns {
			fmt.Fprintf(out, "operator\t%s\n", p)
		}
		return nil
	}
	if args[0] == "add" && len(args) == 2 {
		if err := app.AddAllowlistPattern(ctx, args[1]); err != nil {
			return err
		}
		fmt.Fprintf(out, "allowlist pattern added: %s\n", args[1])
		return nil
	}
	return fmt.Errorf("usage: rules [list|add <regex>]")
}

func taskSource(ctx context.Context, app *frontend.App, out io.Writer, args []string) error {
	fs := flag.NewFlagSet("task-source", flag.ContinueOnError)
	agent := fs.String("agent", "", "agent id/name/type this source applies to")
	workspace := fs.String("workspace", "", "workspace id this source applies to")
	fs.SetOutput(out)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: task-source [--agent A] [--workspace W] <checklist.md>")
	}
	if err := app.AddTaskSource(ctx, *agent, *workspace, fs.Arg(0)); err != nil {
		return err
	}
	fmt.Fprintf(out, "task source added: %s\n", fs.Arg(0))
	return nil
}

func clearData(ctx context.Context, app *frontend.App, out io.Writer, args []string) error {
	if len(args) == 0 || args[0] != "--yes" {
		return fmt.Errorf("this permanently clears learned history and audit data; rerun as: clear-data --yes")
	}
	if err := app.ClearData(ctx); err != nil {
		return err
	}
	fmt.Fprintln(out, "learned history and audit data cleared")
	return nil
}
