package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/store"
	"github.com/0xGosu/herdr-auto-pilot/internal/store/libsql"
	"github.com/0xGosu/herdr-auto-pilot/internal/store/libsql/hranafake"
)

// serveHrana puts an in-process libsql server behind a real /v2/pipeline
// endpoint, so runMigrate reaches it over HTTP exactly as it reaches sqld.
func serveHrana(t *testing.T) string {
	t.Helper()
	srv, err := hranafake.New(filepath.Join(t.TempDir(), "server.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req libsql.PipelineRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		resp, err := srv.Pipeline(r.Context(), "", req)
		var sc *libsql.StreamClosedError
		if errors.As(err, &sc) {
			http.Error(w, "stream not found", http.StatusBadRequest)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(hs.Close)
	return hs.URL
}

// migratePaths is one machine's config + state, pointed at the libsql server
// and sharing a node id (the id is what makes the rows "this node's").
func migratePaths(t *testing.T, url, nodeID string) config.Paths {
	t.Helper()
	p := config.Paths{ConfigDir: t.TempDir(), StateDir: t.TempDir()}
	cfg := config.Default()
	cfg.Database.LibSQLURL = url
	if err := config.Save(p.File(), cfg); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.StateDir, store.NodeIDFile), []byte(nodeID+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestMigrateLibSQLRoundTripThroughTheCommand drives the whole command both
// ways — local file up to a libsql server, then back down into a FRESH local
// file — over HTTP. The store suite proves the copier; this proves the
// command's own wiring on the libsql side: the engine is resolved from the URL
// alone (no --from, engine still sqlite), the schema lease, and a down copy
// whose source is a libsql-backed store rather than a sqlite-shaped stand-in.
func TestMigrateLibSQLRoundTripThroughTheCommand(t *testing.T) {
	ctx := context.Background()
	url := serveHrana(t)
	const node = "0123456789abcdef"

	home := migratePaths(t, url, node)
	st, err := store.Open(home.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	d, err := st.RecordDecision(ctx, domain.DecisionRecord{Signature: "sig-1", SituationType: domain.SituationApproval,
		AgentType: "claude", ChosenAction: "yes", Source: domain.SourceOperator, CreatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertSignature(ctx, domain.SignatureState{Signature: "sig-1", SituationType: domain.SituationApproval,
		AgentType: "claude", Mode: domain.ModeShadow, DecisionFloorID: d, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendAudit(ctx, domain.AuditRecord{DecisionID: d, AgentID: "1", Trigger: "t",
		SituationType: domain.SituationApproval, Action: domain.AuditActionEscalated, Status: "escalated",
		CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	st.Close()

	var up bytes.Buffer
	if err := runMigrate(ctx, home, &up, []string{"--to", "libsql"}); err != nil {
		t.Fatalf("--to libsql: %v\n%s", err, up.String())
	}

	away := migratePaths(t, url, node)
	var down bytes.Buffer
	if err := runMigrate(ctx, away, &down, []string{"--to", "sqlite"}); err != nil {
		t.Fatalf("--to sqlite (from libsql): %v\n%s", err, down.String())
	}
	for _, want := range []string{"into the local sqlite database", "hap config set database.engine sqlite"} {
		if !strings.Contains(down.String(), want) {
			t.Errorf("report does not say %q:\n%s", want, down.String())
		}
	}

	back, err := store.Open(away.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer back.Close()
	sig, err := back.GetSignature(ctx, "sig-1")
	if err != nil || sig == nil || sig.Mode != domain.ModeShadow {
		t.Errorf("the learned rule did not come home: %+v err=%v", sig, err)
	}
	pending, err := back.PendingEscalations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].AgentID != "1" {
		t.Errorf("the escalation did not come home as this node's: %+v", pending)
	}

	// A second copy down onto the now-populated file is refused, and the
	// refusal points at the backup the command just took.
	var again bytes.Buffer
	err = runMigrate(ctx, away, &again, []string{"--to", "sqlite", "--from", "libsql"})
	if !errors.Is(err, store.ErrDestinationNotEmpty) {
		t.Fatalf("a second copy down was not refused as a non-empty destination: %v", err)
	}
	if !strings.Contains(again.String(), "backed up the destination to") {
		t.Errorf("the second run took no backup before refusing:\n%s", again.String())
	}
}
