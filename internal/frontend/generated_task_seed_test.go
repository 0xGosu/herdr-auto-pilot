package frontend_test

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
)

// TestBootstrapSeedsTheListWithItsHeaderNotBlank is the regression guard for the
// gist bootstrap 422.
//
// The generated-task confirm created the agent's list with BLANK initial
// content and wrote the real tasks a moment later, which is invisible on a
// local file and fatal on a gist: GitHub cannot store a blank gist file at all,
// so the create failed with `422 Validation Failed ... Field:files` — an error
// naming neither the list nor the cause — and accepting an LLM-generated task
// escalation was impossible for every operator on a `github_gist` provider.
//
// The seed is observed by making the MUTATION that follows the create fail (a
// suggestion past the default 20-task cap), which leaves the list holding
// exactly what the create seeded. Asserting the created content — rather than
// the finished list — is the point: the finished list is non-blank either way,
// which is why this shipped.
func TestBootstrapSeedsTheListWithItsHeaderNotBlank(t *testing.T) {
	app, st := testApp(t)
	app.Herdr = &fakeHerdr{}
	stateDir := t.TempDir()
	app.StateDir = stateDir
	ctx := context.Background()

	// One task per line, past the cap, so generatedTaskContent refuses and the
	// created-but-unwritten list survives for inspection.
	var lines []string
	for i := range 25 {
		lines = append(lines, "Task "+strconv.Itoa(i))
	}

	name, _ := st.EnsureAgentName(ctx, "w1:p1")
	id, _ := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "w1:p1", SituationType: domain.SituationIdle, Trigger: "t",
		Action: "escalated", Status: "escalated",
		Suggestion: domain.SuggestTaskPrefix + strings.Join(lines, "\n"), CreatedAt: time.Now(),
	})

	if err := app.Confirm(ctx, id, false); err == nil {
		t.Fatal("a suggestion past the cap must refuse, so the seeded list can be inspected")
	}

	data, err := os.ReadFile(filepath.Join(stateDir, "tasks", name+".md"))
	if err != nil {
		t.Fatalf("the create-on-demand seed never ran: %v", err)
	}
	// The assertion below is only about the SEED while the mutation that
	// follows it never ran. If a later change lets that mutation succeed and
	// fails the confirm somewhere downstream instead, the file holds the fully
	// rendered list and a blank seed would sail through green — the exact
	// regression this test exists to catch. So pin the precondition.
	if got := len(domain.ParseChecklist(string(data))); got != 0 {
		t.Fatalf("the mutation ran, so this file is no longer the seed — got %d items; "+
			"restore a refusal between the create and the write, or this test proves nothing", got)
	}
	if strings.TrimSpace(string(data)) == "" {
		t.Fatalf("the list was created BLANK (%q) — a gist cannot hold one, so the "+
			"whole confirm fails with GitHub's unactionable 422", string(data))
	}
	if !strings.Contains(string(data), name) {
		t.Errorf("the seed must be the agent's header; got %q", string(data))
	}
}

// TestNewListHeaderIsNeverBlank holds the one seed every create-on-demand path
// uses to the rule the gist backend enforces (gist.ErrBlankContent): whatever
// the selector looks like, the list is created with something in it.
func TestNewListHeaderIsNeverBlank(t *testing.T) {
	for _, agent := range []string{"", "brave-otter", "2", "#3", "  "} {
		if got := frontend.NewListHeaderForTest(agent); strings.TrimSpace(got) == "" {
			t.Errorf("newListHeader(%q) = %q, want non-blank content — GitHub refuses a "+
				"blank gist file, so a blank seed fails the create outright", agent, got)
		}
	}
}

// TestEnsureListRefusesABlankSeedOnEveryBackend: the never-blank rule is
// enforced where the create is dispatched, not only inside the backend that
// breaks on it.
//
// This is asserted under the LOCAL provider on purpose. A local file takes a
// blank seed happily, so without this guard a caller passing "" is green
// through the entire unit suite and fails for the first operator on a
// github_gist provider — which is precisely how the generated-task bootstrap
// shipped. Catching the class only on gist would let the next caller repeat it.
func TestEnsureListRefusesABlankSeedOnEveryBackend(t *testing.T) {
	app, _ := testApp(t)
	ctx := context.Background()
	locator := filepath.Join(t.TempDir(), "tasks.md")

	for _, initial := range []string{"", "\n", "  \t\n"} {
		created, err := app.EnsureListForTest(ctx, locator, initial)
		if err == nil {
			t.Fatalf("ensureList(%q) must refuse a blank seed even on a local backend "+
				"(created=%v); a gist cannot hold one at all", initial, created)
		}
		if created {
			t.Errorf("ensureList(%q) reported creating a list it refused to create", initial)
		}
		if _, statErr := os.Stat(locator); statErr == nil {
			t.Fatalf("ensureList(%q) wrote a blank list", initial)
		}
	}

	// And the ordinary path still creates.
	created, err := app.EnsureListForTest(ctx, locator, frontend.NewListHeaderForTest("brave-otter"))
	if err != nil || !created {
		t.Fatalf("a header seed must still create the list; created=%v err=%v", created, err)
	}
}
