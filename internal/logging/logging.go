// Package logging wires slog structured logging and the daemon-path
// fail-safe guard: no panics may escape the daemon (NFR-004).
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
)

// Setup configures the default slog logger. When logDir is non-empty, logs
// go to a file inside it (the TUI owns the terminal); otherwise stderr.
func Setup(logDir string, debug bool) (*slog.Logger, error) {
	var w io.Writer = os.Stderr
	if logDir != "" {
		if err := os.MkdirAll(logDir, 0o700); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(filepath.Join(logDir, "herd-auto-prompter.log"),
			os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, err
		}
		w = f
	}
	level := slog.LevelInfo
	if debug {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)
	return logger, nil
}

// Guard runs fn and converts any panic into an error, so faults at adapter
// boundaries resolve to escalate/log instead of crashing the daemon.
func Guard(name string, fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("recovered panic in %s: %v", name, r)
			slog.Error("panic recovered on daemon path", "component", name, "panic", fmt.Sprint(r))
		}
	}()
	return fn()
}
