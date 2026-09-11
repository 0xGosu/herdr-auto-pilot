package domain

import "strings"

// EditOp is one step of a sequence diff.
type EditOp int

const (
	// EditKeep aligns an element of the old sequence with an equal one in the new.
	EditKeep EditOp = iota
	// EditChange pairs an old element with a new one standing in its place.
	EditChange
	// EditInsert is a new element with no counterpart in the old sequence.
	EditInsert
	// EditDelete is an old element with no counterpart in the new sequence.
	EditDelete
	// EditMove is an old element that reappears, equal, at another position.
	EditMove
)

// SequenceEdit is one aligned step. Before and After are 0-based positions in
// the old and new sequences, -1 where the step has no element on that side.
type SequenceEdit struct {
	Op     EditOp
	Before int
	After  int
	// gap numbers the stretch between two kept anchors; a delete and an insert
	// may only pair into a change inside the same stretch.
	gap int
}

// maxDiffCells bounds the LCS table. Beyond it the diff degrades to a
// positional comparison — still correct about WHICH positions differ, only
// blind to insertions shifting everything after them. No real task list comes
// near it; the bound is there so a pathological one cannot stall a writer.
const maxDiffCells = 1 << 22

// DiffSequences aligns an old sequence of n elements with a new one of m by
// longest common subsequence, using same(i, j) as equality. It returns Keep,
// Insert and Delete steps only; PairChanges and moves are the caller's
// choice, because which leftovers are "the same item, edited" depends on
// what the elements are.
func DiffSequences(n, m int, same func(i, j int) bool) []SequenceEdit {
	if n*m > maxDiffCells {
		return positionalDiff(n, m, same)
	}
	// lcs[i][j] is the LCS length of the suffixes old[i:] and new[j:].
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if same(i, j) {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else if lcs[i+1][j] >= lcs[i][j+1] {
				lcs[i][j] = lcs[i+1][j]
			} else {
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}
	var out []SequenceEdit
	gap := 0
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case same(i, j) && lcs[i][j] == lcs[i+1][j+1]+1:
			out = append(out, SequenceEdit{Op: EditKeep, Before: i, After: j})
			gap++
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			out = append(out, SequenceEdit{Op: EditDelete, Before: i, After: -1, gap: gap})
			i++
		default:
			out = append(out, SequenceEdit{Op: EditInsert, Before: -1, After: j, gap: gap})
			j++
		}
	}
	for ; i < n; i++ {
		out = append(out, SequenceEdit{Op: EditDelete, Before: i, After: -1, gap: gap})
	}
	for ; j < m; j++ {
		out = append(out, SequenceEdit{Op: EditInsert, Before: -1, After: j, gap: gap})
	}
	return out
}

func positionalDiff(n, m int, same func(i, j int) bool) []SequenceEdit {
	var out []SequenceEdit
	for k := 0; k < n || k < m; k++ {
		switch {
		case k < n && k < m && same(k, k):
			out = append(out, SequenceEdit{Op: EditKeep, Before: k, After: k})
		case k < n && k < m:
			out = append(out, SequenceEdit{Op: EditChange, Before: k, After: k})
		case k < n:
			out = append(out, SequenceEdit{Op: EditDelete, Before: k, After: -1})
		default:
			out = append(out, SequenceEdit{Op: EditInsert, Before: -1, After: k})
		}
	}
	return out
}

// PairChanges folds a delete and an insert standing in the same stretch
// between two kept anchors into one change, first with first, so an edited
// element reads as edited rather than as removed-and-added. Leftovers stay
// inserts or deletes.
func PairChanges(edits []SequenceEdit) []SequenceEdit {
	byGap := map[int][]int{} // gap → indexes of its unpaired deletes, in order
	for k, e := range edits {
		if e.Op == EditDelete {
			byGap[e.gap] = append(byGap[e.gap], k)
		}
	}
	drop := map[int]bool{}
	for k, e := range edits {
		if e.Op != EditInsert {
			continue
		}
		dels := byGap[e.gap]
		if len(dels) == 0 {
			continue
		}
		d := dels[0]
		byGap[e.gap] = dels[1:]
		edits[k] = SequenceEdit{Op: EditChange, Before: edits[d].Before, After: e.After, gap: e.gap}
		drop[d] = true
	}
	out := edits[:0:0]
	for k, e := range edits {
		if !drop[k] {
			out = append(out, e)
		}
	}
	return out
}

// ChecklistChange is one item-level difference between two parses of a task
// list. Index is the item's 1-based number in the list it lives in afterwards
// (the BEFORE list for a delete); From is the old number of a moved item.
type ChecklistChange struct {
	Op    EditOp
	Index int
	From  int
	Mark  string
}

// DiffChecklist reports what changed, item by item, between two parses of a
// task list: items created, deleted, edited or re-marked (EditChange), and
// moved. It never reports an item that did not change.
//
// Items are aligned by TEXT, the same identity the reservation ledger uses. A
// delete and insert of identical text become a move; what is left in the same
// stretch pairs into an edit.
func DiffChecklist(before, after []ChecklistItem) []ChecklistChange {
	edits := DiffSequences(len(before), len(after), func(i, j int) bool {
		return before[i].Text == after[j].Text
	})
	edits = pairMoves(edits, before, after)
	edits = PairChanges(edits)
	var out []ChecklistChange
	for _, e := range edits {
		switch e.Op {
		case EditKeep:
			if checklistItemChanged(before[e.Before], after[e.After]) {
				out = append(out, ChecklistChange{Op: EditChange, Index: after[e.After].Index, Mark: after[e.After].Mark})
			}
		case EditChange:
			out = append(out, ChecklistChange{Op: EditChange, Index: after[e.After].Index, Mark: after[e.After].Mark})
		case EditMove:
			out = append(out, ChecklistChange{Op: EditMove, Index: after[e.After].Index,
				From: before[e.Before].Index, Mark: after[e.After].Mark})
		case EditInsert:
			out = append(out, ChecklistChange{Op: EditInsert, Index: after[e.After].Index, Mark: after[e.After].Mark})
		case EditDelete:
			out = append(out, ChecklistChange{Op: EditDelete, Index: before[e.Before].Index})
		}
	}
	return out
}

// pairMoves turns a delete and an insert of the same text into one move. It
// runs before PairChanges so a moved item is never misread as two unrelated
// items edited into each other.
func pairMoves(edits []SequenceEdit, before, after []ChecklistItem) []SequenceEdit {
	dels := map[string][]int{} // text → indexes of unpaired deletes
	for k, e := range edits {
		if e.Op == EditDelete {
			t := before[e.Before].Text
			dels[t] = append(dels[t], k)
		}
	}
	drop := map[int]bool{}
	for k, e := range edits {
		if e.Op != EditInsert {
			continue
		}
		t := after[e.After].Text
		if len(dels[t]) == 0 {
			continue
		}
		d := dels[t][0]
		dels[t] = dels[t][1:]
		edits[k] = SequenceEdit{Op: EditMove, Before: edits[d].Before, After: e.After}
		drop[d] = true
	}
	out := edits[:0:0]
	for k, e := range edits {
		if !drop[k] {
			out = append(out, e)
		}
	}
	return out
}

func checklistItemChanged(a, b ChecklistItem) bool {
	return a.Mark != b.Mark || strings.Join(a.Detail, "\n") != strings.Join(b.Detail, "\n")
}
