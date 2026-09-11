package cli_test

import (
	"bytes"
	"context"
	"strings"
	"sync"
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

func appendEvent(t *testing.T, log *streamlog.Log, kind string, at time.Time) int64 {
	t.Helper()
	seq, err := log.Append(context.Background(), domain.StreamEvent{Kind: kind, Author: "operator", At: at})
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
