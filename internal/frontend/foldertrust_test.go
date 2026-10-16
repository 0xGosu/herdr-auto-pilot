package frontend_test

import (
	"context"
	"strings"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// Confirming a folder-trust escalation refuses — there is no one-key answer to
// trusting a folder — and names the two resolves that do answer it, so the
// operator is never left guessing the row labels.
func TestConfirmFolderTrustNamesTheRowsToResolveWith(t *testing.T) {
	app, st := testApp(t)
	id := seedEmptySuggestionEscalation(t, st, domain.ReasonFolderTrust)

	err := app.Confirm(context.Background(), id, true)
	if err == nil {
		t.Fatal("confirm succeeded on a folder-trust escalation")
	}
	for _, want := range []string{`--action "Yes, I trust this folder" --send`, `--action "No, exit" --send`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %s: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "\n") {
		t.Errorf("the TUI status area takes one line, got %q", err.Error())
	}
}
