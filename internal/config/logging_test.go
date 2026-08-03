package config

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoggingLevelParsing(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"info", slog.LevelInfo},
		{"warn", slog.LevelWarn},
		{"warning", slog.LevelWarn},
		{"error", slog.LevelError},
		{"WARN", slog.LevelWarn},
		{"  warn  ", slog.LevelWarn},
		{"", slog.LevelInfo},
		{"nonsense", slog.LevelInfo}, // a typo must never silence the log
	} {
		if got := (Logging{Level: tc.in}).SlogLevel(); got != tc.want {
			t.Errorf("level %q = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestAuditExcerptRetentionExplicitZeroKeepsNothing: 0 reads the way it looks —
// retain for zero days — so it prunes everything rather than disabling the
// sweep. This is why the field is a pointer: fillZeroes must not mistake the
// operator's 0 for "unset" and substitute the default.
func TestAuditExcerptRetentionExplicitZeroKeepsNothing(t *testing.T) {
	cfg, err := Load(writeConfig(t, "[logging]\naudit_excerpt_retention_days = 0\n"))
	if err != nil {
		t.Fatal(err)
	}
	window, enabled := cfg.Logging.AuditExcerptRetention()
	if !enabled {
		t.Fatal("0 must RUN the sweep (keeping nothing), not turn it off")
	}
	if window != 0 {
		t.Errorf("window = %v, want 0 (keep nothing)", window)
	}
}

// TestAuditExcerptRetentionNegativeDisables: negative is the off switch,
// because 0 is taken. Same convention as journal_size_limit = -1.
func TestAuditExcerptRetentionNegativeDisables(t *testing.T) {
	for _, body := range []string{
		"[logging]\naudit_excerpt_retention_days = -1\n",
		"[logging]\naudit_excerpt_retention_days = -30\n",
	} {
		cfg, err := Load(writeConfig(t, body))
		if err != nil {
			t.Fatal(err)
		}
		if _, enabled := cfg.Logging.AuditExcerptRetention(); enabled {
			t.Errorf("%q must turn the sweep off", body)
		}
	}
}

func TestAuditExcerptRetentionAbsentTakesDefault(t *testing.T) {
	cfg, err := Load(writeConfig(t, "[logging]\nlevel = \"warn\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	window, enabled := cfg.Logging.AuditExcerptRetention()
	if !enabled {
		t.Fatal("an absent key must take the default, not disable the sweep")
	}
	if want := time.Duration(DefaultAuditExcerptRetentionDays) * 24 * time.Hour; window != want {
		t.Errorf("window = %v, want %v", window, want)
	}
}

func TestAuditExcerptRetentionExplicitValue(t *testing.T) {
	cfg, err := Load(writeConfig(t, "[logging]\naudit_excerpt_retention_days = 3\n"))
	if err != nil {
		t.Fatal(err)
	}
	window, enabled := cfg.Logging.AuditExcerptRetention()
	if !enabled || window != 72*time.Hour {
		t.Errorf("window = %v (enabled=%v), want 72h", window, enabled)
	}
}

func TestLoggingFillZeroesNormalizesLevelAndSize(t *testing.T) {
	cfg, err := Load(writeConfig(t, "[logging]\nlevel = \"nonsense\"\nmax_size_mb = 0\n"))
	if err != nil {
		t.Fatal(err)
	}
	d := Default()
	if cfg.Logging.Level != d.Logging.Level {
		t.Errorf("level = %q, want the default %q", cfg.Logging.Level, d.Logging.Level)
	}
	if cfg.Logging.MaxSizeMB != d.Logging.MaxSizeMB {
		t.Errorf("max_size_mb = %d, want the default %d", cfg.Logging.MaxSizeMB, d.Logging.MaxSizeMB)
	}
}

// The default cap is what an untouched install actually reserves on disk
// (twice this, counting the ".old" sibling), so it is worth pinning.
func TestDefaultLogCapIsModest(t *testing.T) {
	if mb := Default().Logging.MaxSizeMB; mb <= 0 || mb > 32 {
		t.Errorf("default max_size_mb = %d; a diagnostic file should not reserve more", mb)
	}
}
