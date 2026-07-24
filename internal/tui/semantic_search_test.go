package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
	"github.com/0xGosu/herdr-auto-pilot/internal/store"
)

// tuiSearchEmbedder embeds every query as {1,0,0} so seeded rows rank by their
// stored vector's alignment with it.
type tuiSearchEmbedder struct{}

func (tuiSearchEmbedder) EmbedText(context.Context, string) ([]float32, error) {
	return []float32{1, 0, 0}, nil
}
func (tuiSearchEmbedder) ModelID() string { return "test-model.gguf" }
func (tuiSearchEmbedder) Dims() int       { return 3 }
func (tuiSearchEmbedder) Close() error    { return nil }

// semanticModel builds a Rules-tab Model backed by a real App+store so a
// dispatched semantic search runs end to end against a fake embedder. It seeds
// two rules: "approval:hit" aligned with the query (cosine 1.0) and
// "approval:miss" orthogonal to it (cosine 0.0, below the floor).
func semanticModel(t *testing.T) Model {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	modelPath := filepath.Join(dir, "test-model.gguf")
	if err := os.WriteFile(modelPath, []byte("gguf"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath,
		[]byte(fmt.Sprintf("[embedding]\nmodel_path = %q\n", modelPath)), 0o600); err != nil {
		t.Fatal(err)
	}
	rows := []struct {
		sig string
		vec []float32
	}{
		{"approval:hit", []float32{1, 0, 0}},
		{"approval:miss", []float32{0, 1, 0}},
	}
	var sigRows []frontend.SignatureRow
	for _, r := range rows {
		if err := st.UpsertSignature(ctx, domain.SignatureState{
			Signature: r.sig, SituationType: domain.SituationApproval, AgentType: "claude",
			Mode: domain.ModeShadow, UpdatedAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
		if err := st.UpsertSignatureEmbedding(ctx, domain.SignatureEmbedding{
			Signature: r.sig, SituationType: domain.SituationApproval, AgentType: "claude",
			Model: "test-model.gguf", Dims: len(r.vec), Vector: r.vec,
			Salient: "permission:" + r.sig, CreatedAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
		sigRows = append(sigRows, frontend.SignatureRow{
			SignatureState: domain.SignatureState{
				Signature: r.sig, SituationType: domain.SituationApproval,
				AgentType: "claude", Mode: domain.ModeShadow,
			},
			TopAction: "1",
		})
	}
	app := &frontend.App{
		Store:      st,
		ConfigPath: cfgPath,
		NewEmbedder: func(config.Embedding) ports.EmbedderPort {
			return tuiSearchEmbedder{}
		},
	}
	m := Model{width: 120, height: 40, app: app, ctx: ctx, inflight: &sync.WaitGroup{}}
	upd, _ := m.Update(refreshMsg{cfg: config.Default(), signatures: sigRows})
	m = upd.(Model)
	m.tab = tabSignatures
	return m
}

// typeRunes appends each rune of s to the model one KeyMsg at a time.
func typeRunes(t *testing.T, m Model, s string) Model {
	t.Helper()
	for _, r := range s {
		upd, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = upd.(Model)
	}
	return m
}

func TestSemanticSearchHintVisibility(t *testing.T) {
	m := semanticModel(t)
	m = press(t, m, "/")

	// One word: no semantic hint (a substring filter already covers it).
	m = typeRunes(t, m, "apply")
	if m.semanticHintVisible() {
		t.Error("a single-word query must not show the semantic hint")
	}
	if strings.Contains(m.View(), "semantic search — embed") {
		t.Errorf("single-word view should not advertise semantic search:\n%s", m.View())
	}

	// Two words: the hint appears.
	m = typeRunes(t, m, " now")
	if !m.semanticHintVisible() {
		t.Fatal("a 2-word query on the Rules tab must show the semantic hint")
	}
	if !strings.Contains(m.View(), "enter: semantic search") {
		t.Errorf("2-word view should advertise the enter key:\n%s", m.View())
	}
}

func TestSemanticSearchHintOnlyOnRulesTab(t *testing.T) {
	m := semanticModel(t)
	m.tab = tabAudit
	m = press(t, m, "/")
	m = typeRunes(t, m, "two words")
	if m.semanticHintVisible() {
		t.Error("the semantic hint must not appear on non-Rules tabs")
	}
}

func TestSemanticSearchEndToEnd(t *testing.T) {
	m := semanticModel(t)
	m = press(t, m, "/")
	m = typeRunes(t, m, "approve the write")

	// Enter over a 2+-word Rules query dispatches the semantic search command.
	upd, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = upd.(Model)
	if cmd == nil {
		t.Fatal("Enter over a 2-word Rules query must dispatch a semantic search command")
	}
	if m.searching {
		t.Error("dispatching semantic search should leave search-input mode")
	}
	// Run the command and feed its result back, as the runtime would.
	msg := cmd()
	sm, ok := msg.(semanticSearchMsg)
	if !ok {
		t.Fatalf("command produced %T, want semanticSearchMsg", msg)
	}
	if sm.err != nil {
		t.Fatalf("semantic search errored: %v", sm.err)
	}
	upd, _ = m.Update(sm)
	m = upd.(Model)

	if !m.semanticActive() {
		t.Fatal("a completed semantic search should be active for its query")
	}
	vis := m.visibleSignatures()
	if len(vis) != 1 || vis[0].Signature != "approval:hit" {
		t.Fatalf("visible = %+v, want only approval:hit (miss is below the floor)", vis)
	}
	view := m.View()
	if !strings.Contains(view, "semantic: 1 match(es)") {
		t.Errorf("view should show the semantic match count:\n%s", view)
	}
	if !strings.Contains(view, "SEM") || !strings.Contains(view, "1.00") {
		t.Errorf("view should show the SEM score column:\n%s", view)
	}
	if strings.Contains(view, "approval:miss") {
		t.Errorf("the below-floor rule must not appear:\n%s", view)
	}
}

func TestSemanticSearchSingleWordEnterKeepsKeyword(t *testing.T) {
	m := semanticModel(t)
	m = press(t, m, "/")
	m = typeRunes(t, m, "hit")

	upd, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = upd.(Model)
	if cmd != nil {
		t.Error("a single-word Enter must not dispatch a semantic search")
	}
	if m.searching {
		t.Error("Enter should leave search mode")
	}
	if m.semanticActive() {
		t.Error("no semantic search should be active after a single-word Enter")
	}
	// The keyword filter still applies: "hit" matches only approval:hit.
	vis := m.visibleSignatures()
	if len(vis) != 1 || vis[0].Signature != "approval:hit" {
		t.Fatalf("keyword filter after Enter = %+v, want only approval:hit", vis)
	}
}

func TestSemanticSearchEditingRevertsToKeyword(t *testing.T) {
	m := semanticModel(t)
	// Prime an active semantic search for the exact query.
	m.query[tabSignatures] = "approve the write"
	m.sigSemantic = &semanticSigSearch{
		query: "approve the write",
		results: []frontend.SignatureSearchResult{
			{SignatureRow: frontend.SignatureRow{SignatureState: domain.SignatureState{Signature: "approval:hit"}}, Score: 1},
		},
	}
	if !m.semanticActive() {
		t.Fatal("precondition: semantic search should be active for its query")
	}
	// Re-enter search and edit the query — the semantic view must fall away.
	m = press(t, m, "/")
	m = typeRunes(t, m, "X")
	if m.semanticActive() {
		t.Error("editing the query must drop back to live keyword filtering")
	}
	// And it must NOT resurrect if the operator later re-types the exact phrase
	// as an ordinary keyword filter: the stale result set was torn down, so
	// query-string equality alone can no longer reactivate it.
	m.setQuery(tabSignatures, "approve the write")
	if m.semanticActive() {
		t.Error("re-typing a past semantic query as a keyword filter must not resurrect stale results")
	}
	if m.sigSemantic != nil {
		t.Error("editing should have torn down the semantic result set")
	}
}
