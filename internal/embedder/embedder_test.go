package embedder

import (
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
)

// TestResolveContextWindowFloor covers the override boundaries — 0/negative
// fall to the default, and any positive value below the safe minimum (at or
// below the special-token headroom included) is clamped up so the budget can
// never go sub-headroom (#82, PR review). It is engine-independent, so it runs
// in both the CGO (`cpu`) and stub (`!cpu`) builds.
func TestResolveContextWindowFloor(t *testing.T) {
	cases := []struct{ in, want int }{
		{0, DefaultContextWindow},
		{-5, DefaultContextWindow},
		{1, minContextWindow},
		{specialTokenHeadroom, minContextWindow},
		{255, minContextWindow},
		{minContextWindow, minContextWindow},
		{512, 512},
		{2048, 2048},
	}
	for _, c := range cases {
		if got := ResolveContextWindow(config.Embedding{ModelContextWindow: c.in}); got != c.want {
			t.Errorf("ResolveContextWindow(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}
