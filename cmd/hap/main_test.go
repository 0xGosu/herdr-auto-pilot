package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	skilldoc "github.com/0xGosu/herdr-auto-pilot"
	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/store/sqlbridge"
)

// TestEmbeddingDigestStableAndChangeSensitive guards the crash-loop breaker's
// core invariant: the [embedding] digest must be identical for the same config
// loaded twice (so a plain restart keeps a latch), yet differ when the operator
// edits the section (so the latch clears / semantic matching re-enables).
func TestEmbeddingDigestStableAndChangeSensitive(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")

	write := func(body string) config.Config {
		t.Helper()
		if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		c, err := config.Load(cfgPath)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}

	c1 := write("[embedding]\nsimilarity_threshold = 0.9\n")
	c2 := write("[embedding]\nsimilarity_threshold = 0.9\n")
	if embeddingDigest(c1) != embeddingDigest(c2) {
		t.Fatalf("digest must be stable across identical loads (both go through fillZeroes): %q vs %q",
			embeddingDigest(c1), embeddingDigest(c2))
	}

	c3 := write("[embedding]\nsimilarity_threshold = 0.8\n")
	if embeddingDigest(c3) == embeddingDigest(c1) {
		t.Error("changing the [embedding] section must change the digest so a latch clears on operator edit")
	}

	// Disabling embedding is also a change (relevant since the operator may
	// toggle it to escape a crash-loop).
	c4 := write("[embedding]\ndisabled = true\n")
	if embeddingDigest(c4) == embeddingDigest(c1) {
		t.Error("toggling embedding.disabled must change the digest")
	}
}

// TestReplaceOnlyBowsOutOnlyWhenNothingIsRunning pins both halves of the flag's
// contract. install.sh calls it during the plugin BUILD step, so it must not
// bring a daemon up on a fresh install or in CI — and it must still replace a
// running one, because the gate sitting in EnsureFresh's start callback instead
// would suppress the restart AFTER the stale daemon was stopped, leaving the
// herd with no monitor at all.
func TestReplaceOnlyBowsOutOnlyWhenNothingIsRunning(t *testing.T) {
	tests := []struct {
		name        string
		replaceOnly bool
		running     bool
		wantBowOut  bool
	}{
		{name: "replace-only with nothing running does nothing", replaceOnly: true, wantBowOut: true},
		{name: "replace-only with a daemon running proceeds", replaceOnly: true, running: true},
		{name: "plain ensure with nothing running starts one", running: false},
		{name: "plain ensure with a daemon running proceeds", running: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := replaceOnlyBowsOut(tt.replaceOnly, func() bool { return tt.running })
			if got != tt.wantBowOut {
				t.Fatalf("replaceOnlyBowsOut(%v, running=%v) = %v, want %v",
					tt.replaceOnly, tt.running, got, tt.wantBowOut)
			}
		})
	}
}

// TestDaemonFlagParsingRejectsUnknownAndOrderIndependent guards against the
// worst failure mode of positional parsing: a flag hap does not recognize (or
// one in an unexpected order) silently falling through to the FOREGROUND
// daemon, which takes the lock and blocks forever — inside install.sh that
// would hang `herdr plugin install`.
func TestDaemonFlagParsingRejectsUnknownAndOrderIndependent(t *testing.T) {
	paths := config.Paths{ConfigDir: t.TempDir(), StateDir: t.TempDir()}
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{name: "unknown flag", args: []string{"--ensure", "--nope"}, wantErr: "unknown flag"},
		{name: "unknown flag alone", args: []string{"--replace-onlyy"}, wantErr: "unknown flag"},
		{name: "replace-only without ensure", args: []string{"--replace-only"}, wantErr: "only applies"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := runDaemon(context.Background(), paths, tt.args)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("runDaemon(%v) = %v, want an error containing %q", tt.args, err, tt.wantErr)
			}
		})
	}

	// Reversed order must be accepted and must NOT start a daemon (nothing is
	// running, and --replace-only is in effect).
	if err := runDaemon(context.Background(), paths, []string{"--replace-only", "--ensure"}); err != nil {
		t.Fatalf("reversed flag order = %v, want it accepted", err)
	}
}

// TestRunSkill covers the `hap skill` dispatch main routes before run(): the
// bare form dumps the embedded document, install writes it under $HOME, and a
// typo of "install" is refused rather than answered with the 77KB dump.
func TestRunSkill(t *testing.T) {
	var out strings.Builder
	if err := runSkill(&out, nil); err != nil {
		t.Fatalf("runSkill: %v", err)
	}
	if out.String() != skilldoc.HapSkill {
		t.Fatal("bare `hap skill` should print exactly the embedded document")
	}
	out.Reset()
	if err := runSkill(&out, []string{"show"}); err != nil || out.String() != skilldoc.HapSkill {
		t.Fatalf("`hap skill show` should print exactly the embedded document (err=%v)", err)
	}

	if err := runSkill(io.Discard, []string{"instal"}); err == nil ||
		!strings.Contains(err.Error(), "unknown skill subcommand") {
		t.Fatalf("a typo of install must be refused, got %v", err)
	}
	if err := runSkill(io.Discard, []string{"install"}); err == nil ||
		!strings.Contains(err.Error(), "no install target named") {
		t.Fatalf("install without a target must name the valid ones, got %v", err)
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	out.Reset()
	if err := runSkill(&out, []string{"install", "claude"}); err != nil {
		t.Fatalf("install claude: %v", err)
	}
	dest := filepath.Join(home, ".claude", "skills", "hap", "SKILL.md")
	if !strings.Contains(out.String(), dest) {
		t.Fatalf("install should print the written path %s, got %q", dest, out.String())
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != skilldoc.HapSkill {
		t.Fatal("installed file differs from the embedded document")
	}
}

// TestMCPNeverOpensLocalSQLiteUnderTurso: an MCP server launched without the
// daemon's HAP_STORE_SOCKET_PATH hint (a direct replay, an MCP registration
// carrying only HAP_DB_PATH) must not fall back to a local SQLite file on a
// turso install — decisions staged there would never reach the daemon. Both
// signals a sanitized environment may leave select the proxy, and with no
// daemon the result is the store-unavailable error, not a new database file.
func TestMCPNeverOpensLocalSQLiteUnderTurso(t *testing.T) {
	t.Setenv("HAP_STORE_SOCKET_PATH", "")
	t.Setenv("HAP_NODE_ID", "")
	ctx := context.Background()

	t.Run("config selects turso", func(t *testing.T) {
		paths := config.Paths{ConfigDir: t.TempDir(), StateDir: t.TempDir()}
		if err := os.WriteFile(paths.File(), []byte("[database]\nengine = \"turso\"\nturso_database_url = \"libsql://x\"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		st, err := mcpStore(ctx, paths, paths.DBPath())
		if err == nil {
			st.Close()
			t.Fatal("a turso install with no daemon must not yield a store")
		}
		if !errors.Is(err, sqlbridge.ErrStoreUnavailable) {
			t.Fatalf("err = %v, want ErrStoreUnavailable", err)
		}
		if _, serr := os.Stat(paths.DBPath()); serr == nil {
			t.Fatal("a local SQLite file was created under the turso engine")
		}
	})

	t.Run("turso state dir beside the database", func(t *testing.T) {
		paths := config.Paths{ConfigDir: t.TempDir(), StateDir: t.TempDir()}
		if err := os.MkdirAll(paths.TursoDir(), 0o700); err != nil {
			t.Fatal(err)
		}
		// HAP_DB_PATH only, config dir sanitized away: the dir is the signal.
		sanitized := config.Paths{StateDir: paths.StateDir}
		st, err := mcpStore(ctx, sanitized, paths.DBPath())
		if err == nil {
			st.Close()
			t.Fatal("the turso state dir must select the proxy")
		}
		if !errors.Is(err, sqlbridge.ErrStoreUnavailable) {
			t.Fatalf("err = %v, want ErrStoreUnavailable", err)
		}
		if _, serr := os.Stat(paths.DBPath()); serr == nil {
			t.Fatal("a local SQLite file was created under the turso engine")
		}
	})

	t.Run("sqlite install opens the local file", func(t *testing.T) {
		paths := config.Paths{ConfigDir: t.TempDir(), StateDir: t.TempDir()}
		st, err := mcpStore(ctx, paths, paths.DBPath())
		if err != nil {
			t.Fatal(err)
		}
		st.Close()
		if _, err := os.Stat(paths.DBPath()); err != nil {
			t.Fatalf("the sqlite engine must open the local file: %v", err)
		}
	})
}
