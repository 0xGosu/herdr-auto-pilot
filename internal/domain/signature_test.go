package domain

import (
	"strings"
	"testing"
	"time"
)

func sit(t SituationType, agentType, content string) Situation {
	return Situation{Type: t, AgentType: agentType, Content: content}
}

func TestMaskVolatile(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"absolute path", "delete file /home/user/project/main.go now", "delete file <path> now"},
		{"uuid", "session 550e8400-e29b-41d4-a716-446655440000 expired", "session <uuid> expired"},
		{"hash", "commit 3f785a2b9c1d4e5f6a7b8c9d0e1f2a3b4c5d6e7f pushed", "commit <hash> pushed"},
		{"timestamp", "at 2026-07-09T14:03:22Z retry", "at <timestamp> retry"},
		{"line number", "error on line 42 of parser", "error on <line> of parser"},
		{"whitespace collapse", "a   b\n\t c", "a b c"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := MaskVolatile(c.in); got != c.want {
				t.Errorf("MaskVolatile(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestSignatureStableAcrossVolatileTokens(t *testing.T) {
	// FR-003 acceptance: prompts differing only in volatile tokens produce
	// the same signature.
	a := ComputeSignature(sit(SituationApproval, "claude",
		"Allow write to /home/alice/repo/main.go? (commit 1a2b3c4d5e6f7788)"))
	b := ComputeSignature(sit(SituationApproval, "claude",
		"Allow write to /home/bob/other/util.go? (commit ffeeddccbbaa9900)"))
	if a.Verdict != GuardOK || b.Verdict != GuardOK {
		t.Fatalf("unexpected guard verdicts: %v %v", a.Verdict, b.Verdict)
	}
	if a.Signature != b.Signature {
		t.Errorf("signatures differ across volatile tokens:\n a=%s (%q)\n b=%s (%q)",
			a.Signature, a.Salient, b.Signature, b.Salient)
	}
}

func TestSignatureDivergence(t *testing.T) {
	base := Situation{Type: SituationApproval, AgentType: "claude",
		Content: "Allow running the test suite for this project?"}

	cases := []struct {
		name string
		mut  func(Situation) Situation
	}{
		{"different situation type", func(s Situation) Situation { s.Type = SituationError; return s }},
		{"different agent type", func(s Situation) Situation { s.AgentType = "codex"; return s }},
		{"different content", func(s Situation) Situation {
			s.Content = "Allow deleting the production database contents?"
			return s
		}},
	}
	orig := ComputeSignature(base)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mutated := ComputeSignature(c.mut(base))
			if mutated.Signature == orig.Signature {
				t.Errorf("expected different signature for %s", c.name)
			}
		})
	}
}

func TestSignatureOptionSets(t *testing.T) {
	mk := func(opts ...string) Situation {
		return Situation{Type: SituationChoice, AgentType: "claude",
			Content: "Pick one of the following options", Options: opts}
	}
	same := ComputeSignature(mk("Use library A", "Use library B"))
	reordered := ComputeSignature(mk("Use library B", "Use library A"))
	different := ComputeSignature(mk("Use library A", "Use library C"))

	if same.Signature != reordered.Signature {
		t.Errorf("option order should not change the signature")
	}
	if same.Signature == different.Signature {
		t.Errorf("different option sets must produce different signatures")
	}

	// The encoding must stay injective for labels containing the delimiter:
	// without escaping, both of these sets flatten to "allow;once;deny".
	ambA := ComputeSignature(mk("allow;once", "deny"))
	ambB := ComputeSignature(mk("allow", "once;deny"))
	if ambA.Signature == ambB.Signature {
		t.Errorf("delimiter-bearing labels collide: %q vs %q", ambA.Salient, ambB.Salient)
	}
	// Still deterministic: the same delimiter-bearing set matches itself.
	if again := ComputeSignature(mk("deny", "allow;once")); again.Signature != ambA.Signature {
		t.Errorf("escaped encoding lost order-insensitivity: %q vs %q", again.Salient, ambA.Salient)
	}
}

func TestOverMaskingFloor(t *testing.T) {
	// FR-003a acceptance: a prompt reduced almost entirely to placeholders
	// is unclassifiable rather than matched on a degenerate signature.
	res := ComputeSignature(sit(SituationApproval, "claude",
		"/tmp/a/b /var/log/x/y 2026-07-09T10:00:00Z 550e8400-e29b-41d4-a716-446655440000 deadbeefcafe1234"))
	if res.Verdict != GuardOverMasked {
		t.Errorf("expected over-masked verdict, got %v (salient %q)", res.Verdict, res.Salient)
	}

	ok := ComputeSignature(sit(SituationApproval, "claude",
		"Allow the agent to run the full unit test suite before committing?"))
	if ok.Verdict != GuardOK {
		t.Errorf("normal prompt should pass the floor, got %v", ok.Verdict)
	}
}

func TestVarianceGuard(t *testing.T) {
	now := time.Now()
	mk := func(actions ...string) []DecisionRecord {
		var recs []DecisionRecord
		for i, a := range actions {
			recs = append(recs, DecisionRecord{ChosenAction: a, CreatedAt: now.Add(-time.Duration(i) * time.Minute)})
		}
		return recs
	}

	// FR-003a acceptance: an even split of contradictory decisions forces
	// escalation rather than auto-acting.
	if !VarianceGuardTripped(mk("yes", "no", "yes", "no", "yes", "no"), 0.6, 1.0) {
		t.Error("even split should trip the variance guard")
	}
	if VarianceGuardTripped(mk("yes", "yes", "yes", "yes", "yes", "no"), 0.6, 1.0) {
		t.Error("consistent history should not trip the variance guard")
	}
	if VarianceGuardTripped(mk("yes", "no"), 0.6, 1.0) {
		t.Error("tiny histories are governed by graduation, not the variance guard")
	}
	if VarianceGuardTripped(mk("yes", "no", "yes", "no", "yes", "no"), 0.5, 1.0) {
		t.Error("lower configured minimum should permit this history")
	}
}

func TestSalientContentUsesPermissionVerb(t *testing.T) {
	opts := []string{"Yes", "No, and tell Claude what to do differently"}
	a := ComputeSignature(Situation{Type: SituationApproval, AgentType: "claude",
		PermissionVerb: "run shell command", Options: opts, Content: strings.Repeat("noise ", 100)})
	b := ComputeSignature(Situation{Type: SituationApproval, AgentType: "claude",
		PermissionVerb: "run shell command", Options: opts, Content: "completely different pane noise"})
	if a.Signature != b.Signature {
		t.Error("approval signatures should key on the permission verb and options, not pane noise")
	}
}

func TestApprovalSalientFoldsOptions(t *testing.T) {
	// Issue #155: the verb alone let a Claude plan-approval screen and a Bash
	// command approval — both phrased "…to proceed?" — share one signature, so
	// a rule learned on one auto-fired on the other. The option set is the
	// screen's identity and must fold into the salient.
	mk := func(verb string, opts ...string) Situation {
		return Situation{Type: SituationApproval, AgentType: "claude",
			PermissionVerb: verb, Options: opts,
			Content: "Do you want to proceed with the requested action right now?"}
	}
	planOpts := []string{
		"Yes, and use auto mode",
		"Yes, manually approve edits",
		"No, refine the plan",
		"Tell Claude what to change",
	}
	bashOpts := []string{
		"Yes",
		"Yes, and don't ask again for: hap task commands",
		"No, and tell Claude what to do differently",
	}

	plan := ComputeSignature(mk("proceed", planOpts...))
	bash := ComputeSignature(mk("proceed", bashOpts...))
	if plan.Verdict != GuardOK || bash.Verdict != GuardOK {
		t.Fatalf("verdicts = %v / %v, want ok (salients %q / %q)",
			plan.Verdict, bash.Verdict, plan.Salient, bash.Salient)
	}
	if plan.Signature == bash.Signature {
		t.Errorf("plan approval and bash approval with the same verb must not share a signature (salient %q)", plan.Salient)
	}

	reordered := ComputeSignature(mk("proceed", "  no, refine the plan ",
		"Tell Claude what to change", "YES, AND USE AUTO MODE", "yes, manually approve edits"))
	if reordered.Signature != plan.Signature {
		t.Errorf("option order/case/whitespace must not change the signature:\n%q\nvs\n%q",
			reordered.Salient, plan.Salient)
	}

	bare := ComputeSignature(Situation{Type: SituationApproval, AgentType: "claude",
		PermissionVerb: "proceed with the migration steps",
		Content:        "Do you want to proceed with the migration steps?"})
	if !strings.HasSuffix(bare.Salient, "| options:") {
		t.Errorf("optionless approval salient must still carry the options segment, got %q", bare.Salient)
	}
	withOpts := ComputeSignature(Situation{Type: SituationApproval, AgentType: "claude",
		PermissionVerb: "proceed with the migration steps", Options: []string{"Yes", "No"},
		Content: "Do you want to proceed with the migration steps?"})
	if bare.Signature == withOpts.Signature {
		t.Error("empty and non-empty option sets must produce different signatures")
	}

	// The remote-env picker is exempt: its env labels are the learned action,
	// not the key, so differing environment lists share one signature.
	envA := ComputeSignature(Situation{Type: SituationApproval, AgentType: "claude",
		PermissionVerb: PermissionVerbSelectRemoteEnv,
		Options:        []string{"herdr-auto-pilot (env_A)", "Default (env_B)"},
		Content:        "Select remote environment"})
	envB := ComputeSignature(Situation{Type: SituationApproval, AgentType: "claude",
		PermissionVerb: PermissionVerbSelectRemoteEnv,
		Options:        []string{"other-project (env_C)"},
		Content:        "Select remote environment"})
	if envA.Signature != envB.Signature {
		t.Errorf("remote-env pickers with different env lists must share a signature:\n%q\nvs\n%q",
			envA.Salient, envB.Salient)
	}
}

func TestApprovalRemapCompatible(t *testing.T) {
	cases := []struct {
		name, a, b string
		want       bool
	}{
		{"identical option sets",
			"permission:proceed | options:no;yes",
			"permission:continue | options:no;yes", true},
		{"half overlap passes",
			"permission:proceed | options:no, and tell claude what to do differently;yes;yes, and don't ask again for go test commands",
			"permission:proceed | options:no, and tell claude what to do differently;yes;yes, and don't ask again for npm install commands", true},
		{"disjoint sets veto (plan vs bash)",
			"permission:proceed | options:no, refine the plan;tell claude what to change;yes, and use auto mode;yes, manually approve edits",
			"permission:proceed | options:no, and tell claude what to do differently;yes;yes, and don't ask again for go test commands", false},
		{"below-half overlap vetoes",
			"permission:proceed | options:a;b;c;d",
			"permission:proceed | options:a;x;y;z", false},
		{"both option-less approvals stay compatible",
			"permission:edit the config file | options:",
			"permission:modify the config file | options:", true},
		{"empty vs populated vetoes",
			"permission:proceed | options:",
			"permission:proceed | options:no;yes", false},
		{"verb-only candidate (pre-fix row) vetoes",
			"permission:proceed | options:no;yes",
			"permission:proceed", false},
		{"verb-only query (remote-env picker) vetoes",
			"permission:select remote environment",
			"permission:select remote environment | options:x", false},
		{"two pane-tail salients stay compatible (verbless approvals)",
			"the agent wants approval to frobnicate the widget",
			"the agent asks approval to frobnicate the widget now", true},
		{"pane-tail vs permission salient vetoes",
			"permission:proceed | options:no;yes",
			"some trailing pane content asking for approval", false},
		{"escaped delimiters parse as one label, not two",
			`permission:proceed | options:allow\;once;deny`,
			"permission:proceed | options:allow;deny;once", false},
		{"identical escaped sets stay compatible",
			`permission:proceed | options:allow\;once;deny`,
			`permission:continue | options:allow\;once;deny`, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ApprovalRemapCompatible(c.a, c.b); got != c.want {
				t.Errorf("ApprovalRemapCompatible(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
			}
		})
	}
}

func TestComputeSignatureRawMirrorsKey(t *testing.T) {
	res := ComputeSignature(sit(SituationApproval, "claude",
		strings.Repeat("allow the tool to edit files in the project? ", 2)))
	if res.Verdict != GuardOK {
		t.Fatalf("verdict = %v, want ok", res.Verdict)
	}
	if res.Raw == "" || res.Raw != res.Signature {
		t.Errorf("Raw = %q, want equal to Signature %q", res.Raw, res.Signature)
	}

	over := ComputeSignature(sit(SituationIdle, "claude", "/a/b/c 12345"))
	if over.Raw != "" || over.Signature != "" {
		t.Errorf("over-masked signature should have empty key and Raw, got %q / %q",
			over.Signature, over.Raw)
	}
}

func TestComputeSignatureNWindow(t *testing.T) {
	// salientContent keeps the TRAILING salientChars of pane content, so the
	// distinguishing text goes in the PREFIX with a long shared suffix: a
	// narrow (tail) window sees only the shared suffix, a wide window reaches
	// back into the differing prefix.
	suffix := strings.Repeat("status ok ", 60) // ~600 chars of shared real words
	a := sit(SituationIdle, "claude", "the build finished successfully "+suffix)
	b := sit(SituationIdle, "claude", "the build failed with three errs "+suffix)

	// Narrow window (only the shared suffix fits) → identical salient.
	if narrowA, narrowB := ComputeSignatureN(a, 20), ComputeSignatureN(b, 20); narrowA.Salient != narrowB.Salient {
		t.Fatalf("narrow window premise broken: %q vs %q", narrowA.Salient, narrowB.Salient)
	}

	// Wide window (spans back into the differing prefix) → distinct signatures.
	wideA := ComputeSignatureN(a, 800)
	wideB := ComputeSignatureN(b, 800)
	if wideA.Verdict != GuardOK || wideB.Verdict != GuardOK {
		t.Fatalf("wide window should yield valid signatures: %v / %v", wideA.Verdict, wideB.Verdict)
	}
	if wideA.Raw == wideB.Raw {
		t.Errorf("wider window must distinguish prefixes a narrow one cannot see: both = %q", wideA.Raw)
	}

	// salientChars <= 0 falls back to the default window (same as ComputeSignature).
	if got, want := ComputeSignatureN(a, 0).Raw, ComputeSignature(a).Raw; got != want {
		t.Errorf("salientChars<=0 should match the default: %q vs %q", got, want)
	}
	if DefaultPaneSalientChars != 500 {
		t.Errorf("DefaultPaneSalientChars = %d, want 500", DefaultPaneSalientChars)
	}
}
