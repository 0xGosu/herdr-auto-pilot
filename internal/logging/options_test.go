package logging

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSetupHonoursLevel(t *testing.T) {
	dir := t.TempDir()
	logger, err := Setup(dir, Options{Level: slog.LevelWarn})
	if err != nil {
		t.Fatal(err)
	}
	logger.Info("suppressed")
	logger.Warn("kept")

	data, err := os.ReadFile(filepath.Join(dir, "herd-auto-prompter.log"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "suppressed") {
		t.Errorf("Info written at Warn level, got %q", data)
	}
	if !strings.Contains(string(data), "kept") {
		t.Errorf("Warn not written at Warn level, got %q", data)
	}
}

// TestSetupHonoursMaxSize: the configured cap, not LogCap, is what rotates.
func TestSetupHonoursMaxSize(t *testing.T) {
	dir := t.TempDir()
	logger, err := Setup(dir, Options{MaxSize: 2 << 10}) // 2 KiB
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 200; i++ {
		logger.Info("filler", "pad", strings.Repeat("x", 64))
	}
	if _, err := os.Stat(filepath.Join(dir, "herd-auto-prompter.log.old")); err != nil {
		t.Fatalf("a 2 KiB cap must have rotated by now: %v", err)
	}
	fi, err := os.Stat(filepath.Join(dir, "herd-auto-prompter.log"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() > 2<<10 {
		t.Errorf("live log is %d bytes, past the 2 KiB cap", fi.Size())
	}
}

func TestSetupZeroOptionsKeepsHistoricalDefaults(t *testing.T) {
	dir := t.TempDir()
	logger, err := Setup(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	logger.Info("info-is-default")
	data, err := os.ReadFile(filepath.Join(dir, "herd-auto-prompter.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "info-is-default") {
		t.Errorf("the zero Options must log at Info, got %q", data)
	}
}

func TestWarnOnceLatchesPerKey(t *testing.T) {
	ResetWarnOnceForTest()
	t.Cleanup(ResetWarnOnceForTest)

	dir := t.TempDir()
	if _, err := Setup(dir, Options{}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		WarnOnce("k1", "first condition")
		WarnOnce("k2", "second condition")
	}
	data, err := os.ReadFile(filepath.Join(dir, "herd-auto-prompter.log"))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(data), "first condition"); n != 1 {
		t.Errorf("key k1 logged %d times, want 1", n)
	}
	if n := strings.Count(string(data), "second condition"); n != 1 {
		t.Errorf("key k2 logged %d times, want 1", n)
	}
}
