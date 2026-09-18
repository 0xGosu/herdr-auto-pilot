package config

import (
	"strings"
	"testing"
	"time"
)

func TestLibSQLDatabaseSettings(t *testing.T) {
	t.Setenv(LibSQLAuthTokenEnv, "")
	d := Database{Engine: EngineLibSQL, LibSQLURL: "libsql://db.example.dev"}
	if !d.IsLibSQL() || !d.IsShared() || d.IsTurso() {
		t.Fatalf("libsql predicates wrong: libsql=%v shared=%v turso=%v", d.IsLibSQL(), d.IsShared(), d.IsTurso())
	}
	if got := d.SyncInterval(); got != DefaultLibSQLPollIntervalSeconds*time.Second {
		t.Errorf("default poll = %s", got)
	}
	d.LibSQLPollIntervalSeconds = 1
	if got := d.SyncInterval(); got != MinLibSQLPollIntervalSeconds*time.Second {
		t.Errorf("floored poll = %s", got)
	}
	// The turso interval must not leak into libsql's, nor the reverse.
	d.LibSQLPollIntervalSeconds, d.TursoSyncIntervalSeconds = 30, 60
	if got := d.SyncInterval(); got != 30*time.Second {
		t.Errorf("libsql poll = %s, want its own key", got)
	}

	if d.LibSQLToken() != "" {
		t.Fatal("token from nowhere")
	}
	t.Setenv(LibSQLAuthTokenEnv, " from-env ")
	if got := d.LibSQLToken(); got != "from-env" {
		t.Errorf("env token = %q", got)
	}
	d.LibSQLAuthToken = "from-config"
	if got := d.LibSQLToken(); got != "from-config" {
		t.Errorf("config token = %q, want it ahead of the env", got)
	}
}

func TestValidateLibSQLDatabase(t *testing.T) {
	cfg := Config{Database: Database{Engine: EngineLibSQL}}
	if err := ValidateDatabase(cfg); err == nil || !strings.Contains(err.Error(), "libsql_url") {
		t.Fatalf("missing URL: err = %v", err)
	}
	cfg.Database.LibSQLURL = "https://db.example.dev"
	// No token is legal: a self-hosted sqld may run without auth.
	if err := ValidateDatabase(cfg); err != nil {
		t.Fatalf("valid libsql config refused: %v", err)
	}
	// A pause is ignored under libsql, never a reason to refuse starting —
	// refusing would leave the herd unmonitored over a key that changes
	// nothing.
	cfg.Database.TursoSyncPaused = true
	if err := ValidateDatabase(cfg); err != nil {
		t.Fatalf("turso_sync_paused refused a libsql start: %v", err)
	}
	if cfg.Database.SyncPausedEffective() {
		t.Error("the pause is in effect under libsql")
	}
	turso := Database{Engine: EngineTurso, TursoSyncPaused: true}
	if !turso.SyncPausedEffective() {
		t.Error("control: the pause is not in effect under turso")
	}
}

// TestLibSQLReplicaDatabaseSettings: the replica engine shares the libsql
// server's three keys, and — unlike libsql, like turso — honours the pause,
// because its local replica serves while the round trips are skipped.
func TestLibSQLReplicaDatabaseSettings(t *testing.T) {
	cfg := Config{Database: Database{Engine: EngineLibSQLReplica}}
	d := cfg.Database
	if !d.IsLibSQLReplica() || !d.UsesLibSQLServer() || !d.IsShared() || d.IsLibSQL() || d.IsTurso() {
		t.Fatalf("predicates wrong: replica=%v server=%v shared=%v libsql=%v turso=%v",
			d.IsLibSQLReplica(), d.UsesLibSQLServer(), d.IsShared(), d.IsLibSQL(), d.IsTurso())
	}
	if err := ValidateDatabase(cfg); err == nil || !strings.Contains(err.Error(), "libsql_url") {
		t.Fatalf("missing URL: err = %v", err)
	}
	cfg.Database.LibSQLURL = "http://sqld.lan:8080"
	cfg.Database.LibSQLPollIntervalSeconds, cfg.Database.TursoSyncIntervalSeconds = 30, 60
	if err := ValidateDatabase(cfg); err != nil {
		t.Fatalf("valid replica config refused: %v", err)
	}
	if got := cfg.Database.SyncInterval(); got != 30*time.Second {
		t.Errorf("poll = %s, want libsql_poll_interval_seconds", got)
	}
	cfg.Database.TursoSyncPaused = true
	if !cfg.Database.SyncPausedEffective() {
		t.Error("the pause is not in effect under libsql_replica")
	}
	found := false
	for _, e := range ValidDatabaseEngines {
		found = found || e == EngineLibSQLReplica
	}
	if !found {
		t.Error("libsql_replica is not a settable engine")
	}
}
