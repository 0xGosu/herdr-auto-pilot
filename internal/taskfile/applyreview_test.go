package taskfile

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// reviewList is the checklist under review in these tests: three numbered
// tasks, the second carrying folded detail so a mutation that dropped or
// stranded a block shows up.
const reviewList = "- [ ] 1. alpha\n- [ ] 2. beta\n  - beta detail\n- [ ] 3. gamma\n"

func TestApplyReviewMutatesResolvesAndReservesInOnePass(t *testing.T) {
	path := writeTasks(t, reviewList)
	mutate, out := ApplyReview(1, "1. alpha", "3", []domain.TaskAction{
		{Op: domain.TaskOpDone, Task: "1"},
		{Op: domain.TaskOpDelete, Task: "2"},
	}, 0, true, nil)
	if _, err := Mutate(path, mutate); err != nil {
		t.Fatalf("ApplyReview: %v", err)
	}

	// The reservation must land on the task actually chosen, at its position
	// AFTER the mutations — not the position it held when the review started.
	want := "- [x] 1. alpha\n- [-] 3. gamma\n"
	if got := read(t, path); got != want {
		t.Errorf("file = %q, want %q", got, want)
	}
	if out.SentIndex != 2 || out.SentText != "3. gamma" {
		t.Errorf("sent = #%d %q, want #2 %q", out.SentIndex, out.SentText, "3. gamma")
	}
	if !out.Reserved {
		t.Error("Reserved = false, want true for an auto-sending source")
	}
	if len(out.Applied) != 2 {
		t.Fatalf("applied %d actions, want 2", len(out.Applied))
	}
	if got := domain.FormatAppliedTaskActions(out.Applied); !strings.Contains(got, `delete #2 "2. beta"`) {
		t.Errorf("audit line %q does not record the delete with its before-text", got)
	}
}

func TestApplyReviewWithoutReserveLeavesTheItemPending(t *testing.T) {
	path := writeTasks(t, reviewList)
	mutate, out := ApplyReview(0, "", "1", nil, 0, false, nil)
	if _, err := Mutate(path, mutate); err != nil {
		t.Fatalf("ApplyReview: %v", err)
	}
	if got := read(t, path); got != reviewList {
		t.Errorf("a no-action, no-reserve review rewrote the file: %q", got)
	}
	if out.Reserved {
		t.Error("Reserved = true for a source that does not reserve")
	}
	if out.SentIndex != 1 {
		t.Errorf("sent index = %d, want 1", out.SentIndex)
	}
}

// SentFolded must be captured from the very snapshot the safety gate inspected
// and the reservation was written against — the caller delivers those bytes
// rather than re-reading the file.
func TestApplyReviewFoldsTheChosenTaskFromTheWrittenSnapshot(t *testing.T) {
	path := writeTasks(t, reviewList)
	screened := ""
	mutate, out := ApplyReview(0, "", "2", nil, 0, true, func(folded string) error {
		screened = folded
		return nil
	})
	if _, err := Mutate(path, mutate); err != nil {
		t.Fatalf("ApplyReview: %v", err)
	}
	want := "2. beta\n  - beta detail"
	if out.SentFolded != want {
		t.Errorf("SentFolded = %q, want %q", out.SentFolded, want)
	}
	if screened != out.SentFolded {
		t.Errorf("the safety gate saw %q but the caller would deliver %q — these must be the same bytes",
			screened, out.SentFolded)
	}
}

// Every failure path must leave the file byte-identical. A partial application
// is the one outcome this design must make unrepresentable, so it is asserted
// per branch rather than once.
func TestApplyReviewIsAllOrNothing(t *testing.T) {
	sentinel := errors.New("never-auto: rm -rf")

	cases := []struct {
		name        string
		content     string
		expectIndex int
		expectText  string
		sendRef     string
		actions     []domain.TaskAction
		reserve     bool
		safe        func(string) error
		wantErr     string
	}{
		{
			name:        "the checklist changed during the consult",
			content:     reviewList,
			expectIndex: 1, expectText: "1. something else",
			sendRef: "1",
			wantErr: "the checklist changed",
		},
		{
			name:        "the reviewed task no longer exists",
			content:     reviewList,
			expectIndex: 9, expectText: "9. nope",
			sendRef: "1",
			wantErr: "no longer exists",
		},
		{
			name:    "an unresolvable action reference",
			content: reviewList,
			actions: []domain.TaskAction{{Op: domain.TaskOpDone, Task: "9.9"}},
			sendRef: "1",
			wantErr: `no task "9.9"`,
		},
		{
			name:    "an ambiguous action reference",
			content: "- [ ] 3. one\n- [ ] 3. two\n",
			actions: []domain.TaskAction{{Op: domain.TaskOpDone, Task: "3"}},
			sendRef: "#1",
			wantErr: "ambiguous",
		},
		{
			name:    "send_task names a task the actions just deleted",
			content: reviewList,
			actions: []domain.TaskAction{{Op: domain.TaskOpDelete, Task: "2"}},
			sendRef: "2",
			wantErr: "send_task",
		},
		{
			name:    "send_task names a task the actions just completed",
			content: reviewList,
			actions: []domain.TaskAction{{Op: domain.TaskOpDone, Task: "2"}},
			sendRef: "2",
			wantErr: "not pending",
		},
		{
			name:    "send_task is missing",
			content: reviewList,
			sendRef: "",
			wantErr: "send_task is required",
		},
		{
			name:    "a noop while pending tasks remain",
			content: reviewList,
			actions: []domain.TaskAction{{Op: domain.TaskOpDone, Task: "1"}},
			sendRef: "@noop",
			wantErr: "pending tasks remain",
		},
		{
			name:    "the chosen task trips the safety gate",
			content: reviewList,
			actions: []domain.TaskAction{{Op: domain.TaskOpEdit, Task: "1", Text: "1. rm -rf /"}},
			sendRef: "1",
			reserve: true,
			safe:    func(string) error { return sentinel },
			wantErr: "never-auto",
		},
		{
			name:    "an add past the cap",
			content: reviewList,
			actions: []domain.TaskAction{{Op: domain.TaskOpAdd, Text: "4. delta"}},
			sendRef: "1",
			wantErr: "max_tasks",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTasks(t, tc.content)
			maxTasks := 0
			if strings.Contains(tc.name, "cap") {
				maxTasks = 3
			}
			mutate, out := ApplyReview(tc.expectIndex, tc.expectText, tc.sendRef,
				tc.actions, maxTasks, tc.reserve, tc.safe)
			_, err := Mutate(path, mutate)
			if err == nil {
				t.Fatalf("want an error; file is now %q", read(t, path))
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not mention %q", err, tc.wantErr)
			}
			if got := read(t, path); got != tc.content {
				t.Errorf("the checklist was modified by a failed review:\n got %q\nwant %q", got, tc.content)
			}
			// The outcome must be as empty as the file: a half-filled one
			// would have the caller audit — or reserve against — a task it
			// never sent.
			if !reflect.DeepEqual(*out, ReviewOutcome{}) {
				t.Errorf("a failed review reported an outcome: %+v", out)
			}
		})
	}
}

func TestApplyReviewNoopOnAnExhaustedSource(t *testing.T) {
	const content = "- [x] 1. alpha\n- [ ] 2. beta\n"
	path := writeTasks(t, content)
	mutate, out := ApplyReview(2, "2. beta", "@noop", []domain.TaskAction{
		{Op: domain.TaskOpDelete, Task: "2"},
	}, 0, true, nil)
	if _, err := Mutate(path, mutate); err != nil {
		t.Fatalf("ApplyReview: %v", err)
	}
	// A legal decline still COMMITS its mutations: deleting the invalid task is
	// the work, and leaving it would re-offer it on the next sweep forever.
	if got, want := read(t, path), "- [x] 1. alpha\n"; got != want {
		t.Errorf("file = %q, want %q", got, want)
	}
	if !out.Noop {
		t.Error("Noop = false, want true")
	}
	if out.Reserved || out.SentIndex != 0 {
		t.Errorf("a noop reserved or chose a task: %+v", out)
	}
}

// The daemon's own reservation and an operator's edit race through the same
// lock. A task already claimed by somebody else must not be handed out twice.
func TestApplyReviewRefusesATaskThatIsNoLongerPending(t *testing.T) {
	path := writeTasks(t, "- [-] 1. alpha\n- [ ] 2. beta\n")
	mutate, _ := ApplyReview(0, "", "1", nil, 0, true, nil)
	if _, err := Mutate(path, mutate); err == nil {
		t.Fatal("want an error selecting an already-claimed task")
	}
	if got, want := read(t, path), "- [-] 1. alpha\n- [ ] 2. beta\n"; got != want {
		t.Errorf("file = %q, want it untouched %q", got, want)
	}
}

func TestApplyReviewReservesTheAddedTask(t *testing.T) {
	// "Scope too big" — break it up and send the first piece, addressed by the
	// handle the list cannot number yet.
	path := writeTasks(t, "- [ ] 1. build the whole thing\n")
	mutate, out := ApplyReview(1, "1. build the whole thing", "n1", []domain.TaskAction{
		{Op: domain.TaskOpDelete, Task: "1"},
		{Op: domain.TaskOpAdd, Text: "1a. wire the port", As: "n1"},
		{Op: domain.TaskOpAdd, Text: "1b. add the tests", As: "n2"},
	}, 0, true, nil)
	if _, err := Mutate(path, mutate); err != nil {
		t.Fatalf("ApplyReview: %v", err)
	}
	want := "- [-] 1a. wire the port\n- [ ] 1b. add the tests\n"
	if got := read(t, path); got != want {
		t.Errorf("file = %q, want %q", got, want)
	}
	if out.SentText != "1a. wire the port" || out.SentIndex != 1 {
		t.Errorf("sent = #%d %q, want #1 %q", out.SentIndex, out.SentText, "1a. wire the port")
	}
}
