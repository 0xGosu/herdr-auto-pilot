package frontend_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
	"github.com/0xGosu/herdr-auto-pilot/internal/store"
)

// seedEmptySuggestionEscalation writes a pending escalation carrying the given
// rationale tag and no suggestion — the shape App.Confirm cannot resolve.
func seedEmptySuggestionEscalation(t *testing.T, st *store.Store, reason domain.EscalateReason) int64 {
	t.Helper()
	id, err := st.AppendAudit(context.Background(), domain.AuditRecord{
		AgentID: "w1:p1", SituationType: domain.SituationIdle, Trigger: "t",
		Action: "escalated", Status: "escalated",
		Rationale: "[" + string(reason) + "]",
		CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// TestConfirmNoTaskSourceReturnsGuidance pins the feature: confirming the
// notice hands back guidance the operator can act on, not a refusal.
func TestConfirmNoTaskSourceReturnsGuidance(t *testing.T) {
	app, st := testApp(t)
	id := seedEmptySuggestionEscalation(t, st, domain.ReasonNoTaskSource)

	// Confirm-only and confirm+send answer identically: App.Confirm never
	// consults `send` before the guard.
	for _, send := range []bool{false, true} {
		err := app.Confirm(context.Background(), id, send)
		var notice *frontend.NoTaskSourceNotice
		if !errors.As(err, &notice) {
			t.Fatalf("send=%v: confirm error = %v, want a *NoTaskSourceNotice", send, err)
		}
		if notice.AuditID != id {
			t.Errorf("send=%v: notice names audit %d, want %d", send, notice.AuditID, id)
		}
		g := notice.Guidance()
		for _, want := range []string{
			"hap config set " + frontend.LLMTaskGenerateCommandKey + " --preset claude",
			"hap config set " + frontend.LLMTaskGenerateCommandKey + " --preset codex",
			"hap config task-source add",
			"hap dismiss",
		} {
			if !strings.Contains(g, want) {
				t.Errorf("send=%v: guidance is missing %q:\n%s", send, want, g)
			}
		}
		// The old refusal's wording is what this replaces — it must be gone.
		if strings.Contains(g, "no suggestion to confirm") {
			t.Errorf("send=%v: guidance still carries the old refusal wording:\n%s", send, g)
		}
		// The TUI's durable status area budgets exactly ONE line and flattens
		// anything longer, so Error() must stay printable there whole.
		if strings.Contains(notice.Error(), "\n") {
			t.Errorf("send=%v: Error() must be one line, got %q", send, notice.Error())
		}
	}

	// A notice is not a resolution: the row stays pending for the operator.
	rec, err := st.GetAudit(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != "escalated" {
		t.Errorf("status = %q after confirming a notice, want it left escalated", rec.Status)
	}
}

// TestConfirmNoSuggestionStillErrorsForOtherReasons is the control. Without it,
// a change that hands the notice to EVERY unresolvable row passes — including
// the four safety vetoes, whose whole point is that they withhold a one-key
// answer and must say so.
func TestConfirmNoSuggestionStillErrorsForOtherReasons(t *testing.T) {
	for _, reason := range []domain.EscalateReason{
		domain.ReasonNeverAutoMatch,
		domain.ReasonUnclassifiable,
		domain.ReasonUnfamiliarOptions,
		domain.ReasonTaskGenFailed,
	} {
		t.Run(string(reason), func(t *testing.T) {
			app, st := testApp(t)
			id := seedEmptySuggestionEscalation(t, st, reason)

			err := app.Confirm(context.Background(), id, false)
			var notice *frontend.NoTaskSourceNotice
			if errors.As(err, &notice) {
				t.Fatalf("[%s] must keep the ordinary refusal, got the no_task_source notice", reason)
			}
			if err == nil || !strings.Contains(err.Error(), "has no suggestion to confirm") {
				t.Fatalf("confirm error = %v, want the ordinary no-suggestion refusal", err)
			}
		})
	}
}

// TestConfirmNoTaskSourceWithASuggestionStillResolves pins the scoping. The
// same [no_task_source] tag is stamped on CONFIRMABLE rows — the task-gen
// success path attaches "LLM suggested task: …" — and those must take the
// ordinary Resolve path, never the notice. Placing the branch inside the
// `action == ""` guard is what guarantees it; this fails if it ever moves out.
func TestConfirmNoTaskSourceWithASuggestionStillResolves(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	id, err := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "w1:p1", SituationType: domain.SituationApproval, Trigger: "t",
		Action: "escalated", Status: "escalated",
		Rationale:  "[" + string(domain.ReasonNoTaskSource) + "]",
		Suggestion: "yes",
		CreatedAt:  time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}

	// send=false records the correction only, so no daemon or pane is needed.
	if err := app.Confirm(ctx, id, false); err != nil {
		t.Fatalf("a [no_task_source] row WITH a suggestion must confirm normally: %v", err)
	}
	// send=false records the learning event and nothing else, so the correction
	// is the evidence that the ordinary Resolve path ran.
	corrections, err := st.UnprocessedCorrections(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(corrections) != 1 || corrections[0].AuditID != id || corrections[0].CorrectedAction != "yes" {
		t.Errorf("corrections = %+v, want one recording \"yes\" for audit %d", corrections, id)
	}
}

// TestNoTaskSourceGuidanceNamesEveryPreset holds the text to the preset
// registry rather than to a hand-written list, so adding a third recipe cannot
// leave the advice naming only two.
func TestNoTaskSourceGuidanceNamesEveryPreset(t *testing.T) {
	g := (&frontend.NoTaskSourceNotice{AuditID: 7}).Guidance()
	for _, preset := range frontend.LLMPresetNames {
		want := "hap config set " + frontend.LLMTaskGenerateCommandKey + " --preset " + preset
		if !strings.Contains(g, want) {
			t.Errorf("guidance is missing the %q preset line %q:\n%s", preset, want, g)
		}
	}
	// `--preset=claude` is NOT handled by `config set` (it matches the flag by
	// equality on its own argument) and would be stored as a literal one-word
	// argv, silently. Never print that spelling.
	if strings.Contains(g, "--preset=") {
		t.Errorf("guidance prints the unsupported --preset=<name> spelling:\n%s", g)
	}
}
