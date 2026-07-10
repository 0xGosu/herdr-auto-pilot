// Package cli implements the CLI verbs mirroring the TUI (FR-022), with
// output suitable for scripting.
package cli

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/0xGosu/herdr-auto-pilot/internal/buildinfo"
	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/daemonlock"
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
	case "rename":
		return rename(ctx, app, out, args)
	case "signatures", "sigs":
		return signatures(ctx, app, out, args)
	case "clear-data":
		return clearData(ctx, app, out, args)
	}
	return fmt.Errorf("unknown command %q", verb)
}

func signatures(ctx context.Context, app *frontend.App, out io.Writer, args []string) error {
	sub := "list"
	if len(args) > 0 {
		sub = args[0]
		args = args[1:]
	}
	switch sub {
	case "list":
		return signaturesList(ctx, app, out, args)
	case "show":
		return signaturesShow(ctx, app, out, args)
	case "delete":
		return signaturesDelete(ctx, app, out, args)
	}
	// Bare `signatures --type X` style: treat unknown leading flag as list.
	if strings.HasPrefix(sub, "-") {
		return signaturesList(ctx, app, out, append([]string{sub}, args...))
	}
	return fmt.Errorf("usage: signatures [list|show <sig-or-prefix>|delete <sig-or-prefix> [--yes]]")
}

func signaturesList(ctx context.Context, app *frontend.App, out io.Writer, args []string) error {
	fs := flag.NewFlagSet("signatures list", flag.ContinueOnError)
	situation := fs.String("type", "", "filter by situation type (idle|approval|choice|error)")
	mode := fs.String("mode", "", "filter by mode (shadow|autonomous)")
	agentType := fs.String("agent-type", "", "filter by agent type")
	minConf := fs.Float64("min-conf", 0, "filter by minimum cached confidence")
	fs.SetOutput(out)
	if err := fs.Parse(args); err != nil {
		return err
	}
	switch *situation {
	case "", "idle", "approval", "choice", "error":
	default:
		return fmt.Errorf("invalid --type %q (idle|approval|choice|error)", *situation)
	}
	switch *mode {
	case "", string(domain.ModeShadow), string(domain.ModeAutonomous):
	default:
		return fmt.Errorf("invalid --mode %q (shadow|autonomous)", *mode)
	}
	filtered := *situation != "" || *mode != "" || *agentType != "" || *minConf > 0
	rows, err := app.Signatures(ctx, domain.SignatureFilter{
		SituationType: domain.SituationType(*situation),
		AgentType:     *agentType,
		Mode:          domain.Mode(*mode),
		MinConfidence: *minConf,
	})
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		if filtered {
			fmt.Fprintln(out, "no signatures match the filter")
		} else {
			fmt.Fprintln(out, "no learned signatures yet — confirm suggestions to teach hap")
		}
		return nil
	}
	graduationN := graduationN(app)
	for _, r := range rows {
		fmt.Fprintf(out, "%s\t%s\t%s\t%s\t%d/%d\tconf=%.2f\ttop=%q\t%s\n",
			shortSignature(r.Signature), r.SituationType, orDash(r.AgentType), r.Mode,
			r.ConsecutiveConfirmations, graduationN, r.CachedConfidence,
			r.TopAction, r.UpdatedAt.Format("01-02 15:04:05"))
	}
	fmt.Fprintf(out, "\n%d signature(s); inspect with: signatures show <prefix>\n", len(rows))
	return nil
}

func signaturesShow(ctx context.Context, app *frontend.App, out io.Writer, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: signatures show <sig-or-prefix>")
	}
	row, history, err := app.SignatureDetail(ctx, args[0])
	if err != nil {
		return err
	}
	printSignatureRow(out, row, graduationN(app))
	if len(history) > 0 {
		fmt.Fprintln(out, "recent decisions (newest first):")
		for _, d := range history {
			marker := ""
			if d.IsCorrection {
				marker = "\tCORRECTION"
			}
			fmt.Fprintf(out, "  #%d\t%s\t%q\tsource=%s%s\n",
				d.ID, d.CreatedAt.Format("01-02 15:04:05"), d.ChosenAction, d.Source, marker)
		}
	}
	if a := row.LastAudit; a != nil {
		fmt.Fprintf(out, "last audit #%d (%s): %s — %s\n",
			a.ID, a.Status, a.Action, a.Rationale)
	}
	return nil
}

func signaturesDelete(ctx context.Context, app *frontend.App, out io.Writer, args []string) error {
	prefix, rest := splitLeadingID(args)
	fs := flag.NewFlagSet("signatures delete", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "skip the interactive confirmation")
	fs.SetOutput(out)
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if prefix == "" && fs.NArg() > 0 {
		prefix = fs.Arg(0)
	}
	if prefix == "" {
		return fmt.Errorf("usage: signatures delete <sig-or-prefix> [--yes]")
	}
	// target holds the exact key once shown to the operator: the confirmed
	// row and the deleted row must be the same even if the daemon learns
	// another signature sharing the prefix while the operator types.
	target := prefix
	if !*yes {
		row, _, err := app.SignatureDetail(ctx, prefix)
		if err != nil {
			return err
		}
		target = row.Signature
		printSignatureRow(out, row, graduationN(app))
		if !stdinIsTTY() {
			return fmt.Errorf("deleting a signature erases its learned history; rerun as: signatures delete %s --yes", row.Signature)
		}
		fmt.Fprintf(out, "type 'yes' to delete %s and its %d decision(s): ", shortSignature(row.Signature), row.Decisions)
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && line == "" {
			return fmt.Errorf("read confirmation: %w", err)
		}
		if strings.TrimSpace(line) != "yes" {
			fmt.Fprintln(out, "aborted")
			return nil
		}
	}
	sig, decisions, err := app.DeleteSignature(ctx, target)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "deleted signature %s and %d decision(s); audit rows kept\n", sig, decisions)
	return nil
}

func printSignatureRow(out io.Writer, r frontend.SignatureRow, graduationN int) {
	fmt.Fprintf(out, "signature:   %s\n", r.Signature)
	fmt.Fprintf(out, "situation:   %s\tagent type: %s\n", r.SituationType, orDash(r.AgentType))
	fmt.Fprintf(out, "mode:        %s\tstreak: %d/%d\tconfidence: %.2f\n",
		r.Mode, r.ConsecutiveConfirmations, graduationN, r.CachedConfidence)
	fmt.Fprintf(out, "top action:  %q over %d decision(s)\n", r.TopAction, r.Decisions)
	fmt.Fprintf(out, "updated:     %s\n", r.UpdatedAt.Format(time.RFC3339))
}

// graduationN reads the live graduation threshold for streak display.
func graduationN(app *frontend.App) int {
	if cfg, err := app.Config(); err == nil && cfg.Learning.GraduationN > 0 {
		return cfg.Learning.GraduationN
	}
	return config.Default().Learning.GraduationN
}

// shortSignature abbreviates a signature for one-line listings.
func shortSignature(sig string) string {
	if len(sig) <= 16 {
		return sig
	}
	return sig[:16] + "…"
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// stdinIsTTY reports whether stdin is an interactive terminal;
// scripted/non-TTY deletes must pass --yes explicitly.
func stdinIsTTY() bool {
	return term.IsTerminal(int(os.Stdin.Fd()))
}

func rename(ctx context.Context, app *frontend.App, out io.Writer, args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: rename <agent-or-name> <new-name>")
	}
	if err := app.RenameAgent(ctx, args[0], args[1]); err != nil {
		return err
	}
	fmt.Fprintf(out, "agent %q is now named %q (task-source selectors match this name)\n", args[0], args[1])
	return nil
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
	if app.DaemonInfo != nil {
		running, pid, ver := app.DaemonInfo()
		switch {
		case !running:
			fmt.Fprintf(out, "daemon:              not running\n")
		case ver == buildinfo.Version:
			fmt.Fprintf(out, "daemon:              running %s (pid %d)\n", daemonlock.VersionLabel(ver), pid)
		default:
			// A holder from another binary keeps old bugs alive; make the
			// mismatch and the remedy impossible to miss.
			fmt.Fprintf(out, "daemon:              running %s (pid %d) — STALE, binary is %s; run: hap daemon --ensure\n",
				daemonlock.VersionLabel(ver), pid, buildinfo.Version)
		}
	}
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
		name := st.AgentName(a.AgentID)
		if name == "" {
			name = "-"
		}
		fmt.Fprintf(out, "%s\t%s\t%s\t%s\n", name, a.AgentID, a.AgentType, a.Status)
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
	names, err := app.Names(ctx)
	if err != nil {
		names = map[string]string{}
	}
	for _, e := range esc {
		agent := e.AgentID
		if n := names[e.AgentID]; n != "" {
			agent = n
		}
		fmt.Fprintf(out, "#%d\t%s\t%s\t%s\tagent=%s\tsuggestion=%q\n",
			e.ID, e.CreatedAt.Format("15:04:05"), e.SituationType, e.Rationale, agent, e.Suggestion)
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
	switch args[0] {
	case "fields":
		cfg, err := app.Config()
		if err != nil {
			return err
		}
		for _, key := range frontend.ConfigFieldKeys {
			fmt.Fprintf(out, "%-40s %s\n", key, frontend.FieldValue(cfg, key))
		}
		return nil
	case "set":
		if len(args) < 3 {
			return fmt.Errorf("usage: config set <field> <value> (see: config fields)")
		}
		value := strings.Join(args[2:], " ")
		if err := app.SetField(ctx, args[1], value); err != nil {
			return err
		}
		fmt.Fprintf(out, "%s set to %s (daemon reloaded)\n", args[1], value)
		return nil
	case "set-threshold":
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
	return fmt.Errorf("usage: config [show|fields|set <field> <value>|set-threshold <situation> <value>]")
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
			fmt.Fprintf(out, "seed\t\t%s\n", p)
		}
		for i, p := range cfg.Safety.AllowlistPatterns {
			fmt.Fprintf(out, "operator #%d\t%s\n", i, p)
		}
		return nil
	}
	switch {
	case args[0] == "add" && len(args) == 2:
		if err := app.AddAllowlistPattern(ctx, args[1]); err != nil {
			return err
		}
		fmt.Fprintf(out, "allowlist pattern added: %s\n", args[1])
		return nil
	case args[0] == "remove" && len(args) == 2:
		idx, err := strconv.Atoi(args[1])
		if err != nil {
			return fmt.Errorf("invalid pattern index %q (see: rules list)", args[1])
		}
		cfg, err := app.Config()
		if err != nil {
			return err
		}
		if idx < 0 || idx >= len(cfg.Safety.AllowlistPatterns) {
			return fmt.Errorf("no operator allowlist pattern #%d (see: rules list)", idx)
		}
		expected := cfg.Safety.AllowlistPatterns[idx]
		if err := app.RemoveAllowlistPattern(ctx, idx, expected); err != nil {
			return err
		}
		fmt.Fprintf(out, "operator allowlist pattern #%d removed: %s\n", idx, expected)
		return nil
	}
	return fmt.Errorf("usage: rules [list|add <regex>|remove <index>]")
}

func taskSource(ctx context.Context, app *frontend.App, out io.Writer, args []string) error {
	if len(args) > 0 && args[0] == "list" {
		cfg, err := app.Config()
		if err != nil {
			return err
		}
		if len(cfg.TaskSources) == 0 {
			fmt.Fprintln(out, "no task sources configured")
			return nil
		}
		for i, src := range cfg.TaskSources {
			fmt.Fprintf(out, "#%d\tagent=%q workspace=%q path=%s", i, src.Agent, src.Workspace, src.Path)
			if src.NextTaskTemplate != "" {
				fmt.Fprintf(out, " template=%q", src.NextTaskTemplate)
			}
			fmt.Fprintln(out)
		}
		return nil
	}
	if len(args) > 0 && args[0] == "remove" {
		if len(args) != 2 {
			return fmt.Errorf("usage: task-source remove <index> (see: task-source list)")
		}
		idx, err := strconv.Atoi(args[1])
		if err != nil {
			return fmt.Errorf("invalid task source index %q", args[1])
		}
		cfg, err := app.Config()
		if err != nil {
			return err
		}
		if idx < 0 || idx >= len(cfg.TaskSources) {
			return fmt.Errorf("no task source #%d (see: task-source list)", idx)
		}
		expected := cfg.TaskSources[idx].Path
		if err := app.RemoveTaskSource(ctx, idx, expected); err != nil {
			return err
		}
		fmt.Fprintf(out, "task source #%d removed: %s\n", idx, expected)
		return nil
	}
	if len(args) > 0 && args[0] == "add" {
		args = args[1:]
	}
	fs := flag.NewFlagSet("task-source", flag.ContinueOnError)
	agent := fs.String("agent", "", "agent short name, id, or type this source applies to")
	workspace := fs.String("workspace", "", "workspace name this source applies to (\"*\" wildcards, e.g. \"codex-*\")")
	template := fs.String("template", "", "next-task prompt template ({next_task_content}, {task_list_path} placeholders)")
	fs.SetOutput(out)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: task-source [add] [--agent A] [--workspace W] [--template T] <checklist.md> | list | remove <index>")
	}
	if err := app.AddTaskSource(ctx, *agent, *workspace, fs.Arg(0), *template); err != nil {
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
