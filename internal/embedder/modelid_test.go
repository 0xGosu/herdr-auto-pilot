package embedder

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
)

func writeModel(t *testing.T, dir, name, content string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestTwoDifferentModelsWithTheSameFileNameGetDifferentIDs is the regression
// this identity exists for. `filepath.Base` reported the same id for two
// genuinely different models both installed as `model.gguf`, so
// reembed.Reconcile's skip condition kept a foreign vector and cosine compared
// vectors from unrelated models. No equality filter downstream can catch that —
// the strings agree — so the id itself has to discriminate.
func TestTwoDifferentModelsWithTheSameFileNameGetDifferentIDs(t *testing.T) {
	a := writeModel(t, filepath.Join(t.TempDir(), "a"), "model.gguf", "model A weights")
	b := writeModel(t, filepath.Join(t.TempDir(), "b"), "model.gguf", "model B weights")

	got, want := ModelIDFor(a), ModelIDFor(b)
	if got == want {
		t.Fatalf("two different models share the id %q", got)
	}
	if !strings.HasPrefix(got, modelIDPrefix) || !strings.HasPrefix(want, modelIDPrefix) {
		t.Fatalf("ids are not content-derived: %q / %q", got, want)
	}
	if filepath.Base(a) != filepath.Base(b) {
		t.Fatal("the fixture no longer reproduces the file-name collision")
	}
}

// TestTheSameModelAtDifferentPathsGetsTheSameID is the other half, and it is
// what keeps a fleet convergent: signature_embeddings is shared across nodes
// (no node_id — one row per rule, one model/dims/vector triple), so an id that
// varied with the file's name or location would make every node read every
// other node's rows as stale, re-embed them and push them back forever. Hence
// the id carries no file name at all.
func TestTheSameModelAtDifferentPathsGetsTheSameID(t *testing.T) {
	const weights = "identical model weights"
	a := writeModel(t, filepath.Join(t.TempDir(), "node-a"), "all-minilm-l6-v2-q8_0.gguf", weights)
	b := writeModel(t, filepath.Join(t.TempDir(), "node-b", "custom"), "renamed.gguf", weights)

	if got, want := ModelIDFor(a), ModelIDFor(b); got != want {
		t.Fatalf("the same model reports different ids across machines: %q != %q", got, want)
	}
}

// TestAMissingModelFallsBackToItsFileName: an id is asked for before install.sh
// has fetched the optional model, and on an install that never gets one. An
// empty id would compare unequal to every stored row, so `hap status` would
// report drift no reembed could ever clear; the base name keeps such an install
// on exactly its previous behaviour.
func TestAMissingModelFallsBackToItsFileName(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope", "all-minilm-l6-v2-q8_0.gguf")
	if got := ModelIDFor(missing); got != "all-minilm-l6-v2-q8_0.gguf" {
		t.Fatalf("resolve(missing) = %q, want the base name", got)
	}
}

// TestTheFallbackIsNotLatched: the fallback must not be cached, or an install
// whose model arrives after the first call keeps the legacy scheme for the life
// of the process — and then re-embeds every rule again on the next start.
func TestTheFallbackIsNotLatched(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "model.gguf")

	if got := ModelIDFor(path); got != "model.gguf" {
		t.Fatalf("before the model exists: resolve = %q", got)
	}
	writeModel(t, dir, "model.gguf", "weights that arrived late")
	got := ModelIDFor(path)
	if !strings.HasPrefix(got, modelIDPrefix) {
		t.Fatalf("resolve after the model appeared = %q, want a content id", got)
	}
}

// TestTheIDIsResolvedOnce: the first call streams the whole model file (~25 MB
// for the bundled MiniLM). Re-reading it per call would put that on the daemon
// select loop, which asks for the id on every classification
// (daemon.resolveSignature).
func TestTheIDIsResolvedOnce(t *testing.T) {
	dir := t.TempDir()
	path := writeModel(t, dir, "model.gguf", "original weights")

	first := ModelIDFor(path)
	if err := os.WriteFile(path, []byte("swapped underneath us"), 0o600); err != nil {
		t.Fatal(err)
	}
	if second := ModelIDFor(path); second != first {
		t.Fatalf("the id was recomputed: %q then %q", first, second)
	}
}

// TestClientReportsTheContentID pins the wiring on the daemon's PRODUCTION
// embedder — Client, the out-of-process worker supervisor — since that is the
// id that actually reaches signature_embeddings.model.
func TestClientReportsTheContentID(t *testing.T) {
	path := writeModel(t, t.TempDir(), "model.gguf", "production weights")
	c := New(config.Embedding{ModelPath: path})
	got := c.ModelID()
	if !strings.HasPrefix(got, modelIDPrefix) {
		t.Fatalf("Client.ModelID() = %q, want a content id", got)
	}
	if want := ModelIDFor(path); got != want {
		t.Fatalf("Client.ModelID() = %q, want %q", got, want)
	}
}
