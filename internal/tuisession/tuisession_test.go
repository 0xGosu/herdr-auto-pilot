package tuisession

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"
)

// fakePid returns a pid that is certainly not this process's, so a fabricated
// peer can never collide with the session Register claims for us.
func fakePid(n int) int { return os.Getpid()*100 + n }

// peer fabricates another TUI's registry entry and holds its lock for the rest
// of the test, so Live sees it as live exactly the way a real peer is seen (the
// lock is per open file, so our own process holding it still reads as taken).
func peer(t *testing.T, dir string, pid int, started time.Time) Info {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir registry: %v", err)
	}
	path := filepath.Join(dir, filePrefix+strconv.Itoa(pid)+fileSuffix)
	info := Info{Pid: pid, Started: started, Identity: processIdentity(), Path: path}
	f, err := claim(path)
	if err != nil {
		t.Fatalf("claim peer %d: %v", pid, err)
	}
	if err := write(f, info); err != nil {
		t.Fatalf("write peer %d: %v", pid, err)
	}
	t.Cleanup(func() { release(f) })
	return info
}

// A registered session is visible to peers, and its record disappears again on
// Release — otherwise a closed TUI would keep counting against the limit.
func TestRegisterIsVisibleThenReleased(t *testing.T) {
	state := t.TempDir()
	s, err := Register(state)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	live, err := Live(Dir(state))
	if err != nil {
		t.Fatalf("Live: %v", err)
	}
	if len(live) != 1 || live[0].Pid != os.Getpid() {
		t.Fatalf("want one live session for pid %d, got %+v", os.Getpid(), live)
	}
	if s.Self().Pid != os.Getpid() {
		t.Errorf("Self() pid = %d, want %d", s.Self().Pid, os.Getpid())
	}
	s.Release()
	if live, err = Live(Dir(state)); err != nil || len(live) != 0 {
		t.Fatalf("after Release want no live sessions, got %+v (err %v)", live, err)
	}
}

// Register survives a peer's prune sweep unlinking the file between our open
// and our lock: the retry re-creates it, so the session is never invisible.
func TestRegisterRetriesWhenItsFileIsPruned(t *testing.T) {
	state := t.TempDir()
	s, err := Register(state)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	defer s.Release()
	// Simulate the lost race by removing the file behind our back, then
	// confirm a fresh Register (same pid) still ends up visible.
	if err := os.Remove(s.Self().Path); err != nil {
		t.Fatalf("remove session file: %v", err)
	}
	if live, _ := Live(Dir(state)); len(live) != 0 {
		t.Fatalf("an unlinked record should not be listed, got %+v", live)
	}
	s.Release()
	s2, err := Register(state)
	if err != nil {
		t.Fatalf("re-Register: %v", err)
	}
	defer s2.Release()
	if live, _ := Live(Dir(state)); len(live) != 1 {
		t.Fatalf("want the re-registered session listed, got %+v", live)
	}
}

// A record whose process is gone (its lock is free) is pruned, never counted
// and never signalled — that is what keeps a recycled pid safe.
func TestLivePrunesRecordsWithoutAHolder(t *testing.T) {
	dir := Dir(t.TempDir())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, filePrefix+"424242"+fileSuffix)
	if err := os.WriteFile(stale, []byte("424242\n"+strconv.FormatInt(time.Now().UnixNano(), 10)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	live, err := Live(dir)
	if err != nil {
		t.Fatalf("Live: %v", err)
	}
	if len(live) != 0 {
		t.Fatalf("a dead record must not be live, got %+v", live)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("dead record should have been pruned, stat err = %v", err)
	}
}

// A live record that is not yet parseable (registration writes after locking)
// is skipped rather than guessed at.
func TestLiveSkipsAnUnparseableRecord(t *testing.T) {
	dir := Dir(t.TempDir())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, filePrefix+"777"+fileSuffix)
	f, err := claim(path)
	if err != nil {
		t.Fatal(err)
	}
	defer release(f)
	live, err := Live(dir)
	if err != nil {
		t.Fatalf("Live: %v", err)
	}
	if len(live) != 0 {
		t.Fatalf("an empty (mid-write) record must be skipped, got %+v", live)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("a LOCKED record must never be pruned: %v", err)
	}
}

// Live orders sessions oldest-first, which is the order Enforce closes them in.
func TestLiveOrdersOldestFirst(t *testing.T) {
	dir := Dir(t.TempDir())
	base := time.Now()
	first, second, third := fakePid(1), fakePid(2), fakePid(3)
	peer(t, dir, third, base.Add(2*time.Second))
	peer(t, dir, first, base)
	peer(t, dir, second, base.Add(time.Second))
	live, err := Live(dir)
	if err != nil {
		t.Fatalf("Live: %v", err)
	}
	got := make([]int, 0, len(live))
	for _, i := range live {
		got = append(got, i.Pid)
	}
	if want := []int{first, second, third}; !slices.Equal(got, want) {
		t.Fatalf("Live order = %v, want %v", got, want)
	}
}

// Enforce closes the oldest peers so that exactly max remain, and reports the
// pids it signalled.
func TestEnforceClosesTheOldestPeers(t *testing.T) {
	state := t.TempDir()
	dir := Dir(state)
	base := time.Now()
	oldest, middle := fakePid(1), fakePid(2)
	peer(t, dir, oldest, base.Add(-3*time.Minute))
	peer(t, dir, middle, base.Add(-2*time.Minute))
	s, err := Register(state)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	defer s.Release()
	var signalled []int
	s.stop = func(pid int) error { signalled = append(signalled, pid); return nil }

	closed, err := s.Enforce(1)
	if err != nil {
		t.Fatalf("Enforce: %v", err)
	}
	if want := []int{oldest, middle}; !slices.Equal(closed, want) || !slices.Equal(signalled, want) {
		t.Fatalf("closed = %v, signalled = %v, want %v for both", closed, signalled, want)
	}
}

// Only the NEWEST session signals. With three TUIs live, a middle one that also
// enforced would send the oldest a second SIGTERM from a second process — and
// the grace is per-process, so it could not know. `cmd/hap` releases its signal
// handler after the first signal, so that second one kills the oldest where it
// stands, with no terminal restore and no store close.
func TestEnforceOnlyTheNewestSessionSignals(t *testing.T) {
	state := t.TempDir()
	dir := Dir(state)
	base := time.Now()
	oldest := fakePid(1)
	// self is the MIDDLE session: one peer older, one newer.
	peer(t, dir, oldest, base.Add(-3*time.Minute))
	s, err := Register(state)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	defer s.Release()
	newest := peer(t, dir, fakePid(3), s.Self().Started.Add(time.Second))
	s.stop = func(pid int) error {
		t.Fatalf("a middle session signalled pid %d; only the newest may", pid)
		return nil
	}
	closed, err := s.Enforce(1)
	if err != nil {
		t.Fatalf("Enforce: %v", err)
	}
	if len(closed) != 0 {
		t.Fatalf("closed = %v, want nothing while a newer TUI is live", closed)
	}
	_ = newest
}

// The newest one does the work — for every surplus session, in one sweep.
func TestEnforceNewestClosesEveryOlderSession(t *testing.T) {
	state := t.TempDir()
	dir := Dir(state)
	base := time.Now()
	first, second := fakePid(1), fakePid(2)
	peer(t, dir, first, base.Add(-3*time.Minute))
	peer(t, dir, second, base.Add(-2*time.Minute))
	s, err := Register(state) // registers now, so it is the newest
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	defer s.Release()
	var signalled []int
	s.stop = func(pid int) error { signalled = append(signalled, pid); return nil }
	if _, err := s.Enforce(1); err != nil {
		t.Fatalf("Enforce: %v", err)
	}
	if want := []int{first, second}; !slices.Equal(signalled, want) {
		t.Fatalf("signalled = %v, want %v", signalled, want)
	}
}

// A released session never signals: its own exit path can still run a refresh,
// and closing a peer on the way out would leave the operator with no TUI.
func TestReleasedSessionStopsEnforcing(t *testing.T) {
	state := t.TempDir()
	dir := Dir(state)
	peer(t, dir, fakePid(1), time.Now().Add(-time.Minute))
	s, err := Register(state)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	s.stop = func(pid int) error {
		t.Fatalf("a released session signalled pid %d", pid)
		return nil
	}
	s.Release()
	closed, err := s.Enforce(1)
	if err != nil {
		t.Fatalf("Enforce after Release: %v", err)
	}
	if len(closed) != 0 {
		t.Fatalf("closed = %v after Release, want nothing", closed)
	}
}

// A record whose contents disagree with its file name is not a session we can
// reason about — the lock proves a holder for the FILE, the pid comes from the
// contents, and only their agreement ties the two together.
func TestLiveSkipsARecordWhosePidDisagreesWithItsName(t *testing.T) {
	dir := Dir(t.TempDir())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, filePrefix+"4242"+fileSuffix)
	f, err := claim(path)
	if err != nil {
		t.Fatal(err)
	}
	defer release(f)
	// Live and locked, but pointing at somebody else's pid.
	if err := write(f, Info{Pid: 9999, Started: time.Now(), Identity: processIdentity()}); err != nil {
		t.Fatal(err)
	}
	live, err := Live(dir)
	if err != nil {
		t.Fatalf("Live: %v", err)
	}
	if len(live) != 0 {
		t.Fatalf("live = %+v, want the mismatched record skipped", live)
	}
}

// Registering is best-effort: an unusable state dir must return an error (so
// cmd/hap logs and runs without the limit), never panic or half-succeed.
func TestRegisterFailsCleanlyOnAnUnusableStateDir(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Register(filepath.Join(blocker, "state"))
	if err == nil {
		s.Release()
		t.Fatal("want an error when the registry cannot be created")
	}
	if s != nil {
		t.Errorf("want a nil session alongside the error, got %+v", s)
	}
}

// With room to spare — or with the limit turned off — Enforce closes nothing.
func TestEnforceLeavesSessionsWithinTheLimit(t *testing.T) {
	state := t.TempDir()
	dir := Dir(state)
	base := time.Now()
	peer(t, dir, fakePid(1), base.Add(-time.Minute))
	s, err := Register(state)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	defer s.Release()
	for _, max := range []int{2, 5, 0, -1} {
		var signalled []int
		s.stop = func(pid int) error { signalled = append(signalled, pid); return nil }
		closed, err := s.Enforce(max)
		if err != nil {
			t.Fatalf("Enforce(%d): %v", max, err)
		}
		if len(closed) != 0 || len(signalled) != 0 {
			t.Errorf("Enforce(%d) closed %v (signalled %v), want nothing", max, closed, signalled)
		}
	}
}

// A peer we cannot signal is left alone and reported as not closed, so the
// next sweep tries again instead of pretending the limit is satisfied.
func TestEnforceSkipsAPeerItCannotSignal(t *testing.T) {
	state := t.TempDir()
	dir := Dir(state)
	base := time.Now()
	unsignallable, ordinary := fakePid(1), fakePid(2)
	peer(t, dir, unsignallable, base.Add(-3*time.Minute))
	peer(t, dir, ordinary, base.Add(-2*time.Minute))
	s, err := Register(state)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	defer s.Release()
	s.stop = func(pid int) error {
		if pid == unsignallable {
			return os.ErrPermission
		}
		return nil
	}
	closed, err := s.Enforce(1)
	if err != nil {
		t.Fatalf("Enforce: %v", err)
	}
	if want := []int{ordinary}; !slices.Equal(closed, want) {
		t.Fatalf("closed = %v, want %v (%d could not be signalled)", closed, want, unsignallable)
	}
}

// The sweep runs every 10s, but a TUI asked to close needs longer to unwind
// (its drain is bounded at 15s) and `cmd/hap` releases its signal handler after
// the FIRST signal — so a second SIGTERM inside that window would kill it where
// it stands, with no terminal restore and no store close. A peer
// already asked to exit must therefore be left alone until the grace expires.
func TestEnforceDoesNotResignalAPeerThatIsStillExiting(t *testing.T) {
	state := t.TempDir()
	dir := Dir(state)
	base := time.Now()
	old := fakePid(1)
	peer(t, dir, old, base.Add(-3*time.Minute))
	s, err := Register(state)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	defer s.Release()
	now := base
	s.now = func() time.Time { return now }
	var signalled []int
	s.stop = func(pid int) error { signalled = append(signalled, pid); return nil }

	if closed, err := s.Enforce(1); err != nil || len(closed) != 1 {
		t.Fatalf("first sweep closed %v (err %v), want the one older peer", closed, err)
	}
	// The peer is still holding its lock — it is unwinding, not ignoring us.
	now = now.Add(30 * time.Second)
	closed, err := s.Enforce(1)
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if len(closed) != 0 {
		t.Errorf("second sweep re-signalled %v inside the grace period", closed)
	}
	if len(signalled) != 1 {
		t.Fatalf("stop called %d times, want 1 inside the grace period", len(signalled))
	}

	// Past the grace it has plainly ignored SIGTERM; asking again is the
	// right escalation.
	now = now.Add(signalGrace)
	if closed, err = s.Enforce(1); err != nil || len(closed) != 1 {
		t.Fatalf("sweep after the grace closed %v (err %v), want the peer signalled again", closed, err)
	}
	if len(signalled) != 2 {
		t.Errorf("stop called %d times, want 2 once the grace expired", len(signalled))
	}
}

// A peer that exits is forgotten once its grace has run out, so the
// bookkeeping cannot leak. It is NOT forgotten the moment it stops being
// listed: a sweep that merely failed to read it would otherwise re-signal it
// mid-exit, which is the very thing the grace prevents.
func TestEnforceForgetsAPeerThatIsGone(t *testing.T) {
	state := t.TempDir()
	dir := Dir(state)
	base := time.Now()
	path := filepath.Join(dir, filePrefix+strconv.Itoa(fakePid(1))+fileSuffix)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := claim(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := write(f, Info{Pid: fakePid(1), Started: base.Add(-time.Minute), Identity: processIdentity()}); err != nil {
		t.Fatal(err)
	}
	s, err := Register(state)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	defer s.Release()
	now := base
	s.now = func() time.Time { return now }
	s.stop = func(int) error { return nil }
	if closed, _ := s.Enforce(1); len(closed) != 1 {
		t.Fatalf("first sweep closed %v, want the older peer", closed)
	}
	if got := s.trackedForTest(); got != 1 {
		t.Fatalf("tracking %d signalled peers, want 1", got)
	}
	release(f) // the peer exited
	now = now.Add(signalGrace / 2)
	if closed, _ := s.Enforce(1); len(closed) != 0 {
		t.Fatalf("sweep after the peer exited closed %v, want nothing", closed)
	}
	if got := s.trackedForTest(); got != 1 {
		t.Errorf("tracking %d peers inside the grace, want the entry kept (it may only be unreadable)", got)
	}
	now = now.Add(signalGrace)
	if _, err := s.Enforce(1); err != nil {
		t.Fatal(err)
	}
	if got := s.trackedForTest(); got != 0 {
		t.Errorf("tracking %d signalled peers once the grace expired, want 0", got)
	}
}

// A peer that a single sweep cannot see — an unreadable or mid-write record —
// keeps its grace, so the sweep after it must not deliver a second, fatal
// SIGTERM into the peer's exit path.
func TestEnforceKeepsTheGraceAcrossASweepThatMissesThePeer(t *testing.T) {
	state := t.TempDir()
	dir := Dir(state)
	base := time.Now()
	old := peer(t, dir, fakePid(1), base.Add(-time.Minute))
	s, err := Register(state)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	defer s.Release()
	now := base
	s.now = func() time.Time { return now }
	var signalled []int
	s.stop = func(pid int) error { signalled = append(signalled, pid); return nil }

	if closed, _ := s.Enforce(1); len(closed) != 1 {
		t.Fatalf("first sweep closed %v, want the older peer", closed)
	}
	// One sweep cannot read the peer's record (fd exhaustion, mid-write)...
	s.list = func(dir string) ([]Info, error) {
		live, err := Live(dir)
		return slices.DeleteFunc(live, func(i Info) bool { return i.Pid == old.Pid }), err
	}
	now = now.Add(15 * time.Second)
	if _, err := s.Enforce(1); err != nil {
		t.Fatal(err)
	}
	// ...and the next one sees it again, still inside its grace.
	s.list = nil
	now = now.Add(15 * time.Second)
	if closed, _ := s.Enforce(1); len(closed) != 0 {
		t.Fatalf("re-signalled %v after a sweep that merely missed it", closed)
	}
	if len(signalled) != 1 {
		t.Fatalf("stop called %d times, want 1 — the grace must survive an unreadable sweep", len(signalled))
	}
}

// With the limit off the sweep still prunes: an unlimited registry must not
// accumulate a record per uncleanly-exited TUI forever.
func TestEnforceWithNoLimitStillPrunes(t *testing.T) {
	state := t.TempDir()
	dir := Dir(state)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, filePrefix+strconv.Itoa(fakePid(9))+fileSuffix)
	if err := os.WriteFile(stale, []byte("1\n2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Register(state)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	defer s.Release()
	s.stop = func(int) error { t.Fatal("nothing may be signalled with the limit off"); return nil }
	if _, err := s.Enforce(0); err != nil {
		t.Fatalf("Enforce(0): %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("a dead record survived a no-limit sweep, stat err = %v", err)
	}
}

// A peer from another pid space (a container sharing this state dir with its
// host) is neither counted nor signalled: its pid names a different process
// here, and signalling it would hit an unrelated one.
func TestEnforceIgnoresAPeerFromAnotherProcessSpace(t *testing.T) {
	state := t.TempDir()
	dir := Dir(state)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, filePrefix+strconv.Itoa(fakePid(1))+fileSuffix)
	f, err := claim(path)
	if err != nil {
		t.Fatal(err)
	}
	defer release(f)
	if err := write(f, Info{Pid: fakePid(1), Started: time.Now().Add(-time.Hour), Identity: "some-other-host pid:[1]"}); err != nil {
		t.Fatal(err)
	}
	s, err := Register(state)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	defer s.Release()
	// A local older peer alongside it: with max=1 the foreign one must not
	// even be COUNTED, or this local peer would be the only surplus and the
	// distinction would be invisible.
	local := peer(t, dir, fakePid(2), time.Now().Add(-time.Minute))
	var signalled []int
	s.stop = func(pid int) error {
		if pid == fakePid(1) {
			t.Fatalf("signalled a foreign pid %d", pid)
		}
		signalled = append(signalled, pid)
		return nil
	}
	closed, err := s.Enforce(2) // room for self + one peer
	if err != nil {
		t.Fatalf("Enforce: %v", err)
	}
	if len(closed) != 0 || len(signalled) != 0 {
		t.Fatalf("closed = %v, want nothing: the foreign session must not count toward the limit", closed)
	}
	if closed, err = s.Enforce(1); err != nil {
		t.Fatalf("Enforce: %v", err)
	}
	if want := []int{local.Pid}; !slices.Equal(closed, want) {
		t.Fatalf("closed = %v, want %v (only the local peer)", closed, want)
	}
}

// foreign() only fires on a KNOWN mismatch: an unknown identity on either side
// must degrade to trusting the pid, not to disabling the limit.
func TestForeignNeedsBothIdentities(t *testing.T) {
	self := Info{Pid: 2, Identity: "host-a ns1"}
	tests := []struct {
		name string
		peer Info
		want bool
	}{
		{"same space", Info{Pid: 1, Identity: "host-a ns1"}, false},
		{"different space", Info{Pid: 1, Identity: "host-b ns2"}, true},
		{"peer unknown", Info{Pid: 1}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := foreign(tc.peer, self); got != tc.want {
				t.Errorf("foreign = %v, want %v", got, tc.want)
			}
			if got := foreign(tc.peer, Info{Pid: 2}); got {
				t.Error("an unknown identity on OUR side must never make a peer foreign")
			}
		})
	}
}

// A registered session records an identity, so the guard above has something
// to compare in production.
func TestRegisterRecordsAProcessIdentity(t *testing.T) {
	s, err := Register(t.TempDir())
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	defer s.Release()
	if s.Self().Identity == "" {
		t.Skip("this platform reports no hostname or pid namespace")
	}
	live, err := Live(s.dir)
	if err != nil || len(live) != 1 {
		t.Fatalf("Live = %v (err %v), want our own session", live, err)
	}
	if live[0].Identity != s.Self().Identity {
		t.Errorf("identity round-trip: read %q, wrote %q", live[0].Identity, s.Self().Identity)
	}
}

// Claiming clears the record immediately, so a reused pid can never be read
// with its dead predecessor's start time — which would make the NEWEST TUI
// look like the oldest and get itself closed.
func TestClaimClearsAStaleRecordBeforeItCanBeRead(t *testing.T) {
	dir := Dir(t.TempDir())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, filePrefix+"4242"+fileSuffix)
	// A dead session's record, left behind by an unclean exit.
	if err := os.WriteFile(path, []byte("4242\n1\nold-identity\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := claim(path)
	if err != nil {
		t.Fatal(err)
	}
	defer release(f)
	if _, err := read(path); err == nil {
		t.Error("a freshly claimed record still parsed — the predecessor's bytes are readable")
	}
}

// trackedForTest reports how many peers the grace bookkeeping remembers.
func (s *Session) trackedForTest() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.signalled)
}

// A nil session is inert, so a TUI that could not register never panics.
func TestNilSessionIsInert(t *testing.T) {
	var s *Session
	s.Release()
	if got := s.Self(); got != (Info{}) {
		t.Errorf("Self() on nil = %+v, want zero", got)
	}
	closed, err := s.Enforce(1)
	if err != nil || closed != nil {
		t.Errorf("Enforce on nil = (%v, %v), want (nil, nil)", closed, err)
	}
}

// Surplus is the whole policy: newest wins, never self, never a newer peer.
func TestSurplus(t *testing.T) {
	base := time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)
	at := func(sec int, pid int) Info {
		return Info{Pid: pid, Started: base.Add(time.Duration(sec) * time.Second)}
	}
	a, b, c := at(0, 11), at(1, 12), at(2, 13)

	tests := []struct {
		name string
		live []Info
		self Info
		max  int
		want []int
	}{
		{"newest closes both older", []Info{a, b, c}, c, 1, []int{11, 12}},
		{"middle closes only the older one", []Info{a, b, c}, b, 1, []int{11}},
		{"oldest closes nobody — a newer peer will close it", []Info{a, b, c}, a, 1, nil},
		{"a higher limit keeps more", []Info{a, b, c}, c, 2, []int{11}},
		{"limit off", []Info{a, b, c}, c, 0, nil},
		{"negative limit off", []Info{a, b, c}, c, -3, nil},
		{"alone", []Info{a}, a, 1, nil},
		{"self missing from the registry still counts itself",
			[]Info{a, b}, Info{Pid: 99, Started: base.Add(3 * time.Second)}, 1, []int{11, 12}},
		{"same timestamp: the lower pid is the older one",
			[]Info{{Pid: 7, Started: base}, {Pid: 9, Started: base}}, Info{Pid: 9, Started: base}, 1, []int{7}},
		{"pid 1 and below are never signalled",
			[]Info{{Pid: 1, Started: base}, c}, c, 1, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := make([]int, 0, len(tc.want))
			for _, i := range Surplus(tc.live, tc.self, tc.max) {
				got = append(got, i.Pid)
			}
			if len(got) == 0 {
				got = nil
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("Surplus = %v, want %v", got, tc.want)
			}
		})
	}
}

// The registry survives a state dir that has never held one.
func TestLiveOnMissingRegistry(t *testing.T) {
	live, err := Live(Dir(t.TempDir()))
	if err != nil {
		t.Fatalf("Live on a missing registry must not error: %v", err)
	}
	if len(live) != 0 {
		t.Fatalf("got %+v, want none", live)
	}
}
