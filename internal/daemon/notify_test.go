package daemon

import (
	"context"
	"errors"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
)

// showerNotifier is a NotifyShower — the socket-backed shape, which reports
// what herdr did with the toast.
type showerNotifier struct {
	res    ports.NotifyResult
	err    error
	titles []string
}

func (s *showerNotifier) ShowNotification(_ context.Context, title, _ string) (ports.NotifyResult, error) {
	s.titles = append(s.titles, title)
	return s.res, s.err
}

func (s *showerNotifier) Notify(ctx context.Context, title, body string) error {
	_, err := s.ShowNotification(ctx, title, body)
	return err
}

// plainNotifier is a bare NotifyPort — the CLI-backed shape, which cannot
// report delivery.
type plainNotifier struct {
	titles []string
	err    error
}

func (p *plainNotifier) Notify(_ context.Context, title, _ string) error {
	p.titles = append(p.titles, title)
	return p.err
}

func TestNotifyReportsDeliveryFromAShower(t *testing.T) {
	n := &showerNotifier{res: ports.NotifyResult{Shown: true, Reason: "shown", Known: true}}
	d := &Daemon{opt: Options{Notify: n}}

	res := d.notify(context.Background(), "Agent needs attention", "blocked")
	if !res.Shown || !res.Known {
		t.Fatalf("want a reported, displayed toast, got %+v", res)
	}
	if len(n.titles) != 1 {
		t.Fatalf("want 1 notification, got %v", n.titles)
	}
}

// TestNotifyReportsADroppedToast is the gap this closes: herdr answering
// "no_foreground_client" means the operator was never interrupted, and the
// daemon must be able to tell that apart from a delivered toast. Before the
// socket path it could not — the CLI exits 0 either way.
func TestNotifyReportsADroppedToast(t *testing.T) {
	n := &showerNotifier{res: ports.NotifyResult{Shown: false, Reason: "no_foreground_client", Known: true}}
	d := &Daemon{opt: Options{Notify: n}}

	res := d.notify(context.Background(), "Agent needs attention", "blocked")
	if res.Shown {
		t.Error("Shown must be false — herdr declined it")
	}
	if !res.Known {
		t.Error("herdr answered, so the outcome is Known")
	}
	if res.Reason != "no_foreground_client" {
		t.Errorf("Reason = %q, want the reason herdr gave", res.Reason)
	}
}

// TestNotifyFromAPlainPortClaimsNothing: a NotifyPort with no delivery report
// must yield !Known, never a Shown=false that reads as "herdr dropped it".
// Conflating the two would make every CLI-notified escalation look dropped.
func TestNotifyFromAPlainPortClaimsNothing(t *testing.T) {
	n := &plainNotifier{}
	d := &Daemon{opt: Options{Notify: n}}

	res := d.notify(context.Background(), "Agent needs attention", "blocked")
	if res.Known {
		t.Errorf("a plain NotifyPort cannot report delivery, got %+v", res)
	}
	if res.Shown {
		t.Error("must never claim delivery it has no evidence for")
	}
	if len(n.titles) != 1 {
		t.Fatalf("the toast must still be sent, got %v", n.titles)
	}
}

// A failed round trip is not a dropped toast: it is no evidence at all.
func TestNotifyFailureIsUnknownNotDropped(t *testing.T) {
	n := &showerNotifier{err: errors.New("dial unix: no such file")}
	d := &Daemon{opt: Options{Notify: n}}

	res := d.notify(context.Background(), "Agent needs attention", "blocked")
	if res.Known || res.Shown {
		t.Fatalf("a failed call reports nothing, got %+v", res)
	}
}

func TestNotifyWithNoNotifierIsASafeNoop(t *testing.T) {
	d := &Daemon{opt: Options{}}
	if res := d.notify(context.Background(), "t", "b"); res.Known || res.Shown {
		t.Fatalf("want a zero result, got %+v", res)
	}
}

// A plain NotifyPort that FAILS still reports nothing rather than a dropped
// toast — the degrade path must not manufacture evidence either.
func TestNotifyPlainPortErrorIsStillUnknown(t *testing.T) {
	n := &plainNotifier{err: errors.New("herdr notification show: exit 1")}
	d := &Daemon{opt: Options{Notify: n}}

	res := d.notify(context.Background(), "Agent needs attention", "blocked")
	if res.Known || res.Shown {
		t.Fatalf("a failed CLI notify reports nothing, got %+v", res)
	}
	if len(n.titles) != 1 {
		t.Fatalf("the toast must still have been attempted, got %v", n.titles)
	}
}

// TestEscalationLogAttrs pins the invariant the changelog promises: the
// delivery keys appear ONLY when herdr reported the outcome, so an unknown is
// never logged as either a delivery or a drop.
func TestEscalationLogAttrs(t *testing.T) {
	s := domain.Situation{AgentID: "agent-1", Type: "approval"}
	dec := domain.Decision{Reason: "low_confidence", Suggestion: "1"}

	has := func(attrs []any, key string) (any, bool) {
		for i := 0; i+1 < len(attrs); i += 2 {
			if k, ok := attrs[i].(string); ok && k == key {
				return attrs[i+1], true
			}
		}
		return nil, false
	}

	tests := []struct {
		name           string
		res            ports.NotifyResult
		wantNotified   any
		wantNoNotified bool
		wantReason     string
		wantNoReason   bool
	}{
		{
			name:         "herdr displayed it",
			res:          ports.NotifyResult{Shown: true, Reason: "shown", Known: true},
			wantNotified: true,
			// A delivered toast carries no reason: it would read as a caveat.
			wantNoReason: true,
		},
		{
			name:         "herdr declined it",
			res:          ports.NotifyResult{Shown: false, Reason: "no_foreground_client", Known: true},
			wantNotified: false,
			wantReason:   "no_foreground_client",
		},
		{
			name:           "nothing reported it",
			res:            ports.NotifyResult{},
			wantNoNotified: true,
			wantNoReason:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			attrs := escalationLogAttrs(s, dec, tt.res)
			if len(attrs)%2 != 0 {
				t.Fatalf("slog attrs must be key/value pairs, got %d items", len(attrs))
			}
			// The pre-existing keys must survive in every case.
			if v, ok := has(attrs, "agent"); !ok || v != "agent-1" {
				t.Errorf("agent key lost: %v", attrs)
			}
			if _, ok := has(attrs, "reason"); !ok {
				t.Errorf("reason key lost: %v", attrs)
			}

			v, ok := has(attrs, "notified")
			switch {
			case tt.wantNoNotified && ok:
				t.Errorf("notified must be absent when nothing reported, got %v", v)
			case !tt.wantNoNotified && !ok:
				t.Error("notified missing")
			case !tt.wantNoNotified && v != tt.wantNotified:
				t.Errorf("notified = %v, want %v", v, tt.wantNotified)
			}

			r, ok := has(attrs, "notify_reason")
			switch {
			case tt.wantNoReason && ok:
				t.Errorf("notify_reason must be absent, got %v", r)
			case !tt.wantNoReason && !ok:
				t.Error("notify_reason missing")
			case !tt.wantNoReason && r != tt.wantReason:
				t.Errorf("notify_reason = %v, want %q", r, tt.wantReason)
			}
		})
	}
}
