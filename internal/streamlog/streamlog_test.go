package streamlog

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
)

var _ ports.StreamLog = (*Log)(nil)

func newLog(t *testing.T) *Log {
	t.Helper()
	l := InStateDir(t.TempDir())
	t.Cleanup(func() { _ = l.Close() })
	return l
}

func ev(kind string) domain.StreamEvent {
	return domain.StreamEvent{Kind: kind, Author: "operator", At: time.Now()}
}

func TestFreshLogIsEmpty(t *testing.T) {
	ctx := context.Background()
	l := newLog(t)
	head, err := l.Head(ctx)
	if err != nil || head != 0 {
		t.Fatalf("Head = %d, %v; want 0", head, err)
	}
	floor, err := l.Floor(ctx)
	if err != nil || floor != 0 {
		t.Fatalf("Floor = %d, %v; want 0", floor, err)
	}
	evs, err := l.Since(ctx, 0, 10)
	if err != nil || len(evs) != 0 {
		t.Fatalf("Since = %v, %v; want none", evs, err)
	}
}

func TestAppendSinceAndRendering(t *testing.T) {
	ctx := context.Background()
	l := newLog(t)
	e := ev(domain.StreamEscalation)
	e.Fields = []domain.StreamField{domain.StreamInt("id", 5), domain.StreamStr("agent", "calm-pika")}
	seq, err := l.Append(ctx, e)
	if err != nil || seq != 1 {
		t.Fatalf("Append = %d, %v; want 1", seq, err)
	}
	if seq, err := l.Append(ctx, ev(domain.StreamPauseOn)); err != nil || seq != 2 {
		t.Fatalf("second Append = %d, %v; want 2", seq, err)
	}
	evs, err := l.Since(ctx, 1, 10)
	if err != nil || len(evs) != 1 || evs[0].Seq != 2 || evs[0].Kind != domain.StreamPauseOn {
		t.Fatalf("Since(1) = %+v, %v; want only seq 2", evs, err)
	}
	all, err := l.Since(ctx, 0, 10)
	if err != nil || len(all) != 2 {
		t.Fatalf("Since(0) = %+v, %v", all, err)
	}
	if got := all[0].Rendered; got != "id=5 agent=calm-pika" {
		t.Fatalf("rendered fields = %q", got)
	}
}

func TestDedupeDropsTheSecondEvent(t *testing.T) {
	ctx := context.Background()
	l := newLog(t)
	e := ev(domain.StreamEscalation)
	e.Dedupe = "escalation:9"
	if seq, err := l.Append(ctx, e); err != nil || seq != 1 {
		t.Fatalf("first = %d, %v", seq, err)
	}
	if seq, err := l.Append(ctx, e); err != nil || seq != 0 {
		t.Fatalf("duplicate = %d, %v; want 0 (dropped)", seq, err)
	}
	if seen, err := l.Seen(ctx, "escalation:9"); err != nil || !seen {
		t.Fatalf("Seen = %v, %v", seen, err)
	}
	// Events without a key never collide with each other.
	for i := range 2 {
		if seq, err := l.Append(ctx, ev(domain.StreamPauseOn)); err != nil || seq == 0 {
			t.Fatalf("keyless append %d = %d, %v", i, seq, err)
		}
	}
}

// A prune must never let the counter go back: a reader resuming from a seq it
// already handled would otherwise be handed a DIFFERENT event under that
// number.
func TestPruneKeepsTheCounterMonotone(t *testing.T) {
	ctx := context.Background()
	l := newLog(t)
	old := ev(domain.StreamPauseOn)
	old.At = time.Now().Add(-10 * 24 * time.Hour)
	for range 3 {
		if _, err := l.Append(ctx, old); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := l.Prune(ctx, time.Now().Add(-Retention)); err != nil || n != 3 {
		t.Fatalf("Prune = %d, %v; want 3", n, err)
	}
	if head, _ := l.Head(ctx); head != 3 {
		t.Fatalf("Head after prune = %d, want 3 (the high-water mark survives)", head)
	}
	if floor, _ := l.Floor(ctx); floor != 0 {
		t.Fatalf("Floor after pruning everything = %d, want 0", floor)
	}
	seq, err := l.Append(ctx, ev(domain.StreamPauseOff))
	if err != nil || seq != 4 {
		t.Fatalf("Append after prune = %d, %v; want 4", seq, err)
	}
	if floor, _ := l.Floor(ctx); floor != 4 {
		t.Fatalf("Floor = %d, want 4", floor)
	}
}

// Every hap process on the machine writes the same file; two handles stand in
// for two processes.
func TestTwoHandlesShareOneCounter(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), FileName)
	a, b := New(path), New(path)
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	var wg sync.WaitGroup
	const each = 25
	errs := make(chan error, 2*each)
	for _, l := range []*Log{a, b} {
		wg.Add(1)
		go func(l *Log) {
			defer wg.Done()
			for range each {
				if _, err := l.Append(ctx, ev(domain.StreamPauseOn)); err != nil {
					errs <- err
				}
			}
		}(l)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent append: %v", err)
	}
	evs, err := a.Since(ctx, 0, 1000)
	if err != nil || len(evs) != 2*each {
		t.Fatalf("Since = %d events, %v; want %d", len(evs), err, 2*each)
	}
	for i, e := range evs {
		if e.Seq != int64(i+1) {
			t.Fatalf("event %d has seq %d, want a dense sequence", i, e.Seq)
		}
	}
}

// A dedupe mark outlives the event it guarded — the prune is about replay, and
// "already recorded" must hold for as long as the caller's subject is live —
// and goes only when ForgetMarks is told it is no longer wanted.
func TestMarksOutlivePruneUntilForgotten(t *testing.T) {
	ctx := context.Background()
	l := newLog(t)
	for _, key := range []string{"escalation:1", "escalation:2", "other:1"} {
		e := ev(domain.StreamEscalation)
		e.Dedupe = key
		e.At = time.Now().Add(-10 * 24 * time.Hour)
		if _, err := l.Append(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := l.Prune(ctx, time.Now().Add(-Retention)); err != nil {
		t.Fatal(err)
	}
	e := ev(domain.StreamEscalation)
	e.Dedupe = "escalation:1"
	if seq, err := l.Append(ctx, e); err != nil || seq != 0 {
		t.Fatalf("re-append after its event was pruned = %d, %v; want dropped", seq, err)
	}
	n, err := l.ForgetMarks(ctx, "escalation:", func(key string) bool { return key == "escalation:1" })
	if err != nil || n != 1 {
		t.Fatalf("ForgetMarks = %d, %v; want 1", n, err)
	}
	for key, want := range map[string]bool{"escalation:1": true, "escalation:2": false, "other:1": true} {
		if seen, err := l.Seen(ctx, key); err != nil || seen != want {
			t.Errorf("Seen(%s) = %v, %v; want %v", key, seen, err, want)
		}
	}
}
