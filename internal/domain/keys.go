package domain

// MCQ multi-tab answer protocol constants, shared by the daemon (sweep +
// autonomous delivery) and the operator-confirm frontend so the two delivery
// paths can never diverge on how they navigate a form.
const (
	// MCQAdvanceKey advances past a MULTI-SELECT tab after its toggles. A
	// multi-select tab does not auto-advance on a digit press. Right-arrow
	// matches the capture sweep's proven tab navigation.
	MCQAdvanceKey = "right"
	// MCQResetKeys is a fixed Left-arrow count larger than any real form's tab
	// count, so a reset burst lands on the first question regardless of size.
	MCQResetKeys = 10
)

// NOTE: there is deliberately no pure "plan the whole keystroke series" helper
// here. Claude binds digits differently per tab — a plain option commits on the
// digit, a preview option only moves the caret and needs Enter (see
// ClaudeTabForm) — so the keys a tab needs are not knowable until the form is
// read back between presses. Planning them all up front is exactly what made
// delivery a silent no-op on preview forms. The answer protocol therefore lives
// in internal/mcqdeliver, which verifies each keystroke against the live pane.
