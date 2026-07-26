package frontend

import (
	"context"

	"github.com/0xGosu/herdr-auto-pilot/internal/buildinfo"
	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/updatecheck"
)

// UpdateStatus is what the front-ends know about a newer release. It carries
// only what the last cached check recorded — reading it never touches the
// network (see App.CheckForUpdate for the call that does).
type UpdateStatus struct {
	// Available is true when Latest is a release newer than this binary.
	Available bool
	// Latest is the newest published release tag ("v0.5.2"), when known.
	Latest string
}

// Hint is the one-line operator notice, or "" when nothing is available. Same
// shape as DaemonHealth.Banner so a front-end can render it without branching.
func (u UpdateStatus) Hint() string {
	if !u.Available || u.Latest == "" {
		return ""
	}
	return "↑ " + u.Latest + " available"
}

// UpdateCheckEnabled reports whether the release check may run at all: the
// operator has not disabled it, a state dir exists to cache into, and this
// binary is a release (a linked working-tree build never nags its developer).
func (a *App) UpdateCheckEnabled(cfg config.Config) bool {
	return !cfg.TUI.DisableCheckForUpdate &&
		a.StateDir != "" &&
		updatecheck.IsRelease(buildinfo.Version)
}

// UpdateStatus reports a newer release from the cached check. It reads one
// local file, never errors, and never reaches the network, so front-ends can
// call it on every refresh.
func (a *App) UpdateStatus(cfg config.Config) UpdateStatus {
	if !a.UpdateCheckEnabled(cfg) {
		return UpdateStatus{}
	}
	st, ok := updatecheck.Read(a.StateDir)
	if !ok || !updatecheck.IsNewer(buildinfo.Version, st.LatestVersion) {
		return UpdateStatus{}
	}
	return UpdateStatus{Available: true, Latest: st.LatestVersion}
}

// UpdateCheckDue reports whether a fresh fetch is warranted — the check is
// enabled and the cached result has aged out.
func (a *App) UpdateCheckDue(cfg config.Config) bool {
	if !a.UpdateCheckEnabled(cfg) {
		return false
	}
	st, _ := updatecheck.Read(a.StateDir)
	return updatecheck.Due(st, a.now(), updatecheck.TTL)
}

// fetchLatest resolves the injected fetcher, defaulting to the real one. Tests
// set App.FetchLatestVersion so they never open a socket.
func (a *App) fetchLatest(ctx context.Context) (string, error) {
	if a.FetchLatestVersion != nil {
		return a.FetchLatestVersion(ctx)
	}
	return updatecheck.Latest(ctx, nil)
}

// CheckForUpdate performs the release check and records the result. It is the
// only path that reaches the network, so callers run it off the hot path (the
// TUI fires it as a background command, `hap update` calls it directly).
//
// A failed fetch is recorded — not returned as a front-end error — so the
// retry backs off instead of running on every tick; the previously known
// version is preserved. The returned error is for callers that want to report
// it (the CLI does); the TUI ignores it.
func (a *App) CheckForUpdate(ctx context.Context) (UpdateStatus, error) {
	if a.StateDir == "" {
		return UpdateStatus{}, nil
	}
	prev, _ := updatecheck.Read(a.StateDir)
	latest, err := a.fetchLatest(ctx)
	if err != nil {
		prev.FailedAt = a.now()
		_ = updatecheck.Write(a.StateDir, prev)
		return UpdateStatus{}, err
	}
	// A cache we cannot persist only costs a re-fetch next time, so the write
	// error is dropped rather than failing a check that already succeeded.
	_ = updatecheck.Write(a.StateDir, updatecheck.State{CheckedAt: a.now(), LatestVersion: latest})
	return statusFor(latest), nil
}

// statusFor builds the status for a freshly fetched tag.
func statusFor(latest string) UpdateStatus {
	if !updatecheck.IsNewer(buildinfo.Version, latest) {
		return UpdateStatus{Latest: latest}
	}
	return UpdateStatus{Available: true, Latest: latest}
}
