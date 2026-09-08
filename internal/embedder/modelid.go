package embedder

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// modelIDPrefix tags a content-derived model id, so a log line, a stored
// signature_embeddings.model value and an `hap signatures reembed` line all say
// which scheme produced the id. A legacy row minted before this change carries
// a bare file name instead and therefore never compares equal to a fresh id —
// which is exactly right: nothing recorded what those vectors were actually
// computed with, so they are re-embedded once on the next daemon start.
const modelIDPrefix = "sha256:"

// modelIDDigestRunes is how much of the digest the id carries. 16 hex chars is
// 64 bits — far beyond any accidental collision between the handful of
// embedding models one fleet ever holds — and short enough to stay readable in
// a log line and in the `model` column.
const modelIDDigestRunes = 16

// modelIDCache memoizes resolved ids by model path. Only SUCCESSES are cached:
// an id is asked for before install.sh has finished fetching the optional
// model, and latching the fallback would keep an install that later grew a
// model on the legacy scheme for the life of the process. The lock is held
// ACROSS the file read on purpose, so concurrent first callers hash once rather
// than racing.
var (
	modelIDMu    sync.Mutex
	modelIDCache = map[string]string{}
)

// ModelIDFor returns the identity of the embedding model file at path, derived
// from its CONTENT rather than its name.
//
// The identity has to satisfy two things at once, and the file NAME satisfies
// neither:
//
//   - Two different models must never share an id. `filepath.Base` made that
//     false for the case that matters: `[embedding] model_path` pointing at two
//     genuinely different 384-dimension models both installed as `model.gguf`
//     reported the SAME id on both machines, so reembed.Reconcile's skip
//     condition (`r.Model == emb.ModelID() && len(r.Vector) == res.Dims`) kept
//     the foreign vector and cosine then compared vectors from two unrelated
//     models. No equality filter anywhere downstream can catch that — the
//     strings agree.
//
//   - The SAME model must have the same id on every machine, because
//     `signature_embeddings` is shared across the fleet (no node_id: one row per
//     rule, one model/dims/vector triple). An id that varied per machine would
//     make every node read every other node's rows as stale, re-embed them and
//     push them back — an unbounded rewrite ping-pong through Turso Cloud, plus
//     a permanent "N rules need re-compute" nag in every TUI. This is why the id
//     carries no file name at all: one operator pointing `model_path` at a
//     renamed copy of the bundled model must not be enough to start that.
//
// Unreadable (a missing model, a permission error) falls back to the file's
// base name, NOT to "": an empty id compares unequal to every stored row, so a
// model-less install would report drift that no re-embed could ever clear.
// That fallback is exactly the previous behaviour, so such an install is
// unaffected by this change.
//
// The first call for a path streams the file once (~25 MB for the bundled
// MiniLM). In the daemon that first call is reembed.Reconcile's, on the
// background semantic-init goroutine, never the select loop; the loop's own
// caller (daemon.resolveSignature) reaches the cached value. Caching by path
// also means a model replaced IN PLACE under the same name is not noticed until
// the next process start or `[embedding]` reload — unchanged from the basename
// scheme, and already documented on frontend.EmbeddingDrift.
func ModelIDFor(path string) string {
	modelIDMu.Lock()
	defer modelIDMu.Unlock()
	if id, ok := modelIDCache[path]; ok {
		return id
	}
	sum, err := modelDigest(path)
	if err != nil {
		return filepath.Base(path)
	}
	id := modelIDPrefix + sum
	modelIDCache[path] = id
	return id
}

// modelDigest streams path through SHA-256 and returns the leading
// modelIDDigestRunes hex characters.
func modelDigest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil))[:modelIDDigestRunes], nil
}
