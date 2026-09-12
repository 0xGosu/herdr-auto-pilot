package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
)

// agyOutsidePane is an agy file approval for a path in a DIFFERENT checkout
// from the one the agent was started in — the shape that cost an approval
// round trip per file read in the run this came from.
const agyOutsidePane = "● Read(/workspaces/other-checkout/internal/x.go) (ctrl+o to expand)\n\n" +
	"File access\n────────────────\n\n" +
	"Read: /workspaces/other-checkout/internal/x.go\n" +
	"Reason: outside workspace\n\n" +
	"Allow access to this file?\n" +
	"> 1. Yes, allow access\n" +
	"  2. Yes, and always allow non-workspace access\n" +
	"  3. No, deny access\n\n" +
	"  ↑/↓ Navigate\n" +
	"esc to cancel                                              Gemini 3.6 Flash · low\n"

// warmPaneCwdAndPush drives events until the mismatch is observed or the wait
// expires. The first event on a cold pane CANNOT see it: paneCwd is a cache
// that never shells out on the main loop, so it returns "" and refreshes in
// the background — and "" is deliberately read as "unknown", not "mismatch".
// One further event picks it up, which is why this pushes rather than asserts
// after a single transition.
func warmPaneCwdAndPush(t *testing.T, h *harness, agentID string, want int) []struct {
	Root, Elsewhere string
} {
	t.Helper()
	var got []struct{ Root, Elsewhere string }
	waitFor(t, 5*time.Second, func() bool {
		h.pushAgy(agentID, "done")
		got = got[:0]
		for _, m := range h.daemon.agyWorkspaceHealth() {
			got = append(got, struct{ Root, Elsewhere string }{m.Root, m.Elsewhere})
		}
		return len(got) == want
	})
	return got
}

// TestAgyWorkspaceMismatchIsNoticedFromTheApproval: hap cannot ask an agent
// where its work is — the agy process never leaves the directory it was
// started in — so the "outside workspace" prompt is the only evidence, and it
// must be read from the CLASSIFY path. Once the rule graduates hap answers
// these itself and no escalation is ever raised, so noticing at escalate()
// would go quiet exactly when the agent is busiest.
func TestAgyWorkspaceMismatchIsNoticedFromTheApproval(t *testing.T) {
	h := newHarness(t, "")
	h.herdr.mu.Lock()
	h.herdr.paneInfo = domain.PaneInfo{ForegroundCwd: "/workspaces/started-here"}
	h.herdr.mu.Unlock()
	h.herdr.setPane(agyOutsidePane)

	got := warmPaneCwdAndPush(t, h, "agent-agy-ws", 1)
	if len(got) != 1 {
		t.Fatalf("mismatches = %+v, want exactly one", got)
	}
	if got[0].Root != "/workspaces/started-here" {
		t.Errorf("root = %q, want where the agent was started", got[0].Root)
	}
	// The DIRECTORY, not the first file that happened to be read: the file
	// changes every run and tells the operator nothing about where to relaunch.
	if got[0].Elsewhere != "/workspaces/other-checkout" {
		t.Errorf("elsewhere = %q, want the directory the work is in", got[0].Elsewhere)
	}
}

// The control: the same pane on an agent working INSIDE its own root is not a
// mismatch. Without this, a predicate that answered "yes" for everything would
// pass the case above.
func TestAgyWorkspaceInsideItsRootIsNotReported(t *testing.T) {
	h := newHarness(t, "")
	h.herdr.mu.Lock()
	h.herdr.paneInfo = domain.PaneInfo{ForegroundCwd: "/workspaces/other-checkout"}
	h.herdr.mu.Unlock()
	h.herdr.setPane(agyOutsidePane)

	// Wait for the cwd cache to WARM before concluding anything. paneCwd
	// returns "" while cold, and an unknown root is not a mismatch by design —
	// so without this the test would pass whether or not the root-containment
	// logic works, and would be indistinguishable from the unknown-cwd case
	// below. The containment rule is what stops /workspaces/repo-two reading
	// as inside /workspaces/repo.
	waitFor(t, 5*time.Second, func() bool {
		h.pushAgy("agent-agy-inside", "done")
		return h.daemon.paneCwd(context.Background(), "agent-agy-inside") != ""
	})
	if got := h.daemon.agyWorkspaceHealth(); len(got) != 0 {
		t.Errorf("an agent working inside its own root is not a mismatch: %+v", got)
	}
}

// An unread cwd must never render as a misconfiguration. paneCwd returns ""
// for a pane it has not resolved (no inspector surface here), and reporting
// that as "started somewhere else" would flag every agent on a herdr build
// that cannot answer `pane get`.
func TestAgyWorkspaceUnknownCwdIsNotAMismatch(t *testing.T) {
	h := newHarnessWrapped(t, "", func(f *fakeHerdr) ports.HerdrPort {
		return inspectorlessHerdr{f}
	})
	h.herdr.setPane(agyOutsidePane)

	for i := 0; i < 3; i++ {
		h.pushAgy("agent-agy-nocwd", "done")
		time.Sleep(30 * time.Millisecond)
	}
	if got := h.daemon.agyWorkspaceHealth(); len(got) != 0 {
		t.Errorf("an unread cwd is not evidence of a mismatch: %+v", got)
	}
}

// A claude pane carrying the same words is not an agy workspace prompt: the
// detection is agy-specific because the permission model is.
func TestAgyWorkspaceIgnoresOtherAgentTypes(t *testing.T) {
	h := newHarness(t, "")
	h.herdr.mu.Lock()
	h.herdr.paneInfo = domain.PaneInfo{ForegroundCwd: "/workspaces/started-here"}
	h.herdr.mu.Unlock()
	h.herdr.setPane(agyOutsidePane)

	for i := 0; i < 3; i++ {
		h.push("agent-claude-ws", "blocked")
		time.Sleep(30 * time.Millisecond)
	}
	if got := h.daemon.agyWorkspaceHealth(); len(got) != 0 {
		t.Errorf("only agy scopes permissions this way: %+v", got)
	}
}
