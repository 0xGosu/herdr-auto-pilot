package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
	"github.com/0xGosu/herdr-auto-pilot/internal/streamlog"
	"github.com/0xGosu/herdr-auto-pilot/internal/tasklocator"
)

// The harness's store must expose the capability the announcement pass
// type-asserts for; if this breaks, every test below passes by doing nothing.
var _ ports.EscalationAttentionLister = (*failingStore)(nil)

func newStreamHarness(t *testing.T, cfgTOML string) (*harness, *streamlog.Log) {
	t.Helper()
	log := streamlog.InStateDir(t.TempDir())
	t.Cleanup(func() { _ = log.Close() })
	fl := &fakeLLM{}
	h := newHarnessCore(t, cfgTOML, nil, fl, fl, nil, func(o *Options) { o.Stream = log })
	return h, log
}

// streamKinds returns "<kind> <fields> by=<author>" for every event of the
// given kind.
func streamOf(t *testing.T, log *streamlog.Log, kind string) []string {
	t.Helper()
	evs, err := log.Since(context.Background(), 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, ev := range evs {
		if ev.Kind == kind {
			out = append(out, strings.SplitN(ev.Line(), " ", 3)[2])
		}
	}
	return out
}

func seedStreamEscalation(t *testing.T, h *harness, agent string, at time.Time) int64 {
	t.Helper()
	id, err := h.raw.AppendAudit(context.Background(), domain.AuditRecord{
		AgentID: agent, AgentType: "claude", SituationType: domain.SituationApproval, Trigger: "t",
		Action: domain.AuditActionEscalated, Status: "escalated", CreatedAt: at,
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestDaemonStartIsAnnounced(t *testing.T) {
	_, log := newStreamHarness(t, "")
	waitFor(t, 2*time.Second, func() bool { return len(streamOf(t, log, domain.StreamDaemonStarted)) == 1 })
}

// An escalation is announced only once the auto-accept pass that ran after it
// became eligible has left it pending — never at insert, and never twice.
func TestAnnouncePendingEscalationsOnceAfterThePass(t *testing.T) {
	h, log := newStreamHarness(t, "")
	ctx := context.Background()
	if err := h.raw.AssignAgentName(ctx, "p1", "otter"); err != nil {
		t.Fatal(err)
	}
	passStart := time.Now().Truncate(time.Second)
	before := seedStreamEscalation(t, h, "p1", passStart.Add(-time.Minute))
	seedStreamEscalation(t, h, "p1", passStart.Add(time.Minute)) // raised after the pass began
	claimed := seedStreamEscalation(t, h, "p2", passStart.Add(-time.Minute))
	if ok, err := h.raw.ClaimForAutoAccept(ctx, claimed); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}

	h.daemon.announcePendingEscalations(ctx, autoAcceptPassReport{}, passStart)
	want := "escalation id=" + domain.StreamInt("", before).Value + " agent=otter type=approval by=daemon"
	got := streamOf(t, log, domain.StreamEscalation)
	if len(got) != 1 || got[0] != want {
		t.Fatalf("announced %v, want exactly [%s]", got, want)
	}

	// Again, and again after "a restart" forgot what it announced: the log's
	// dedupe mark is what holds.
	h.daemon.announcePendingEscalations(ctx, autoAcceptPassReport{}, passStart)
	h.daemon.stream.announced = nil
	h.daemon.announcePendingEscalations(ctx, autoAcceptPassReport{}, passStart)
	if got := streamOf(t, log, domain.StreamEscalation); len(got) != 1 {
		t.Fatalf("an escalation was announced more than once: %v", got)
	}
}

// Under full self-prompting every type's cutoff is the pass start, so an
// auto-accepted row (no longer 'escalated') is never announced while one the
// pass left pending is.
func TestAnnounceSkipsAnAutoAcceptedEscalation(t *testing.T) {
	h, log := newStreamHarness(t, "")
	ctx := context.Background()
	passStart := time.Now().Truncate(time.Second)
	accepted := seedStreamEscalation(t, h, "p1", passStart.Add(-time.Minute))
	if ok, err := h.raw.ClaimForAutoAccept(ctx, accepted); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	if ok, err := h.raw.MarkAutoAccepted(ctx, accepted, true); err != nil || !ok {
		t.Fatalf("mark auto-accepted: %v %v", ok, err)
	}
	h.daemon.announcePendingEscalations(ctx, autoAcceptPassReport{}, passStart)
	if got := streamOf(t, log, domain.StreamEscalation); len(got) != 0 {
		t.Fatalf("an auto-accepted escalation was announced: %v", got)
	}
}

func TestDaemonTaskListWriteIsAnnounced(t *testing.T) {
	h, log := newStreamHarness(t, "[task_source_provider]\nprovider = \"local_fs\"\n")
	path := filepath.Join(t.TempDir(), "tasks.md")
	if err := os.WriteFile(path, []byte("# Tasks\n- [ ] first\n- [ ] second\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.daemon.mutateTaskList(path, func(content string) (string, error) {
		return strings.Replace(content, "- [ ] second", "- [-] second", 1), nil
	}); err != nil {
		t.Fatal(err)
	}
	// The list is named by its canonical locator (macOS temp dirs sit behind
	// the /var -> /private/var symlink).
	want := "task.updated list=" + tasklocator.Canonical(path) + " index=2 mark=- by=daemon"
	waitFor(t, 2*time.Second, func() bool { return len(streamOf(t, log, domain.StreamTaskUpdated)) > 0 })
	if got := streamOf(t, log, domain.StreamTaskUpdated); len(got) != 1 || got[0] != want {
		t.Fatalf("announced %v, want [%s]", got, want)
	}
	// A write that changes nothing says nothing. The marker event after it
	// goes through the same queue, so once it lands the no-op had its chance.
	if err := h.daemon.mutateTaskList(path, func(content string) (string, error) { return content, nil }); err != nil {
		t.Fatal(err)
	}
	h.daemon.emitStream(context.Background(), domain.StreamPauseOn)
	waitFor(t, 2*time.Second, func() bool { return len(streamOf(t, log, domain.StreamPauseOn)) == 1 })
	if got := streamOf(t, log, domain.StreamTaskUpdated); len(got) != 1 {
		t.Fatalf("a no-op write was announced: %v", got)
	}
}

func TestStreamPruneRunsOnceADay(t *testing.T) {
	h, log := newStreamHarness(t, "")
	ctx := context.Background()
	if _, err := log.Append(ctx, domain.StreamEvent{Kind: domain.StreamPauseOn,
		At: time.Now().Add(-2 * streamlog.Retention)}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	h.daemon.maybePruneStream(now)
	waitFor(t, 2*time.Second, func() bool { return len(streamOf(t, log, domain.StreamPauseOn)) == 0 })

	// Within the day the throttle holds: the decision is synchronous, so the
	// latch not moving proves no second prune was scheduled.
	h.daemon.maybePruneStream(now.Add(time.Hour))
	h.daemon.mu.Lock()
	last := h.daemon.lastStreamPrune
	h.daemon.mu.Unlock()
	if !last.Equal(now) {
		t.Fatalf("a second prune was scheduled within a day (latch moved to %v)", last)
	}
}

// seedCandidate writes a pending escalation auto-accept CAN take: a suggestion
// and a signature baseline.
func seedCandidate(t *testing.T, h *harness, agent string, at time.Time) int64 {
	t.Helper()
	id, err := h.raw.AppendAudit(context.Background(), domain.AuditRecord{
		AgentID: agent, AgentType: "claude", SituationType: domain.SituationApproval, Trigger: "t",
		Action: domain.AuditActionEscalated, Status: "escalated", Suggestion: "respond: Yes", CreatedAt: at,
	}.WithSignatureBaseline(domain.SignatureResult{
		Signature: "sig-" + agent, Raw: "sig-" + agent, Salient: "permission:proceed | options:no;yes",
		Verdict: domain.GuardOK, SalientChars: 500,
	}))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// A row the auto-accept pass merely PUT OFF — another row on its agent went
// first, its pane was busy — or never examined (past the candidate cap) is not
// a row left for a human: auto-accept takes it on a later sweep, and
// announcing it would set the orchestrator racing the daemon for the pane.
func TestAnnounceWaitsForARowThePassPutOff(t *testing.T) {
	h, log := newStreamHarness(t, "")
	ctx := context.Background()
	passStart := time.Now().Truncate(time.Second)
	refused := seedCandidate(t, h, "p1", passStart.Add(-time.Minute))
	putOff := seedCandidate(t, h, "p1", passStart.Add(-time.Minute))
	unexamined := seedCandidate(t, h, "p2", passStart.Add(-time.Minute))
	noSuggestion := seedStreamEscalation(t, h, "p3", passStart.Add(-time.Minute))

	h.daemon.announcePendingEscalations(ctx, autoAcceptPassReport{
		ran:      true,
		cutoffs:  map[domain.SituationType]time.Time{domain.SituationApproval: passStart},
		examined: map[int64]bool{refused: true, putOff: true},
		deferred: map[int64]bool{putOff: true},
	}, passStart)

	got := map[string]bool{}
	for _, line := range streamOf(t, log, domain.StreamEscalation) {
		got[strings.Fields(line)[1]] = true
	}
	for id, want := range map[int64]bool{refused: true, putOff: false, unexamined: false, noSuggestion: true} {
		if got["id="+domain.StreamInt("", id).Value] != want {
			t.Errorf("escalation %d announced = %v, want %v (all: %v)", id, !want, want, got)
		}
	}

	// A pass that never reached its candidates (paused, say) examined nothing.
	h.daemon.announcePendingEscalations(ctx, autoAcceptPassReport{
		cutoffs: map[domain.SituationType]time.Time{domain.SituationApproval: passStart},
	}, passStart)
	if n := len(streamOf(t, log, domain.StreamEscalation)); n != 2 {
		t.Fatalf("a pass that did not run released its candidates: %d announced, want 2", n)
	}
}

// Every pending escalation is announced, at any age and however long the
// backlog: an age window, or a newest-first cap applied before the dedupe,
// leaves some pending row unannounced forever.
func TestAnnounceWalksTheWholeBacklogAtAnyAge(t *testing.T) {
	h, log := newStreamHarness(t, "")
	ctx := context.Background()
	prev := streamEscalationPage
	streamEscalationPage = 2
	t.Cleanup(func() { streamEscalationPage = prev })
	passStart := time.Now().Truncate(time.Second)
	seedStreamEscalation(t, h, "p0", passStart.Add(-40*24*time.Hour)) // outlived the event log's retention
	for i := range 4 {
		seedStreamEscalation(t, h, "p1", passStart.Add(-time.Duration(i+1)*time.Minute))
	}
	h.daemon.announcePendingEscalations(ctx, autoAcceptPassReport{}, passStart)
	if got := streamOf(t, log, domain.StreamEscalation); len(got) != 5 {
		t.Fatalf("announced %d of 5 pending escalations: %v", len(got), got)
	}
}

// A mark outlives the event it guarded (so a pruned event never re-announces
// a still-pending row), and is forgotten only once its escalation is settled.
func TestEscalationMarksOutliveTheEventAndGoWhenSettled(t *testing.T) {
	h, log := newStreamHarness(t, "")
	ctx := context.Background()
	passStart := time.Now().Truncate(time.Second)
	live := seedStreamEscalation(t, h, "p1", passStart.Add(-time.Minute))
	settled := seedStreamEscalation(t, h, "p2", passStart.Add(-time.Minute))
	h.daemon.announcePendingEscalations(ctx, autoAcceptPassReport{}, passStart)
	if _, err := log.Prune(ctx, time.Now().Add(time.Hour)); err != nil { // every event gone
		t.Fatal(err)
	}
	if _, err := h.raw.ResolveEscalation(ctx, settled); err != nil {
		t.Fatal(err)
	}
	h.daemon.stream.announced = nil // a restart
	h.daemon.stream.lastForget = time.Time{}
	h.daemon.announcePendingEscalations(ctx, autoAcceptPassReport{}, passStart)
	if got := streamOf(t, log, domain.StreamEscalation); len(got) != 0 {
		t.Fatalf("a pending escalation was announced again after its event aged out: %v", got)
	}
	if seen, _ := log.Seen(ctx, escalationMark(live)); !seen {
		t.Error("the pending escalation's mark was forgotten")
	}
	if seen, _ := log.Seen(ctx, escalationMark(settled)); seen {
		t.Error("the settled escalation's mark was kept forever")
	}
}

// blockingStreamLog holds every append until released — a stream database
// locked by another hap process.
type blockingStreamLog struct {
	*streamlog.Log
	release chan struct{}
}

func (b blockingStreamLog) Append(ctx context.Context, ev domain.StreamEvent) (int64, error) {
	<-b.release
	return b.Log.Append(ctx, ev)
}

// The select loop never waits on the event log: an emit queues and returns,
// and the announcement pass runs in the background.
func TestStreamWritesNeverBlockTheLoop(t *testing.T) {
	log := streamlog.InStateDir(t.TempDir())
	t.Cleanup(func() { _ = log.Close() })
	blocked := blockingStreamLog{Log: log, release: make(chan struct{})}
	fl := &fakeLLM{}
	h := newHarnessCore(t, "", nil, fl, fl, nil, func(o *Options) { o.Stream = blocked })
	t.Cleanup(func() { close(blocked.release) })
	seedStreamEscalation(t, h, "p1", time.Now().Add(-time.Minute))

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.daemon.emitStream(context.Background(), domain.StreamPauseOn)
		h.daemon.startAnnouncePass(time.Now())
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("a stream write blocked its caller while the log was locked")
	}
}
