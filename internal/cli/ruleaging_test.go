package cli_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// The operator-facing half of rule aging through the real CLI: the listed
// score is the aged one, `show` says where the rule stands, an aged-out rule
// reads 0.00 rather than "-", and `signatures confirm` gives the score back.
func TestSignaturesReportAgingAndConfirmRenews(t *testing.T) {
	app, st := testApp(t)
	seedSignatures(t, st) // approval:aaaa… — two operator confirmations, raw 1.00
	ctx := context.Background()
	const day = 24 * time.Hour
	setAnchor := func(unconfirmed time.Duration) {
		t.Helper()
		rule, err := st.GetSignature(ctx, "approval:aaaa1111bbbb2222")
		if err != nil || rule == nil {
			t.Fatalf("rule: %v, %v", rule, err)
		}
		if err := st.UpsertSignature(ctx, domain.RestartDecay(*rule, time.Now().Add(-unconfirmed))); err != nil {
			t.Fatal(err)
		}
	}

	// 29 days unconfirmed under the default policy (14 grace, 30 to drain) is
	// 15 days of aging: half the score.
	setAnchor(29 * day)
	out, err := run(t, app, "signatures", "list")
	if err != nil || !strings.Contains(out, "conf=0.50") {
		t.Errorf("list must show the aged score (%v):\n%s", err, out)
	}
	out, err = run(t, app, "signatures", "show", "approval:")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"confidence: 0.50", "aging:", "unconfirmed 29d", "aged from 1.00", "reaches 0 on"} {
		if !strings.Contains(out, want) {
			t.Errorf("show output missing %q:\n%s", want, out)
		}
	}

	// Fully aged: a real 0.00, not the "-" of a rule nobody ever scored.
	setAnchor(400 * day)
	out, _ = run(t, app, "signatures", "list")
	if !strings.Contains(out, "conf=0.00") {
		t.Errorf("an aged-out rule must list as conf=0.00:\n%s", out)
	}
	out, _ = run(t, app, "signatures", "show", "approval:")
	if !strings.Contains(out, "at 0") || !strings.Contains(out, "unless confirmed") {
		t.Errorf("show must say the rule is at zero and how to save it:\n%s", out)
	}

	// `signatures confirm` is the renewal.
	out, err = run(t, app, "signatures", "confirm", "approval:")
	if err != nil || !strings.Contains(out, "confidence renewed to 1.00") {
		t.Errorf("confirm must renew the aged rule (%v):\n%s", err, out)
	}
	out, _ = run(t, app, "signatures", "list")
	if !strings.Contains(out, "conf=1.00") {
		t.Errorf("after the renewal the rule must list at its full score:\n%s", out)
	}
}

func TestConfigShowsTheEffectiveAgingPolicy(t *testing.T) {
	app, _ := testApp(t)
	out, err := run(t, app, "config")
	if err != nil || !strings.Contains(out,
		"confidence_decay_grace_days=14 confidence_decay_days=30 stale_rule_removal_days=14") {
		t.Errorf("config must show the default aging policy (%v):\n%s", err, out)
	}
	if _, err := run(t, app, "config", "set", "learning.confidence_decay_days", "0"); err != nil {
		t.Fatal(err)
	}
	out, _ = run(t, app, "config")
	if !strings.Contains(out, "confidence_decay_days=0") {
		t.Errorf("config must show aging switched off:\n%s", out)
	}
	if _, err := run(t, app, "config", "set", "learning.stale_rule_removal_days", "-3"); err == nil {
		t.Error("a negative day count must be rejected")
	}
}
