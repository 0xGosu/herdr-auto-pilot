//go:build !windows

package control

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/testutil"
)

func TestNudgeRoundTrip(t *testing.T) {
	path := filepath.Join(testutil.SocketDir(t), "ctl.sock")
	var mu sync.Mutex
	var got []Kind
	srv, err := NewServer(path, func(k Kind) {
		mu.Lock()
		got = append(got, k)
		mu.Unlock()
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	start := time.Now()
	if err := Nudge(context.Background(), path, KindReload); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n == 1 {
			// NFR-009: nudge → handler within the 1s budget.
			if time.Since(start) > time.Second {
				t.Error("nudge exceeded the 1s propagation budget")
			}
			if got[0] != KindReload {
				t.Errorf("kind = %v", got[0])
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("nudge never reached the handler")
}

func TestReembedNudgeRoundTrip(t *testing.T) {
	path := filepath.Join(testutil.SocketDir(t), "ctl.sock")
	var mu sync.Mutex
	var got []Kind
	srv, err := NewServer(path, func(k Kind) {
		mu.Lock()
		got = append(got, k)
		mu.Unlock()
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	if err := Nudge(context.Background(), path, KindReembed); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n == 1 {
			if got[0] != KindReembed {
				t.Errorf("kind = %v, want %v", got[0], KindReembed)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("reembed nudge never reached the handler")
}

func TestCaptureNudgeRoundTrip(t *testing.T) {
	path := filepath.Join(testutil.SocketDir(t), "ctl.sock")
	got := make(chan Kind, 1)
	srv, err := NewServer(path, func(k Kind) { got <- k })
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	if err := NudgeCapture(context.Background(), path, "w2:pB"); err != nil {
		t.Fatal(err)
	}
	select {
	case kind := <-got:
		if target, ok := CaptureTarget(kind); !ok || target != "w2:pB" {
			t.Fatalf("capture kind %q decoded as target=%q ok=%v", kind, target, ok)
		}
	case <-time.After(time.Second):
		t.Fatal("capture nudge never reached the handler")
	}

	for _, target := range []string{"", "bad\nagent", string(make([]byte, 257))} {
		if _, err := CaptureKind(target); err == nil {
			t.Errorf("CaptureKind(%q) should fail", target)
		}
	}
	for _, kind := range []Kind{"capture: padded", Kind("capture:" + string(make([]byte, 257)))} {
		if target, ok := CaptureTarget(kind); ok {
			t.Errorf("CaptureTarget(%q) = %q, true; want rejected", kind, target)
		}
	}
}

func TestSocketOwnerOnlyPermissions(t *testing.T) {
	path := filepath.Join(testutil.SocketDir(t), "ctl.sock")
	srv, err := NewServer(path, func(Kind) {})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("control socket permissions = %o, want 0600", perm)
	}
}

func TestMalformedNudgeIgnored(t *testing.T) {
	path := filepath.Join(testutil.SocketDir(t), "ctl.sock")
	var mu sync.Mutex
	calls := 0
	srv, err := NewServer(path, func(Kind) {
		mu.Lock()
		calls++
		mu.Unlock()
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	conn, err := dial(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	conn.Write([]byte("this is not json\n"))
	conn.Write([]byte(`{"kind":"bogus"}` + "\n"))
	conn.Close()

	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if calls != 0 {
		t.Errorf("malformed/unknown nudges must be ignored, handler ran %d times", calls)
	}
}

func TestNudgeDebounce(t *testing.T) {
	path := filepath.Join(testutil.SocketDir(t), "ctl.sock")
	var mu sync.Mutex
	calls := 0
	srv, err := NewServer(path, func(Kind) {
		mu.Lock()
		calls++
		mu.Unlock()
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	for i := 0; i < 10; i++ {
		Nudge(context.Background(), path, KindReload)
	}
	time.Sleep(500 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if calls == 0 || calls >= 10 {
		t.Errorf("burst of 10 nudges should debounce to fewer handler calls, got %d", calls)
	}
}
