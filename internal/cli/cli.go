// Package cli implements the CLI verbs mirroring the TUI (FR-022), with
// output suitable for scripting.
package cli

import (
	"bufio"
	"context"
	"errors"
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

// ErrUnhealthy signals that `hap status` found the daemon in an unhealthy
// state (hung — a held lock with a stale heartbeat). status prints the human
// detail itself; main maps this sentinel to a non-zero exit WITHOUT an
// "error:" prefix, so scripted health checks can detect it.
var ErrUnhealthy = errors.New("daemon unhealthy")

// Run dispatches one CLI verb against the shared front-end layer.
func Run(ctx context.Context, app *frontend.App, out io.Writer, verb string, args []string) error {
	switch verb {
	case "status":
		return status(ctx, app, out)
	case "agents":
		return agents(ctx, app, out)
	case "escalations":
		return escalations(ctx, app, out, args)
	case "audit":
		return audit(ctx, app, out, args)
	case "confirm":
		return confirm(ctx, app, out, args)
	case "resolve", "correct":
		return resolve(ctx, app, out, args)
	case "dismiss":
		return dismiss(ctx, app, out, args)
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
	case "state-dir":
		fmt.Fprintln(out, app.StateDir)
		return nil
	case "paths":
		return paths(out, app)
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
	case "reembed":
		return signaturesReembed(ctx, app, out, args)
	}
	// Bare `signatures --type X` style: treat unknown leading flag as list.
	if strings.HasPrefix(sub, "-") {
		return signaturesList(ctx, app, out, append([]string{sub}, args...))
	}
	return fmt.Errorf("usage: signatures [list|show <sig-or-prefix>|delete <sig-or-prefix> [--yes]|reembed [--force]]")
}

// signaturesReembed re-computes stored signature embeddings for the
// currently configured model: via a daemon nudge when one is running (the
// daemon owns signature_embeddings writes), in-process otherwise.
func signaturesReembed(ctx context.Context, app *frontend.App, out io.Writer, args []string) error {
	fs := flag.NewFlagSet("signatures reembed", flag.ContinueOnError)
	force := fs.Bool("force", false, "re-run even when no drift is detected (retries a previously failed pass)")
	fs.SetOutput(out)
	if err := fs.Parse(args); err != nil {
		return err
	}
	drift, err := app.EmbeddingDrift(ctx)
	if err != nil {
		return err
	}
	if drift.ModelID == "" {
		return fmt.Errorf("embedding is disabled in config — nothing to re-embed")
	}
	if drift.ModelMissing {
		return fmt.Errorf("embedding model %s not found — fix embedding.model_path first", drift.ModelID)
	}
	if !drift.Detected && !*force {
		fmt.Fprintf(out, "all %d stored signature embeddings match model %s — nothing to do (--force re-runs anyway)\n",
			drift.Total, drift.ModelID)
		return nil
	}
	if drift.Detected {
		fmt.Fprintf(out, "%d of %d stored signature embeddings need re-compute for model %s\n",
			drift.Stale, drift.Total, drift.ModelID)
	}

	if app.DaemonInfo != nil {
		if running, _, ver := app.DaemonInfo(); running {
			// A daemon from an older binary ignores the reembed nudge
			// (unknown control kind), so "nudged" would wait forever.
			// Replacing it re-embeds anyway: startup reconciles the rows.
			if ver != buildinfo.Version {
				return fmt.Errorf("daemon is STALE (running %s, binary is %s) — run: hap daemon --ensure (its startup re-embeds automatically)",
					daemonlock.VersionLabel(ver), buildinfo.Version)
			}
			if err := app.RequestReembed(ctx); err != nil {
				return err
			}
			fmt.Fprintln(out, "daemon nudged — re-embedding runs in the background; check: hap status")
			return nil
		}
	}

	res, err := app.ReembedStandalone(ctx, func(done, total int, sig string, rowErr error) {
		if rowErr != nil {
			fmt.Fprintf(out, "  %s: %v\n", sig, rowErr)
		} else if done%25 == 0 || done == total {
			fmt.Fprintf(out, "  %d/%d\n", done, total)
		}
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "re-embedded %d, kept %d, downgraded %d (text-only) — model %s\n",
		res.Reembedded, res.Kept, res.Downgraded, drift.ModelID)
	if res.PersistFailed > 0 {
		fmt.Fprintf(out, "WARNING: %d re-embedded row(s) failed to persist and stay stale — re-run: hap signatures reembed\n",
			res.PersistFailed)
	}
	return nil
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
	if row.PaneExcerpt != "" {
		fmt.Fprintln(out, "original situation:")
		for _, line := range strings.Split(strings.TrimRight(row.PaneExcerpt, "\n"), "\n") {
			fmt.Fprintf(out, "  %s\n", line)
		}
	} else {
		fmt.Fprintln(out, "original situation: (not captured yet — recorded on the rule's next sighting)")
	}
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
	// Daemon health combines the lock, heartbeat, and crash-loop breaker into
	// one assessment shared with the TUI banner (frontend.AssessDaemonHealth),
	// so CLI and TUI can never disagree about whether the daemon is healthy.
	h := app.AssessDaemonHealth()
	if app.DaemonInfo != nil {
		label := fmt.Sprintf("running %s (pid %d)", daemonlock.VersionLabel(h.Version), h.PID)
		switch {
		case !h.Running && h.GaveUp:
			// Respawns are suppressed until the [embedding] config changes.
			fmt.Fprintf(out, "daemon:              NOT STARTING — crash-loop breaker gave up: %s\n", h.Reason)
		case !h.Running && h.CrashLooping:
			// Down with a recent boot cluster: crashing and being respawned.
			fmt.Fprintf(out, "daemon:              DOWN — crash-looping (%d restarts recently); recent output: %s\n",
				h.RecentRestarts, h.StderrLog)
		case !h.Running:
			fmt.Fprintf(out, "daemon:              not running\n")
		case h.Hung:
			// A held lock with a dead heartbeat: alive but not progressing (or
			// mid-crash-loop). Point at the captured stderr for the reason; if
			// it is also a different binary, keep the --ensure remedy visible.
			line := fmt.Sprintf("%s — NOT RESPONDING (last heartbeat %s ago); recent output: %s",
				label, roundDuration(h.HeartbeatAge), h.StderrLog)
			if h.VersionStale {
				line += fmt.Sprintf(" [also STALE: binary is %s; run: hap daemon --ensure]", buildinfo.Version)
			}
			fmt.Fprintf(out, "daemon:              %s\n", line)
		case h.VersionStale:
			// A holder from another binary keeps old bugs alive; make the
			// mismatch and the remedy impossible to miss.
			fmt.Fprintf(out, "daemon:              %s — STALE, binary is %s; run: hap daemon --ensure\n",
				label, buildinfo.Version)
		default:
			fmt.Fprintf(out, "daemon:              %s\n", label)
		}
	}
	fmt.Fprintf(out, "pending escalations: %d\n", st.PendingEscalations)
	fmt.Fprintf(out, "monitored agents:    %d\n", len(st.MonitoredAgents))
	// The crash-loop breaker's auto-disable is the authoritative state and
	// replaces the config-derived line (which can't know matching was forced
	// off); it latches until the [embedding] config changes.
	switch {
	case h.EmbeddingAutoDisabled:
		fmt.Fprintf(out, "semantic matching:   AUTO-DISABLED by crash-loop breaker — %s\n", h.Reason)
	case st.Embedding != "":
		fmt.Fprintf(out, "semantic matching:   %s\n", st.Embedding)
	}
	// A running embedder can still be soft-degraded (embed calls latched to
	// text fallback) — the config-derived line above would otherwise hide it.
	if h.EmbedderDegraded {
		fmt.Fprintf(out, "embedder health:     DEGRADED at runtime — %s\n", h.EmbedderNote)
	}
	if st.Drift.Detected {
		// Same shape as the STALE-daemon line: the mismatch and the remedy
		// in one glance.
		fmt.Fprintf(out, "embedding drift:     %d of %d rules embedded with a previous model; run: hap signatures reembed\n",
			st.Drift.Stale, st.Drift.Total)
	}
	if st.LatestKill != nil {
		fmt.Fprintf(out, "last kill event:     %s by %s at %s\n",
			st.LatestKill.State, st.LatestKill.Author, st.LatestKill.CreatedAt.Format(time.RFC3339))
	}
	// A hung daemon or a latched crash-loop give-up is a failure state: exit
	// non-zero so scripted checks and the operator notice, even though the
	// status body already explained it.
	if h.Hung || h.GaveUp || h.CrashLooping {
		return ErrUnhealthy
	}
	return nil
}

// roundDuration renders a heartbeat age compactly for status output ("45s",
// "2m0s"), dropping sub-second noise and flooring at zero.
func roundDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	return d.Round(time.Second).String()
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

func escalations(ctx context.Context, app *frontend.App, out io.Writer, args []string) error {
	if len(args) > 0 {
		if args[0] != "prune" {
			return fmt.Errorf("usage: escalations [prune [minutes]]")
		}
		return escalationsPrune(ctx, app, out, args[1:])
	}
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
	rules, gradN := ruleIndex(ctx, app)
	for _, e := range esc {
		agent := e.AgentID
		if n := names[e.AgentID]; n != "" {
			agent = n
		}
		rule := "none yet"
		if row, ok := rules[e.Signature]; ok {
			rule = frontend.RuleSummary(row, gradN)
		}
		fmt.Fprintf(out, "#%d\t%s\t%s\t%s\tagent=%s\tllm=%s\tsuggestion=%q\trule=[%s]\n",
			e.ID, e.CreatedAt.Format("15:04:05"), e.SituationType, e.Rationale, agent,
			llmConfCLI(e.LLMConfidence), e.Suggestion, rule)
	}
	fmt.Fprintf(out, "\n%d pending; respond with: confirm <id> | resolve <id> --action TEXT [--send] | dismiss <id>...\n", len(esc))
	return nil
}

// escalationsPrune dismisses pending escalations older than the given age
// in minutes (default 360). Audit rows are kept; nothing is sent or learned.
func escalationsPrune(ctx context.Context, app *frontend.App, out io.Writer, args []string) error {
	minutes := frontend.DefaultPruneMinutes
	if len(args) > 1 {
		return fmt.Errorf("usage: escalations prune [minutes]")
	}
	if len(args) == 1 {
		v, err := strconv.Atoi(args[0])
		if err != nil || v <= 0 {
			return fmt.Errorf("invalid age %q — whole minutes, e.g. escalations prune 120", args[0])
		}
		minutes = v
	}
	n, err := app.PruneEscalations(ctx, time.Duration(minutes)*time.Minute)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "pruned %d escalation(s) older than %d minute(s); audit rows kept as dismissed\n", n, minutes)
	return nil
}

// dismiss removes pending escalations from the queue without responding.
func dismiss(ctx context.Context, app *frontend.App, out io.Writer, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: dismiss <audit-id> [<audit-id>...]")
	}
	for _, arg := range args {
		id, err := strconv.ParseInt(arg, 10, 64)
		if err != nil {
			return fmt.Errorf("invalid audit id %q", arg)
		}
		if err := app.Dismiss(ctx, id); err != nil {
			return err
		}
		fmt.Fprintf(out, "dismissed escalation #%d (audit row kept; nothing sent or learned)\n", id)
	}
	return nil
}

// llmConfCLI renders an audit/escalation row's LLM confidence: the 0-100
// score, or "-" when the row carries no LLM score.
func llmConfCLI(v *int) string {
	if v == nil {
		return "-"
	}
	return strconv.Itoa(*v)
}

// ruleIndex loads the learned signatures keyed by signature, plus the
// graduation N, for annotating escalation/audit rows with their matched
// rule. Degrades to an empty index on error — the listing must not fail
// because rule enrichment did.
func ruleIndex(ctx context.Context, app *frontend.App) (map[string]frontend.SignatureRow, int) {
	gradN := 5
	if cfg, err := app.Config(); err == nil {
		gradN = cfg.Learning.GraduationN
	}
	rows, err := app.Signatures(ctx, domain.SignatureFilter{})
	if err != nil {
		return map[string]frontend.SignatureRow{}, gradN
	}
	return frontend.IndexSignatures(rows), gradN
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
	rules, _ := ruleIndex(ctx, app)
	for _, r := range recs {
		rule := "-"
		if row, ok := rules[r.Signature]; ok {
			rule = string(row.Mode)
		}
		fmt.Fprintf(out, "#%d\t%s\t%s\t%s\t%s\tconf=%.2f\tllm=%s\trule=%s\t%s\n",
			r.ID, r.CreatedAt.Format("01-02 15:04:05"), r.Status, r.SituationType,
			r.Action, r.Confidence, llmConfCLI(r.LLMConfidence), rule, r.Rationale)
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
	action := fs.String("action", "", "the response the agent should have received (@noop = no reply was needed; nothing is ever sent)")
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
	case "path":
		fmt.Fprintln(out, app.ConfigPath)
		return nil
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
	return fmt.Errorf("usage: config [show|fields|path|set <field> <value>|set-threshold <situation> <value>]")
}

// paths prints the resolved config and state paths, labeled, for human use.
// The discrete `state-dir` and `config path` verbs stay bare for scripting.
func paths(out io.Writer, app *frontend.App) error {
	fmt.Fprintf(out, "config:    %s\n", app.ConfigPath)
	fmt.Fprintf(out, "state:     %s\n", app.StateDir)
	return nil
}

func printConfig(out io.Writer, cfg config.Config) {
	fmt.Fprintf(out, "thresholds: idle=%.2f approval=%.2f choice=%.2f error=%.2f inferred_task_bar=%.2f\n",
		cfg.Thresholds.Idle, cfg.Thresholds.Approval, cfg.Thresholds.Choice,
		cfg.Thresholds.Error, cfg.Thresholds.InferredTaskBar)
	fmt.Fprintf(out, "learning:   graduation_n=%d\n", cfg.Learning.GraduationN)
	fmt.Fprintf(out, "limits:     consecutive=%d per_minute=%d error_retries=%d\n",
		cfg.Limits.MaxConsecutiveAutoPrompts, cfg.Limits.MaxAutoPromptsPerMinute, cfg.Limits.MaxErrorRetries)
	fmt.Fprintf(out, "llm:        configured=%v timeout=%ds auto_act_confidence_threshold=%d\n",
		len(cfg.LLM.Command) > 0, cfg.LLM.TimeoutSeconds, cfg.LLM.AutoActConfidenceThreshold)
	fmt.Fprintf(out, "task sources: %d, operator never-auto patterns: %d (+%d seed)\n",
		len(cfg.TaskSources), len(cfg.Safety.NeverAutoPatterns), len(domain.SeedNeverAutoPatterns))
}

func rules(ctx context.Context, app *frontend.App, out io.Writer, args []string) error {
	if len(args) == 0 || args[0] == "list" {
		cfg, err := app.Config()
		if err != nil {
			return err
		}
		fmt.Fprintln(out, "# seed never-auto patterns (always active unless disable_seed=true)")
		for _, p := range domain.SeedNeverAutoPatterns {
			fmt.Fprintf(out, "seed\t\t%s\n", p)
		}
		for i, p := range cfg.Safety.NeverAutoPatterns {
			fmt.Fprintf(out, "operator #%d\t%s\n", i, p)
		}
		return nil
	}
	switch {
	case args[0] == "add" && len(args) == 2:
		if err := app.AddNeverAutoPattern(ctx, args[1]); err != nil {
			return err
		}
		fmt.Fprintf(out, "never-auto pattern added: %s\n", args[1])
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
		if idx < 0 || idx >= len(cfg.Safety.NeverAutoPatterns) {
			return fmt.Errorf("no operator never-auto pattern #%d (see: rules list)", idx)
		}
		expected := cfg.Safety.NeverAutoPatterns[idx]
		if err := app.RemoveNeverAutoPattern(ctx, idx, expected); err != nil {
			return err
		}
		fmt.Fprintf(out, "operator never-auto pattern #%d removed: %s\n", idx, expected)
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
	template := fs.String("template", "", "next-task prompt template ({next_task_content}, {task_list_path}, {agent_name} placeholders)")
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
