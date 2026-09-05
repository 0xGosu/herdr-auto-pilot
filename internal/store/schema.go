package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// autoincPlaceholder marks where the sqlite engine's schema says AUTOINCREMENT.
//
// The turso engine's schema leaves it out: ids there are allocated by
// TimeOrderedIDs (see IDAllocator), and AUTOINCREMENT would additionally keep
// a sqlite_sequence row that every node's inserts update — one row, every
// machine, every write, replayed to every other machine for no reason.
const autoincPlaceholder = "{AUTOINC}"

// schemaFor renders the schema for an engine.
func schemaFor(engine Engine) string {
	ai := " AUTOINCREMENT"
	if engine == EngineTurso {
		ai = ""
	}
	return strings.ReplaceAll(schema, autoincPlaceholder, ai)
}

// withoutIndexes drops the CREATE INDEX statements from a rendered schema.
//
// The migration runs the schema twice. The first pass may only create TABLES: a
// legacy database has audit_log without node_id yet, and an index on
// (node_id, …) created in the same batch would fail before the ADD COLUMN that
// supplies it. The second pass, after every column and rebuild, creates the
// indexes — including the ones a rebuild dropped with its table. Every index
// statement in the schema is written on ONE line so this can be a line filter.
func withoutIndexes(ddl string) string {
	lines := strings.Split(ddl, "\n")
	out := lines[:0]
	for _, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "CREATE INDEX") {
			continue
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

// Every node-owned table carries node_id, and every table whose natural key is
// a MACHINE-LOCAL identifier (a pane id, an error signature this machine saw, a
// task list this machine hands out from) keys on (node_id, …). The tables below
// that are rebuilt for a legacy database (see migrate) are declared as separate
// constants so the rebuild can create the new shape from the same text.

const createAgentRate = `CREATE TABLE IF NOT EXISTS agent_rate (
	node_id TEXT NOT NULL DEFAULT '',
	agent_id TEXT NOT NULL,
	consecutive_auto INTEGER NOT NULL DEFAULT 0,
	window_start INTEGER NOT NULL DEFAULT 0,
	count_in_window INTEGER NOT NULL DEFAULT 0,
	paused INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (node_id, agent_id)
);`

const createErrorRetries = `CREATE TABLE IF NOT EXISTS error_retries (
	node_id TEXT NOT NULL DEFAULT '',
	error_signature TEXT NOT NULL,
	agent_id TEXT NOT NULL DEFAULT '',
	retry_count INTEGER NOT NULL DEFAULT 0,
	updated_at INTEGER NOT NULL,
	PRIMARY KEY (node_id, error_signature)
);`

// agent_names: a name is unique WITHIN a node. Two machines may both have a
// "claude"; the unified view disambiguates with the node label.
const createAgentNames = `CREATE TABLE IF NOT EXISTS agent_names (
	node_id TEXT NOT NULL DEFAULT '',
	agent_id TEXT NOT NULL,
	name TEXT NOT NULL,
	disabled INTEGER NOT NULL DEFAULT 0,
	terminal_id TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	PRIMARY KEY (node_id, agent_id),
	UNIQUE (node_id, name)
);`

// The DAEMON's published view of what herdr is running, so the front ends can
// read the herd without listing agents themselves.
//
// Written only by the daemon, from the ListAgents calls it already makes plus
// the transitions it already receives. gone_at soft-deletes rather than
// DELETE: herdr recycles pane ids and an agent id IS a pane id, so a row whose
// terminal_id changed is a DIFFERENT agent and must replace rather than merge —
// the same doctrine as task_reservations.terminal_id.
const createAgentRoster = `CREATE TABLE IF NOT EXISTS agent_roster (
	node_id TEXT NOT NULL DEFAULT '',
	agent_id TEXT NOT NULL,
	pane_id TEXT NOT NULL DEFAULT '',
	tab_id TEXT NOT NULL DEFAULT '',
	workspace_id TEXT NOT NULL DEFAULT '',
	agent_type TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL DEFAULT '',
	terminal_id TEXT NOT NULL DEFAULT '',
	-- The agent's working directory, refreshed on its own slower TTL: it costs
	-- one herdr pane-get subprocess per agent, which is why the front ends only
	-- ever asked for it on the two surfaces that display one.
	cwd TEXT NOT NULL DEFAULT '',
	cwd_read_at INTEGER NOT NULL DEFAULT 0,
	-- The agent's position in herdr's own agent listing, recorded by a publish
	-- and preserved by a per-agent event.
	--
	-- Reading the roster back ordered by agent_id would NOT reproduce it: an
	-- agent id is a pane id, so the ordering is lexicographic over ids like
	-- w1:p2 and w1:p10 -- which puts the tenth pane before the second. The
	-- Agents tab renders the slice as given (TestAgentsListPreservesHerdrOrder),
	-- and herdr's order is the only one AgentTransition carries enough to
	-- reconstruct, since it has no intra-tab pane ordinal.
	--
	-- An agent first seen through an EVENT has no position -- rosterSeqUnknown
	-- sorts it last until the next publish places it -- which is a new agent
	-- appearing at the end for up to one tick, never an existing one moving.
	list_seq INTEGER NOT NULL DEFAULT 0,
	seen_at INTEGER NOT NULL,
	-- 0 while live. Set when a publish no longer sees the agent.
	gone_at INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (node_id, agent_id)
);`

// Workspace and tab display metadata, published alongside the roster. Separate
// from agent_roster because these are per-LOCATION, not per-agent, and the TUI
// renders a number as well as a label.
const createHerdrLocations = `CREATE TABLE IF NOT EXISTS herdr_locations (
	node_id TEXT NOT NULL DEFAULT '',
	kind TEXT NOT NULL,
	id TEXT NOT NULL,
	label TEXT NOT NULL DEFAULT '',
	number INTEGER NOT NULL DEFAULT 0,
	workspace_id TEXT NOT NULL DEFAULT '',
	seen_at INTEGER NOT NULL,
	PRIMARY KEY (node_id, kind, id)
);`

// One row per node recording when its daemon last published a roster.
//
// It is what tells "no agents are running" from "no daemon has ever published",
// which an empty agent_roster cannot do on its own. Same doctrine as
// Status.AgentsKnown, which reads it: a caller acting on an agent's ABSENCE
// must be able to tell absence from ignorance.
const createRosterMeta = `CREATE TABLE IF NOT EXISTS roster_meta (
	node_id TEXT PRIMARY KEY,
	published_at INTEGER NOT NULL
);`

// Per-item hand-out counter, kept SEPARATELY from task_reservations so it
// survives the reservation row being retired on every reclaim. It is what caps
// an item that can never be delivered from being resent forever.
const createTaskHandouts = `CREATE TABLE IF NOT EXISTS task_handouts (
	node_id TEXT NOT NULL DEFAULT '',
	source_path TEXT NOT NULL,
	task_text TEXT NOT NULL,
	attempts INTEGER NOT NULL DEFAULT 0,
	updated_at INTEGER NOT NULL,
	PRIMARY KEY (node_id, source_path, task_text)
);`

// task_lists holds the checklists of the `sqlite` task-source provider: one
// row per (node, name), the whole markdown list as one blob, and a revision the
// writers compare-and-swap on. Node-owned like every other operational table,
// but unlike them an OPERATOR on another node may edit a row (the unified
// Tasks view), which the daemon's hand-out path tolerates: it re-reads the
// list on every sweep and reserves through the same CAS.
const createTaskLists = `CREATE TABLE IF NOT EXISTS task_lists (
	node_id TEXT NOT NULL,
	name TEXT NOT NULL,
	agent_name TEXT NOT NULL DEFAULT '',
	content TEXT NOT NULL,
	revision INTEGER NOT NULL DEFAULT 1,
	updated_at INTEGER NOT NULL,
	PRIMARY KEY (node_id, name)
);`

// legacy_imports records, inside the import's own transaction, that a node's
// local sqlite database has been folded into the shared store. A file marker
// written AFTER the commit could be lost to a crash in between, and the next
// start would import everything again under fresh ids — duplicating every
// audit and decision row, which INSERT OR IGNORE cannot catch.
const createLegacyImports = `CREATE TABLE IF NOT EXISTS legacy_imports (
	node_id TEXT PRIMARY KEY,
	legacy_path TEXT NOT NULL,
	imported_at INTEGER NOT NULL
);`

const schema = `
CREATE TABLE IF NOT EXISTS signatures (
	signature TEXT PRIMARY KEY,
	situation_type TEXT NOT NULL,
	agent_type TEXT NOT NULL,
	mode TEXT NOT NULL DEFAULT 'shadow',
	consecutive_confirmations INTEGER NOT NULL DEFAULT 0,
	cached_confidence REAL NOT NULL DEFAULT 0,
	decision_floor_id INTEGER NOT NULL DEFAULT 0,
	guard_state TEXT NOT NULL DEFAULT '',
	updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS decisions (
	id INTEGER PRIMARY KEY` + autoincPlaceholder + `,
	node_id TEXT NOT NULL DEFAULT '',
	signature TEXT NOT NULL,
	situation_type TEXT NOT NULL,
	agent_type TEXT NOT NULL,
	chosen_action TEXT NOT NULL,
	source TEXT NOT NULL,
	confidence_at_decision REAL NOT NULL DEFAULT 0,
	is_correction INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_decisions_signature ON decisions(signature, id DESC);
-- CountSignaturesByMode is on the decision path once full self-prompting is on
-- (fspActive, and limitsInert through it), and this is the one table the plugin
-- grows without bound. Unindexed the count is a full scan per decision.
CREATE INDEX IF NOT EXISTS idx_signatures_mode ON signatures(mode);
CREATE TABLE IF NOT EXISTS audit_log (
	id INTEGER PRIMARY KEY` + autoincPlaceholder + `,
	node_id TEXT NOT NULL DEFAULT '',
	decision_id INTEGER NOT NULL DEFAULT 0,
	agent_id TEXT NOT NULL DEFAULT '',
	agent_type TEXT NOT NULL DEFAULT '',
	signature TEXT NOT NULL DEFAULT '',
	trigger TEXT NOT NULL,
	situation_type TEXT NOT NULL,
	action_or_escalation TEXT NOT NULL,
	input TEXT NOT NULL DEFAULT '',
	confidence REAL NOT NULL DEFAULT 0,
	llm_confidence INTEGER,
	rationale TEXT NOT NULL DEFAULT '',
	llm_output TEXT NOT NULL DEFAULT '',
	corrects_audit_id INTEGER NOT NULL DEFAULT 0,
	status TEXT NOT NULL DEFAULT '',
	suggestion TEXT NOT NULL DEFAULT '',
	pane_excerpt TEXT NOT NULL DEFAULT '',
	match_method TEXT NOT NULL DEFAULT '',
	match_score REAL NOT NULL DEFAULT 0,
	embed_error TEXT NOT NULL DEFAULT '',
	-- The row's SignatureResult, kept as the baseline for a later staleness
	-- comparison (see domain.AuditRecord.SigRaw). '' = no baseline.
	sig_raw TEXT NOT NULL DEFAULT '',
	sig_salient TEXT NOT NULL DEFAULT '',
	sig_verdict TEXT NOT NULL DEFAULT '',
	sig_salient_chars INTEGER NOT NULL DEFAULT 0,
	llm_session_id TEXT NOT NULL DEFAULT '',
	-- 1 when this row was auto-accepted while full self-prompting was active,
	-- so an operator can tell FSP's answers from timed auto-accept's (the
	-- status is 'auto_accepted' for both). 0 on every other row.
	while_fsp_mode_on INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_audit_status ON audit_log(status, id DESC);
CREATE INDEX IF NOT EXISTS idx_audit_agent ON audit_log(agent_id);
-- Serves LatestAuditForSignature (WHERE signature = ? ORDER BY id DESC LIMIT 1)
-- and the batched LatestAuditsForSignatures (MAX(id) GROUP BY signature) that
-- feeds the Rules-tab LAST column on every ~2s refresh.
CREATE INDEX IF NOT EXISTS idx_audit_signature ON audit_log(signature, id DESC);
-- The node-scoped twins of the two above: the daemon's own queue reads
-- (candidates, dedup, open-escalation checks) always filter on node_id first.
CREATE INDEX IF NOT EXISTS idx_audit_node_status ON audit_log(node_id, status, id DESC);
CREATE INDEX IF NOT EXISTS idx_audit_node_agent ON audit_log(node_id, agent_id);
` + createAgentRate + `
` + createErrorRetries + `
CREATE TABLE IF NOT EXISTS corrections (
	id INTEGER PRIMARY KEY` + autoincPlaceholder + `,
	node_id TEXT NOT NULL DEFAULT '',
	audit_id INTEGER NOT NULL,
	corrected_action TEXT NOT NULL,
	author TEXT NOT NULL DEFAULT 'operator',
	processed INTEGER NOT NULL DEFAULT 0,
	sent INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS kill_events (
	id INTEGER PRIMARY KEY` + autoincPlaceholder + `,
	node_id TEXT NOT NULL DEFAULT '',
	state TEXT NOT NULL,
	scope TEXT NOT NULL DEFAULT 'global',
	author TEXT NOT NULL DEFAULT 'operator',
	created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS llm_requests (
	id INTEGER PRIMARY KEY` + autoincPlaceholder + `,
	node_id TEXT NOT NULL DEFAULT '',
	request_id TEXT NOT NULL UNIQUE,
	signature TEXT NOT NULL,
	situation_type TEXT NOT NULL,
	agent_type TEXT NOT NULL,
	agent_id TEXT NOT NULL DEFAULT '',
	context_json TEXT NOT NULL,
	status TEXT NOT NULL DEFAULT 'pending',
	created_at INTEGER NOT NULL,
	session_id TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_llm_requests_node_status ON llm_requests(node_id, status);
CREATE TABLE IF NOT EXISTS llm_decisions (
	id INTEGER PRIMARY KEY` + autoincPlaceholder + `,
	node_id TEXT NOT NULL DEFAULT '',
	request_id TEXT NOT NULL,
	signature TEXT NOT NULL DEFAULT '',
	situation_type TEXT NOT NULL DEFAULT '',
	agent_type TEXT NOT NULL DEFAULT '',
	action TEXT NOT NULL,
	option_id TEXT NOT NULL DEFAULT '',
	rationale TEXT NOT NULL DEFAULT '',
	confident_score INTEGER NOT NULL DEFAULT -1,
	captured_output TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL DEFAULT 'pending',
	created_at INTEGER NOT NULL,
	task_actions_json TEXT NOT NULL DEFAULT '',
	send_task TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS llm_retries (
	id INTEGER PRIMARY KEY` + autoincPlaceholder + `,
	node_id TEXT NOT NULL DEFAULT '',
	audit_id INTEGER NOT NULL,
	processed INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS operator (
	id TEXT PRIMARY KEY,
	label TEXT NOT NULL DEFAULT ''
);
INSERT OR IGNORE INTO operator (id, label) VALUES ('operator', 'Operator');
` + createAgentNames + `
CREATE TABLE IF NOT EXISTS signature_embeddings (
	signature TEXT PRIMARY KEY,
	situation_type TEXT NOT NULL,
	agent_type TEXT NOT NULL,
	model TEXT NOT NULL DEFAULT '',
	dims INTEGER NOT NULL DEFAULT 0,
	vector BLOB,
	salient TEXT NOT NULL,
	created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_sig_embed_scope ON signature_embeddings(situation_type, agent_type);
CREATE TABLE IF NOT EXISTS signature_snapshots (
	signature TEXT PRIMARY KEY,
	pane_excerpt TEXT NOT NULL,
	created_at INTEGER NOT NULL
);
-- Ledger of unattended task hand-outs: which "[-]" marks the daemon wrote
-- itself, for which agent, and whether that agent was ever seen working
-- afterwards. Unconfirmed rows whose agent parked again are what the idle
-- sweep returns to "[ ]"; a "[-]" with no row here is somebody else's and is
-- never touched.
CREATE TABLE IF NOT EXISTS task_reservations (
	id INTEGER PRIMARY KEY` + autoincPlaceholder + `,
	node_id TEXT NOT NULL DEFAULT '',
	source_path TEXT NOT NULL,
	task_text TEXT NOT NULL,
	item_index INTEGER NOT NULL DEFAULT 0,
	agent_id TEXT NOT NULL,
	pane_id TEXT NOT NULL,
	terminal_id TEXT NOT NULL DEFAULT '',
	audit_id INTEGER NOT NULL DEFAULT 0,
	reserved_at INTEGER NOT NULL,
	restamps INTEGER NOT NULL DEFAULT 0,
	confirmed_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_task_res_agent ON task_reservations(agent_id);
CREATE INDEX IF NOT EXISTS idx_task_res_node_agent ON task_reservations(node_id, agent_id);
-- Operator-requested actions the DAEMON must perform against a live agent.
-- The front ends may not touch herdr, so a confirmed reply, a task hand-out, a
-- permission-mode change and a manual capture are all queued here and drained
-- by the daemon. The control socket carries no reply channel, which is why the
-- row itself carries status/error/result: that is the ONLY way the surface that
-- queued the action can learn whether it landed.
--
-- node_id is the node whose daemon must run it — the TARGET agent's node, which
-- under a shared database may not be the node the operator typed on.
CREATE TABLE IF NOT EXISTS agent_actions (
	id INTEGER PRIMARY KEY` + autoincPlaceholder + `,
	node_id TEXT NOT NULL DEFAULT '',
	kind TEXT NOT NULL,
	target TEXT NOT NULL DEFAULT '',
	payload_json TEXT NOT NULL DEFAULT '',
	-- The correction this action delivers, 0 for kinds that deliver none.
	-- An explicit column rather than a field inside payload_json because the
	-- correction drain must JOIN on it: a correction whose delivery is still
	-- queued must not be processed yet (see UnprocessedCorrections).
	correction_id INTEGER NOT NULL DEFAULT 0,
	-- Herdr's terminal identity for the target pane, as it stood when the
	-- action was queued. Herdr RECYCLES pane ids, so a pane id alone is not an
	-- address: between queueing and delivery the terminal behind it can be
	-- replaced, and the reply would be typed at a stranger. Empty means "not
	-- observed", which is not evidence of sameness and is never treated as a
	-- match. Same doctrine as task_reservations.terminal_id.
	terminal_id TEXT NOT NULL DEFAULT '',
	-- 1 once this action may already have had its side effect — set
	-- IMMEDIATELY BEFORE the keystrokes, so a daemon that dies between the
	-- send and the outcome write leaves evidence behind. Delivery is not
	-- idempotent, so such a row must never be replayed; the startup reclaim
	-- fails it instead of returning it to the queue.
	side_effect INTEGER NOT NULL DEFAULT 0,
	author TEXT NOT NULL DEFAULT 'operator',
	status TEXT NOT NULL DEFAULT 'pending',
	error TEXT NOT NULL DEFAULT '',
	result_json TEXT NOT NULL DEFAULT '',
	attempts INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_agent_actions_status ON agent_actions(status, id);
CREATE INDEX IF NOT EXISTS idx_agent_actions_correction ON agent_actions(correction_id);
CREATE INDEX IF NOT EXISTS idx_agent_actions_node_status ON agent_actions(node_id, status, id);
` + createAgentRoster + `
CREATE INDEX IF NOT EXISTS idx_agent_roster_live ON agent_roster(node_id, gone_at, list_seq, agent_id);
` + createHerdrLocations + `
` + createRosterMeta + `
` + createTaskHandouts + `
` + createTaskLists + `
` + createLegacyImports + `
-- One row per installation sharing this database: who is out there, what
-- version they run, and when their daemon last checked in. A node whose
-- last_seen is older than a few heartbeats is shown as stale, and its rows are
-- never acted on by anyone else's daemon.
CREATE TABLE IF NOT EXISTS nodes (
	node_id TEXT PRIMARY KEY,
	label TEXT NOT NULL DEFAULT '',
	hap_version TEXT NOT NULL DEFAULT '',
	started_at INTEGER NOT NULL DEFAULT 0,
	last_seen INTEGER NOT NULL DEFAULT 0,
	-- Until when a TUI on this node is watching the fleet; other nodes' daemons
	-- publish their roster faster while anyone is (see daemon.rosterDemand).
	watching_until INTEGER NOT NULL DEFAULT 0
);
`

// columnAdd is one ALTER TABLE … ADD COLUMN a legacy database may still need.
type columnAdd struct{ table, column, ddl string }

// columnAdds are the columns added since each table first shipped, in order.
var columnAdds = []columnAdd{
	{"audit_log", "agent_type", `ALTER TABLE audit_log ADD COLUMN agent_type TEXT NOT NULL DEFAULT ''`},
	{"audit_log", "pane_excerpt", `ALTER TABLE audit_log ADD COLUMN pane_excerpt TEXT NOT NULL DEFAULT ''`},
	// Nullable: NULL = no LLM score (learned/operator/pre-decision rows),
	// distinct from a reported 0.
	{"audit_log", "llm_confidence", `ALTER TABLE audit_log ADD COLUMN llm_confidence INTEGER`},
	// How an escalation's signature resolved to its rule, plus any
	// per-event embedding failure (empty/zero on legacy and auto rows).
	{"audit_log", "match_method", `ALTER TABLE audit_log ADD COLUMN match_method TEXT NOT NULL DEFAULT ''`},
	{"audit_log", "match_score", `ALTER TABLE audit_log ADD COLUMN match_score REAL NOT NULL DEFAULT 0`},
	{"audit_log", "embed_error", `ALTER TABLE audit_log ADD COLUMN embed_error TEXT NOT NULL DEFAULT ''`},
	// The row's full SignatureResult: the never-remapped content hash, the
	// masked salient, the over-mask verdict, and the salient window used.
	// Written on every decision-pipeline row (status 'auto' as well as
	// 'escalated'), which is what lets a row later demoted to escalated
	// carry a comparable baseline. NOT backfilled: '' means "no baseline",
	// so every pre-migration row is permanently ineligible for auto-accept
	// — the fail-closed default.
	{"audit_log", "sig_raw", `ALTER TABLE audit_log ADD COLUMN sig_raw TEXT NOT NULL DEFAULT ''`},
	{"audit_log", "sig_salient", `ALTER TABLE audit_log ADD COLUMN sig_salient TEXT NOT NULL DEFAULT ''`},
	{"audit_log", "sig_verdict", `ALTER TABLE audit_log ADD COLUMN sig_verdict TEXT NOT NULL DEFAULT ''`},
	{"audit_log", "sig_salient_chars", `ALTER TABLE audit_log ADD COLUMN sig_salient_chars INTEGER NOT NULL DEFAULT 0`},
	// The CLI conversation the LLM ran as, and the name of the transcript
	// file it left behind. '' on learned/operator rows, on rows from before
	// this column existed, and whenever the CLI neither accepted nor
	// reported an id — it is bookkeeping, never load-bearing. NOT
	// backfilled: there is nothing to backfill it FROM.
	{"audit_log", "llm_session_id", `ALTER TABLE audit_log ADD COLUMN llm_session_id TEXT NOT NULL DEFAULT ''`},
	// 1 when full self-prompting caused this auto-accept. The status alone
	// cannot say so — 'auto_accepted' is what timed auto-accept writes too —
	// and the two have very different meanings to an operator reviewing the
	// log. NOT backfilled: 0 on every pre-migration row, which reads as
	// "not attributable to FSP" rather than as a false claim either way.
	{"audit_log", "while_fsp_mode_on", `ALTER TABLE audit_log ADD COLUMN while_fsp_mode_on INTEGER NOT NULL DEFAULT 0`},
	{"llm_requests", "session_id", `ALTER TABLE llm_requests ADD COLUMN session_id TEXT NOT NULL DEFAULT ''`},
	{"llm_decisions", "confident_score", `ALTER TABLE llm_decisions ADD COLUMN confident_score INTEGER NOT NULL DEFAULT -1`},
	// A pre-delivery task review's submission: the ordered checklist edits
	// (JSON) and the reference of the task to deliver once they are
	// applied. Empty on every other kind of decision, and on every row
	// written before the review existed.
	{"llm_decisions", "task_actions_json", `ALTER TABLE llm_decisions ADD COLUMN task_actions_json TEXT NOT NULL DEFAULT ''`},
	{"llm_decisions", "send_task", `ALTER TABLE llm_decisions ADD COLUMN send_task TEXT NOT NULL DEFAULT ''`},
	{"llm_requests", "agent_id", `ALTER TABLE llm_requests ADD COLUMN agent_id TEXT NOT NULL DEFAULT ''`},
	// sent = 1 when the correction actually reached the agent pane;
	// drives the daemon's post-action unblock self-check. Written by the
	// daemon after its own delivery (front ends record it unsent).
	{"corrections", "sent", `ALTER TABLE corrections ADD COLUMN sent INTEGER NOT NULL DEFAULT 0`},
	// Per-signature decision-id floor: decisions with id <= this are kept
	// but excluded from confidence/graduation (stamped by an operator reset).
	{"signatures", "decision_floor_id", `ALTER TABLE signatures ADD COLUMN decision_floor_id INTEGER NOT NULL DEFAULT 0`},
	// Operator-owned per-agent automation switch. Kept on agent_names so
	// renames preserve it and disabled agents remain visible by name.
	{"agent_names", "disabled", `ALTER TABLE agent_names ADD COLUMN disabled INTEGER NOT NULL DEFAULT 0`},
	// Herdr's unique per-terminal id. Herdr reuses compact pane ids, so
	// this is what tells "same agent" from "new terminal on a recycled
	// pane id" (issue #158). '' = not yet observed.
	{"agent_names", "terminal_id", `ALTER TABLE agent_names ADD COLUMN terminal_id TEXT NOT NULL DEFAULT ''`},
	// The terminal behind the action's target pane at queue time, and the
	// "this may already have been delivered" marker. Both were added after
	// agent_actions shipped, so CREATE TABLE IF NOT EXISTS would skip them
	// on any database that already has the table.
	{"agent_actions", "terminal_id", `ALTER TABLE agent_actions ADD COLUMN terminal_id TEXT NOT NULL DEFAULT ''`},
	{"agent_actions", "side_effect", `ALTER TABLE agent_actions ADD COLUMN side_effect INTEGER NOT NULL DEFAULT 0`},
	{"agent_roster", "list_seq", `ALTER TABLE agent_roster ADD COLUMN list_seq INTEGER NOT NULL DEFAULT 0`},
}

// nodeOwnedIntegerPKTables are the INTEGER PRIMARY KEY tables that gained a
// node_id column. They are ALTERed and backfilled rather than rebuilt: their
// key is already fleet-unique (ids are allocated with node bits under turso,
// and a single sqlite node never collides with itself).
var nodeOwnedIntegerPKTables = []string{
	"decisions", "audit_log", "corrections", "kill_events",
	"llm_requests", "llm_decisions", "llm_retries", "task_reservations", "agent_actions",
}

// rebuild describes a legacy table whose PRIMARY KEY has to gain node_id, which
// SQLite cannot do in place: a new table is created from the current DDL, the
// rows are copied across stamped with this node's id, and the old one is
// dropped and renamed over. Guarded by the presence of node_id on the live
// table, so a second open finds nothing to do.
type rebuild struct {
	table  string
	create string // the CREATE TABLE IF NOT EXISTS … constant
	// columns copied from the legacy table, in the new table's order after
	// node_id. Every one must exist on the legacy table by the time the
	// rebuild runs — the ADD COLUMN loop runs first for exactly that reason.
	columns string
}

var rebuilds = []rebuild{
	{"agent_rate", createAgentRate, "agent_id, consecutive_auto, window_start, count_in_window, paused"},
	{"error_retries", createErrorRetries, "error_signature, agent_id, retry_count, updated_at"},
	{"agent_names", createAgentNames, "agent_id, name, disabled, terminal_id, created_at"},
	{"agent_roster", createAgentRoster,
		"agent_id, pane_id, tab_id, workspace_id, agent_type, status, terminal_id, cwd, cwd_read_at, list_seq, seen_at, gone_at"},
	{"herdr_locations", createHerdrLocations, "kind, id, label, number, workspace_id, seen_at"},
	{"task_handouts", createTaskHandouts, "source_path, task_text, attempts, updated_at"},
}

func (s *Store) migrate(between func() error) error {
	ctx := context.Background()
	// step runs the caller's between-steps hook (see MigrateWith); nil = none.
	step := func() error {
		if between == nil {
			return nil
		}
		return between()
	}
	ddl := schemaFor(s.engine)
	if err := step(); err != nil {
		return err
	}
	if _, err := s.db.Exec(withoutIndexes(ddl)); err != nil {
		return fmt.Errorf("migrate schema: %w", err)
	}
	// Column additions to pre-existing tables (CREATE IF NOT EXISTS above
	// only covers new tables). Idempotent by inspection: the column is added
	// only when PRAGMA table_info does not already list it. (Matching the
	// engine's "duplicate column name" error text instead would tie this to one
	// engine's wording.)
	for _, add := range columnAdds {
		if err := step(); err != nil {
			return err
		}
		if err := s.addColumnIfMissing(ctx, add.table, add.column, add.ddl); err != nil {
			return err
		}
	}
	// node_id on the INTEGER PRIMARY KEY tables, then every row that predates
	// it becomes this node's: a legacy database has exactly one owner.
	for _, table := range nodeOwnedIntegerPKTables {
		if err := step(); err != nil {
			return err
		}
		if err := s.addColumnIfMissing(ctx, table, "node_id",
			`ALTER TABLE `+table+` ADD COLUMN node_id TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
		if _, err := s.db.ExecContext(ctx,
			`UPDATE `+table+` SET node_id = ? WHERE node_id = ''`, s.self); err != nil {
			return fmt.Errorf("migrate: stamp %s with node id: %w", table, err)
		}
	}
	// Composite keys cannot be added in place: rebuild each legacy table once.
	for _, r := range rebuilds {
		has, err := s.hasColumn(ctx, r.table, "node_id")
		if err != nil {
			return err
		}
		if has {
			continue
		}
		if err := step(); err != nil {
			return err
		}
		if err := s.rebuildWithNodeID(ctx, r); err != nil {
			return err
		}
	}
	// roster_meta went from one row (id = 1) to one row per node.
	if has, err := s.hasColumn(ctx, "roster_meta", "node_id"); err != nil {
		return err
	} else if !has {
		if err := step(); err != nil {
			return err
		}
		if err := s.tx(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, strings.Replace(createRosterMeta,
				"IF NOT EXISTS roster_meta", "roster_meta_new", 1)); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO roster_meta_new (node_id, published_at) SELECT ?, published_at FROM roster_meta`,
				s.self); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `DROP TABLE roster_meta`); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `ALTER TABLE roster_meta_new RENAME TO roster_meta`)
			return err
		}); err != nil {
			return fmt.Errorf("migrate: rebuild roster_meta: %w", err)
		}
	}
	// A rebuild drops the table's indexes with it; the second pass recreates
	// them (and is a no-op for everything else).
	if err := step(); err != nil {
		return err
	}
	if _, err := s.db.Exec(ddl); err != nil {
		return fmt.Errorf("migrate schema (indexes): %w", err)
	}
	// Issue #155: pre-fix approval salients carried only the permission verb,
	// so very different approval screens shared one embedding row. Post-fix
	// salients always carry a "| options:" segment. The old verb-only rows
	// must go: left in place, a new salient could cosine/BM25-match one and
	// remap onto the old over-broad signature, re-bridging the collision the
	// format change closed. Learned rules and audit rows are kept — their old
	// keys simply become unreachable. The remote-env picker's salient is
	// verb-only by design and stays. Idempotent: matches zero rows once the
	// old-format rows are gone.
	if _, err := s.db.Exec(
		`DELETE FROM signature_embeddings
		  WHERE situation_type = ?
		    AND salient LIKE 'permission:%'
		    AND salient NOT LIKE '%| options:%'
		    AND salient <> 'permission:' || ?`,
		string(domain.SituationApproval), domain.PermissionVerbSelectRemoteEnv,
	); err != nil {
		return fmt.Errorf("migrate prune verb-only approval embeddings: %w", err)
	}
	// Issue #175: LLM decisions used to be recorded without a signatures state
	// row, leaving the learned rule invisible to `signatures list` and
	// unaddressable by delete/reset. The daemon now creates the row at
	// decision time; this backfills the rows such databases already lack.
	// SQLite's bare-column-with-MAX rule makes the inner select carry each
	// signature's newest decision BY ID — the autoincrement PK is strictly
	// insertion-ordered, unlike created_at, whose millisecond values can tie
	// within a burst and break the tie arbitrarily. Idempotent: INSERT OR
	// IGNORE never touches an existing row.
	//
	// sqlite ONLY. Turso does not implement the bare-column rule (it returns an
	// arbitrary row's columns beside the MAX — verified against 0.7.2), so the
	// same statement there would backfill the wrong situation_type silently. A
	// turso database is created by builds that write the signatures row at
	// decision time, so it never has the rows this repairs.
	if s.engine == EngineSQLite {
		if _, err := s.db.Exec(
			`INSERT OR IGNORE INTO signatures (signature, situation_type, agent_type,
			    mode, consecutive_confirmations, cached_confidence, decision_floor_id,
			    guard_state, updated_at)
			 SELECT signature, situation_type, agent_type, ?, 0, 0, 0, '', created_at
			   FROM (SELECT signature, situation_type, agent_type, created_at, MAX(id)
			           FROM decisions GROUP BY signature)`,
			string(domain.ModeShadow),
		); err != nil {
			return fmt.Errorf("migrate backfill signature rows: %w", err)
		}
	}
	return nil
}

// SchemaCurrent reports whether every migration step has already been applied
// — every table exists with every column, including node_id on the rebuilt
// ones. Under the shared engine a node that finds the schema current issues no
// DDL at all, and a node that does not waits for the schema lead (see
// turso.PrepareSharedSchema) before issuing any.
func (s *Store) SchemaCurrent(ctx context.Context) (bool, error) {
	for _, add := range columnAdds {
		has, err := s.hasColumn(ctx, add.table, add.column)
		if err != nil || !has {
			return false, err
		}
	}
	for _, table := range nodeOwnedIntegerPKTables {
		has, err := s.hasColumn(ctx, table, "node_id")
		if err != nil || !has {
			return false, err
		}
	}
	for _, r := range rebuilds {
		has, err := s.hasColumn(ctx, r.table, "node_id")
		if err != nil || !has {
			return false, err
		}
	}
	// Tables that exist only since the node-scoped schema. Every table the
	// migration can CREATE must be listed here: under the shared engine an
	// already-migrated fleet reads as current and issues no DDL at all, so a
	// table missing from this list would never be created on any node that
	// bootstrapped before it existed. (PRAGMA table_info on an absent table
	// yields no rows, so hasColumn answers false for it.)
	for _, table := range []string{"roster_meta", "nodes", "task_lists", "legacy_imports"} {
		has, err := s.hasColumn(ctx, table, "node_id")
		if err != nil || !has {
			return false, err
		}
	}
	return true, nil
}

// rebuildWithNodeID converts one legacy table to its node-keyed shape in a
// single transaction: create the new table from the current DDL, copy every
// row stamped with this node's id, drop the old table, rename.
func (s *Store) rebuildWithNodeID(ctx context.Context, r rebuild) error {
	newTable := r.table + "_new"
	create := strings.Replace(r.create, "IF NOT EXISTS "+r.table, newTable, 1)
	err := s.tx(ctx, func(tx *sql.Tx) error {
		// A crashed earlier attempt may have left the scratch table behind.
		if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS `+newTable); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, create); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO `+newTable+` (node_id, `+r.columns+`) SELECT ?, `+r.columns+` FROM `+r.table,
			s.self); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DROP TABLE `+r.table); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `ALTER TABLE `+newTable+` RENAME TO `+r.table)
		return err
	})
	if err != nil {
		return fmt.Errorf("migrate: rebuild %s with node_id: %w", r.table, err)
	}
	return nil
}

// addColumnIfMissing runs ddl unless table already has column.
func (s *Store) addColumnIfMissing(ctx context.Context, table, column, ddl string) error {
	has, err := s.hasColumn(ctx, table, column)
	if err != nil {
		return err
	}
	if has {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, ddl); err != nil {
		return fmt.Errorf("migrate column %s.%s: %w", table, column, err)
	}
	return nil
}

// hasColumn reports whether table has a column named column, by reading
// PRAGMA table_info — the one schema-inspection form both engines answer the
// same way. table is always one of this file's constants, never operator input.
func (s *Store) hasColumn(ctx context.Context, table, column string) (bool, error) {
	rows, err := s.db.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return false, fmt.Errorf("migrate: inspect %s: %w", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return false, fmt.Errorf("migrate: inspect %s: %w", table, err)
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}
