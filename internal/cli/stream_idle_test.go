package cli_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/cli"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/streamlog"
)

// queryCountingLog counts the two statements an idle stream can issue per
// tick.
type queryCountingLog struct {
	*streamlog.Log
	sinces atomic.Int32
	floors atomic.Int32
}

func (l *queryCountingLog) Since(ctx context.Context, after int64, limit int) ([]domain.StreamEvent, error) {
	l.sinces.Add(1)
	return l.Log.Since(ctx, after, limit)
}

func (l *queryCountingLog) Floor(ctx context.Context) (int64, error) {
	l.floors.Add(1)
	return l.Log.Floor(ctx)
}

func waitForCount(t *testing.T, n *atomic.Int32, want int32) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for n.Load() < want {
		if time.Now().After(deadline) {
			t.Fatalf("count stuck at %d, want at least %d", n.Load(), want)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestStreamIdleReadsSkipTheFloorQuery: once a read comes back empty and its
// gap check passes, the stream is caught up and later empty reads cannot hide
// a gap — so an idle stream issues one statement per tick, not two. The
// control half proves an event re-arms the check.
func TestStreamIdleReadsSkipTheFloorQuery(t *testing.T) {
	app, log := streamApp(t)
	appendEvent(t, log, domain.StreamPauseOn, time.Now())
	counting := &queryCountingLog{Log: log}
	app.Stream = counting

	out, _ := startStream(t, app)
	waitForOutput(t, out, "# hap stream orchestrator head=1 floor=1\n")
	// A third read has started, so the first read's gap check is done.
	waitForCount(t, &counting.sinces, 3)
	settledFloors := counting.floors.Load()
	start := counting.sinces.Load()
	waitForCount(t, &counting.sinces, start+20)
	if got := counting.floors.Load(); got != settledFloors {
		t.Errorf("%d floor queries over 20 idle reads, want 0", got-settledFloors)
	}

	appendEvent(t, log, domain.StreamFSPOn, time.Now())
	waitForOutput(t, out, " fsp.on by=operator\n")
	if got := counting.floors.Load(); got == settledFloors {
		t.Error("a batch that arrived was not gap-checked")
	}
}

// pruneWhileSettledLog, once armed, appends an event already past retention
// and prunes it inside the next read — the one gap a caught-up stream's empty
// reads cannot see, since the event is gone before it could be returned.
type pruneWhileSettledLog struct {
	*streamlog.Log
	armed  atomic.Bool
	fired  atomic.Bool
	sinces atomic.Int32
}

func (l *pruneWhileSettledLog) Since(ctx context.Context, after int64, limit int) ([]domain.StreamEvent, error) {
	l.sinces.Add(1)
	if l.armed.CompareAndSwap(true, false) {
		_, _ = l.Append(ctx, domain.StreamEvent{Kind: domain.StreamPauseOn, Author: "operator",
			At: time.Now().Add(-30 * 24 * time.Hour)})
		_, _ = l.Prune(ctx, time.Now().Add(-24*time.Hour))
		l.fired.Store(true)
	}
	return l.Log.Since(ctx, after, limit)
}

// TestStreamGapRecheckBoundsASettledStream: skipping the floor query on idle
// reads is safe only because it is re-asked every streamGapRecheck. With a
// short recheck the gap is reported; with an hour it is not reported within
// the same reads — the control showing the recheck, not some other path,
// found it.
func TestStreamGapRecheckBoundsASettledStream(t *testing.T) {
	for _, tc := range []struct {
		name    string
		recheck time.Duration
		want    bool
	}{
		{"short recheck reports it", time.Millisecond, true},
		{"hour-long recheck defers it", time.Hour, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Cleanup(cli.SetStreamGapRecheck(tc.recheck))
			app, log := streamApp(t)
			wrapped := &pruneWhileSettledLog{Log: log}
			app.Stream = wrapped

			out, _ := startStream(t, app)
			waitForOutput(t, out, "# hap stream orchestrator head=0 floor=0\n")
			waitForCount(t, &wrapped.sinces, 3) // settled
			wrapped.armed.Store(true)
			waitFor(t, func() bool { return wrapped.fired.Load() })
			waitForCount(t, &wrapped.sinces, wrapped.sinces.Load()+20)

			got := strings.Contains(out.String(), "# gap missed=1..1 ")
			if got != tc.want {
				t.Errorf("gap reported = %v, want %v:\n%s", got, tc.want, out.String())
			}
		})
	}
}

// failingEmptyCheckLog fails the floor query of the first EMPTY read, once.
type failingEmptyCheckLog struct {
	*streamlog.Log
	lastEmpty atomic.Bool
	failOnce  atomic.Bool
	sinces    atomic.Int32
	floors    atomic.Int32
}

func (l *failingEmptyCheckLog) Since(ctx context.Context, after int64, limit int) ([]domain.StreamEvent, error) {
	l.sinces.Add(1)
	evs, err := l.Log.Since(ctx, after, limit)
	l.lastEmpty.Store(err == nil && len(evs) == 0)
	return evs, err
}

func (l *failingEmptyCheckLog) Floor(ctx context.Context) (int64, error) {
	l.floors.Add(1)
	if l.lastEmpty.Load() && l.failOnce.CompareAndSwap(true, false) {
		return 0, errors.New("induced floor failure")
	}
	return l.Log.Floor(ctx)
}

// TestStreamAFailedGapCheckIsNotSettled: a floor read that failed is not
// evidence of NO gap, so the next empty read must ask again. Settling on it
// would leave the stream skipping every check until the recheck interval.
func TestStreamAFailedGapCheckIsNotSettled(t *testing.T) {
	app, log := streamApp(t)
	appendEvent(t, log, domain.StreamPauseOn, time.Now())
	wrapped := &failingEmptyCheckLog{Log: log}
	wrapped.failOnce.Store(true)
	app.Stream = wrapped

	out, _ := startStream(t, app)
	waitForOutput(t, out, "# hap stream orchestrator head=1 floor=1\n")
	waitForCount(t, &wrapped.sinces, 12)
	// The header's floor, the failed check, the retry — then settled.
	if got := wrapped.floors.Load(); got != 3 {
		t.Errorf("floor queries = %d, want 3 (header, failed check, retry)", got)
	}
}

// TestStreamIdleBackoffStillDeliversEvents: past streamIdleAfter the poll slows
// down; an event arriving then must still be printed on the slower tick.
func TestStreamIdleBackoffStillDeliversEvents(t *testing.T) {
	t.Cleanup(cli.SetStreamIdlePolling(20*time.Millisecond, 0))
	app, log := streamApp(t)
	counting := &queryCountingLog{Log: log}
	app.Stream = counting

	out, _ := startStream(t, app)
	waitForOutput(t, out, "# hap stream orchestrator head=0 floor=0\n")
	waitForCount(t, &counting.sinces, 3)
	appendEvent(t, log, domain.StreamPauseOn, time.Now())
	waitForOutput(t, out, " pause.on by=operator\n")
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("condition never held")
		}
		time.Sleep(time.Millisecond)
	}
}
