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

// streamOrchestrator is `hap stream orchestrator [--resume N]`.
//
// It reads the machine-local event log only (app.Stream), so it needs no
// running daemon and never touches herdr.
func streamOrchestrator(ctx context.Context, app *frontend.App, out io.Writer, args []string) error {
	const usage = "usage: hap stream orchestrator [--resume N]"
	fs := flag.NewFlagSet("stream orchestrator", flag.ContinueOnError)
	resume := fs.Int64("resume", 0, "replay every event after N, then keep following")
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
				fmt.Fprintf(out, "# gap missed=%d..%d (pruned while you were reading — re-survey with hap)\n",
					cursor+1, last)
				cursor = last
			}
			checkedAt = time.Now()
			settled = checkErr == nil && !gap && len(evs) == 0
		}
		if err != nil {
			settled = false
			if ctx.Err() != nil {
				return nil
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
		for _, ev := range evs {
			if _, err := fmt.Fprintln(out, ev.Line()); err != nil {
				return nil // the reader went away
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
			return nil
		case <-time.After(wait):
		}
	}
}

// prunedUnder reports the last seq of events pruned beneath the cursor that
// the reader never saw: everything from cursor+1 up to (not including) the
// retained floor, and never past the first event the batch did return. gap is
// false when nothing was lost — and when the floor could not be read (err),
// since a failed read is not evidence of a gap; nor is it evidence of NONE,
// which is why the caller keeps checking until a read succeeds.
func prunedUnder(ctx context.Context, app *frontend.App, cursor int64, batch []domain.StreamEvent) (last int64, gap bool, err error) {
	floor, err := app.Stream.Floor(ctx)
	if err != nil {
		return 0, false, err
	}
	if floor == 0 { // nothing retained: everything up to the head is gone
		head, err := app.Stream.Head(ctx)
		if err != nil {
			return 0, false, err
		}
		floor = head + 1
	}
	last = floor - 1
	if len(batch) > 0 && batch[0].Seq-1 < last {
		last = batch[0].Seq - 1
	}
	return last, last > cursor, nil
}
