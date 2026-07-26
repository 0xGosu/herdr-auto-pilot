package frontend_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/buildinfo"
	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
	"github.com/0xGosu/herdr-auto-pilot/internal/updatecheck"
)

// updateApp builds an App wired for the update check: a state dir to cache
// into, a fixed clock, and an injected fetcher so no test opens a socket.
func updateApp(t *testing.T, latest string, fetchErr error) (*frontend.App, string, time.Time) {
	t.Helper()
	dir := t.TempDir()
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	app := &frontend.App{
		StateDir: dir,
		Clock:    func() time.Time { return now },
		FetchLatestVersion: func(context.Context) (string, error) {
			if fetchErr != nil {
				return "", fetchErr
			}
			return latest, nil
		},
	}
	return app, dir, now
}

// stampVersion pins the running binary's version for one test.
func stampVersion(t *testing.T, v string) {
	t.Helper()
	orig := buildinfo.Version
	t.Cleanup(func() { buildinfo.Version = orig })
	buildinfo.Version = v
}

func TestUpdateStatusHint(t *testing.T) {
	if got := (frontend.UpdateStatus{}).Hint(); got != "" {
		t.Errorf("empty status hint = %q, want empty", got)
	}
	if got := (frontend.UpdateStatus{Latest: "v0.5.2"}).Hint(); got != "" {
		t.Errorf("not-available status hint = %q, want empty", got)
	}
	want := "↑ v0.5.2 available"
	if got := (frontend.UpdateStatus{Available: true, Latest: "v0.5.2"}).Hint(); got != want {
		t.Errorf("hint = %q, want %q", got, want)
	}
}

func TestUpdateStatusReadsCache(t *testing.T) {
	stampVersion(t, "v0.5.1")
	app, dir, now := updateApp(t, "", nil)
	if err := updatecheck.Write(dir, updatecheck.State{CheckedAt: now, LatestVersion: "v0.5.2"}); err != nil {
		t.Fatal(err)
	}
	got := app.UpdateStatus(config.Config{})
	if !got.Available || got.Latest != "v0.5.2" {
		t.Fatalf("UpdateStatus = %+v, want v0.5.2 available", got)
	}
}

// TestUpdateStatusSuppressed covers every reason the header must stay quiet.
func TestUpdateStatusSuppressed(t *testing.T) {
	cases := []struct {
		name    string
		version string
		cached  string
		disable bool
		noState bool
	}{
		{name: "disabled by config", version: "v0.5.1", cached: "v0.5.2", disable: true},
		{name: "dev build", version: "dev", cached: "v9.9.9"},
		{name: "makefile dev build", version: "dev-20260726120000", cached: "v9.9.9"},
		{name: "already current", version: "v0.5.2", cached: "v0.5.2"},
		{name: "cache is older", version: "v0.5.2", cached: "v0.5.1"},
		{name: "no cache", version: "v0.5.1"},
		{name: "no state dir", version: "v0.5.1", cached: "v0.5.2", noState: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stampVersion(t, tc.version)
			app, dir, now := updateApp(t, "", nil)
			if tc.cached != "" {
				if err := updatecheck.Write(dir, updatecheck.State{CheckedAt: now, LatestVersion: tc.cached}); err != nil {
					t.Fatal(err)
				}
			}
			if tc.noState {
				app.StateDir = ""
			}
			var cfg config.Config
			cfg.TUI.DisableCheckForUpdate = tc.disable

			if got := app.UpdateStatus(cfg); got.Available {
				t.Errorf("UpdateStatus = %+v, want nothing available", got)
			}
			if got := app.UpdateStatus(cfg).Hint(); got != "" {
				t.Errorf("hint = %q, want empty", got)
			}
		})
	}
}

func TestUpdateCheckDue(t *testing.T) {
	stampVersion(t, "v0.5.1")
	app, dir, now := updateApp(t, "", nil)

	if !app.UpdateCheckDue(config.Config{}) {
		t.Error("a never-checked state dir must be due")
	}
	if err := updatecheck.Write(dir, updatecheck.State{CheckedAt: now, LatestVersion: "v0.5.2"}); err != nil {
		t.Fatal(err)
	}
	if app.UpdateCheckDue(config.Config{}) {
		t.Error("a just-checked state dir must not be due")
	}
	if err := updatecheck.Write(dir, updatecheck.State{CheckedAt: now.Add(-updatecheck.TTL), LatestVersion: "v0.5.2"}); err != nil {
		t.Fatal(err)
	}
	if !app.UpdateCheckDue(config.Config{}) {
		t.Error("an aged-out check must be due")
	}

	// Disabled and dev builds must never schedule a network call at all.
	var off config.Config
	off.TUI.DisableCheckForUpdate = true
	if app.UpdateCheckDue(off) {
		t.Error("a disabled check must never be due")
	}
	stampVersion(t, "dev")
	if app.UpdateCheckDue(config.Config{}) {
		t.Error("a dev build must never schedule a check")
	}
}

func TestCheckForUpdateWritesCache(t *testing.T) {
	stampVersion(t, "v0.5.1")
	app, dir, now := updateApp(t, "v0.5.2", nil)

	got, err := app.CheckForUpdate(context.Background())
	if err != nil {
		t.Fatalf("CheckForUpdate: %v", err)
	}
	if !got.Available || got.Latest != "v0.5.2" {
		t.Errorf("CheckForUpdate = %+v, want v0.5.2 available", got)
	}
	st, ok := updatecheck.Read(dir)
	if !ok || st.LatestVersion != "v0.5.2" || !st.CheckedAt.Equal(now) {
		t.Errorf("cache = %+v (ok=%v), want v0.5.2 stamped at %v", st, ok, now)
	}
	if app.UpdateCheckDue(config.Config{}) {
		t.Error("a fresh check must leave the next one not due")
	}
}

// TestCheckForUpdateFailureBacksOff pins the offline path: the failure is
// recorded (so the retry backs off) and the last known version survives.
func TestCheckForUpdateFailureBacksOff(t *testing.T) {
	stampVersion(t, "v0.5.1")
	app, dir, now := updateApp(t, "", errors.New("no network"))
	if err := updatecheck.Write(dir, updatecheck.State{
		CheckedAt: now.Add(-48 * time.Hour), LatestVersion: "v0.5.2"}); err != nil {
		t.Fatal(err)
	}

	if _, err := app.CheckForUpdate(context.Background()); err == nil {
		t.Fatal("expected the fetch error to reach the caller")
	}
	st, ok := updatecheck.Read(dir)
	if !ok || !st.FailedAt.Equal(now) {
		t.Errorf("failure was not recorded: %+v (ok=%v)", st, ok)
	}
	if st.LatestVersion != "v0.5.2" {
		t.Errorf("a failed check discarded the known version: %+v", st)
	}
	if app.UpdateCheckDue(config.Config{}) {
		t.Error("a failed check must back the next one off, not retry immediately")
	}
	// The header still reports what was last known.
	if got := app.UpdateStatus(config.Config{}); !got.Available {
		t.Errorf("UpdateStatus after a failed check = %+v, want the cached version", got)
	}
}

// TestCheckForUpdateSameVersion records the result without claiming an update.
func TestCheckForUpdateSameVersion(t *testing.T) {
	stampVersion(t, "v0.5.2")
	app, _, _ := updateApp(t, "v0.5.2", nil)

	got, err := app.CheckForUpdate(context.Background())
	if err != nil {
		t.Fatalf("CheckForUpdate: %v", err)
	}
	if got.Available {
		t.Errorf("CheckForUpdate = %+v, want nothing available", got)
	}
	if got.Latest != "v0.5.2" {
		t.Errorf("latest = %q, want it recorded anyway", got.Latest)
	}
}

// TestCheckForUpdateWithoutStateDir must not panic or fetch.
func TestCheckForUpdateWithoutStateDir(t *testing.T) {
	called := false
	app := &frontend.App{FetchLatestVersion: func(context.Context) (string, error) {
		called = true
		return "v9.9.9", nil
	}}
	got, err := app.CheckForUpdate(context.Background())
	if err != nil || got.Available {
		t.Errorf("CheckForUpdate = %+v, %v; want a quiet no-op", got, err)
	}
	if called {
		t.Error("fetched despite having nowhere to cache the result")
	}
}
