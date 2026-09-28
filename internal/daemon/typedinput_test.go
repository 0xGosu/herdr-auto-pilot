package daemon

import (
	"sync"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
)

// typedInputHerdr is the daemon's fake herdr plus ports.ClaudeTypedInputSetter,
// recording every value the daemon pushes. Embedding keeps every other
// optional interface the fake implements.
type typedInputHerdr struct {
	*fakeHerdr
	mu    sync.Mutex
	calls []bool
}

func (f *typedInputHerdr) SetClaudeTypedInput(on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, on)
}

func (f *typedInputHerdr) pushed() []bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]bool(nil), f.calls...)
}

// TestReloadPushesClaudeTypedInputToTheHerdrAdapter proves the wiring the
// feature rests on. The key is enforced inside the herdr ADAPTER, which the
// daemon's ordinary fake does not implement, so without this test the typed
// route could be switched off in production — never pushed, or pushed only at
// start — while every send-path test still passed.
func TestReloadPushesClaudeTypedInputToTheHerdrAdapter(t *testing.T) {
	var fake *typedInputHerdr
	h := newHarnessWrapped(t, "", func(f *fakeHerdr) ports.HerdrPort {
		fake = &typedInputHerdr{fakeHerdr: f}
		return fake
	})

	last := func() bool {
		t.Helper()
		got := fake.pushed()
		if len(got) == 0 {
			t.Fatal("the daemon never pushed [agents] claude_typed_input to the herdr adapter")
		}
		return got[len(got)-1]
	}
	// The first load pushes too, so a daemon STARTED with the key on types
	// from its first send — and one started with it off says so explicitly.
	if last() {
		t.Fatal("claude_typed_input is off by default, but the adapter was switched on")
	}

	h.writeConfig(t, "[agents]\nclaude_typed_input = true\n")
	h.daemon.reload()
	if !last() {
		t.Fatal("turning claude_typed_input on did not reach the adapter on reload")
	}

	h.writeConfig(t, "[agents]\nclaude_typed_input = false\n")
	h.daemon.reload()
	if last() {
		t.Fatal("turning claude_typed_input off did not reach the adapter on reload")
	}
}
