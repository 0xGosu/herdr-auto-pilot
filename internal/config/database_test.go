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
	// libsql keeps a local replica, so the pause applies exactly as under
	// turso: the replica serves while the round trips are skipped.
	cfg.Database.TursoSyncPaused = true
	if err := ValidateDatabase(cfg); err != nil {
		t.Fatalf("turso_sync_paused refused a libsql start: %v", err)
	}
	if !cfg.Database.SyncPausedEffective() {
		t.Error("the pause is not in effect under libsql")
	}
	if (Database{Engine: EngineSQLite, TursoSyncPaused: true}).SyncPausedEffective() {
		t.Error("the pause is in effect under sqlite, which has nothing to sync")
	}
	turso := Database{Engine: EngineTurso, TursoSyncPaused: true}
	if !turso.SyncPausedEffective() {
		t.Error("control: the pause is not in effect under turso")
	}
}
