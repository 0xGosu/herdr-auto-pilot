package cli_test

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/cli"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
	"github.com/0xGosu/herdr-auto-pilot/internal/streamlog"
)

// syncBuffer is a bytes.Buffer safe to read while the stream goroutine writes.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func streamApp(t *testing.T) (*frontend.App, *streamlog.Log) {
	t.Helper()
	app, _ := testApp(t)
	log := streamlog.InStateDir(app.StateDir)
	t.Cleanup(func() { _ = log.Close() })
	app.Stream = log
	return app, log
}

// appendEvent appends an event somebody OTHER than the stream's reader wrote,
// which is what a stream is for; appendSelfEvent is the orchestrator's own,
// suppressed unless --include-self. Two named entry points over one body
// because "who wrote it" is now behaviour rather than decoration, and a bare
// author string at the call site does not say which side of the filter it is.
func appendEvent(t *testing.T, log *streamlog.Log, kind string, at time.Time) int64 {
	t.Helper()
	return appendEventBy(t, log, kind, "operator", at)
}

func appendSelfEvent(t *testing.T, log *streamlog.Log, kind string, at time.Time) int64 {
	t.Helper()
	return appendEventBy(t, log, kind, domain.OrchestratorAuthor, at)
}

func appendEventBy(t *testing.T, log *streamlog.Log, kind, author string, at time.Time) int64 {
	t.Helper()
	seq, err := log.Append(context.Background(), domain.StreamEvent{Kind: kind, Author: author, At: at})
	if err != nil {
		t.Fatal(err)
	}
	return seq
}

// startStream runs `hap stream orchestrator args...` until the test ends,
// returning its output buffer and a stop function that waits for it to exit.
func startStream(t *testing.T, app *frontend.App, args ...string) (*syncBuffer, func() error) {
	t.Helper()
	t.Cleanup(cli.SetStreamPollInterval(5 * time.Millisecond))
	ctx, cancel := context.WithCancel(context.Background())
	out := &syncBuffer{}
	done := make(chan error, 1)
	go func() {
		done <- cli.Run(ctx, app, out, "stream", append([]string{"orchestrator"}, args...))
	}()
	stopped := false
	stop := func() error {
		if stopped {
			return nil
		}
		stopped = true
		cancel()
		select {
		case err := <-done:
			return err
		case <-time.After(5 * time.Second):
			t.Fatal("stream did not exit after its context was cancelled")
			return nil
		}
	}
	t.Cleanup(func() { _ = stop() })
	return out, stop
}

func waitForOutput(t *testing.T, out *syncBuffer, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(out.String(), want) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("stream output never contained %q:\n%s", want, out.String())
}

func TestStreamOrchestratorStartsAtTheHead(t *testing.T) {
	app, log := streamApp(t)
	now := time.Now()
	appendEvent(t, log, domain.StreamPauseOn, now)
	appendEvent(t, log, domain.StreamPauseOff, now)

	out, stop := startStream(t, app)
	waitForOutput(t, out, "# hap stream orchestrator head=2 floor=1\n")
	appendEvent(t, log, domain.StreamFSPOn, now)
	waitForOutput(t, out, " fsp.on by=operator\n")
	if err := stop(); err != nil {
		t.Fatalf("stream returned %v on cancel, want nil", err)
	}
	got := out.String()
	if strings.Contains(got, "pause.") {
		t.Fatalf("a stream without --resume replayed past events:\n%s", got)
	}
	if !strings.Contains(got, "\n3 ") {
		t.Fatalf("the new event was not printed with its seq:\n%s", got)
	}
}

// TestStreamOrchestratorSuppressesItsOwnEvents: the reader of this stream is
// the orchestrator, so its own actions coming back at it are noise it would
// have to recognize and discard on every line.
func TestStreamOrchestratorSuppressesItsOwnEvents(t *testing.T) {
	app, log := streamApp(t)
	now := time.Now()

	out, stop := startStream(t, app)
	waitForOutput(t, out, "# hap stream orchestrator head=0 floor=0\n")
	appendSelfEvent(t, log, domain.StreamPauseOn, now)
	appendEvent(t, log, domain.StreamFSPOn, now)
	// The operator's event is the ONLY thing that may be printed, and waiting
	// for it is what proves the batch carrying both was read rather than merely
	// not delivered yet.
	waitForOutput(t, out, " fsp.on by=operator\n")
	if err := stop(); err != nil {
		t.Fatalf("stream returned %v on cancel, want nil", err)
	}
	// Matched on "by=<author>" rather than the bare word: the banner line is
	// itself "# hap stream orchestrator …".
	got := out.String()
	if strings.Contains(got, "by="+domain.OrchestratorAuthor) || strings.Contains(got, "pause.on") {
		t.Fatalf("the stream echoed an event the orchestrator itself authored:\n%s", got)
	}
}

// TestStreamOrchestratorIncludeSelfPrintsThem is the debugging opt-out, and
// the control for the test above: without it that one would pass on a stream
// that prints nothing at all.
func TestStreamOrchestratorIncludeSelfPrintsThem(t *testing.T) {
	app, log := streamApp(t)
	now := time.Now()

	out, stop := startStream(t, app, "--include-self")
	waitForOutput(t, out, "# hap stream orchestrator head=0 floor=0\n")
	appendSelfEvent(t, log, domain.StreamPauseOn, now)
	waitForOutput(t, out, " pause.on by="+domain.OrchestratorAuthor+"\n")
	if err := stop(); err != nil {
		t.Fatalf("stream returned %v on cancel, want nil", err)
	}
}

// eventLines returns the event lines of a stream's output, dropping the "#"
// notices — what a reader would actually have had to handle.
func eventLines(out string) []string {
	var lines []string
	for _, l := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if l != "" && !strings.HasPrefix(l, "#") {
			lines = append(lines, l)
		}
	}
	return lines
}

// TestStreamResumeSpansARunOfSuppressedEvents: a resume whose window is mostly
// the reader's own work must replay the rest of it and say nothing else. The
// banner is the part most easily broken by a filter — head and floor describe
// the LOG, so they count suppressed events like any other.
func TestStreamResumeSpansARunOfSuppressedEvents(t *testing.T) {
	app, log := streamApp(t)
	now := time.Now()
	appendEvent(t, log, domain.StreamPauseOn, now)                 // 1, before the cursor
	appendSelfEvent(t, log, domain.StreamTaskUpdated, now)         // 2
	appendSelfEvent(t, log, domain.StreamCorrection, now)          // 3
	appendSelfEvent(t, log, domain.StreamEscalationDismissed, now) // 4
	last := appendEvent(t, log, domain.StreamFSPOn, now)           // 5, the only line owed

	out, stop := startStream(t, app, "--resume", "1")
	waitForOutput(t, out, " fsp.on by=operator\n")
	if err := stop(); err != nil {
		t.Fatalf("stream returned %v on cancel, want nil", err)
	}
	got := out.String()
	if !strings.Contains(got, "# hap stream orchestrator head=5 floor=1\n") {
		t.Errorf("the banner stopped counting suppressed events — it describes the log, not the output:\n%s", got)
	}
	// A suppressed event is not a lost one: the cursor moves over it, so
	// nothing may be reported as pruned or reset.
	if strings.Contains(got, "# gap") || strings.Contains(got, "# reset") {
		t.Errorf("a run of suppressed events was mistaken for missing sequence numbers:\n%s", got)
	}
	lines := eventLines(got)
	if len(lines) != 1 || !strings.HasPrefix(lines[0], fmt.Sprintf("%d ", last)) {
		t.Errorf("--resume 1 over 3 self-authored events printed %v, want just seq %d", lines, last)
	}
}

// cursorRecordingLog records the highest cursor the stream has read from. It
// is the only way to watch the cursor cross a SUPPRESSED event: by
// construction nothing is printed, so the output cannot witness it.
type cursorRecordingLog struct {
	*streamlog.Log
	cursor atomic.Int64
}

func (l *cursorRecordingLog) Since(ctx context.Context, after int64, limit int) ([]domain.StreamEvent, error) {
	for {
		seen := l.cursor.Load()
		if after <= seen || l.cursor.CompareAndSwap(seen, after) {
			break
		}
	}
	return l.Log.Since(ctx, after, limit)
}

// TestStreamNewestEventSuppressedStillAdvancesTheCursor: when the newest event
// in the log is the reader's own, the stream prints nothing — and must still
// move its cursor past it. Two things go wrong if it does not, and the second
// is the visible one: the event is re-read on every poll forever, and once
// retention removes it the stale cursor makes the prune check report a gap
// that never happened, sending the reader off to re-survey for its own work.
func TestStreamNewestEventSuppressedStillAdvancesTheCursor(t *testing.T) {
	app, log := streamApp(t)
	old := time.Now().Add(-30 * 24 * time.Hour)
	appendEvent(t, log, domain.StreamPauseOn, old) // 1, before the cursor
	appendSelfEvent(t, log, domain.StreamTaskUpdated, old)
	newest := appendSelfEvent(t, log, domain.StreamTaskDeleted, old)
	rec := &cursorRecordingLog{Log: log}
	app.Stream = rec

	out, _ := startStream(t, app, "--resume", "1")
	waitForOutput(t, out, "# hap stream orchestrator head=3 floor=1\n")
	deadline := time.Now().Add(5 * time.Second)
	for rec.cursor.Load() < newest {
		if time.Now().After(deadline) {
			t.Fatalf("the cursor stalled at %d and never passed the suppressed seq %d — "+
				"a suppressed event it does not step over is re-read on every poll forever",
				rec.cursor.Load(), newest)
		}
		time.Sleep(time.Millisecond)
	}

	// The retention sweep now takes every event the cursor has passed, and a
	// visible one arrives behind it. A cursor left at 1 reports
	// "# gap missed=2..3" here; a correct one has nothing to report.
	if _, err := log.Prune(context.Background(), time.Now().Add(-24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	appendEvent(t, log, domain.StreamFSPOn, time.Now())
	waitForOutput(t, out, " fsp.on by=operator\n")
	if got := out.String(); strings.Contains(got, "# gap") {
		t.Errorf("suppressed events the cursor had passed were reported as pruned-unseen:\n%s", got)
	}
}

func TestStreamOrchestratorResumeReplaysAfterTheCursor(t *testing.T) {
	app, log := streamApp(t)
	now := time.Now()
	appendEvent(t, log, domain.StreamPauseOn, now)
	appendEvent(t, log, domain.StreamPauseOff, now)
	appendEvent(t, log, domain.StreamFSPOn, now)

	out, _ := startStream(t, app, "--resume", "1")
	waitForOutput(t, out, " fsp.on by=operator\n")
	got := out.String()
	if strings.Contains(got, "pause.on") || !strings.Contains(got, "pause.off") {
		t.Fatalf("--resume 1 must replay exactly the events after seq 1:\n%s", got)
	}
	if strings.Contains(got, "# gap") || strings.Contains(got, "# reset") {
		t.Fatalf("an in-range cursor reported a gap or reset:\n%s", got)
	}
}

func TestStreamOrchestratorResumeZeroReplaysEverything(t *testing.T) {
	app, log := streamApp(t)
	appendEvent(t, log, domain.StreamPauseOn, time.Now())
	out, _ := startStream(t, app, "--resume", "0")
	waitForOutput(t, out, "\n1 ")
	if strings.Contains(out.String(), "# gap") {
		t.Fatalf("resuming from 0 on an unpruned log reported a gap:\n%s", out.String())
	}
}

func TestStreamOrchestratorResumeBelowTheFloorSaysSo(t *testing.T) {
	app, log := streamApp(t)
	old := time.Now().Add(-30 * 24 * time.Hour)
	appendEvent(t, log, domain.StreamPauseOn, old)
	appendEvent(t, log, domain.StreamPauseOff, old)
	if _, err := log.Prune(context.Background(), time.Now().Add(-streamlog.Retention)); err != nil {
		t.Fatal(err)
	}
	appendEvent(t, log, domain.StreamFSPOn, time.Now())

	out, _ := startStream(t, app, "--resume", "0")
	waitForOutput(t, out, " fsp.on by=operator\n")
	if !strings.Contains(out.String(), "# gap missed=1..2 ") {
		t.Fatalf("a cursor below the retained floor must be reported, never skipped silently:\n%s", out.String())
	}
}

func TestStreamOrchestratorResumeAboveTheHeadFollowsFromTheHead(t *testing.T) {
	app, log := streamApp(t)
	appendEvent(t, log, domain.StreamPauseOn, time.Now())
	out, _ := startStream(t, app, "--resume", "99")
	waitForOutput(t, out, "# reset resume=99 head=1 ")
	appendEvent(t, log, domain.StreamPauseOff, time.Now())
	waitForOutput(t, out, "\n2 ")
}

func TestStreamOrchestratorOnAFreshLog(t *testing.T) {
	app, _ := streamApp(t)
	out, _ := startStream(t, app)
	waitForOutput(t, out, "# hap stream orchestrator head=0 floor=0\n")
}

func TestStreamRefusesBadArguments(t *testing.T) {
	app, _ := streamApp(t)
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"stream"}, "name a stream"},
		{[]string{"stream", "nope"}, `unknown stream "nope"`},
		{[]string{"stream", "orchestrator", "--resume", "-1"}, "0 or more"},
		{[]string{"stream", "orchestrator", "extra"}, `unexpected argument "extra"`},
	}
	for _, tc := range cases {
		_, err := run(t, app, tc.args[0], tc.args[1:]...)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("hap %s: err = %v, want it to mention %q", strings.Join(tc.args, " "), err, tc.want)
		}
	}
	app.Stream = nil
	if _, err := run(t, app, "stream", "orchestrator"); err == nil {
		t.Error("a process with no event log must refuse rather than stream nothing forever")
	}
}

// pruningLog prunes every event older than a day just before the first read —
// the daemon's retention landing between the stream's floor check and its
// first batch.
type pruningLog struct {
	*streamlog.Log
	once sync.Once
}

func (p *pruningLog) Since(ctx context.Context, after int64, limit int) ([]domain.StreamEvent, error) {
	p.once.Do(func() { _, _ = p.Prune(ctx, time.Now().Add(-24*time.Hour)) })
	return p.Log.Since(ctx, after, limit)
}

func TestStreamOrchestratorReportsAPruneDuringTheRead(t *testing.T) {
	app, log := streamApp(t)
	old := time.Now().Add(-30 * 24 * time.Hour)
	appendEvent(t, log, domain.StreamPauseOn, old)
	appendEvent(t, log, domain.StreamPauseOff, old)
	appendEvent(t, log, domain.StreamFSPOn, time.Now())
	app.Stream = &pruningLog{Log: log}

	out, _ := startStream(t, app, "--resume", "0")
	waitForOutput(t, out, " fsp.on by=operator\n")
	got := out.String()
	if !strings.Contains(got, "# hap stream orchestrator head=3 floor=1\n") {
		t.Fatalf("the floor was read after the prune; the case needs it read before:\n%s", got)
	}
	if !strings.Contains(got, "# gap missed=1..2 ") {
		t.Fatalf("events pruned under the cursor mid-read were skipped silently:\n%s", got)
	}
}
