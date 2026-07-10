//go:build !windows

package daemonlock

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/0xGosu/herdr-auto-pilot/internal/buildinfo"
	"github.com/0xGosu/herdr-auto-pilot/internal/config"
)

// Lock is a flock-based singleton guard so only one daemon monitors the
// herd per state directory.
type Lock struct {
	f *os.File
}

func lockPath(paths config.Paths) string {
	return filepath.Join(paths.StateDir, "daemon.lock")
}

// Acquire takes the daemon lock and records this process's pid and version,
// so --ensure and `hap status` can identify the holder.
func Acquire(paths config.Paths) (*Lock, error) {
	f, err := os.OpenFile(lockPath(paths), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("another daemon already holds the lock: %w", err)
	}
	// O_RDWR keeps longer content from a previous holder around; clear it
	// so a shorter pid/version pair isn't followed by stale bytes.
	if err := f.Truncate(0); err != nil {
		f.Close()
		return nil, fmt.Errorf("truncate lock file: %w", err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		f.Close()
		return nil, fmt.Errorf("rewind lock file: %w", err)
	}
	// An unwritten pid would permanently dead-end every future --ensure in
	// its "pid unreadable" branch, so a failed write must fail the acquire.
	if _, err := fmt.Fprintf(f, "%d\n%s\n", os.Getpid(), buildinfo.Version); err != nil {
		f.Close()
		return nil, fmt.Errorf("write lock file: %w", err)
	}
	return &Lock{f: f}, nil
}

// Release drops the lock.
func (l *Lock) Release() {
	if l != nil && l.f != nil {
		syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
		l.f.Close()
	}
}

// Info reports whether a daemon holds the lock, and the pid/version it
// recorded. flock semantics guarantee the content was written by the live
// holder (a dead holder releases the lock), so pid recycling is not a
// concern. version is "" for holders older than the pid+version format;
// pid is 0 when unreadable.
func Info(paths config.Paths) (running bool, pid int, version string) {
	f, err := os.OpenFile(lockPath(paths), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return false, 0, ""
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		return false, 0, ""
	}
	// Read through the already-open fd: re-opening the path could race a
	// holder that just replaced the file.
	data, err := io.ReadAll(f)
	if err != nil {
		return true, 0, ""
	}
	lines := strings.SplitN(string(data), "\n", 3)
	pid, _ = strconv.Atoi(strings.TrimSpace(lines[0]))
	if len(lines) > 1 {
		version = strings.TrimSpace(lines[1])
	}
	return true, pid, version
}

// Stop asks a daemon to shut down gracefully (SIGTERM cancels its run
// context). A pid that is already gone is not an error.
func Stop(pid int) error {
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

// Detach configures cmd to run in its own session, surviving hook exit.
func Detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
