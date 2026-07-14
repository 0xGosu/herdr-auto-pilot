package domain

// MCQ multi-tab answer protocol constants, shared by the daemon (sweep +
// autonomous delivery) and the operator-confirm frontend so the two delivery
// paths can never diverge on how they navigate a form.
const (
	// MCQAdvanceKey advances past a MULTI-SELECT tab after its toggles. A
	// multi-select tab does not auto-advance on a digit press; a single-select
	// tab does, so it needs no explicit advance. Right-arrow matches the
	// capture sweep's proven tab navigation.
	MCQAdvanceKey = "right"
	// MCQResetKeys is a fixed Left-arrow count larger than any real form's tab
	// count, so a reset burst lands on the first question regardless of size.
	MCQResetKeys = 10
)

// MultiTabKeys builds the ordered keystrokes that answer a multi-tab MCQ form,
// EXCLUDING the leading reset burst (the caller owns resetting focus to the
// first tab). For each tab it presses the chosen option digits in order; then,
// for a MULTI-SELECT tab, it presses an explicit advanceKey to move on —
// toggling a checkbox does NOT auto-advance the form. A single-select tab
// emits only its one digit, which both selects and auto-advances.
//
// CRITICAL: the advance decision keys off tabMultiSelect[i], NOT the number of
// digits chosen. A multi-select tab where the answer picks a single option is
// still multi-select and still will not auto-advance, so it still needs the
// explicit advanceKey. The final Submit tab is single-select (its digit
// submits), so it must not be marked multi-select.
func MultiTabKeys(groups [][]string, tabMultiSelect []bool, advanceKey string) []string {
	keys := make([]string, 0, len(groups))
	for i, g := range groups {
		keys = append(keys, g...)
		if i < len(tabMultiSelect) && tabMultiSelect[i] {
			keys = append(keys, advanceKey)
		}
	}
	return keys
}
