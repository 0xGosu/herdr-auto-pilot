// Package tuisession tracks the live `hap tui` processes for one state
// directory and closes the older ones when too many run at once.
//
// Every TUI polls the same SQLite state on a 2s tick and shells out to herdr
// for pane metadata; each extra instance multiplies that work, and a handful of
// forgotten panes is enough to keep a core busy. The operator only ever reads
// one of them, so the newest wins: a starting TUI closes the oldest peers until
// at most `[tui] max_instances` remain (default 1).
//
// Liveness is proved by an advisory file lock, not by a pid probe: a session
// holds an exclusive flock on its registry file for its whole run, so a file
// whose lock is free belongs to a process that is gone and is pruned. That is
// what makes signalling safe — the pid we read always belongs to a live holder,
// so a recycled pid can never be mistaken for a TUI (the same guarantee
// daemonlock relies on). Live also requires the pid inside a record to match
// the pid in its file name, so the lock and the pid cannot come apart.
//
// One bound is deliberately left open. A record is created, locked, and only
// then written, so for the microseconds in between it is live but unparseable
// and Live skips it. Three TUIs starting at the same instant can therefore
// have a middle one briefly believe it is the newest and signal the oldest
// alongside the true newest — a duplicate SIGTERM. Closing it would mean
// treating any unreadable record as "possibly newer" and standing down, which
// hands a single corrupt file the power to disable the limit for good. The
// window is far smaller than that risk.
package tuisession

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// dirName is the registry directory under the state dir.
const dirName = "tui-sessions"

const (
	filePrefix = "session-"
	fileSuffix = ".lock"
)

// claimAttempts bounds the retries Register makes. A claim fails transiently
// while another process probes the same file (the probe takes the lock to test
// it) or prunes it, both of which resolve in microseconds.
const claimAttempts = 5

// claimRetryDelay spaces those retries.
var claimRetryDelay = 20 * time.Millisecond

// signalGrace is how long a peer gets to unwind before it is signalled again.
//
// It is load-bearing, not politeness. `cmd/hap` releases its signal handler on
// the FIRST signal so a wedged process stays killable, which means a second
// SIGTERM inside the exit path kills the TUI where it stands — no terminal
// restore, no store close — and that exit path is not instant: bubbletea's
// teardown is followed by a bounded wait for in-flight commands. (The
// submit-retry drain is NOT among the stakes: cmd/hap documents that those
// workers run on the context the signal already cancelled, so any signalled
// exit forfeits it.) An orderly exit must therefore never be interrupted by
// the next sweep, and the sweeps are at least 10s apart — further apart once a
// TUI backs off its idle poll, which only widens the margin. Past the grace the peer has
// plainly ignored the request, and signalling again — now fatal — is the
// right escalation.
const signalGrace = 60 * time.Second

// Info identifies one live TUI session.
type Info struct {
	Pid int
	// Started is when the session registered. It orders sessions, so
	// "oldest" means "registered first", not "lowest pid".
	Started time.Time
	// Identity names the process space the pid belongs to (see
	// processIdentity). A state dir reached from two pid namespaces — a
	// container and its host sharing a bind-mounted home — is the one case
	// where a live record's pid names a different process to us, so a peer
	// whose identity differs is left entirely alone. Empty means unknown,
	// which compares equal to everything rather than stranding the limit.
	Identity string
	// Path is the registry file backing the session.
	Path string
}

// signalKey identifies one session for the already-signalled bookkeeping.
// Keying on the start time as well as the pid keeps a recycled pid from
// inheriting its predecessor's grace period.
type signalKey struct {
	pid     int
	started int64
}

func keyOf(i Info) signalKey { return signalKey{pid: i.Pid, started: i.Started.UnixNano()} }

// Dir returns the registry directory for a state dir.
func Dir(stateDir string) string { return filepath.Join(stateDir, dirName) }

// Session is this process's entry in the registry. It stays valid until
// Release; a nil *Session is inert, so callers that could not register (a
// read-only state dir, say) need no special case — the TUI simply runs
// without enforcing the limit.
type Session struct {
	dir  string
	self Info
	file *os.File
	// stop signals a peer to shut down; nil means SIGTERM. Tests inject it.
	stop func(pid int) error
	// now is the clock the grace period is measured on; nil means time.Now.
	now func() time.Time

	// list reads the registry; nil means the real Live. Tests inject it to
	// exercise a sweep that misses a peer it can still see the next time.
	list func(dir string) ([]Info, error)

	// mu guards signalled and released. Enforce runs on the front-end's
	// refresh goroutine, and nothing promises there is only ever one of those.
	mu sync.Mutex
	// signalled records when each peer was last asked to exit.
	signalled map[signalKey]time.Time
	released  bool
	// sweeping is held for the length of one Enforce. The front-end throttles
	// by start time, which bounds how often a sweep BEGINS but not how long
	// one takes — Live is per-file open/flock/read I/O and can block on a
	// network home — so without this two overlapping sweeps in this one
	// process could both find a peer unsignalled and both signal it, which is
	// the fatal second SIGTERM signalGrace exists to prevent.
	sweeping bool
}

// Register claims a session slot for this process.
func Register(stateDir string) (*Session, error) {
	dir := Dir(stateDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create the TUI session registry %s: %w", dir, err)
	}
	pid := os.Getpid()
	path := filepath.Join(dir, filePrefix+strconv.Itoa(pid)+fileSuffix)
	self := Info{Pid: pid, Identity: processIdentity(), Path: path}
	var lastErr error
	for attempt := range claimAttempts {
		if attempt > 0 {
			time.Sleep(claimRetryDelay)
		}
		f, err := claim(path)
		if err != nil {
			if errors.Is(err, errUnsupported) {
				// No file locks here (Windows): retrying only delays the TUI.
				return nil, fmt.Errorf("claim a TUI session slot in %s: %w", dir, err)
			}
			lastErr = err
			continue
		}
		self.Started = time.Now()
		if err := write(f, self); err != nil {
			f.Close()
			lastErr = err
			continue
		}
		// A prune that ran between our open and our lock would have unlinked
		// the file we now hold, leaving this session invisible to every peer
		// (and so never counted, never closed). Confirm the path still names
		// our inode before trusting the claim.
		if !stillLinked(f, path) {
			release(f)
			lastErr = errors.New("session file was pruned during registration")
			continue
		}
		return &Session{dir: dir, self: self, file: f, signalled: map[signalKey]time.Time{}}, nil
	}
	return nil, fmt.Errorf("claim a TUI session slot in %s: %w", dir, lastErr)
}

// Release drops the session, so peers stop counting it.
func (s *Session) Release() {
	if s == nil || s.file == nil {
		return
	}
	// Flag FIRST, then let go of the record. The other order leaves a window —
	// the unlock and unlink syscalls — in which a concurrent sweep sees a
	// session that is not released yet but is already invisible in the
	// registry, and signals a peer on our way out.
	s.mu.Lock()
	s.released = true
	s.mu.Unlock()
	release(s.file)
	s.file = nil
	// Best-effort: a leftover file is pruned by the next reader anyway (its
	// lock is free), so a failed unlink is not worth surfacing.
	_ = os.Remove(s.self.Path)
}

// beginSweep claims the single-sweep slot, reporting false when one is already
// in flight.
func (s *Session) beginSweep() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sweeping {
		return false
	}
	s.sweeping = true
	return true
}

func (s *Session) endSweep() {
	s.mu.Lock()
	s.sweeping = false
	s.mu.Unlock()
}

// done reports that Release has run. The front-end's refresh runs on its own
// goroutine and can still be in flight while the TUI unwinds (Release is
// deferred last in cmd/hap, so it runs FIRST), and a released session that
// still enforced would re-add itself in Surplus and close a peer on its way
// out — leaving the operator with no TUI at all.
func (s *Session) done() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.released
}

// Self reports this session's own identity.
func (s *Session) Self() Info {
	if s == nil {
		return Info{}
	}
	return s.self
}

// Enforce asks the oldest peer sessions to exit until at most max remain live,
// and returns the pids it signalled this sweep. max <= 0 disables the limit —
// but the sweep still runs far enough to prune the records of TUIs that are
// gone, so an unlimited registry does not accumulate forever.
//
// ONLY THE NEWEST live session signals, and it only ever signals peers older
// than itself. Both halves matter. The second is what keeps two TUIs starting
// at the same moment from closing each other — the ordering is total, so the
// newest survives whichever runs first. The first is what keeps the grace
// period meaningful: it is per-process state, so with three TUIs the middle one
// and the newest would each independently SIGTERM the oldest, and the second of
// those two signals arrives after `cmd/hap` has released its handler — killing
// a TUI in the middle of its exit path, which is exactly what signalGrace
// exists to prevent. Standing down costs nothing: the newest closes everyone
// past the limit, and if it ever goes away the next-newest takes over.
func (s *Session) Enforce(max int) ([]int, error) {
	if s == nil {
		return nil, nil
	}
	if !s.beginSweep() {
		return nil, nil
	}
	defer s.endSweep()
	listLive := s.list
	if listLive == nil {
		listLive = Live
	}
	live, err := listLive(s.dir)
	if err != nil {
		return nil, err
	}
	// Peers from another pid space share the directory but not the meaning of
	// a pid, so they are neither counted nor signalled.
	live = slices.DeleteFunc(live, func(i Info) bool {
		if !foreign(i, s.self) {
			return false
		}
		slog.Debug("ignoring a TUI session from another process space",
			"pid", i.Pid, "peer_identity", i.Identity, "self_identity", s.self.Identity)
		return true
	})
	if s.done() {
		// This TUI is already unwinding; nothing it does now can help, and
		// signalling on the way out could leave the operator with none at all.
		return nil, nil
	}
	if max <= 0 {
		s.forgetDeparted(live)
		return nil, nil
	}
	if slices.ContainsFunc(live, func(i Info) bool { return older(s.self, i) }) {
		s.forgetDeparted(live)
		return nil, nil
	}
	stop := s.stop
	if stop == nil {
		stop = signalStop
	}
	s.forgetDeparted(live)
	var closed []int
	for _, doomed := range Surplus(live, s.self, max) {
		if since, waiting := s.awaitingExit(doomed); waiting {
			// Signalling again here would be fatal rather than graceful (see
			// signalGrace), and this peer is still inside its exit path.
			slog.Debug("older hap TUI is still exiting", "pid", doomed.Pid, "asked_ago", since)
			continue
		}
		if s.done() {
			// Released while this sweep was reading the registry.
			return closed, nil
		}
		if err := stop(doomed.Pid); err != nil {
			// A peer we cannot signal is left alone; the next sweep retries.
			slog.Warn("could not close an older hap TUI", "pid", doomed.Pid, "error", err)
			continue
		}
		s.markSignalled(doomed)
		slog.Info("asked an older hap TUI to close, to stay within the instance limit",
			"pid", doomed.Pid, "max_instances", max)
		closed = append(closed, doomed.Pid)
	}
	return closed, nil
}

// clock is the time source the grace period is measured on.
func (s *Session) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// awaitingExit reports whether this peer was already asked to exit recently,
// and how long ago.
func (s *Session) awaitingExit(peer Info) (time.Duration, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	at, ok := s.signalled[keyOf(peer)]
	if !ok {
		return 0, false
	}
	since := s.clock().Sub(at)
	return since, since < signalGrace
}

func (s *Session) markSignalled(peer Info) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.signalled == nil {
		s.signalled = map[signalKey]time.Time{}
	}
	s.signalled[keyOf(peer)] = s.clock()
}

// forgetDeparted drops bookkeeping for sessions that are gone AND out of their
// grace, so the map cannot grow across a long-lived TUI's many peers.
//
// Absence alone is not enough to forget one. Live skips a record it cannot
// probe or parse — fd exhaustion, a mid-write file — which is exactly the state
// a struggling TUI is in, and forgetting it there would let the next sweep
// re-signal it inside its exit path. The grace bounds the map either way.
func (s *Session) forgetDeparted(live []Info) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.signalled) == 0 {
		return
	}
	present := make(map[signalKey]bool, len(live))
	for _, i := range live {
		present[keyOf(i)] = true
	}
	now := s.clock()
	for k, at := range s.signalled {
		if !present[k] && now.Sub(at) >= signalGrace {
			delete(s.signalled, k)
		}
	}
}

// foreign reports whether peer's pid belongs to a different process space than
// self's. Only a KNOWN mismatch counts: an unknown identity on either side
// degrades to trusting the pid, which is what every previous build did.
func foreign(peer, self Info) bool {
	return peer.Identity != "" && self.Identity != "" && peer.Identity != self.Identity
}

// Live lists the sessions currently holding their lock, oldest first, and
// prunes the records of processes that are gone.
func Live(dir string) ([]Info, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the TUI session registry %s: %w", dir, err)
	}
	var out []Info
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, filePrefix) || !strings.HasSuffix(name, fileSuffix) {
			continue
		}
		path := filepath.Join(dir, name)
		locked, err := held(path)
		if err != nil {
			// Unreadable: treat it as none of our business rather than
			// guessing a pid to signal.
			continue
		}
		if !locked {
			_ = os.Remove(path)
			continue
		}
		info, err := read(path)
		if err != nil || info.Pid <= 1 {
			// Live but not yet parseable (registration writes its lines after
			// taking the lock). Skipping it means this sweep neither counts nor
			// closes it; the next one will.
			continue
		}
		// The lock proves a live holder for this FILE; the pid comes from the
		// file's contents. Requiring the two to agree is what ties the two
		// facts together — otherwise a corrupt or hand-edited record could aim
		// a signal at any pid in the operator's session.
		if named, ok := pidFromName(name); !ok || named != info.Pid {
			continue
		}
		info.Path = path
		out = append(out, info)
	}
	slices.SortFunc(out, byAge)
	return out, nil
}

// pidFromName reads the pid a registry file name claims.
func pidFromName(name string) (int, bool) {
	digits := strings.TrimSuffix(strings.TrimPrefix(name, filePrefix), fileSuffix)
	pid, err := strconv.Atoi(digits)
	return pid, err == nil
}

// byAge orders sessions oldest-first, with the pid as a tie-break so the order
// is total (two sessions can share a timestamp on a coarse clock). It is the
// same comparison older() makes, and the two must stay in step: Surplus takes a
// prefix of this order and then re-checks each entry against self with older().
func byAge(a, b Info) int {
	if !a.Started.Equal(b.Started) {
		return a.Started.Compare(b.Started)
	}
	return cmp.Compare(a.Pid, b.Pid)
}

// Surplus picks the sessions self should close so that at most max remain. It
// returns them oldest first, and never includes self or a session newer than
// self.
func Surplus(live []Info, self Info, max int) []Info {
	if max <= 0 {
		return nil
	}
	ordered := slices.Clone(live)
	if !slices.ContainsFunc(ordered, func(i Info) bool { return i.Pid == self.Pid }) {
		// Our own record was missed (mid-write, or an unreadable file). Count
		// ourselves anyway: the limit is about how many TUIs the operator has
		// open, and we are one of them.
		ordered = append(ordered, self)
		slices.SortFunc(ordered, byAge)
	}
	surplus := len(ordered) - max
	if surplus <= 0 {
		return nil
	}
	var out []Info
	for _, c := range ordered[:surplus] {
		if c.Pid == self.Pid || c.Pid <= 1 {
			continue
		}
		if !older(c, self) {
			continue
		}
		out = append(out, c)
	}
	return out
}

// older reports whether a registered before b, with the pid as a tie-break so
// the ordering is total (two sessions can share a timestamp on a coarse clock).
func older(a, b Info) bool {
	if !a.Started.Equal(b.Started) {
		return a.Started.Before(b.Started)
	}
	return a.Pid < b.Pid
}

// write records the holder: pid, the registration time as Unix nanoseconds,
// and the process space that pid means something in.
func write(f *os.File, self Info) error {
	if err := f.Truncate(0); err != nil {
		return fmt.Errorf("truncate session file: %w", err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		return fmt.Errorf("rewind session file: %w", err)
	}
	if _, err := fmt.Fprintf(f, "%d\n%d\n%s\n", self.Pid, self.Started.UnixNano(), self.Identity); err != nil {
		return fmt.Errorf("write session file: %w", err)
	}
	return f.Sync()
}

// read parses a session file. A short or malformed file is an error, which
// callers treat as "skip this record for now". A missing identity line is not
// malformed — it reads as unknown, and foreign() then trusts the pid.
func read(path string) (Info, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Info{}, err
	}
	lines := strings.SplitN(string(data), "\n", 4)
	if len(lines) < 2 {
		return Info{}, fmt.Errorf("session file %s is incomplete", path)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(lines[0]))
	if err != nil {
		return Info{}, fmt.Errorf("session file %s has no pid: %w", path, err)
	}
	nanos, err := strconv.ParseInt(strings.TrimSpace(lines[1]), 10, 64)
	if err != nil {
		return Info{}, fmt.Errorf("session file %s has no start time: %w", path, err)
	}
	info := Info{Pid: pid, Started: time.Unix(0, nanos), Path: path}
	if len(lines) > 2 {
		info.Identity = strings.TrimSpace(lines[2])
	}
	return info, nil
}

// stillLinked reports whether path still names the file we hold open.
func stillLinked(f *os.File, path string) bool {
	held, err := f.Stat()
	if err != nil {
		return false
	}
	linked, err := os.Stat(path)
	if err != nil {
		return false
	}
	return os.SameFile(held, linked)
}
