package embedder

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	// modelIDDir is where resolved ids persist across processes (see
	// SetModelIDCacheDir); "" keeps them in memory only.
	modelIDDir string
	// modelHashFn is hashModel; tests count calls through it.
	modelHashFn = hashModel
)

// modelIDFile is the cross-process cache inside the state dir.
const modelIDFile = "model-ids.json"

// SetModelIDCacheDir persists resolved model ids under dir, so a process does
// not re-hash a model file whose identity an earlier one already computed.
//
// This is the one-shot CLI's cost: `hap status` and `hap agents` both report
// embedding drift, which needs the id, and every one of them streamed the whole
// ~25 MB model through SHA-256 — tens of milliseconds idle and far more on a
// busy machine, for a verb whose useful work is a few store queries. An entry
// is trusted only while the file's size, mtime, inode, device AND change time
// all still match what was hashed. The change time is what makes that sound:
// nothing in user space can set it, so any rewrite — in place, by rename, or
// with its mtime put back (`cp -p`, `touch -r`) — is re-hashed at the next
// process start, which the per-process cache alone never did. Deleting
// <dir>/model-ids.json forces a re-hash. Best-effort throughout: an unreadable
// or unwritable cache just hashes, exactly as before.
func SetModelIDCacheDir(dir string) {
	modelIDMu.Lock()
	defer modelIDMu.Unlock()
	modelIDDir = dir
}

// modelIDEntry is one persisted id with the file facts it was computed from.
type modelIDEntry struct {
	Size    int64  `json:"size"`
	MtimeNs int64  `json:"mtime_ns"`
	CtimeNs int64  `json:"ctime_ns,omitempty"`
	Inode   uint64 `json:"inode,omitempty"`
	Device  uint64 `json:"device,omitempty"`
	ID      string `json:"id"`
}

// factsOf is what a persisted entry must still match.
func factsOf(fi os.FileInfo) modelIDEntry {
	e := modelIDEntry{Size: fi.Size(), MtimeNs: fi.ModTime().UnixNano()}
	e.Inode, e.Device, e.CtimeNs = fileIdentity(fi)
	return e
}

// sameFacts compares everything but the id.
func sameFacts(a, b modelIDEntry) bool {
	a.ID, b.ID = "", ""
	return a == b
}

// modelFileFacts is what an entry must still match for path.
func modelFileFacts(path string) (modelIDEntry, bool) {
	fi, err := os.Stat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return modelIDEntry{}, false
	}
	return factsOf(fi), true
}

// persistedModelID returns path's id from the cache file when its facts still
// match. Caller holds modelIDMu.
func persistedModelID(path string, facts modelIDEntry) (string, bool) {
	if modelIDDir == "" {
		return "", false
	}
	data, err := os.ReadFile(filepath.Join(modelIDDir, modelIDFile))
	if err != nil {
		return "", false
	}
	var all map[string]modelIDEntry
	if json.Unmarshal(data, &all) != nil {
		return "", false
	}
	e, ok := all[path]
	if !ok || e.ID == "" || !sameFacts(e, facts) {
		return "", false
	}
	return e.ID, true
}

// persistModelID records path's id beside the facts it was computed from, by
// write-and-rename so a concurrent reader never sees half a file. Two processes
// racing can drop each other's entry for ANOTHER path; that only costs a hash.
// Caller holds modelIDMu.
func persistModelID(path string, facts modelIDEntry, id string) {
	if modelIDDir == "" {
		return
	}
	file := filepath.Join(modelIDDir, modelIDFile)
	all := map[string]modelIDEntry{}
	if data, err := os.ReadFile(file); err == nil {
		_ = json.Unmarshal(data, &all)
	}
	facts.ID = id
	all[path] = facts
	data, err := json.Marshal(all)
	if err != nil {
		return
	}
	tmp, err := os.CreateTemp(modelIDDir, modelIDFile+".*")
	if err != nil {
		return
	}
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if werr != nil || cerr != nil || os.Rename(tmp.Name(), file) != nil {
		_ = os.Remove(tmp.Name())
	}
}

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
// MiniLM) — once per MACHINE when SetModelIDCacheDir is set, which every hap
// process does; later processes reuse it while the file is unchanged (its
// change time included, so a replacement is always re-hashed). In the
// daemon that first call is reembed.Reconcile's, on the
// background semantic-init goroutine, never the select loop; the loop's own
// caller (daemon.resolveSignature) reaches the cached value. Caching by path
// also means a model replaced IN PLACE under the same name is not noticed by a
// running process until its next start or `[embedding]` reload — documented on
// frontend.EmbeddingDrift.
func ModelIDFor(path string) string {
	modelIDMu.Lock()
	defer modelIDMu.Unlock()
	if id, ok := modelIDCache[path]; ok {
		return id
	}
	if facts, ok := modelFileFacts(path); ok {
		if id, ok := persistedModelID(path, facts); ok {
			modelIDCache[path] = id
			return id
		}
	}
	sum, facts, stable, err := modelHashFn(path)
	if err != nil {
		return filepath.Base(path)
	}
	id := modelIDPrefix + sum
	modelIDCache[path] = id
	if stable {
		persistModelID(path, facts, id)
	}
	return id
}

// hashModel streams path through SHA-256 and returns the leading
// modelIDDigestRunes hex characters, with the facts of the file it actually
// hashed. stable is false when those facts moved during the hash (the file was
// being replaced): the id is still returned, but must not be persisted against
// facts that describe different bytes.
func hashModel(path string) (sum string, facts modelIDEntry, stable bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", modelIDEntry{}, false, err
	}
	defer f.Close()
	before, berr := f.Stat()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", modelIDEntry{}, false, err
	}
	sum = hex.EncodeToString(h.Sum(nil))[:modelIDDigestRunes]
	after, aerr := os.Stat(path)
	if berr != nil || aerr != nil || !before.Mode().IsRegular() {
		return sum, modelIDEntry{}, false, nil
	}
	facts = factsOf(before)
	return sum, facts, sameFacts(facts, factsOf(after)), nil
}
