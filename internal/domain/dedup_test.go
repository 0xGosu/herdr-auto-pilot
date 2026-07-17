package domain

import "testing"

func TestNormalizeForDedup(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"trims and collapses whitespace", "  a   b\t c \n", "a b c"},
		{"collapses newlines", "line one\nline two", "line one line two"},
		{"empty stays empty", "   \n\t ", ""},
		{
			"elides claude api-retry countdown",
			"✻ Waiting for API response · will retry in 2m 2s · check your network\nAllow bash?",
			"<chrome> Allow bash?",
		},
		{
			"countdown tick reads identical",
			"✻ Waiting for API response · will retry in 2m 0s · check your network\nAllow bash?",
			"<chrome> Allow bash?",
		},
		{
			"elides elapsed/token spinner line",
			"✽ Thinking… (12s · ↑ 1.2k tokens · esc to interrupt)\nEdit main.go?",
			"<chrome> Edit main.go?",
		},
		{
			"different question is NOT collapsed",
			"Bash(rm -rf /tmp/foo)?",
			"Bash(rm -rf /tmp/foo)?",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := NormalizeForDedup(tc.in); got != tc.want {
				t.Errorf("NormalizeForDedup(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// The decisive property: two DIFFERENT commands/paths must never normalize
// equal (the MaskVolatile trap this design exists to avoid).
func TestNormalizeForDedupKeepsDistinctContentDistinct(t *testing.T) {
	pairs := [][2]string{
		{"Bash(rm -rf /tmp/foo)?", "Bash(rm -rf /tmp/bar)?"},
		{"Edit /a/b.go?", "Edit /a/c.go?"},
		{"Delete 3 files?", "Delete 4 files?"},
		// ● is Claude's SETTLED assistant-message bullet — real content, not a
		// spinner frame. Two escalations distinguished only by a ●-led line must
		// stay distinct (regression: the glyph was once wrongly elided).
		{"● I'll delete the staging bucket.\nProceed?", "● I'll delete the prod bucket.\nProceed?"},
		{"* run tests\nProceed?", "* run lint\nProceed?"},
	}
	for _, p := range pairs {
		if NormalizeForDedup(p[0]) == NormalizeForDedup(p[1]) {
			t.Errorf("distinct content collapsed: %q vs %q", p[0], p[1])
		}
	}
}

// Timer/spinner jitter on one standing screen must normalize equal.
func TestNormalizeForDedupAbsorbsChromeJitter(t *testing.T) {
	pairs := [][2]string{
		{
			"✻ Waiting for API response · will retry in 2m 2s · check your network\nAllow bash(ls)?",
			"✻ Waiting for API response · will retry in 1m 58s · check your network\nAllow bash(ls)?",
		},
		{
			"✽ Thinking… (12s · esc to interrupt)\nEdit main.go?",
			"✳ Thinking… (14s · esc to interrupt)\nEdit main.go?",
		},
	}
	for _, p := range pairs {
		if NormalizeForDedup(p[0]) != NormalizeForDedup(p[1]) {
			t.Errorf("chrome jitter not absorbed:\n a=%q -> %q\n b=%q -> %q",
				p[0], NormalizeForDedup(p[0]), p[1], NormalizeForDedup(p[1]))
		}
	}
}

func pend(sit SituationType, excerpt string) PendingEscalation {
	return PendingEscalation{SituationType: sit, PaneExcerpt: excerpt}
}

func TestDuplicatesPendingEscalation(t *testing.T) {
	const x = "Allow bash(ls)?"
	tests := []struct {
		name    string
		sit     SituationType
		excerpt string
		pending []PendingEscalation
		want    bool
	}{
		{"no pending escalations", SituationApproval, x, nil, false},
		{"exact repeat of a pending escalation", SituationApproval, x,
			[]PendingEscalation{pend(SituationApproval, x)}, true},
		{
			// The headline case: herdr re-fires with a flipped status, so the
			// re-classified situation_type differs — must still dedup.
			"same content, different situation_type still duplicates",
			SituationIdle, x,
			[]PendingEscalation{pend(SituationApproval, x)}, true,
		},
		{"whitespace/repaint jitter", SituationApproval, "a  b\nc",
			[]PendingEscalation{pend(SituationApproval, "a b c")}, true},
		{
			"different question same shape is NOT a duplicate",
			SituationApproval, "Edit /a/b.go?",
			[]PendingEscalation{pend(SituationApproval, "Edit /a/c.go?")}, false,
		},
		{"first match among several wins", SituationApproval, x,
			[]PendingEscalation{pend(SituationApproval, "something else"), pend(SituationApproval, x)}, true},
		{
			// Empty content: the read-failure path keys on "" and must match an
			// empty-excerpt pending escalation of the same type.
			"empty excerpt matches on situation type",
			SituationUnclassifiable, "",
			[]PendingEscalation{pend(SituationUnclassifiable, "")}, true,
		},
		{
			// ...but not an empty-excerpt escalation of a DIFFERENT type.
			"empty excerpt does not match a different empty type",
			SituationUnclassifiable, "",
			[]PendingEscalation{pend(SituationApproval, "")}, false,
		},
		{"empty candidate vs real content", SituationApproval, x,
			[]PendingEscalation{pend(SituationApproval, "")}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := DuplicatesPendingEscalation(tc.sit, tc.excerpt, tc.pending); got != tc.want {
				t.Errorf("DuplicatesPendingEscalation = %v, want %v", got, tc.want)
			}
		})
	}
}
