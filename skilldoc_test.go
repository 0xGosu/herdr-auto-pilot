package skilldoc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The embed path names the file explicitly, so a move or rename of
// .claude/skills/hap/SKILL.md fails the build — this guards the content
// being the hap skill at all.
func TestHapSkillIsEmbedded(t *testing.T) {
	if len(HapSkill) == 0 {
		t.Fatal("embedded skill document is empty")
	}
	if !strings.Contains(HapSkill, "name: hap") {
		t.Fatalf("embedded document does not carry the hap skill frontmatter:\n%.200s", HapSkill)
	}
}

func TestInstallToWritesEverySelectedTarget(t *testing.T) {
	home := t.TempDir()
	written, err := InstallTo(home, []string{"claude", "codex", "agents"})
	if err != nil {
		t.Fatalf("InstallTo: %v", err)
	}
	want := []string{
		filepath.Join(home, ".claude", "skills", "hap", "SKILL.md"),
		filepath.Join(home, ".codex", "skills", "hap", "SKILL.md"),
		filepath.Join(home, ".agents", "skills", "hap", "SKILL.md"),
	}
	if len(written) != len(want) {
		t.Fatalf("written = %v, want %v", written, want)
	}
	for i, path := range want {
		if written[i] != path {
			t.Errorf("written[%d] = %q, want %q", i, written[i], path)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if string(got) != HapSkill {
			t.Errorf("%s content differs from the embedded document", path)
		}
	}
}

func TestInstallToOverwritesAnExistingInstall(t *testing.T) {
	home := t.TempDir()
	dest := filepath.Join(home, ".claude", "skills", "hap", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, []byte("an older release's skill"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := InstallTo(home, []string{"claude"}); err != nil {
		t.Fatalf("InstallTo over an existing file: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != HapSkill {
		t.Error("existing install was not replaced with the embedded document")
	}
}

// An unknown name fails before ANY target is written, so a typo in a list
// never half-installs.
func TestInstallToRefusesAnUnknownTargetBeforeWriting(t *testing.T) {
	home := t.TempDir()
	_, err := InstallTo(home, []string{"claude", "cursor"})
	if err == nil || !strings.Contains(err.Error(), `unknown install target "cursor"`) {
		t.Fatalf("expected an unknown-target error naming the valid set, got %v", err)
	}
	if !strings.Contains(err.Error(), "claude, codex, agents") {
		t.Errorf("error should list the valid targets, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(home, ".claude")); !os.IsNotExist(statErr) {
		t.Errorf("the valid target was written despite the refusal (stat err = %v)", statErr)
	}
}

func TestInstallToRequiresATarget(t *testing.T) {
	_, err := InstallTo(t.TempDir(), nil)
	if err == nil || !strings.Contains(err.Error(), "no install target named") {
		t.Fatalf("expected a no-target error, got %v", err)
	}
}
