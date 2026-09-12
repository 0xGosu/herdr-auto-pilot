package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// ImportLegacy copies this machine's local sqlite database into dst — the
// shared store — once: the first time the turso engine starts on a machine that
// already has history. A missing legacy file is a no-op.
//
// "Once" is recorded in the shared store ITSELF, inside the import's
// transaction (legacy_imports), so a crash between the commit and any
// bookkeeping cannot cause a second import — which would duplicate every audit
// and decision row under fresh ids. markerPath is only a local cache of that
// fact: present, it saves opening the legacy file; absent, the row decides.
//
// Ids are re-allocated through dst's allocator, in ascending old-id order per
// table so relative order (and every "newest by id" query) survives, and every
// cross reference is remapped through the old→new maps: decisions ←
// signatures.decision_floor_id; audit_log ← corrections, llm_retries,
// task_reservations, audit_log.corrects_audit_id; corrections ←
// agent_actions.correction_id. Content-keyed knowledge merges into what the
// remote already holds (INSERT OR IGNORE: the fleet's copy wins).
//
// Deliberately NOT imported: pending LLM requests and decisions (in-flight IPC
// of a daemon that no longer exists), pending or running agent actions (an
// operator's answer decided against a screen from before the switch), and the
// roster (republished within a minute). A daemon's in-flight
// auto_accepting claim becomes escalated, as the startup reclaim would make it.
func ImportLegacy(ctx context.Context, legacyPath, markerPath string, dst *Store) error {
	if dst.ids == nil {
		return errors.New("import: the destination store needs an id allocator")
	}
	if _, err := os.Stat(markerPath); err == nil {
		return nil
	}
	if _, err := os.Stat(legacyPath); errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	src, err := Open(legacyPath) // migrates the legacy file in place first
	if err != nil {
		return fmt.Errorf("import: open %s: %w", legacyPath, err)
	}
	defer src.Close()

	imp := newImporter(ctx, src, dst)
	imp.legacyPath = legacyPath
	err = dst.tx(ctx, imp.run)
	switch {
	case errors.Is(err, errAlreadyImported):
		slog.Info("the local sqlite database was already imported into the shared store", "from", legacyPath)
	case err != nil:
		return fmt.Errorf("import: %w", err)
	default:
		slog.Info("imported the local sqlite database into the shared store",
			"from", legacyPath, "decisions", len(imp.decisions), "audit_rows", len(imp.audits))
	}
	// Best effort: the row above is the record; this only spares the next
	// start the legacy open.
	if err := os.WriteFile(markerPath, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600); err != nil {
		slog.Warn("import: marker file not written; the shared store still records the import", "path", markerPath, "error", err)
	}
	return nil
}

// errAlreadyImported is the importer's own signal that legacy_imports already
// holds this node's row: the transaction rolls back untouched.
var errAlreadyImported = errors.New("already imported")

type importer struct {
	src, dst    *Store
	ctx         context.Context
	legacyPath  string
	idErr       error
	decisions   map[int64]int64
	audits      map[int64]int64
	corrections map[int64]int64

	// srcNode, when non-empty, restricts every NODE-SCOPED table to that
	// node's rows. Empty means "take the table whole", which is what
	// ImportLegacy does (a legacy file is one machine's by construction) and
	// what `hap migrate --all-nodes` asks for.
	srcNode string
	// seq holds a per-table id counter, used ONLY when the destination has no
	// allocator — the sqlite engine, where ids are normally AUTOINCREMENT.
	// See nextID for why an explicit id is still assigned there.
	seq map[string]int64
	// table is the table being copied, which is what nextID keys seq on.
	table string
	// counts records rows written per table, in copy order, for the migrate
	// report. Nil for ImportLegacy, which reports through the log instead.
	counts map[string]int
	order  []string
}

// newImporter builds a copier from src into dst with empty remap tables. The
// caller sets legacyPath, srcNode and counts for what it needs.
func newImporter(ctx context.Context, src, dst *Store) *importer {
	return &importer{
		src: src, dst: dst, ctx: ctx,
		decisions: map[int64]int64{}, audits: map[int64]int64{}, corrections: map[int64]int64{},
		seq: map[string]int64{},
	}
}

// row is one legacy row, by column name.
type row map[string]any

// copyStep is one table's copy: which rows come over, and how each is rewritten
// before it is inserted.
type copyStep struct {
	table string
	cols  string
	order string
	// keep decides whether a row is imported; xform rewrites it (ids,
	// references, node) before the insert.
	keep  func(r row) bool
	xform func(r row)
}

func (im *importer) run(tx *sql.Tx) error {
	var done int
	if err := tx.QueryRowContext(im.ctx, `SELECT count(*) FROM legacy_imports WHERE node_id = ?`, im.dst.self).Scan(&done); err != nil {
		return err
	}
	if done > 0 {
		return errAlreadyImported
	}
	if err := im.copyAll(tx); err != nil {
		return err
	}
	_, err := tx.ExecContext(im.ctx, `INSERT INTO legacy_imports (node_id, legacy_path, imported_at) VALUES (?, ?, ?)`,
		im.dst.self, im.legacyPath, unix(time.Now()))
	return err
}

// copyAll runs every step of the copy list against tx. It is the whole of the
// data movement, shared by ImportLegacy and Migrate so the two can never drift
// — the id re-allocation, the reference remapping and the deliberate omissions
// are the hard part, and a second implementation of them would be the worst
// outcome for a database this one writes into.
func (im *importer) copyAll(tx *sql.Tx) error {
	for _, st := range im.copySteps() {
		if err := im.copyTable(tx, st.table, st.cols, st.order, st.keep, st.xform); err != nil {
			return fmt.Errorf("%s: %w", st.table, err)
		}
		if im.idErr != nil {
			return fmt.Errorf("%s: allocate id: %w", st.table, im.idErr)
		}
	}
	return nil
}

// copySteps is the copy list, in dependency order: a table that is remapped
// THROUGH another (audit_log through decisions, corrections through audit_log,
// agent_actions through corrections) is copied after it, because the old→new
// map is built as the referenced table is written.
func (im *importer) copySteps() []copyStep {
	self := im.dst.self
	return []copyStep{
		{"operator", "id, label", "id", nil, nil},
		{"decisions", decisionCols, "id", nil, func(r row) {
			old := r["id"].(int64)
			id := im.nextID()
			im.decisions[old] = id
			r["id"], r["node_id"] = id, self
		}},
		{"signatures", `signature, situation_type, agent_type, mode, consecutive_confirmations,
			cached_confidence, decision_floor_id, guard_state, updated_at`, "signature", nil, func(r row) {
			r["decision_floor_id"] = remapFloor(im.decisions, r["decision_floor_id"].(int64))
		}},
		{"signature_embeddings", "signature, situation_type, agent_type, model, dims, vector, salient, created_at", "signature", nil, nil},
		{"signature_snapshots", "signature, pane_excerpt, created_at", "signature", nil, nil},
		{"agent_names", "node_id, agent_id, name, disabled, terminal_id, created_at", "agent_id", nil, stampNode(self)},
		{"agent_rate", "node_id, agent_id, consecutive_auto, window_start, count_in_window, paused", "agent_id", nil, stampNode(self)},
		{"error_retries", "node_id, error_signature, agent_id, retry_count, updated_at", "error_signature", nil, stampNode(self)},
		{"task_handouts", "node_id, source_path, task_text, attempts, updated_at", "source_path, task_text", nil, stampNode(self)},
		{"audit_log", auditCols, "id", nil, func(r row) {
			old := r["id"].(int64)
			id := im.nextID()
			im.audits[old] = id
			r["id"], r["node_id"] = id, self
			r["decision_id"] = remap(im.decisions, r["decision_id"].(int64))
			r["corrects_audit_id"] = remap(im.audits, r["corrects_audit_id"].(int64))
			if r["status"] == domain.AuditStatusAutoAccepting {
				r["status"] = "escalated"
			}
		}},
		{"corrections", "id, node_id, audit_id, corrected_action, author, processed, sent, created_at", "id", nil, func(r row) {
			old := r["id"].(int64)
			id := im.nextID()
			im.corrections[old] = id
			r["id"], r["node_id"] = id, self
			r["audit_id"] = remap(im.audits, r["audit_id"].(int64))
		}},
		{"llm_retries", "id, node_id, audit_id, processed, created_at", "id",
			func(r row) bool { return r["processed"].(int64) != 0 }, func(r row) {
				r["id"], r["node_id"] = im.nextID(), self
				r["audit_id"] = remap(im.audits, r["audit_id"].(int64))
			}},
		{"task_reservations", `id, node_id, source_path, task_text, item_index, agent_id, pane_id, terminal_id,
			audit_id, reserved_at, restamps, confirmed_at`, "id", nil, func(r row) {
			r["id"], r["node_id"] = im.nextID(), self
			r["audit_id"] = remap(im.audits, r["audit_id"].(int64))
		}},
		{"kill_events", "id, node_id, state, scope, author, created_at", "id", nil, func(r row) {
			r["id"], r["node_id"] = im.nextID(), self
		}},
		{"agent_actions", agentActionColumns, "id",
			func(r row) bool {
				st := domain.AgentActionStatus(r["status"].(string))
				return st != domain.AgentActionPending && st != domain.AgentActionRunning
			}, func(r row) {
				r["id"], r["node_id"] = im.nextID(), self
				r["correction_id"] = remap(im.corrections, r["correction_id"].(int64))
			}},
		{"llm_requests", "id, node_id, request_id, signature, situation_type, agent_type, agent_id, context_json, status, created_at, session_id", "id",
			func(r row) bool { return r["status"].(string) != "pending" }, func(r row) {
				r["id"], r["node_id"] = im.nextID(), self
			}},
		{"llm_decisions", llmDecisionCols, "id",
			func(r row) bool { return r["status"].(string) != "pending" }, func(r row) {
				r["id"], r["node_id"] = im.nextID(), self
			}},
	}
}

// nextID allocates an id for an imported row, remembering the first failure;
// copyAll aborts the transaction on it rather than writing rows with no id.
//
// A destination with no allocator is the sqlite engine, whose INTEGER PRIMARY
// KEYs are normally AUTOINCREMENT. The copy still assigns them EXPLICITLY,
// counting up from the table's current MAX(id): the copy list is walked in
// ascending old-id order precisely so relative order — and every "newest by
// id" query — survives, and letting the database assign would put that
// property at the mercy of insert order while giving nothing back. SQLite
// advances sqlite_sequence for an explicit rowid above the current maximum, so
// ordinary AUTOINCREMENT inserts continue after the copied rows rather than
// colliding with them.
func (im *importer) nextID() int64 {
	if im.dst.ids == nil {
		im.seq[im.table]++
		return im.seq[im.table]
	}
	id, err := im.dst.ids.Next()
	if err != nil && im.idErr == nil {
		im.idErr = err
	}
	return id
}

func stampNode(self string) func(r row) { return func(r row) { r["node_id"] = self } }

// remap follows an id through a map; 0 ("none") stays 0, and a reference to a
// row that was not imported also becomes 0 rather than pointing at a stranger.
func remap(m map[int64]int64, old int64) int64 {
	if old == 0 {
		return 0
	}
	return m[old]
}

// remapFloor maps a decision-id floor ("exclude ids <= this"). The floor's own
// decision may be gone (DeleteSignature), so the nearest surviving OLD id at or
// below it carries the intent; none below means no floor.
func remapFloor(decisions map[int64]int64, oldFloor int64) int64 {
	if oldFloor == 0 {
		return 0
	}
	if id, ok := decisions[oldFloor]; ok {
		return id
	}
	best := int64(0)
	var bestOld int64 = -1
	for old, id := range decisions {
		if old <= oldFloor && old > bestOld {
			best, bestOld = id, old
		}
	}
	return best
}

// copyTable streams one table from src into tx.
//
// The node filter is applied to the SOURCE read, not to the write: a shared
// turso database holds every node's rows, and a local sqlite file belongs to
// one machine, so a copy out of the fleet has to choose. It is a predicate on
// the read rather than a `keep` func because a fleet-sized audit_log should not
// cross the process boundary only to be discarded.
//
// (This statement is invisible to TestEveryNodeOwnedStatementIsNodeScoped — the
// guard flattens a call's SQL argument, and every part of this one, the table
// name included, is a variable. That is pre-existing and unavoidable for a
// generic copier; the scoping is enforced here by migrateNodeScoped, which a
// test pins against the guard's own list.)
func (im *importer) copyTable(tx *sql.Tx, table, cols, order string, keep func(row) bool, xform func(row)) error {
	im.table = table
	names := splitCols(cols)
	query := `SELECT ` + cols + ` FROM ` + table + ` ORDER BY ` + order
	var args []any
	if im.srcNode != "" && migrateNodeScoped[table] {
		query = `SELECT ` + cols + ` FROM ` + table + ` WHERE node_id = ? ORDER BY ` + order
		args = []any{im.srcNode}
	}
	// Seeded before the first insert, from the DESTINATION: an explicit id
	// counting up from a stale maximum would collide with rows already there.
	if im.dst.ids == nil && migrateExplicitID[table] {
		var maxID int64
		if err := tx.QueryRowContext(im.ctx, `SELECT COALESCE(MAX(id), 0) FROM `+table).Scan(&maxID); err != nil {
			return err
		}
		im.seq[table] = maxID
	}
	rows, err := im.src.db.QueryContext(im.ctx, query, args...)
	if err != nil {
		return err
	}
	var batch []row
	for rows.Next() {
		vals := make([]any, len(names))
		ptrs := make([]any, len(names))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			rows.Close()
			return err
		}
		r := row{}
		for i, n := range names {
			r[n] = vals[i]
		}
		batch = append(batch, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	// The whole table is read before the first insert: the source and the
	// destination may be one and the same engine in tests, and an open reader
	// blocks a writer on Turso.
	insert := `INSERT OR IGNORE INTO ` + table + ` (` + cols + `) VALUES (` + inPlaceholders(len(names)) + `)`
	written := 0
	for _, r := range batch {
		if keep != nil && !keep(r) {
			continue
		}
		if xform != nil {
			xform(r)
		}
		vals := make([]any, len(names))
		for i, n := range names {
			vals[i] = r[n]
		}
		res, err := tx.ExecContext(im.ctx, insert, vals...)
		if err != nil {
			return err
		}
		// RowsAffected, not the loop count: content-keyed knowledge merges
		// into what the destination already holds (INSERT OR IGNORE), so a row
		// the destination already had is offered and not written — and a
		// report that counted it would tell the operator their rules were
		// copied when they were skipped. An engine that cannot answer is
		// counted optimistically rather than dropping the table from the
		// report altogether.
		if im.counts != nil {
			n, err := res.RowsAffected()
			if err != nil || n > 0 {
				written++
			}
		}
	}
	if im.counts != nil {
		if _, seen := im.counts[table]; !seen {
			im.order = append(im.order, table)
		}
		im.counts[table] += written
	}
	return nil
}

func splitCols(cols string) []string {
	parts := strings.Split(cols, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
