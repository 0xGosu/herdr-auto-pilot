package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
)

// streamPollInterval is how often a stream looks for new events while they are
// arriving. A variable so tests can shorten it.
var streamPollInterval = 500 * time.Millisecond

// streamIdlePollInterval is the cadence once nothing has arrived for
// streamIdleAfter: an idle orchestrator stream is the common state, and it
// used to poll at the fast rate forever. The first event read snaps the poll
// back to fast, so at most one event per quiet spell waits the slower tick.
// Variables so tests can shorten them.
var (
	streamIdlePollInterval = 2 * time.Second
	streamIdleAfter        = 30 * time.Second
)

// streamGapRecheck bounds how long a caught-up stream goes without re-asking
// for the retained floor (see the settled flag in streamOrchestrator).
var streamGapRecheck = time.Minute

// streamBatch is how many events one read returns; a full batch is followed by
// another read at once rather than a poll wait, so a long replay is not paced.
const streamBatch = 500

// streamOrchestrator is `hap stream orchestrator [--resume N] [--include-self]`.
//
// It reads the machine-local event log only (app.Stream), so it needs no
// running daemon and never touches herdr.
//
// The stream's reader is normally the orchestrator itself, and its OWN actions
// are in the log beside everyone else's — so without a filter every task it
// marks done, every escalation it answers and every config key it sets comes
// straight back at it as an event it must recognize and ignore. That is pure
// noise on the one surface whose whole job is to say what the orchestrator has
// not yet seen, so events it authored (domain.OrchestratorAuthor, the author a
// hap command carries under HAP_ACTOR=orchestrator) are suppressed by default.
// --include-self prints them, for debugging the emitters.
func streamOrchestrator(ctx context.Context, app *frontend.App, out io.Writer, args []string) error {
	const usage = "usage: hap stream orchestrator [--resume N] [--include-self]"
	fs := flag.NewFlagSet("stream orchestrator", flag.ContinueOnError)
	resume := fs.Int64("resume", 0, "replay every event after N, then keep following")
	includeSelf := fs.Bool("include-self", false,
		"also print the events the orchestrator itself authored (suppressed by default)")
	fs.SetOutput(out)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q (%s)", fs.Arg(0), usage)
	}
	resuming := isFlagPassed(fs, "resume")
	if resuming && *resume < 0 {
		return fmt.Errorf("--resume takes the last seq you handled, 0 or more (%s)", usage)
	}
	if app.Stream == nil {
		return fmt.Errorf("the orchestrator event log is not available in this process")
	}
	head, err := app.Stream.Head(ctx)
	if err != nil {
		return fmt.Errorf("read the orchestrator event log: %w", err)
	}
	floor, err := app.Stream.Floor(ctx)
	if err != nil {
		return fmt.Errorf("read the orchestrator event log: %w", err)
	}
	fmt.Fprintf(out, "# hap stream orchestrator head=%d floor=%d\n", head, floor)

	cursor := head
	if resuming {
		cursor = *resume
		oldest := floor
		if oldest == 0 {
			oldest = head + 1 // everything ever written has been pruned
		}
		switch {
		case cursor > head:
			// Never wait for a seq that is not coming: the log was reset
			// underneath the reader's cursor.
			fmt.Fprintf(out, "# reset resume=%d head=%d (the log restarted below your cursor; following from the head — re-survey with hap)\n",
				cursor, head)
			cursor = head
		case cursor+1 < oldest:
			fmt.Fprintf(out, "# gap missed=%d..%d (pruned before you resumed — re-survey with hap)\n", cursor+1, oldest-1)
			cursor = oldest - 1
		}
	}

	var lastErr string
	// suppressed counts the self-authored events skipped since the last write,
	// through seq suppressedThrough. They are owed ONE "# suppressed" notice, and
	// it is never written on its own: every line of output wakes the reader, and
	// a notice announcing only the reader's own work is exactly the echo the
	// filter exists to remove — observed live as an orchestrator waking to say
	// "that was my own dismissal, nothing to act on" after each of its actions.
	// So the notice rides IN FRONT of the next line that is written anyway (an
	// event or a "# gap", in one write so the reader gets one chunk), or is the
	// stream's last line when it is stopped, where it still hands a resuming
	// reader the seq it reached.
	//
	// Accepted cost: a write is the only way this loop learns its reader went
	// away (a closed pipe answers with EPIPE, or SIGPIPE on stdout), so a stream
	// whose Monitor died while only the orchestrator was acting now follows the
	// log until the next event someone else writes — the limit a genuinely idle
	// stream always had, and closing it would mean a heartbeat on every quiet one.
	var suppressed, suppressedThrough int64
	write := func(text string) bool {
		if suppressed > 0 {
			text = suppressedNotice(suppressed, suppressedThrough) + text
			suppressed = 0
		}
		_, err := io.WriteString(out, text)
		return err == nil
	}
	// finish writes the notice still owed when the stream is stopped. Never
	// called on a failed write: that reader is gone. A failure here needs no
	// handling — the stream is ending either way.
	finish := func() error {
		if suppressed > 0 {
			_, _ = io.WriteString(out, suppressedNotice(suppressed, suppressedThrough))
		}
		return nil
	}
	// settled means the previous read came back empty AND its gap check found
	// nothing, so the cursor had reached the head. From there an empty read
	// cannot hide a gap — only an event appended AND aged past the retention
	// window between two polls could — so the floor query, a second statement
	// on every idle tick, is skipped until a batch arrives, a read fails, or
	// streamGapRecheck passes.
	settled := false
	var checkedAt time.Time
	lastEventAt := time.Now()
	for {
		evs, err := app.Stream.Since(ctx, cursor, streamBatch)
		if err == nil && (!settled || len(evs) > 0 || time.Since(checkedAt) >= streamGapRecheck) {
			// Asked AFTER the read: a prune landing between the floor above
			// (or the previous batch) and this read removes events the cursor
			// had not reached, and they must be reported, never skipped.
			last, gap, checkErr := prunedUnder(ctx, app, cursor, evs)
			if gap {
				if !write(fmt.Sprintf("# gap missed=%d..%d (pruned while you were reading — re-survey with hap)\n",
					cursor+1, last)) {
					return nil // the reader went away
				}
				cursor = last
			}
			checkedAt = time.Now()
			settled = checkErr == nil && !gap && len(evs) == 0
		}
		if err != nil {
			settled = false
			if ctx.Err() != nil {
				return finish()
			}
			// A transient read failure (a busy database) must not end a stream
			// an agent is watching; say it once per distinct error and retry.
			if msg := err.Error(); msg != lastErr {
				fmt.Fprintf(os.Stderr, "hap stream: reading the event log failed, retrying: %v\n", err)
				lastErr = msg
			}
		} else {
			lastErr = ""
		}
		// The cursor advances over a SUPPRESSED event exactly as it does over a
		// printed one, so a self-authored event is never re-delivered on the
		// next resume and never reads as a gap. Everything else in this loop is
		// accounted on the RAW batch — prunedUnder's batch[0].Seq, settled,
		// lastEventAt and the full-batch continue above — because each answers a
		// question about the LOG, not about what was printed: counting only
		// printed events would re-query the floor forever on a log the
		// orchestrator alone wrote, and would end the no-wait replay mid-stream
		// on a full batch it happened to author.
		for _, ev := range evs {
			if *includeSelf || ev.Author != domain.OrchestratorAuthor {
				if !write(ev.Line() + "\n") {
					return nil // the reader went away
				}
			} else {
				suppressed++
				suppressedThrough = ev.Seq
			}
			cursor = ev.Seq
		}
		if len(evs) == streamBatch {
			continue
		}
		wait := streamPollInterval
		if len(evs) > 0 {
			lastEventAt = time.Now()
		} else if time.Since(lastEventAt) >= streamIdleAfter {
			wait = streamIdlePollInterval
		}
		select {
		case <-ctx.Done():
			return finish()
		case <-time.After(wait):
		}
	}
}

// suppressedNotice is the one line owed for a run of self-authored events.
func suppressedNotice(n, through int64) string {
	return fmt.Sprintf("# suppressed %d self-authored event(s) through seq=%d (--include-self shows them)\n", n, through)
}

// prunedUnder reports the last seq of events pruned beneath the cursor that
// the reader never saw: everything from cursor+1 up to (not including) the
// retained floor, and never past the first event the batch did return. gap is
// false when nothing was lost — and when the floor could not be read (err),
// since a failed read is not evidence of a gap; nor is it evidence of NONE,
// which is why the caller keeps checking until a read succeeds.
//
// The head is read BEFORE the floor. The other order races an append on an
// empty log: floor reads 0, an event lands, head reads 1, and that event —
// never pruned — is reported as a gap and skipped. Read first, the head can
// only be older than the floor, and an append after it makes the floor
// non-zero.
func prunedUnder(ctx context.Context, app *frontend.App, cursor int64, batch []domain.StreamEvent) (last int64, gap bool, err error) {
	head, err := app.Stream.Head(ctx)
	if err != nil {
		return 0, false, err
	}
	floor, err := app.Stream.Floor(ctx)
	if err != nil {
		return 0, false, err
	}
	if floor == 0 { // nothing retained: everything up to the head is gone
		floor = head + 1
	}
	last = floor - 1
	if len(batch) > 0 && batch[0].Seq-1 < last {
		last = batch[0].Seq - 1
	}
	return last, last > cursor, nil
}
