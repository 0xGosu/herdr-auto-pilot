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

	h.daemon.announcePendingEscalations(ctx, passStart)
	want := "escalation id=" + domain.StreamInt("", before).Value + " agent=otter type=approval by=daemon"
	got := streamOf(t, log, domain.StreamEscalation)
	if len(got) != 1 || got[0] != want {
		t.Fatalf("announced %v, want exactly [%s]", got, want)
	}

	// Again, and again after "a restart" forgot what it announced: the log's
	// dedupe key is what holds.
	h.daemon.announcePendingEscalations(ctx, passStart)
	h.daemon.streamAnnounced = nil
	h.daemon.announcePendingEscalations(ctx, passStart)
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
	h.daemon.announcePendingEscalations(ctx, passStart)
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
	got := streamOf(t, log, domain.StreamTaskUpdated)
	want := "task.updated list=" + path + " index=2 mark=- by=daemon"
	if len(got) != 1 || got[0] != want {
		t.Fatalf("announced %v, want [%s]", got, want)
	}
	// A write that changes nothing says nothing.
	if err := h.daemon.mutateTaskList(path, func(content string) (string, error) { return content, nil }); err != nil {
		t.Fatal(err)
	}
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

	// Within the day the throttle holds, whatever the log contains.
	if _, err := log.Append(ctx, domain.StreamEvent{Kind: domain.StreamPauseOn,
		At: time.Now().Add(-2 * streamlog.Retention)}); err != nil {
		t.Fatal(err)
	}
	h.daemon.maybePruneStream(now.Add(time.Hour))
	time.Sleep(50 * time.Millisecond)
	if got := streamOf(t, log, domain.StreamPauseOn); len(got) != 1 {
		t.Fatalf("the prune ran twice within a day: %v", got)
	}
}
