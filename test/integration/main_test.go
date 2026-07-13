//go:build integration && vectors && cpu

package integration

import (
	"os"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/embedder"
)

// TestMain lets the semantic test's subprocess-isolated embedder Client spawn
// its worker by re-execing this test binary: when the child sets the worker
// helper env, RunWorkerHelperIfChild runs the embed worker and exits instead of
// running the suite. A normal run returns immediately and proceeds to the tests.
//
// Tagged the same as semantic_test.go (the only test needing the worker); the
// lighter `-tags integration` build has no TestMain and is unaffected.
func TestMain(m *testing.M) {
	embedder.RunWorkerHelperIfChild()
	os.Exit(m.Run())
}
