package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
)

func TestAdoptAgentNameTakesThePlainNameWhenItIsFree(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	if _, err := s.EnsureAgentName(ctx, "w1:p1"); err != nil {
		t.Fatal(err)
	}
	got, err := s.AdoptAgentName(ctx, "w1:p1", "add-sweep-command-grid")
	if err != nil {
		t.Fatal(err)
	}
	if got != "add-sweep-command-grid" {
		t.Fatalf("got %q, want the plain name", got)
	}
	name, err := s.agentNameByID(ctx, "w1:p1")
	if err != nil || name != got {
		t.Fatalf("stored %q (err %v), want %q", name, err, got)
	}
}

// Two worktrees on one feature is the ordinary cause, and Claude itself allows
// the duplicate conversation name (verified live 2026-09-01). The second agent
// must land on a suffix rather than fail.
func TestAdoptAgentNameSuffixesOnCollision(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	for _, id := range []string{"w1:p1", "w1:p2", "w1:p3"} {
		if _, err := s.EnsureAgentName(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"feature", "feature-2", "feature-3"}
	for i, id := range []string{"w1:p1", "w1:p2", "w1:p3"} {
		got, err := s.AdoptAgentName(ctx, id, "feature")
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if got != want[i] {
			t.Fatalf("%s: got %q, want %q", id, got, want[i])
		}
	}
}

// The idempotence guarantee. Without it the collision loser is walked to the
// next suffix at every capture — renamed forever, and its new name pushed back
// into its pane each time.
func TestAdoptAgentNameIsIdempotentForASuffixedHolder(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	for _, id := range []string{"w1:p1", "w1:p2"} {
		if _, err := s.EnsureAgentName(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.AdoptAgentName(ctx, "w1:p1", "feature"); err != nil {
		t.Fatal(err)
	}
	first, err := s.AdoptAgentName(ctx, "w1:p2", "feature")
	if err != nil || first != "feature-2" {
		t.Fatalf("got %q (err %v), want feature-2", first, err)
	}
	for i := 0; i < 5; i++ {
		again, err := s.AdoptAgentName(ctx, "w1:p2", "feature")
		if err != nil {
			t.Fatal(err)
		}
		if again != "feature-2" {
			t.Fatalf("sweep %d moved the name to %q; it must stay feature-2", i, again)
		}
	}
}

// The sync ADJUSTS an existing agent's name; it must never invent a row for an
// id herdr has not reported. EnsureAgentName owns creation.
func TestAdoptAgentNameRefusesAnUnknownAgent(t *testing.T) {
	s, _ := openTestStore(t)
	_, err := s.AdoptAgentName(context.Background(), "w9:p9", "feature")
	if !errors.Is(err, ports.ErrUnknownAgent) {
		t.Fatalf("got %v, want ErrUnknownAgent", err)
	}
}

func TestAdoptAgentNameRefusesAnUnstorableName(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	if _, err := s.EnsureAgentName(ctx, "w1:p1"); err != nil {
		t.Fatal(err)
	}
	before, _ := s.agentNameByID(ctx, "w1:p1")
	for _, bad := range []string{"", "Has Caps", "-leading", strings.Repeat("a", 33), "has space"} {
		if _, err := s.AdoptAgentName(ctx, "w1:p1", bad); err == nil {
			t.Errorf("AdoptAgentName(%q) should have been refused", bad)
		}
	}
	after, _ := s.agentNameByID(ctx, "w1:p1")
	if after != before {
		t.Fatalf("a refused adopt changed the name from %q to %q", before, after)
	}
}

// Whatever the normalizer produces must be storable here, or the daemon writes
// a name this refuses and retries it on every single pane capture. The two
// sides share one regex precisely so this cannot drift.
func TestAdoptAgentNameAcceptsEveryNormalizedSessionName(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	if _, err := s.EnsureAgentName(ctx, "w1:p1"); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		"add-sweep-command-grid", "My Feature: Work #2",
		"this-is-a-really-long-conversation-name-that-goes-past-thirty-two",
		"snake_case_kept", "9lives",
	} {
		base, ok := domain.NormalizeAgentName(raw)
		if !ok {
			t.Fatalf("%q did not normalize", raw)
		}
		if _, err := s.AdoptAgentName(ctx, "w1:p1", base); err != nil {
			t.Errorf("AdoptAgentName(%q → %q): %v", raw, base, err)
		}
	}
}

// The store's validator and the domain's producers must agree by construction,
// not by coincidence: a drift here is invisible until a real session name hits
// the edge of the pattern.
func TestStoreAgentNameValidatorIsTheDomainOne(t *testing.T) {
	if agentNameRE != domain.AgentNameRE {
		t.Fatal("internal/store must validate agent names with domain.AgentNameRE, not a copy")
	}
}
