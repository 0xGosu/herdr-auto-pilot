// Package testutil holds shared test helpers.
package testutil

import (
	"os"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"
)

// TimeoutScaleEnv is the environment variable that multiplies every shared test
// wait deadline. CI sets it (.github/workflows/ci.yml); locally it is unset.
const TimeoutScaleEnv = "HAP_TEST_TIMEOUT_SCALE"

var timeoutScale = sync.OnceValue(func() float64 {
	return scaleFrom(os.Getenv(TimeoutScaleEnv), runtime.GOMAXPROCS(0))
})

// scaleFrom resolves the multiplier from its two inputs.
//
// Pure, and deliberately separate from the sync.OnceValue above so it can be
// tested: the cached value is resolved once per PROCESS, so a test that set the
// environment and then called Scale would assert against whatever the first
// caller in that binary already resolved — passing or failing for reasons that
// have nothing to do with the code under test.
//
// A malformed or below-1 value is IGNORED rather than reported: this runs
// inside every test binary, so a typo in CI's environment must not fail a
// suite. Falling back can only ever lengthen a wait, never shorten one.
func scaleFrom(env string, procs int) float64 {
	if env != "" {
		if f, err := strconv.ParseFloat(env, 64); err == nil && f >= 1 {
			return f
		}
	}
	// Fallback heuristic for a small runner. It cannot see load imposed from
	// OUTSIDE this process — a busy shared devbox reports every core — which is
	// exactly why the env knob above exists and is what CI sets.
	switch {
	case procs < 4:
		return 4
	case procs < 8:
		return 2
	default:
		return 1
	}
}

// Scale stretches a test wait deadline by TimeoutScaleEnv.
//
// Why this exists, measured on an idle devbox over one full internal/daemon run
// (1721 waits, no timeouts): the median wait is 10ms and p99 is 260ms, so the
// deadlines look enormously generous — but the TAIL is what fails a job. The
// slowest single wait was 1862ms against a 3s deadline (62% consumed) and the
// next 1988ms against 4s, so the worst cases carry only ~1.6x headroom while
// the machine is otherwise doing nothing. A CI runner needs very little
// contention to cross that, and one crossing fails the whole job: in a single
// night seven distinct tests failed this way with "condition not met within
// timeout", every one of them passing on a quiet machine.
//
// So the deadlines are not wrong in shape — a hung condition still fails within
// seconds — they are simply sized for an unloaded machine. Scaling them in ONE
// place keeps that property while letting a loaded runner finish, and is
// deliberately preferred over editing individual tests: the per-test deadline
// still expresses "this should take well under N seconds", and the multiplier
// expresses "this machine is X times slower than the one that number was
// chosen on".
//
// It only ever LENGTHENS a wait (the factor is floored at 1), so it can never
// make a test flakier than its author intended, and it never touches an
// assertion — a condition that is genuinely never satisfied still fails.
func Scale(d time.Duration) time.Duration {
	return time.Duration(float64(d) * timeoutScale())
}

// TimeoutScale reports the multiplier Scale applies, for a failure message that
// needs to say what deadline was actually in force.
func TimeoutScale() float64 { return timeoutScale() }

// SocketDir returns a directory with a short absolute path for unix-domain
// sockets. macOS caps socket paths at 104 bytes, and t.TempDir() embeds the
// full test name — long enough to overflow the cap and fail net.Listen.
func SocketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "hap")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}
