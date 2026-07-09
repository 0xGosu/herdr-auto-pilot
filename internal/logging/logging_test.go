package logging

import (
	"errors"
	"testing"
)

func TestGuardRecoversPanic(t *testing.T) {
	// NFR-004: an induced error on the daemon path is handled, not a panic.
	err := Guard("test", func() error { panic("boom") })
	if err == nil {
		t.Fatal("guard must convert the panic into an error")
	}
}

func TestGuardPassesThroughErrors(t *testing.T) {
	want := errors.New("regular failure")
	if got := Guard("test", func() error { return want }); !errors.Is(got, want) {
		t.Errorf("guard should pass through errors, got %v", got)
	}
	if err := Guard("test", func() error { return nil }); err != nil {
		t.Errorf("guard should return nil on success, got %v", err)
	}
}
