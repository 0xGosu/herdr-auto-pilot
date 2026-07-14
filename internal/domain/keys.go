package domain

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
