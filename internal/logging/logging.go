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

// Options configures Setup. The zero value is the historical behaviour: Info
// level, LogCap-sized rotation.
type Options struct {
	// Level is the minimum severity written. Zero means slog.LevelInfo.
	Level slog.Level
	// MaxSize caps the log file before rotation. <= 0 means LogCap.
	MaxSize int64
}

// Setup configures the default slog logger. When logDir is non-empty, logs
// go to a file inside it (the TUI owns the terminal); otherwise stderr.
//
// Every hap process appends to the same file, so the level is per-process: the
// TUI and the daemon can be configured together but are set up separately.
func Setup(logDir string, opt Options) (*slog.Logger, error) {
	max := opt.MaxSize
	if max <= 0 {
		max = LogCap
	}
	var w io.Writer = os.Stderr
	if logDir != "" {
		if err := os.MkdirAll(logDir, 0o700); err != nil {
			return nil, err
		}
		f, err := newRotatingFile(filepath.Join(logDir, "herd-auto-prompter.log"), max)
		if err != nil {
			return nil, err
		}
		w = f
	}
	logger := slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: opt.Level}))
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
