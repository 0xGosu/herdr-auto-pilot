package frontend_test

import (
	"context"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/embedder"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
)

// cwdHerdr is a HerdrPort that also implements the optional InspectorPort,
// counting PaneInfo calls so the caching contract can be asserted.
type cwdHerdr struct {
	agents []domain.AgentTransition
	info   map[string]domain.PaneInfo
	err    error
	calls  atomic.Int32
}

func (f *cwdHerdr) Send(context.Context, string, string) error { return nil }
func (f *cwdHerdr) ReadPane(context.Context, string, int) (string, error) {
	return "", nil
}
func (f *cwdHerdr) ListAgents(context.Context) ([]domain.AgentTransition, error) {
	return f.agents, nil
}
func (f *cwdHerdr) PaneInfo(_ context.Context, paneID string) (domain.PaneInfo, error) {
	f.calls.Add(1)
	if f.err != nil {
		return domain.PaneInfo{}, f.err
	}
	return f.info[paneID], nil
}

// TestStatusReportsAgentCwd covers the value the TUI detail view and
// `hap agents` both display: the foreground process's cwd wins over the pane's
// (an agent run from a subdirectory shows where it actually is), and an agent
// herdr cannot answer for is simply absent rather than blank-filled.
func TestStatusReportsAgentCwd(t *testing.T) {
	app, _ := testApp(t)
	app.Herdr = &cwdHerdr{
		agents: []domain.AgentTransition{
			{AgentID: "w1:p1", PaneID: "w1:p1", AgentType: "claude", Status: "idle"},
			{AgentID: "w1:p2", PaneID: "w1:p2", AgentType: "claude", Status: "idle"},
			{AgentID: "w1:p3", PaneID: "w1:p3", AgentType: "claude", Status: "idle"},
		},
		info: map[string]domain.PaneInfo{
			"w1:p1": {Cwd: "/repo", ForegroundCwd: "/repo/worktree"},
			"w1:p2": {Cwd: "/other"},
			// w1:p3 has no entry: herdr answers with an empty PaneInfo.
		},
	}

	st, err := app.GetStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Cwds are opt-in: GetStatus alone must not pay for the lookups.
	if len(st.AgentCwds) != 0 {
		t.Errorf("GetStatus filled AgentCwds = %v; the lookups must be opt-in", st.AgentCwds)
	}
	app.FillAgentCwds(context.Background(), &st)

	if got := st.AgentCwd("w1:p1"); got != "/repo/worktree" {
		t.Errorf("AgentCwd(w1:p1) = %q, want the foreground cwd", got)
	}
	if got := st.AgentCwd("w1:p2"); got != "/other" {
		t.Errorf("AgentCwd(w1:p2) = %q, want the pane cwd fallback", got)
	}
	if got := st.AgentCwd("w1:p3"); got != "" {
		t.Errorf("AgentCwd(w1:p3) = %q, want empty when herdr reports none", got)
	}
}

// TestStatusAgentCwdIsCached guards the cost of putting a cwd on a 2s-refresh
// screen: repeated GetStatus calls inside the TTL must not re-shell out to
// herdr once per agent per refresh.
func TestStatusAgentCwdIsCached(t *testing.T) {
	app, _ := testApp(t)
	h := &cwdHerdr{
		agents: []domain.AgentTransition{{AgentID: "w1:p1", PaneID: "w1:p1", AgentType: "claude", Status: "idle"}},
		info:   map[string]domain.PaneInfo{"w1:p1": {Cwd: "/repo"}},
	}
	app.Herdr = h

	ctx := context.Background()
	for i := 0; i < 5; i++ {
		st, err := app.GetStatus(ctx)
		if err != nil {
			t.Fatal(err)
		}
		app.FillAgentCwds(ctx, &st)
		if st.AgentCwd("w1:p1") != "/repo" {
			t.Fatalf("refresh %d: cwd lost", i)
		}
	}
	if n := h.calls.Load(); n != 1 {
		t.Errorf("PaneInfo called %d times across 5 refreshes, want 1 (the TTL cache is not holding)", n)
	}
}

// TestStatusAgentCwdExpires: the cache must not be a one-way latch. An agent
// that `cd`s has to show its new directory once the TTL passes, so this drives
// the clock across the boundary rather than trusting the hit path alone.
func TestStatusAgentCwdExpires(t *testing.T) {
	app, _ := testApp(t)
	h := &cwdHerdr{
		agents: []domain.AgentTransition{{AgentID: "w1:p1", PaneID: "w1:p1", AgentType: "claude", Status: "idle"}},
		info:   map[string]domain.PaneInfo{"w1:p1": {Cwd: "/repo"}},
	}
	app.Herdr = h
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	app.Clock = func() time.Time { return now }

	ctx := context.Background()
	st, err := app.GetStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	app.FillAgentCwds(ctx, &st)
	if st.AgentCwd("w1:p1") != "/repo" {
		t.Fatalf("first fill lost the cwd: %v", st.AgentCwds)
	}

	// The agent moves; still inside the TTL, so the cached answer stands.
	h.info["w1:p1"] = domain.PaneInfo{Cwd: "/repo/worktree"}
	now = now.Add(5 * time.Second)
	st.AgentCwds = nil
	app.FillAgentCwds(ctx, &st)
	if got := st.AgentCwd("w1:p1"); got != "/repo" {
		t.Errorf("inside the TTL: cwd = %q, want the cached %q", got, "/repo")
	}

	// Past the TTL: re-read, and the move shows up.
	now = now.Add(2 * frontend.CwdTTLForTest)
	st.AgentCwds = nil
	app.FillAgentCwds(ctx, &st)
	if got := st.AgentCwd("w1:p1"); got != "/repo/worktree" {
		t.Errorf("after the TTL: cwd = %q, want the refreshed %q", got, "/repo/worktree")
	}
	if n := h.calls.Load(); n != 2 {
		t.Errorf("PaneInfo calls = %d, want exactly 2 (one per TTL window)", n)
	}
}

// TestStatusAgentCwdSurvivesInspectorFailure keeps the rule that a display
// nicety can never break status: a herdr that errors on every PaneInfo must
// still yield a usable Status.
func TestStatusAgentCwdSurvivesInspectorFailure(t *testing.T) {
	app, _ := testApp(t)
	app.Herdr = &cwdHerdr{
		agents: []domain.AgentTransition{{AgentID: "w1:p1", PaneID: "w1:p1", AgentType: "claude", Status: "idle"}},
		err:    context.DeadlineExceeded,
	}
	st, err := app.GetStatus(context.Background())
	if err != nil {
		t.Fatalf("GetStatus must survive a PaneInfo failure: %v", err)
	}
	app.FillAgentCwds(context.Background(), &st)
	if len(st.MonitoredAgents) != 1 {
		t.Errorf("MonitoredAgents = %d, want the agent listed regardless", len(st.MonitoredAgents))
	}
	if got := st.AgentCwd("w1:p1"); got != "" {
		t.Errorf("AgentCwd = %q, want empty on a failed lookup", got)
	}
}

// TestEmbeddingTimeoutFieldsDisplayDefaults covers the config surface an
// operator uses to rescue a slow model: an unset key must show the default
// actually in force (not a bare 0), and a set one must show its value.
func TestEmbeddingTimeoutFieldsDisplayDefaults(t *testing.T) {
	def := config.Default()
	cases := []struct {
		key  string
		want int
	}{
		{"embedding.embed_timeout_ms", embedder.DefaultEmbedTimeoutMs},
		{"embedding.warm_timeout_ms", embedder.DefaultWarmTimeoutMs},
		{"embedding.max_consecutive_failures", embedder.DefaultMaxConsecutiveFailures},
	}
	for _, c := range cases {
		got := frontend.FieldValue(def, c.key)
		if !strings.Contains(got, strconv.Itoa(c.want)) || !strings.Contains(got, "default") {
			t.Errorf("FieldValue(default, %s) = %q, want it to name the in-force default %d", c.key, got, c.want)
		}
	}

	cfg := config.Default()
	cfg.Embedding.EmbedTimeoutMs = 8000
	cfg.Embedding.WarmTimeoutMs = 120000
	cfg.Embedding.MaxConsecutiveFailures = 10
	for key, want := range map[string]string{
		"embedding.embed_timeout_ms":         "8000",
		"embedding.warm_timeout_ms":          "120000",
		"embedding.max_consecutive_failures": "10",
	} {
		if got := frontend.FieldValue(cfg, key); got != want {
			t.Errorf("FieldValue(%s) = %q, want %q", key, got, want)
		}
	}
}

// TestEmbeddingTimeoutFieldsRejectNegatives pins SetField validation: 0 is the
// "restore the default" sentinel, negatives are a misconfiguration.
func TestEmbeddingTimeoutFieldsRejectNegatives(t *testing.T) {
	app, _ := testApp(t)
	ctx := context.Background()
	for _, key := range []string{"embedding.embed_timeout_ms", "embedding.warm_timeout_ms", "embedding.max_consecutive_failures"} {
		if err := app.SetField(ctx, key, "-1"); err == nil {
			t.Errorf("SetField(%s, -1) was accepted; a negative budget is never valid", key)
		}
		if err := app.SetField(ctx, key, "0"); err != nil {
			t.Errorf("SetField(%s, 0) rejected; 0 must restore the default: %v", key, err)
		}
	}
}
