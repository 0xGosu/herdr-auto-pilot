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

// liveDir makes a directory that exists for the duration of the test.
func liveDir(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// deadDir returns the path of a directory that has been removed.
func deadDir(t *testing.T, name string) string {
	t.Helper()
	dir := liveDir(t, name)
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	return dir
}

// resolve normalizes a path for comparison against `pwd` output: macOS temp
// dirs live under the /var → /private/var symlink, so the two spellings of the
// same directory are not string-equal.
func resolve(t *testing.T, path string) string {
	t.Helper()
	got, err := filepath.EvalSymlinks(strings.TrimSpace(path))
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// consultPWD runs one consult through a script that records its working
// directory, and returns that directory.
func consultPWD(t *testing.T, a *Adapter, req domain.LLMRequest) string {
	t.Helper()
	st, db := testStore(t)
	out := filepath.Join(t.TempDir(), "pwd.txt")
	a.CommandTemplate = []string{writeScript(t, "pwd > '"+out+"'\n")}
	a.Timeout = 5 * time.Second
	a.DBPath = db
	a.Store = st
	a.SelfPath = "/bin/true"

	req.RequestID = "req-agentcwd"
	req.CreatedAt = time.Now()
	ctx := context.Background()
	if _, err := st.StageLLMRequest(ctx, req); err != nil {
		t.Fatal(err)
	}
	if _, err := st.InsertLLMDecision(ctx, domain.LLMDecision{
		RequestID: req.RequestID, Action: "ok", Status: "pending", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Consult(ctx, req); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return resolve(t, string(data))
}

// TestRunDirPrefersALiveAgentCwd covers the decision itself: which directory a
// run starts in, given what herdr reported. Every rejection degrades to
// WorkDir() rather than failing, because a consult is advisory — an answer from
// the wrong directory beats a refused spawn that escalates a question nobody
// asked.
func TestRunDirPrefersALiveAgentCwd(t *testing.T) {
	live := liveDir(t, "project")
	dead := deadDir(t, "gone")
	db := filepath.Join(t.TempDir(), "t.db")
	regularFile := filepath.Join(live, "CLAUDE.md")
	if err := os.WriteFile(regularFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		enabled bool
		cwd     string
		want    string // "" means "whatever WorkDir() decides"
	}{
		{name: "live absolute dir is used", enabled: true, cwd: live, want: live},
		{name: "surrounding whitespace is trimmed", enabled: true, cwd: "  " + live + "\n", want: live},
		{name: "disabled ignores a live dir", enabled: false, cwd: live},
		{name: "empty falls back", enabled: true, cwd: ""},
		{name: "blank falls back", enabled: true, cwd: "   "},
		{name: "deleted dir falls back", enabled: true, cwd: dead},
		// herdr renders a deleted directory with this suffix, which only LOOKS
		// like a path — passing it through would spawn into a directory that
		// does not exist.
		{name: "herdr's (deleted) rendering falls back", enabled: true, cwd: live + " (deleted)"},
		// A relative path would resolve against the DAEMON's cwd — a different
		// directory entirely, silently.
		{name: "relative path falls back", enabled: true, cwd: "project"},
		{name: "a regular file is not a directory", enabled: true, cwd: regularFile},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := &Adapter{DBPath: db, RunInAgentCwd: tc.enabled}
			want := tc.want
			if want == "" {
				want = a.WorkDir()
			}
			if got := a.runDir(tc.cwd); got != want {
				t.Errorf("runDir(%q) = %q, want %q", tc.cwd, got, want)
			}
		})
	}
}

func TestConsultRunsInTheAgentsCwd(t *testing.T) {
	project := liveDir(t, "project")
	got := consultPWD(t, &Adapter{RunInAgentCwd: true}, domain.LLMRequest{Cwd: project})
	if want := resolve(t, project); got != want {
		t.Errorf("CLI ran in %q, want the agent's cwd %q", got, want)
	}
}

func TestConsultIgnoresTheAgentsCwdWhenDisabled(t *testing.T) {
	// The historical behavior: an operator who turns the key off keeps hap's
	// own directory even though the agent reported a perfectly good one.
	project := liveDir(t, "project")
	hapDir := liveDir(t, "hap-home")
	t.Chdir(hapDir)

	got := consultPWD(t, &Adapter{RunInAgentCwd: false}, domain.LLMRequest{Cwd: project})
	if want := resolve(t, hapDir); got != want {
		t.Errorf("CLI ran in %q, want hap's own dir %q", got, want)
	}
	if got == resolve(t, project) {
		t.Error("a disabled key must not run the CLI in the agent's cwd")
	}
}

func TestConsultFallsBackWhenTheAgentCwdIsUnusable(t *testing.T) {
	live := liveDir(t, "project")
	for _, tc := range []struct {
		name string
		cwd  string
	}{
		{name: "unreported", cwd: ""},
		{name: "deleted", cwd: deadDir(t, "gone")},
		{name: "herdr's (deleted) rendering", cwd: live + " (deleted)"},
		{name: "relative", cwd: "project"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hapDir := liveDir(t, "hap-home")
			t.Chdir(hapDir)
			got := consultPWD(t, &Adapter{RunInAgentCwd: true}, domain.LLMRequest{Cwd: tc.cwd})
			if want := resolve(t, hapDir); got != want {
				t.Errorf("CLI ran in %q, want the fallback %q", got, want)
			}
		})
	}
}

// TestFallbackUsesTheRealWorkDirChain is the case the other fallback tests
// cannot see. With a LIVE process cwd, WorkDir() returns "" and every rejected
// agent cwd asserts only `runDir(x) == ""` — which a broken runDir returning ""
// directly would also satisfy. Killing the daemon's own cwd makes WorkDir()'s
// fallback chain observable, so this pins that runDir really delegates to it.
// It is also the shape the daemon actually hits: a daemon outliving its launch
// directory is why WorkDir exists at all.
func TestFallbackUsesTheRealWorkDirChain(t *testing.T) {
	live := liveDir(t, "project")
	db := filepath.Join(t.TempDir(), "t.db")
	for _, tc := range []struct {
		name string
		cwd  string
	}{
		{name: "unreported", cwd: ""},
		{name: "deleted", cwd: deadDir(t, "gone")},
		{name: "herdr deleted rendering", cwd: live + " (deleted)"},
		{name: "relative", cwd: "project"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &Adapter{DBPath: db, RunInAgentCwd: true}
			chdirDeleted(t)
			if got, want := a.runDir(tc.cwd), filepath.Dir(db); got != want {
				t.Errorf("runDir(%q) = %q, want the state dir %q", tc.cwd, got, want)
			}
		})
	}
}

// TestConsultRunsInTheAgentsCwdWhenTheDaemonsOwnCwdIsDead pins the two
// mechanisms composing: the agent's directory is chosen on its own merits, not
// as a rescue from a dead daemon cwd, so it still wins when WorkDir() would
// have had to rescue the run.
func TestConsultRunsInTheAgentsCwdWhenTheDaemonsOwnCwdIsDead(t *testing.T) {
	project := liveDir(t, "project")
	req := domain.LLMRequest{Cwd: project}
	st, db := testStore(t)
	out := filepath.Join(t.TempDir(), "pwd.txt")
	a := &Adapter{
		CommandTemplate: []string{writeScript(t, "pwd > '"+out+"'\n")},
		Timeout:         5 * time.Second,
		DBPath:          db, Store: st, SelfPath: "/bin/true",
		RunInAgentCwd: true,
	}
	req.RequestID = "req-deadcwd"
	req.CreatedAt = time.Now()
	ctx := context.Background()
	if _, err := st.StageLLMRequest(ctx, req); err != nil {
		t.Fatal(err)
	}
	if _, err := st.InsertLLMDecision(ctx, domain.LLMDecision{
		RequestID: req.RequestID, Action: "ok", Status: "pending", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	chdirDeleted(t)
	if _, err := a.Consult(ctx, req); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := resolve(t, string(data)), resolve(t, project); got != want {
		t.Errorf("CLI ran in %q, want the agent's cwd %q", got, want)
	}
}

func TestGenerateTaskRunsInTheAgentsCwd(t *testing.T) {
	project := liveDir(t, "project")
	out := filepath.Join(t.TempDir(), "pwd.txt")
	a := &Adapter{
		TaskGenTemplate: []string{writeScript(t, "pwd > '"+out+"'\necho a task\n")},
		TaskGenTimeout:  5 * time.Second,
		RunInAgentCwd:   true,
	}
	if _, err := a.GenerateTask(context.Background(), domain.TaskGenRequest{Cwd: project}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := resolve(t, string(data)), resolve(t, project); got != want {
		t.Errorf("task generation ran in %q, want the agent's cwd %q", got, want)
	}
}

func TestGenerateTaskFallsBackWhenTheAgentCwdIsGone(t *testing.T) {
	hapDir := liveDir(t, "hap-home")
	t.Chdir(hapDir)
	out := filepath.Join(t.TempDir(), "pwd.txt")
	a := &Adapter{
		TaskGenTemplate: []string{writeScript(t, "pwd > '"+out+"'\necho a task\n")},
		TaskGenTimeout:  5 * time.Second,
		RunInAgentCwd:   true,
	}
	if _, err := a.GenerateTask(context.Background(), domain.TaskGenRequest{
		Cwd: deadDir(t, "gone"),
	}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := resolve(t, string(data)), resolve(t, hapDir); got != want {
		t.Errorf("task generation ran in %q, want the fallback %q", got, want)
	}
}

// TestLearnFromUserIgnoresRunInAgentCwd pins the exemption: learn-from-user
// edits a project's own memory file, so it always runs in the agent's directory
// and refuses when there is none. The key must not be able to redirect it into
// a stranger's project — nor into hap's.
func TestLearnFromUserIgnoresRunInAgentCwd(t *testing.T) {
	project := liveDir(t, "project")
	out := filepath.Join(t.TempDir(), "pwd.txt")
	a := &Adapter{
		LearnTemplate: []string{writeScript(t, "pwd > '"+out+"'\n")},
		LearnTimeout:  5 * time.Second,
		RunInAgentCwd: false,
	}
	if _, err := a.LearnFromUser(context.Background(), domain.LearnRequest{Cwd: project}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := resolve(t, string(data)), resolve(t, project); got != want {
		t.Errorf("learn-from-user ran in %q, want the agent's cwd %q", got, want)
	}

	// And it still refuses rather than falling back, with the key off.
	if _, err := a.LearnFromUser(context.Background(), domain.LearnRequest{}); err == nil {
		t.Error("learn-from-user must refuse to run without a live cwd")
	}
}

// TestLearnFromUserRefusesARelativeCwd is the shape a bare existence check waves
// through: "project" resolves against the DAEMON's cwd, so it names a real,
// live directory that is not the agent's — and this CLI holds write permission
// on the memory file it is told to edit. The test chdirs somewhere that really
// does contain a "project" subdirectory, so the refusal can only come from the
// absoluteness check.
func TestLearnFromUserRefusesARelativeCwd(t *testing.T) {
	project := liveDir(t, "project")
	t.Chdir(filepath.Dir(project))
	if !dirLives("project") {
		t.Fatal("setup: the relative path must resolve to a live directory")
	}

	out := filepath.Join(t.TempDir(), "pwd.txt")
	a := &Adapter{
		LearnTemplate: []string{writeScript(t, "pwd > '"+out+"'\n")},
		LearnTimeout:  5 * time.Second,
	}
	_, err := a.LearnFromUser(context.Background(), domain.LearnRequest{Cwd: "project"})
	if err == nil {
		t.Fatal("a relative cwd must be refused, not resolved against the daemon's cwd")
	}
	if !strings.Contains(err.Error(), "not absolute") {
		t.Errorf("error should name the absoluteness problem: %v", err)
	}
	if _, statErr := os.Stat(out); statErr == nil {
		t.Error("the CLI must not have been spawned at all")
	}
}
