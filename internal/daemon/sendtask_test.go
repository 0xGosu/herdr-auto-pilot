package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
)

// sendTaskSeam records what the hand-out seam was called with.
type sendTaskSeam struct {
	mu    sync.Mutex
	calls []sendTaskCall
	err   error
}

type sendTaskCall struct {
	payload   domain.SendTaskPayload
	agentID   string
	agentType string
	agentName string
}

func (s *sendTaskSeam) send(_ context.Context, p domain.SendTaskPayload,
	agentID, agentType, agentName string, _ ports.TaskSendHost) error {

	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, sendTaskCall{p, agentID, agentType, agentName})
	return s.err
}

func (s *sendTaskSeam) seen() []sendTaskCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]sendTaskCall(nil), s.calls...)
}

func newSendTaskHarness(t *testing.T, seam *sendTaskSeam, agentIDs ...string) *harness {
	t.Helper()
	rows := make([]domain.AgentTransition, 0, len(agentIDs))
	for _, id := range agentIDs {
		rows = append(rows, domain.AgentTransition{
			AgentID: id, PaneID: id, AgentType: "claude", Status: "idle",
		})
	}
	fl := &fakeLLM{}
	h := newHarnessCore(t, "", func(fh *fakeHerdr) ports.HerdrPort {
		fh.setAgents(rows)
		return fh
	}, fl, fl, nil, func(o *Options) {
		if seam != nil {
			o.SendTask = seam.send
		}
	})
	ctx := context.Background()
	for _, id := range agentIDs {
		if _, err := h.raw.EnsureAgentName(ctx, id); err != nil {
			t.Fatalf("ensure agent name %q: %v", id, err)
		}
	}
	h.waitForRoster(t, agentIDs...)
	return h
}

func queueSendTask(t *testing.T, h *harness, target, locator string, index int, text string) int64 {
	t.Helper()
	payload, err := json.Marshal(domain.SendTaskPayload{Locator: locator, Index: index, TaskText: text})
	if err != nil {
		t.Fatal(err)
	}
	return h.queueAction(domain.AgentAction{
		Kind: domain.AgentActionSendTask, Target: target,
		Payload: string(payload), Author: "operator",
	})
}

// The happy path, and the reason the payload is as thin as it is: the agent's
// id, its TYPE and its short name are all resolved HERE and handed to the seam.
// None of them can be carried, because a pane id repeats on every machine and
// agent_names is unique only per node.
func TestAQueuedTaskHandOutResolvesTheAgentItself(t *testing.T) {
	seam := &sendTaskSeam{}
	h := newSendTaskHarness(t, seam, "w1:p1")

	// Addressed by the operator's SPELLING — the short name — not a pane id.
	name, err := h.raw.EnsureAgentName(context.Background(), "w1:p1")
	if err != nil {
		t.Fatal(err)
	}
	got := h.awaitAction(queueSendTask(t, h, name, "/tmp/tasks.md", 2, "work"))
	if got.Status != domain.AgentActionDone {
		t.Fatalf("status = %q (%s), want done", got.Status, got.Error)
	}
	calls := seam.seen()
	if len(calls) != 1 {
		t.Fatalf("seam calls = %+v, want exactly 1", calls)
	}
	c := calls[0]
	if c.agentID != "w1:p1" || c.agentType != "claude" || c.agentName != name {
		t.Errorf("seam got agent %q/%q/%q, want w1:p1/claude/%s", c.agentID, c.agentType, c.agentName, name)
	}
	if c.payload.Locator != "/tmp/tasks.md" || c.payload.Index != 2 || c.payload.TaskText != "work" {
		t.Errorf("seam payload = %+v, want the list and item as queued", c.payload)
	}
}

// The idle re-check moved here WITH the pane access, and it keeps failing
// CLOSED on every shape of "we could not ask".
//
// The operator's own status read is as old as their keypress — or, for a remote
// hand-out, as old as two sync intervals — and delivering into a working agent's
// live conversation is what the idle-only rule exists to prevent. The seam must
// not run in any of these cases: it is what marks the item "[-]".
func TestAQueuedTaskHandOutRechecksIdle(t *testing.T) {
	for _, tc := range []struct {
		name   string
		agents []domain.AgentTransition
		failed bool
		want   string
	}{
		{
			name: "the agent started working after the operator looked",
			agents: []domain.AgentTransition{
				{AgentID: "w1:p1", PaneID: "w1:p1", AgentType: "claude", Status: "working"}},
			want: "cleanly idle",
		},
		{
			name:   "the agent vanished entirely",
			agents: []domain.AgentTransition{},
			want:   "no longer live",
		},
		{
			name:   "an unreadable agent list is not an idle agent",
			failed: true,
			want:   "cannot confirm",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seam := &sendTaskSeam{}
			h := newSendTaskHarness(t, seam, "w1:p1")
			if tc.failed {
				h.herdr.setFailListAgents(true)
			} else {
				h.herdr.setAgents(tc.agents)
			}

			got := h.awaitAction(queueSendTask(t, h, "w1:p1", "/tmp/tasks.md", 1, "work"))
			if got.Status != domain.AgentActionFailed {
				t.Fatalf("status = %q (%s), want failed", got.Status, got.Error)
			}
			if !strings.Contains(got.Error, tc.want) {
				t.Errorf("error = %q, want it to contain %q", got.Error, tc.want)
			}
			if len(seam.seen()) != 0 {
				t.Errorf("nothing may be reserved for a refused hand-out: %+v", seam.seen())
			}
		})
	}
}

// A hand-out whose pane has been recycled under a different terminal is refused.
func TestAQueuedTaskHandOutRefusesARecycledTerminal(t *testing.T) {
	seam := &sendTaskSeam{}
	h := newSendTaskHarness(t, seam, "w1:p1")
	if _, err := h.raw.SyncAgentTerminalID(context.Background(), "w1:p1", "term_new"); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(domain.SendTaskPayload{Locator: "/tmp/tasks.md", Index: 1, TaskText: "work"})
	id := h.queueAction(domain.AgentAction{
		Kind: domain.AgentActionSendTask, Target: "w1:p1",
		TerminalID: "term_from_another_life", Payload: string(payload), Author: "operator",
	})
	got := h.awaitAction(id)
	if got.Status != domain.AgentActionFailed {
		t.Fatalf("status = %q, want failed", got.Status)
	}
	if len(seam.seen()) != 0 {
		t.Errorf("the hand-out ran against a recycled pane: %+v", seam.seen())
	}
}

// A malformed hand-out is refused before any resolution: an item index of zero
// names nothing, and reserveTask would answer with a confusing text mismatch.
func TestAQueuedTaskHandOutRefusesAnEmptyRequest(t *testing.T) {
	for _, tc := range []struct{ name, locator, want string }{
		{"no list", "", "names no list"},
		{"no item", "/tmp/tasks.md", "names no item"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seam := &sendTaskSeam{}
			h := newSendTaskHarness(t, seam, "w1:p1")
			got := h.awaitAction(queueSendTask(t, h, "w1:p1", tc.locator, 0, "work"))
			if got.Status != domain.AgentActionFailed {
				t.Fatalf("status = %q, want failed", got.Status)
			}
			if !strings.Contains(got.Error, tc.want) {
				t.Errorf("error = %q, want %q", got.Error, tc.want)
			}
		})
	}
}

// A build with no hand-out seam refuses with the SHARED marker, so a front end
// on another machine can re-phrase the advice with that node's version.
func TestAQueuedTaskHandOutWithNoSeamIsRefused(t *testing.T) {
	h := newSendTaskHarness(t, nil, "w1:p1")
	got := h.awaitAction(queueSendTask(t, h, "w1:p1", "/tmp/tasks.md", 1, "work"))
	if got.Status != domain.AgentActionFailed {
		t.Fatalf("status = %q, want failed", got.Status)
	}
	if !strings.Contains(got.Error, domain.ActionUnsupportedMarker) {
		t.Errorf("error = %q, want the shared unsupported marker", got.Error)
	}
}

// The seam's own failure reaches the operator verbatim.
func TestAQueuedTaskHandOutReportsTheSeamsError(t *testing.T) {
	seam := &sendTaskSeam{err: errors.New("task #1 is no longer pending")}
	h := newSendTaskHarness(t, seam, "w1:p1")
	got := h.awaitAction(queueSendTask(t, h, "w1:p1", "/tmp/tasks.md", 1, "work"))
	if got.Status != domain.AgentActionFailed {
		t.Fatalf("status = %q, want failed", got.Status)
	}
	if !strings.Contains(got.Error, "no longer pending") {
		t.Errorf("error = %q, want the seam's own sentence", got.Error)
	}
}

// A disabled agent suppresses the hand-out: it runs inside the same per-agent
// lifecycle barrier every other delivery takes.
func TestAQueuedTaskHandOutHonoursADisabledAgent(t *testing.T) {
	seam := &sendTaskSeam{}
	h := newSendTaskHarness(t, seam, "w1:p1")
	if err := h.raw.SetAgentDisabled(context.Background(), "w1:p1", true); err != nil {
		t.Fatal(err)
	}
	got := h.awaitAction(queueSendTask(t, h, "w1:p1", "/tmp/tasks.md", 1, "work"))
	if got.Status != domain.AgentActionFailed {
		t.Fatalf("status = %q, want failed", got.Status)
	}
	if !strings.Contains(got.Error, "disabled for automation") {
		t.Errorf("error = %q, want it to name the disable", got.Error)
	}
	if len(seam.seen()) != 0 {
		t.Errorf("the hand-out ran for a disabled agent: %+v", seam.seen())
	}
}
