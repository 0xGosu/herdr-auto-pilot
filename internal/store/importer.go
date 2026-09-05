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
// already has history. markerPath records that it happened; a missing legacy
// file or a present marker is a no-op.
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

	imp := &importer{src: src, dst: dst, ctx: ctx,
		decisions: map[int64]int64{}, audits: map[int64]int64{}, corrections: map[int64]int64{}}
	if err := dst.tx(ctx, imp.run); err != nil {
		return fmt.Errorf("import: %w", err)
	}
	if err := os.WriteFile(markerPath, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600); err != nil {
		return fmt.Errorf("import: write marker: %w", err)
	}
	slog.Info("imported the local sqlite database into the shared store",
		"from", legacyPath, "decisions", len(imp.decisions), "audit_rows", len(imp.audits))
	return nil
}

type importer struct {
	src, dst    *Store
	ctx         context.Context
	decisions   map[int64]int64
	audits      map[int64]int64
	corrections map[int64]int64
}

// row is one legacy row, by column name.
type row map[string]any

func (im *importer) run(tx *sql.Tx) error {
	self := im.dst.self
	steps := []struct {
		table string
		cols  string
		order string
		// keep decides whether a row is imported; xform rewrites it (ids,
		// references, node) before the insert.
		keep  func(r row) bool
		xform func(r row)
	}{
		{"operator", "id, label", "id", nil, nil},
		{"decisions", decisionCols, "id", nil, func(r row) {
			old := r["id"].(int64)
			id := im.dst.ids.Next()
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
			id := im.dst.ids.Next()
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
			id := im.dst.ids.Next()
			im.corrections[old] = id
			r["id"], r["node_id"] = id, self
			r["audit_id"] = remap(im.audits, r["audit_id"].(int64))
		}},
		{"llm_retries", "id, node_id, audit_id, processed, created_at", "id",
			func(r row) bool { return r["processed"].(int64) != 0 }, func(r row) {
				r["id"], r["node_id"] = im.dst.ids.Next(), self
				r["audit_id"] = remap(im.audits, r["audit_id"].(int64))
			}},
		{"task_reservations", `id, node_id, source_path, task_text, item_index, agent_id, pane_id, terminal_id,
			audit_id, reserved_at, restamps, confirmed_at`, "id", nil, func(r row) {
			r["id"], r["node_id"] = im.dst.ids.Next(), self
			r["audit_id"] = remap(im.audits, r["audit_id"].(int64))
		}},
		{"kill_events", "id, node_id, state, scope, author, created_at", "id", nil, func(r row) {
			r["id"], r["node_id"] = im.dst.ids.Next(), self
		}},
		{"agent_actions", agentActionColumns, "id",
			func(r row) bool {
				st := domain.AgentActionStatus(r["status"].(string))
				return st != domain.AgentActionPending && st != domain.AgentActionRunning
			}, func(r row) {
				r["id"], r["node_id"] = im.dst.ids.Next(), self
				r["correction_id"] = remap(im.corrections, r["correction_id"].(int64))
			}},
		{"llm_requests", "id, node_id, request_id, signature, situation_type, agent_type, agent_id, context_json, status, created_at, session_id", "id",
			func(r row) bool { return r["status"].(string) != "pending" }, func(r row) {
				r["id"], r["node_id"] = im.dst.ids.Next(), self
			}},
		{"llm_decisions", llmDecisionCols, "id",
			func(r row) bool { return r["status"].(string) != "pending" }, func(r row) {
				r["id"], r["node_id"] = im.dst.ids.Next(), self
			}},
	}
	for _, st := range steps {
		if err := im.copyTable(tx, st.table, st.cols, st.order, st.keep, st.xform); err != nil {
			return fmt.Errorf("%s: %w", st.table, err)
		}
	}
	return nil
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
func (im *importer) copyTable(tx *sql.Tx, table, cols, order string, keep func(row) bool, xform func(row)) error {
	names := splitCols(cols)
	rows, err := im.src.db.QueryContext(im.ctx, `SELECT `+cols+` FROM `+table+` ORDER BY `+order)
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
	for _, r := range batch {
		if keep != nil && !keep(r) {
			continue
		}
		if xform != nil {
			xform(r)
		}
		args := make([]any, len(names))
		for i, n := range names {
			args[i] = r[n]
		}
		if _, err := tx.ExecContext(im.ctx, insert, args...); err != nil {
			return err
		}
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
