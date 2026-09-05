package store

import (
	"context"
	"errors"
	"testing"
)

// TestMigrateWithCallsTheHookAfterTheLastWrite: a caller proving a lease must
// be asked again AFTER the migration's final statement, or a lease lost during
// that statement goes unnoticed. The #155 cleanup is the last write on every
// engine, so a verb-only embedding row seeded beforehand must be GONE by the
// time the final hook call runs.
func TestMigrateWithCallsTheHookAfterTheLastWrite(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO signature_embeddings
		(signature, situation_type, agent_type, model, dims, vector, salient, created_at)
		VALUES ('approval:verbonly', 'approval', 'claude', 'm', 1, X'00', 'permission:proceed', 1)`); err != nil {
		t.Fatal(err)
	}
	calls, sawGone := 0, false
	hook := func() error {
		calls++
		var n int
		if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM signature_embeddings WHERE signature = 'approval:verbonly'`).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			sawGone = true
		}
		return nil
	}
	if err := s.MigrateWith(hook); err != nil {
		t.Fatal(err)
	}
	if calls < 2 {
		t.Fatalf("hook ran %d time(s); it must run before every step and after the last", calls)
	}
	if !sawGone {
		t.Fatal("no hook call observed the final cleanup's effect — the last write runs after the last ownership check")
	}
	// And a hook that refuses stops the migration with its error.
	want := errors.New("lease gone")
	if err := s.MigrateWith(func() error { return want }); !errors.Is(err, want) {
		t.Fatalf("MigrateWith with a refusing hook = %v, want its error", err)
	}
}
