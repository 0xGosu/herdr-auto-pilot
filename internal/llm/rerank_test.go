package llm

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

func TestRerankConfigured(t *testing.T) {
	var nilAdapter *Adapter
	if nilAdapter.RerankConfigured() {
		t.Error("nil adapter must report not configured")
	}
	if (&Adapter{}).RerankConfigured() {
		t.Error("empty template must report not configured — that is how the feature stays off by default")
	}
	if !(&Adapter{RerankTemplate: []string{"cat"}}).RerankConfigured() {
		t.Error("non-empty template must report configured")
	}
}

func TestRerankSubstitutesPlaceholders(t *testing.T) {
	dir := t.TempDir()
	argvFile := filepath.Join(dir, "argv")
	script := writeScript(t,
		`printf '%s\n' "$@" > `+argvFile+"\n"+
			`echo '[{"id": 1, "score": 0.97}]'`+"\n")
	a := &Adapter{
		RerankTemplate: []string{script,
			"name={agent_name}", "type={agent_type}", "cwd={cwd}", "sit={situation_type}",
			"k={top_k}", "bar={relevance_score_threshold}",
			"salient={salient}", "cands={candidates}"},
		RerankTimeout: 5 * time.Second,
	}
	got, err := a.Rerank(context.Background(), domain.RerankRequest{
		AgentName: "brave-otter", AgentType: "claude", Cwd: "/workspaces/proj",
		SituationType: domain.SituationApproval, TopK: 3, RelevanceThreshold: 0.95,
		Salient: "permission:proceed | options:no;yes", Candidates: "--- rule 1 ---",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != `[{"id": 1, "score": 0.97}]` {
		t.Errorf("result = %q, want the trimmed stdout verbatim", got)
	}
	argv, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatal(err)
	}
	want := "name=brave-otter\ntype=claude\ncwd=/workspaces/proj\nsit=approval\nk=3\nbar=0.95\n" +
		"salient=permission:proceed | options:no;yes\ncands=--- rule 1 ---\n"
	if string(argv) != want {
		t.Errorf("argv = %q, want %q", argv, want)
	}
}

// TestRerankNeverPutsUntrustedTextInTheEnvironment: the salient, the candidate
// listing and the pane excerpt are unbounded text derived from agent screens.
// {pane_excerpt} has always been argv-only for that reason, and the two new
// placeholders carry exactly the same content, so they follow the same rule.
func TestRerankNeverPutsUntrustedTextInTheEnvironment(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, "env")
	script := writeScript(t, "env > "+envFile+"\n"+`echo '[]'`+"\n")
	a := &Adapter{
		RerankTemplate: []string{script},
		RerankEnv: EnvSpec{Vars: map[string]string{
			"SALIENT": "{salient}", "CANDS": "{candidates}", "PANE": "{pane_excerpt}",
			"AGENT": "{agent_name}",
		}},
		RerankTimeout: 5 * time.Second,
	}
	if _, err := a.Rerank(context.Background(), domain.RerankRequest{
		AgentName: "brave-otter",
		Salient:   "SALIENT-SENTINEL", Candidates: "CANDS-SENTINEL", PaneExcerpt: "PANE-SENTINEL",
	}); err != nil {
		t.Fatal(err)
	}
	env, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, sentinel := range []string{"SALIENT-SENTINEL", "CANDS-SENTINEL", "PANE-SENTINEL"} {
		if strings.Contains(string(env), sentinel) {
			t.Errorf("%s reached the child environment; untrusted pane-derived text is argv-only", sentinel)
		}
	}
	// The bounded, hap-owned values still expand there, or the exclusion would
	// be indistinguishable from the environment not working at all.
	if !strings.Contains(string(env), "brave-otter") {
		t.Error("{agent_name} should still expand in the environment")
	}
}

func TestRerankEmptyOutputErrors(t *testing.T) {
	script := writeScript(t, "echo 'why it failed' >&2\nexit 0\n")
	a := &Adapter{RerankTemplate: []string{script}, RerankTimeout: 5 * time.Second}
	_, err := a.Rerank(context.Background(), domain.RerankRequest{})
	if err == nil || !strings.Contains(err.Error(), "empty output") {
		t.Fatalf("empty stdout must error, got %v", err)
	}
	if !strings.Contains(err.Error(), "why it failed") {
		t.Errorf("error should carry the stderr tail: %v", err)
	}
}

func TestRerankNonZeroExitErrors(t *testing.T) {
	script := writeScript(t, "echo 'boom' >&2\nexit 3\n")
	a := &Adapter{RerankTemplate: []string{script}, RerankTimeout: 5 * time.Second}
	if _, err := a.Rerank(context.Background(), domain.RerankRequest{}); err == nil ||
		!strings.Contains(err.Error(), "re-ranking CLI failed") {
		t.Fatalf("a non-zero exit must error, got %v", err)
	}
}

// TestRerankTimeoutIsClassified: the judge sits inside the classify→decide path
// with a parked agent waiting, so a hung CLI must be named as a timeout rather
// than surfacing as a generic failure the operator cannot act on.
func TestRerankTimeoutIsClassified(t *testing.T) {
	script := writeScript(t, "sleep 5\n")
	a := &Adapter{RerankTemplate: []string{script}, RerankTimeout: 200 * time.Millisecond}
	_, err := a.Rerank(context.Background(), domain.RerankRequest{})
	if err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("a hung judge must be reported as a timeout, got %v", err)
	}
}

// TestRerankOversizedOutputIsRefused: a verdict is a short JSON array. Anything
// huge is a misbehaving CLI, and scanning megabytes of it for a trailing array
// is work on behalf of a run that has already gone wrong.
func TestRerankOversizedOutputIsRefused(t *testing.T) {
	script := writeScript(t, "head -c 20000 /dev/zero | tr '\\0' 'x'\n")
	a := &Adapter{RerankTemplate: []string{script}, RerankTimeout: 5 * time.Second}
	if _, err := a.Rerank(context.Background(), domain.RerankRequest{}); err == nil ||
		!strings.Contains(err.Error(), "oversized") {
		t.Fatalf("oversized stdout must be refused, got %v", err)
	}
}

// TestRerankBoundsCaptureWhileTheChildRuns: the cap is enforced DURING the run,
// not after it.
//
// The judge is launched on every attention event and is the only LLM adapter
// with a short deadline it is expected to sometimes hit, so a CLI writing
// continuously until its timeout would otherwise hold everything it produced in
// that window. The output here is far larger than the caps; what the test
// asserts is that the refusal still arrives and that nothing near that volume
// was retained.
func TestRerankBoundsCaptureWhileTheChildRuns(t *testing.T) {
	script := writeScript(t,
		"yes 'xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx' | head -c 4000000\n"+
			"yes 'noise on stderr' | head -c 4000000 >&2\n")
	a := &Adapter{RerankTemplate: []string{script}, RerankTimeout: 20 * time.Second}
	out, err := a.Rerank(context.Background(), domain.RerankRequest{})
	if err == nil || !strings.Contains(err.Error(), "oversized") {
		t.Fatalf("a flood of output must be refused as oversized, got out=%d bytes err=%v", len(out), err)
	}
	// The error carries a stderr tail, and that tail must come from a BOUNDED
	// capture: an unbounded one would have kept all 4 MB to take 500 bytes from.
	if len(err.Error()) > 4096 {
		t.Errorf("the refusal itself carries %d bytes; the stderr tail is not bounded", len(err.Error()))
	}
}

// TestCapWriterKeepsTheCapAndReportsOverflow pins the writer's two contracts:
// it never keeps more than the cap, and it never reports a short write — a
// short count or an error would make exec tear the pipe down and surface a
// misbehaving CLI as a broken one.
func TestCapWriterKeepsTheCapAndReportsOverflow(t *testing.T) {
	w := newCapWriter(10)
	if n, err := w.Write([]byte("abcde")); n != 5 || err != nil {
		t.Fatalf("Write under the cap = (%d, %v), want (5, nil)", n, err)
	}
	if w.Overflowed() {
		t.Error("a write under the cap must not report overflow")
	}
	if n, err := w.Write([]byte("fghijklmno")); n != 10 || err != nil {
		t.Fatalf("Write past the cap = (%d, %v), want the full length and no error", n, err)
	}
	if !w.Overflowed() {
		t.Error("a write past the cap must report overflow")
	}
	if got := w.String(); got != "abcdefghij" {
		t.Errorf("kept %q, want exactly the first 10 bytes", got)
	}
	// Every later write is discarded, and still reports success.
	if n, err := w.Write([]byte("zzz")); n != 3 || err != nil {
		t.Fatalf("Write after overflow = (%d, %v), want (3, nil)", n, err)
	}
	if got := w.String(); got != "abcdefghij" {
		t.Errorf("kept %q after a post-overflow write", got)
	}
}

// TestRerankUnconfiguredErrorsWithoutSpawning keeps "off" a hard gate in the
// adapter too, not only at the daemon's port assertion.
func TestRerankUnconfiguredErrorsWithoutSpawning(t *testing.T) {
	if _, err := (&Adapter{}).Rerank(context.Background(), domain.RerankRequest{}); err == nil ||
		!strings.Contains(err.Error(), "no re-ranking CLI configured") {
		t.Fatalf("unconfigured Rerank must error, got %v", err)
	}
}
