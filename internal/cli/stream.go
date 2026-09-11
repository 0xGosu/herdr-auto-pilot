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

// streamPollInterval is how often a stream looks for new events. A variable so
// tests can shorten it.
var streamPollInterval = 500 * time.Millisecond

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
	for {
		evs, err := app.Stream.Since(ctx, cursor, streamBatch)
		if err == nil {
			// Asked AFTER the read: a prune landing between the floor above
			// (or the previous batch) and this read removes events the cursor
			// had not reached, and they must be reported, never skipped.
			if last, ok := prunedUnder(ctx, app, cursor, evs); ok {
				fmt.Fprintf(out, "# gap missed=%d..%d (pruned while you were reading — re-survey with hap)\n",
					cursor+1, last)
				cursor = last
			}
		}
		if err != nil {
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
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(streamPollInterval):
		}
	}
}

// prunedUnder reports the last seq of events pruned beneath the cursor that
// the reader never saw: everything from cursor+1 up to (not including) the
// retained floor, and never past the first event the batch did return. ok is
// false when nothing was lost — or when the floor could not be read, since a
// failed read is not evidence of a gap.
func prunedUnder(ctx context.Context, app *frontend.App, cursor int64, batch []domain.StreamEvent) (int64, bool) {
	floor, err := app.Stream.Floor(ctx)
	if err != nil {
		return 0, false
	}
	if floor == 0 { // nothing retained: everything up to the head is gone
		head, err := app.Stream.Head(ctx)
		if err != nil {
			return 0, false
		}
		floor = head + 1
	}
	last := floor - 1
	if len(batch) > 0 && batch[0].Seq-1 < last {
		last = batch[0].Seq - 1
	}
	return last, last > cursor
}
