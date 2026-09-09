//go:build unix

package fdprobe

import (
	"errors"
	"os"
	"syscall"
)

func read() Probe {
	var p Probe
	var rl syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rl); err == nil {
		p.Soft, p.Hard = uint64(rl.Cur), uint64(rl.Max)
	}
	// /proc/self/fd exists on Linux and not on macOS. Absence is "unavailable",
	// which is why OpenKnown is a separate field: a zero Open on darwin must
	// never read as "no descriptors are open".
	if names, err := readDirNames("/proc/self/fd"); err == nil {
		// One of the entries is the handle the read itself opened; it is closed
		// by the time a caller sees this, so discount it rather than report a
		// count that is always one high.
		if names > 0 {
			names--
		}
		p.Open, p.OpenKnown = names, true
	}
	p.Exhausted = exhausted()
	return p
}

// readDirNames counts the entries in dir. A partial read is still a useful
// order-of-magnitude answer, so a mid-read error returns what was counted
// rather than nothing.
func readDirNames(dir string) (int, error) {
	f, err := os.Open(dir)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	names, err := f.Readdirnames(-1)
	if err != nil && len(names) == 0 {
		return 0, err
	}
	return len(names), nil
}

// exhausted asks the kernel directly: can this process open one more
// descriptor? A refusal with EMFILE (per-process) or ENFILE (system-wide) is
// the proof; any other error is not evidence and answers false.
func exhausted() bool {
	f, err := os.Open(os.DevNull)
	if err == nil {
		f.Close()
		return false
	}
	return errors.Is(err, syscall.EMFILE) || errors.Is(err, syscall.ENFILE)
}
