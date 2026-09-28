package cli_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
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
	// A suppressed event the cursor does not step over is re-read on every
	// poll forever.
	waitForCursor(t, rec, newest)

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

// closingWriter stands in for a pipe whose reader has gone: writes succeed
// until closed is set, then fail as a closed pipe does — while still recording
// what was ATTEMPTED, which is what the control assertion needs to see.
type closingWriter struct {
	syncBuffer
	closed    atomic.Bool
	attempted syncBuffer
}

func (w *closingWriter) Write(p []byte) (int, error) {
	if w.closed.Load() {
		_, _ = w.attempted.Write(p)
		return 0, io.ErrClosedPipe
	}
	return w.syncBuffer.Write(p)
}

// waitForCursor waits until the stream has read past seq — the only witness
// that a batch it printed nothing for was actually consumed.
func waitForCursor(t *testing.T, rec *cursorRecordingLog, seq int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for rec.cursor.Load() < seq {
		if time.Now().After(deadline) {
			t.Fatalf("the cursor stalled at %d and never passed seq %d", rec.cursor.Load(), seq)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestStreamSelfWorkAloneWritesNothing: every line wakes the reader, so a run
// of the orchestrator's own work must produce NO output on its own — not even
// the "# suppressed" notice, which used to be written 10s after each burst and
// woke the orchestrator to say it had nothing to act on. The notice rides in
// front of the next foreign event instead, as one line, naming the last
// suppressed seq.
func TestStreamSelfWorkAloneWritesNothing(t *testing.T) {
	app, log := streamApp(t)
	rec := &cursorRecordingLog{Log: log}
	app.Stream = rec
	out, stop := startStream(t, app)
	waitForOutput(t, out, "# hap stream orchestrator head=0 floor=0\n")
	banner := out.String()
	now := time.Now()
	appendSelfEvent(t, log, domain.StreamTaskUpdated, now)
	last := appendSelfEvent(t, log, domain.StreamCorrection, now)
	waitForCursor(t, rec, last)
	time.Sleep(100 * time.Millisecond) // many polls with nothing new
	if got := out.String(); got != banner {
		t.Fatalf("the orchestrator's own work alone produced output, which wakes it for nothing:\n%s", got)
	}

	fsp := appendEvent(t, log, domain.StreamFSPOn, now)
	waitForOutput(t, out, " fsp.on by=operator\n")
	time.Sleep(50 * time.Millisecond)
	if err := stop(); err != nil {
		t.Fatalf("stream returned %v on cancel, want nil", err)
	}
	want := banner +
		fmt.Sprintf("# suppressed 2 self-authored event(s) through seq=%d (--include-self shows them)\n", last) +
		fmt.Sprintf("%d ", fsp)
	if got := out.String(); !strings.HasPrefix(got, want) || strings.Count(got, "# suppressed") != 1 {
		t.Fatalf("want exactly one notice directly ahead of the foreign event, got:\n%s", got)
	}
}

// TestStreamStopWritesTheOwedNotice: a stream stopped with self-authored
// events still unannounced ends with the notice, so the seq it reached is not
// lost to a reader resuming from its last line — and one with nothing owed
// adds nothing.
func TestStreamStopWritesTheOwedNotice(t *testing.T) {
	for _, owed := range []bool{true, false} {
		t.Run(fmt.Sprintf("owed=%v", owed), func(t *testing.T) {
			app, log := streamApp(t)
			rec := &cursorRecordingLog{Log: log}
			app.Stream = rec
			out, stop := startStream(t, app)
			waitForOutput(t, out, "# hap stream orchestrator head=0 floor=0\n")
			now := time.Now()
			last := appendEvent(t, log, domain.StreamPauseOn, now)
			waitForOutput(t, out, " pause.on by=operator\n")
			if owed {
				last = appendSelfEvent(t, log, domain.StreamTaskUpdated, now)
			}
			waitForCursor(t, rec, last)
			if err := stop(); err != nil {
				t.Fatalf("stream returned %v on cancel, want nil", err)
			}
			got := out.String()
			notice := fmt.Sprintf("# suppressed 1 self-authored event(s) through seq=%d (--include-self shows them)\n", last)
			if owed && !strings.HasSuffix(got, notice) {
				t.Fatalf("a stopped stream dropped the notice it owed:\n%s", got)
			}
			if !owed && strings.Contains(got, "# suppressed") {
				t.Fatalf("a stopped stream owing nothing wrote a notice:\n%s", got)
			}
		})
	}
}

// TestStreamNoticesAVanishedReaderAtTheNextForeignEvent: a write is the only
// signal that the reader went away, and the orchestrator's own work no longer
// writes anything — so a stream whose Monitor died while only the orchestrator
// was acting learns it at the next event somebody else writes, and the owed
// notice goes out in that same write.
func TestStreamNoticesAVanishedReaderAtTheNextForeignEvent(t *testing.T) {
	app, log := streamApp(t)
	rec := &cursorRecordingLog{Log: log}
	app.Stream = rec
	t.Cleanup(cli.SetStreamPollInterval(5 * time.Millisecond))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	out := &closingWriter{}
	done := make(chan error, 1)
	go func() { done <- cli.Run(ctx, app, out, "stream", []string{"orchestrator"}) }()
	waitForOutput(t, &out.syncBuffer, "# hap stream orchestrator head=0 floor=0\n")
	out.closed.Store(true)
	now := time.Now()
	var last int64
	for _, kind := range []string{domain.StreamTaskUpdated, domain.StreamCorrection, domain.StreamEscalationDismissed} {
		last = appendSelfEvent(t, log, kind, now)
	}
	waitForCursor(t, rec, last)
	time.Sleep(50 * time.Millisecond)
	if got := out.attempted.String(); got != "" {
		t.Fatalf("self-authored events alone attempted a write:\n%s", got)
	}

	fsp := appendEvent(t, log, domain.StreamFSPOn, now)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("stream returned %v when its reader went away, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the stream never noticed its reader was gone at the next foreign event")
	}
	want := fmt.Sprintf("# suppressed 3 self-authored event(s) through seq=%d (--include-self shows them)\n%d ", last, fsp)
	if got := out.attempted.String(); !strings.HasPrefix(got, want) {
		t.Fatalf("want the notice and the foreign event in one attempted write, got:\n%s", got)
	}
}

// TestStreamOwedNoticeRidesAheadOfAGap: a "# gap" line is written output too,
// so a notice owed when one is found goes out in the SAME write, ahead of it —
// the suppressed seqs are all below the gap, so that is also seq order.
func TestStreamOwedNoticeRidesAheadOfAGap(t *testing.T) {
	app, log := streamApp(t)
	pl := newPruneOnDemandLog(log)
	app.Stream = pl
	// A caught-up stream skips the floor query until this passes.
	t.Cleanup(cli.SetStreamGapRecheck(time.Millisecond))
	out, stop := startStream(t, app)
	waitForOutput(t, out, "# hap stream orchestrator head=0 floor=0\n")
	old := time.Now().Add(-30 * 24 * time.Hour)
	// Aged as well: the gap check reads the retained FLOOR, so a surviving
	// event below the hole would hide it.
	self := appendSelfEvent(t, log, domain.StreamTaskUpdated, old)
	waitForCursor(t, &pl.cursorRecordingLog, self)
	// Two events the stream never reaches: aged out between two polls. The
	// lock keeps the stream from reading between the appends and the prune.
	pl.mu.Lock()
	appendEvent(t, log, domain.StreamPauseOn, old)
	lost := appendEvent(t, log, domain.StreamPauseOff, old)
	pl.arm.Store(true)
	pl.mu.Unlock()
	waitForOutput(t, out, "# gap ")
	if err := stop(); err != nil {
		t.Fatalf("stream returned %v on cancel, want nil", err)
	}
	want := fmt.Sprintf("# suppressed 1 self-authored event(s) through seq=%d (--include-self shows them)\n"+
		"# gap missed=%d..%d ", self, self+1, lost)
	if got := out.String(); !strings.Contains(got, want) || strings.Count(got, "# suppressed") != 1 {
		t.Fatalf("want the owed notice directly ahead of the gap, exactly once, got:\n%s", got)
	}
}

// pruneOnDemandLog prunes every event older than a day just before the first
// read after arm is set. Holding mu keeps every read out.
type pruneOnDemandLog struct {
	cursorRecordingLog
	mu  sync.Mutex
	arm atomic.Bool
}

func newPruneOnDemandLog(log *streamlog.Log) *pruneOnDemandLog {
	return &pruneOnDemandLog{cursorRecordingLog: cursorRecordingLog{Log: log}}
}

func (p *pruneOnDemandLog) Since(ctx context.Context, after int64, limit int) ([]domain.StreamEvent, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.arm.CompareAndSwap(true, false) {
		_, _ = p.Prune(ctx, time.Now().Add(-24*time.Hour))
	}
	return p.cursorRecordingLog.Since(ctx, after, limit)
}
