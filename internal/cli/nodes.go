package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
)

// nodes lists the other machines sharing the store, or prunes the ones that
// have been offline too long (`hap nodes prune`).
func nodes(ctx context.Context, app *frontend.App, out io.Writer, args []string) error {
	if len(args) > 0 {
		if args[0] == "prune" {
			return nodesPrune(ctx, app, out, args[1:])
		}
		return fmt.Errorf("usage: nodes [prune [--older-than 7d] [--node <label|id>] [--yes]] (see: hap help nodes)")
	}
	all, err := app.OtherNodes(ctx)
	if err != nil {
		return err
	}
	if len(all) == 0 {
		fmt.Fprintln(out, "no other nodes share this store")
		PrintNextSteps(out, []Hint{{Cmd: "hap status", Why: "this node's own state"}})
		return nil
	}
	now := time.Now()
	for _, n := range all {
		fmt.Fprintf(out, "%s\t%s\t%s\t%s\t%d agent(s)\t%s\n",
			domain.NodeLabelOrID(n.Node), n.Node.ID, orDash(n.Node.HapVersion),
			nodeLiveness(n, now), n.Agents, lastHeardPhrase(n.LastHeard, now))
	}
	PrintNextSteps(out, []Hint{
		{Cmd: "hap nodes prune", Why: "list the nodes offline longer than a week, and what pruning them removes"},
		{Cmd: "hap nodes prune --older-than 30d --yes", Why: "remove the agents of nodes unheard for 30 days"},
	})
	return nil
}

// nodeLiveness is the one-word state column: live, stale (missed a few
// beats), or offline (past the TUI's hide threshold).
func nodeLiveness(n domain.OfflineNode, now time.Time) string {
	switch {
	case domain.NodeOfflineLongerThan(n.LastHeard, now, domain.NodeOfflineHideAfter):
		return "offline"
	case domain.NodeStale(domain.NodeInfo{LastSeen: n.LastHeard}, now):
		return "stale"
	}
	return "live"
}

// lastHeardPhrase renders when a node last reported, relative to now.
func lastHeardPhrase(at, now time.Time) string {
	if at.IsZero() {
		return "never heard from"
	}
	return "last heard " + agoPhrase(now.Sub(at)) + " (" + at.Local().Format("2006-01-02 15:04") + ")"
}

// agoPhrase is a coarse "how long ago": days past a day, else ShortDuration.
func agoPhrase(d time.Duration) string {
	if d >= 48*time.Hour {
		return strconv.Itoa(int(d/(24*time.Hour))) + "d ago"
	}
	return domain.ShortDuration(d) + " ago"
}

// nodesPrune removes the agents of nodes offline longer than --older-than.
// Without --yes it only lists what would go: the rows are deleted from the
// SHARED database, for every machine, so the destructive half is opt-in.
func nodesPrune(ctx context.Context, app *frontend.App, out io.Writer, args []string) error {
	fs := flag.NewFlagSet("nodes prune", flag.ContinueOnError)
	olderThan := fs.String("older-than", ageWords(domain.DefaultNodePruneAfter), "prune nodes unheard for longer than this (e.g. 7d, 36h)")
	nodeRef := fs.String("node", "", "prune only this node (label, id or id prefix)")
	yes := fs.Bool("yes", false, "actually delete; without it the command only lists what would go")
	dryRun := fs.Bool("dry-run", false, "list what would go and change nothing (the default without --yes)")
	fs.SetOutput(out)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q (see: hap help nodes)", fs.Arg(0))
	}
	age, err := parseAge(*olderThan)
	if err != nil {
		return fmt.Errorf("--older-than: %w", err)
	}
	candidates, err := app.OfflineNodes(ctx, age)
	if err != nil {
		return err
	}
	if *nodeRef != "" {
		one, err := frontend.ResolveOfflineNode(candidates, *nodeRef)
		if err != nil {
			if id, rerr := app.ResolveNode(ctx, *nodeRef); rerr == nil {
				if id == app.Store.NodeID() {
					return domain.ErrNodePruneSelf
				}
				return fmt.Errorf("node %s has reported within the last %s; it is not offline long enough to prune",
					*nodeRef, ageWords(age))
			}
			return err
		}
		candidates = []domain.OfflineNode{one}
	}
	now := time.Now()
	if len(candidates) == 0 {
		fmt.Fprintf(out, "no node has been offline longer than %s\n", ageWords(age))
		PrintNextSteps(out, []Hint{{Cmd: "hap nodes", Why: "every node sharing this store, with when it last reported"}})
		return nil
	}
	if !*yes || *dryRun {
		fmt.Fprintf(out, "%d node(s) offline longer than %s:\n", len(candidates), ageWords(age))
		for _, n := range candidates {
			fmt.Fprintf(out, "  %s\t%s\t%d agent(s)\t%s\n", domain.NodeLabelOrID(n.Node), n.Node.ID, n.Agents,
				lastHeardPhrase(n.LastHeard, now))
		}
		fmt.Fprintln(out, "\nPruning removes each one's agent roster, agent names, herdr locations and node")
		fmt.Fprintln(out, "row from the shared database, and dismisses its pending escalations. Its audit")
		fmt.Fprintln(out, "history, learned rules and task lists are kept; names of agents you DISABLED are")
		fmt.Fprintln(out, "kept too. A node that comes back simply re-registers, with fresh agent names.")
		fmt.Fprintln(out, "Nothing was changed.")
		PrintNextSteps(out, []Hint{{
			Cmd: "hap nodes prune --older-than " + *olderThan + nodeFlagEcho(*nodeRef) + " --yes",
			Why: "prune them",
		}})
		return nil
	}
	var failed []string
	for _, n := range candidates {
		label := domain.NodeLabelOrID(n.Node)
		c, err := app.PruneOfflineNode(ctx, n.Node.ID, age)
		if err != nil {
			if errors.Is(err, domain.ErrNodeHeardSince) {
				fmt.Fprintf(out, "skipped %s: %v\n", label, err)
				continue
			}
			failed = append(failed, label)
			fmt.Fprintf(out, "failed %s: %v\n", label, err)
			continue
		}
		fmt.Fprintf(out, "pruned %s (%s): %d roster rows, %d names, %d locations, %d other rows; %d escalation(s) dismissed",
			label, n.Node.ID, c.Roster, c.Names, c.Locations,
			c.Tombstones+c.RateRows+c.ErrorRetries+c.RosterMeta+c.Node, c.Escalations)
		if c.KeptDisabled > 0 {
			fmt.Fprintf(out, "; kept %d disabled agent name(s)", c.KeptDisabled)
		}
		fmt.Fprintln(out)
	}
	if len(failed) > 0 {
		return fmt.Errorf("could not prune %s", strings.Join(failed, ", "))
	}
	PrintNextSteps(out, []Hint{
		{Cmd: "hap nodes", Why: "the nodes still sharing this store"},
		{Cmd: "hap agents", Why: "the herd without the pruned machines"},
	})
	return nil
}

// nodeFlagEcho repeats a --node argument in a suggested command line.
func nodeFlagEcho(ref string) string {
	if ref == "" {
		return ""
	}
	return " --node " + ref
}

// parseAge reads an age as whole days ("7d") or a Go duration ("36h", "90m").
func parseAge(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n <= 0 {
			return 0, fmt.Errorf("invalid age %q — whole days like 7d, or a duration like 36h", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("invalid age %q — whole days like 7d, or a duration like 36h", s)
	}
	return d, nil
}

// ageWords renders an age the way the operator typed it: days when whole.
func ageWords(d time.Duration) string {
	if d >= 24*time.Hour && d%(24*time.Hour) == 0 {
		return strconv.Itoa(int(d/(24*time.Hour))) + "d"
	}
	return strings.TrimSuffix(strings.TrimSuffix(d.String(), "0s"), "0m")
}
