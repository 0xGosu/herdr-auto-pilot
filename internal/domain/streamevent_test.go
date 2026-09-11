package domain

import (
	"testing"
	"time"
)

func TestStreamEventLine(t *testing.T) {
	at := time.Date(2026, 9, 11, 10, 2, 3, 0, time.FixedZone("x", 3600))
	cases := []struct {
		name string
		ev   StreamEvent
		want string
	}{
		{
			name: "ids only",
			ev: StreamEvent{Seq: 1024, At: at, Kind: StreamEscalation, Author: "daemon",
				Fields: []StreamField{StreamInt("id", 5812), StreamStr("agent", "calm-pika"), StreamStr("type", "approval")}},
			want: "1024 2026-09-11T09:02:03Z escalation id=5812 agent=calm-pika type=approval by=daemon",
		},
		{
			name: "no fields",
			ev:   StreamEvent{Seq: 7, At: at, Kind: StreamPauseOn, Author: "operator"},
			want: "7 2026-09-11T09:02:03Z pause.on by=operator",
		},
		{
			name: "locator with a space is quoted",
			ev: StreamEvent{Seq: 8, At: at, Kind: StreamTaskUpdated, Author: "operator",
				Fields: []StreamField{StreamStr("list", "/home/me/my tasks.md"), StreamInt("index", 3)}},
			want: `8 2026-09-11T09:02:03Z task.updated list="/home/me/my tasks.md" index=3 by=operator`,
		},
		{
			name: "equals sign and empty value are quoted",
			ev: StreamEvent{Seq: 9, At: at, Kind: StreamConfigChanged, Author: "",
				Fields: []StreamField{StreamStr("a", "x=y"), StreamStr("b", "")}},
			want: `9 2026-09-11T09:02:03Z config.changed a="x=y" b="" by=unknown`,
		},
		{
			name: "rendered wins over fields",
			ev: StreamEvent{Seq: 10, At: at, Kind: StreamFSPOff, Author: "daemon", Rendered: "reason=ceiling",
				Fields: []StreamField{StreamStr("ignored", "1")}},
			want: "10 2026-09-11T09:02:03Z fsp.off reason=ceiling by=daemon",
		},
		{
			name: "db locator and signed delta stay bare",
			ev: StreamEvent{Seq: 11, At: at, Kind: StreamRuleStreak, Author: "operator",
				Fields: []StreamField{StreamStr("list", "db://n1/calm-pika"), StreamStr("delta", "+1")}},
			want: "11 2026-09-11T09:02:03Z rule.streak list=db://n1/calm-pika delta=+1 by=operator",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.ev.Line(); got != tc.want {
				t.Fatalf("Line() =\n  %s\nwant\n  %s", got, tc.want)
			}
		})
	}
}

func TestDiffChecklist(t *testing.T) {
	parse := ParseChecklist
	cases := []struct {
		name          string
		before, after string
		want          []ChecklistChange
	}{
		{name: "no-op", before: "- [ ] a\n- [ ] b\n", after: "- [ ] a\n- [ ] b\n"},
		{
			name: "append", before: "- [ ] a\n", after: "- [ ] a\n- [ ] b\n",
			want: []ChecklistChange{{Op: EditInsert, Index: 2, Mark: " "}},
		},
		{
			name: "delete middle", before: "- [ ] a\n- [ ] b\n- [ ] c\n", after: "- [ ] a\n- [ ] c\n",
			want: []ChecklistChange{{Op: EditDelete, Index: 2}},
		},
		{
			name: "mark done", before: "- [ ] a\n- [ ] b\n", after: "- [ ] a\n- [x] b\n",
			want: []ChecklistChange{{Op: EditChange, Index: 2, Mark: "x"}},
		},
		{
			name: "reserve in-progress", before: "- [ ] a\n", after: "- [-] a\n",
			want: []ChecklistChange{{Op: EditChange, Index: 1, Mark: "-"}},
		},
		{
			name: "edit text in place", before: "- [ ] a\n- [ ] b\n- [ ] c\n", after: "- [ ] a\n- [ ] B!\n- [ ] c\n",
			want: []ChecklistChange{{Op: EditChange, Index: 2, Mark: " "}},
		},
		{
			name: "move to front", before: "- [ ] a\n- [ ] b\n- [ ] c\n", after: "- [ ] c\n- [ ] a\n- [ ] b\n",
			want: []ChecklistChange{{Op: EditMove, Index: 1, From: 3, Mark: " "}},
		},
		{
			name: "detail change", before: "- [ ] a\n", after: "- [ ] a\n  - note\n",
			want: []ChecklistChange{{Op: EditChange, Index: 1, Mark: " "}},
		},
		{
			name: "header lines ignored", before: "# Tasks\n- [ ] a\n", after: "# Tasks for x\n\n- [ ] a\n",
		},
		{
			name: "create from empty", before: "", after: "# T\n- [ ] a\n- [ ] b\n",
			want: []ChecklistChange{{Op: EditInsert, Index: 1, Mark: " "}, {Op: EditInsert, Index: 2, Mark: " "}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DiffChecklist(parse(tc.before), parse(tc.after))
			if len(got) != len(tc.want) {
				t.Fatalf("DiffChecklist = %+v, want %+v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("DiffChecklist[%d] = %+v, want %+v (all: %+v)", i, got[i], tc.want[i], got)
				}
			}
		})
	}
}

func TestDiffSequencesPositionalFallback(t *testing.T) {
	// Past the LCS bound the diff is positional: still exact about which
	// positions differ.
	n := 1 << 12
	a := make([]int, n)
	b := make([]int, n+1)
	for i := range a {
		a[i] = i
		b[i] = i
	}
	a[5] = -1
	b[n] = 99
	edits := DiffSequences(len(a), len(b), func(i, j int) bool { return a[i] == b[j] })
	var changes, inserts int
	for _, e := range edits {
		switch e.Op {
		case EditChange:
			changes++
		case EditInsert:
			inserts++
		case EditDelete, EditMove:
			t.Fatalf("unexpected op %v", e.Op)
		}
	}
	if changes != 1 || inserts != 1 {
		t.Fatalf("changes=%d inserts=%d, want 1 and 1", changes, inserts)
	}
}
