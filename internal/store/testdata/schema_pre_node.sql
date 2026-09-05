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
	id INTEGER PRIMARY KEY AUTOINCREMENT,
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
	id INTEGER PRIMARY KEY AUTOINCREMENT,
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
CREATE TABLE IF NOT EXISTS agent_rate (
	agent_id TEXT PRIMARY KEY,
	consecutive_auto INTEGER NOT NULL DEFAULT 0,
	window_start INTEGER NOT NULL DEFAULT 0,
	count_in_window INTEGER NOT NULL DEFAULT 0,
	paused INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS error_retries (
	error_signature TEXT PRIMARY KEY,
	agent_id TEXT NOT NULL DEFAULT '',
	retry_count INTEGER NOT NULL DEFAULT 0,
	updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS corrections (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	audit_id INTEGER NOT NULL,
	corrected_action TEXT NOT NULL,
	author TEXT NOT NULL DEFAULT 'operator',
	processed INTEGER NOT NULL DEFAULT 0,
	sent INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS kill_events (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	state TEXT NOT NULL,
	scope TEXT NOT NULL DEFAULT 'global',
	author TEXT NOT NULL DEFAULT 'operator',
	created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS llm_requests (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
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
CREATE TABLE IF NOT EXISTS llm_decisions (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
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
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	audit_id INTEGER NOT NULL,
	processed INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS operator (
	id TEXT PRIMARY KEY,
	label TEXT NOT NULL DEFAULT ''
);
INSERT OR IGNORE INTO operator (id, label) VALUES ('operator', 'Operator');
CREATE TABLE IF NOT EXISTS agent_names (
	agent_id TEXT PRIMARY KEY,
	name TEXT NOT NULL UNIQUE,
	disabled INTEGER NOT NULL DEFAULT 0,
	terminal_id TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL
);
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
CREATE INDEX IF NOT EXISTS idx_sig_embed_scope
	ON signature_embeddings(situation_type, agent_type);
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
	id INTEGER PRIMARY KEY AUTOINCREMENT,
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
-- Operator-requested actions the DAEMON must perform against a live agent.
-- The front ends may not touch herdr, so a confirmed reply, a task hand-out, a
-- permission-mode change and a manual capture are all queued here and drained
-- by the daemon. The control socket carries no reply channel, which is why the
-- row itself carries status/error/result: that is the ONLY way the surface that
-- queued the action can learn whether it landed.
CREATE TABLE IF NOT EXISTS agent_actions (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
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

-- The DAEMON's published view of what herdr is running, so the front ends can
-- read the herd without listing agents themselves.
--
-- Written only by the daemon, from the ListAgents calls it already makes plus
-- the transitions it already receives. gone_at soft-deletes rather than
-- DELETE: herdr recycles pane ids and an agent id IS a pane id, so a row whose
-- terminal_id changed is a DIFFERENT agent and must replace rather than merge —
-- the same doctrine as task_reservations.terminal_id.
CREATE TABLE IF NOT EXISTS agent_roster (
	agent_id TEXT PRIMARY KEY,
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
	gone_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_agent_roster_live ON agent_roster(gone_at, list_seq, agent_id);

-- Workspace and tab display metadata, published alongside the roster. Separate
-- from agent_roster because these are per-LOCATION, not per-agent, and the TUI
-- renders a number as well as a label.
CREATE TABLE IF NOT EXISTS herdr_locations (
	kind TEXT NOT NULL,
	id TEXT NOT NULL,
	label TEXT NOT NULL DEFAULT '',
	number INTEGER NOT NULL DEFAULT 0,
	workspace_id TEXT NOT NULL DEFAULT '',
	seen_at INTEGER NOT NULL,
	PRIMARY KEY (kind, id)
);

-- One row (id = 1) recording when the daemon last published a roster.
--
-- It is what tells "no agents are running" from "no daemon has ever published",
-- which an empty agent_roster cannot do on its own. Same doctrine as
-- Status.AgentsKnown, which reads it: a caller acting on an agent's ABSENCE
-- must be able to tell absence from ignorance.
CREATE TABLE IF NOT EXISTS roster_meta (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	published_at INTEGER NOT NULL
);

-- Per-item hand-out counter, kept SEPARATELY from task_reservations so it
-- survives the reservation row being retired on every reclaim. It is what caps
-- an item that can never be delivered from being resent forever.
CREATE TABLE IF NOT EXISTS task_handouts (
	source_path TEXT NOT NULL,
	task_text TEXT NOT NULL,
	attempts INTEGER NOT NULL DEFAULT 0,
	updated_at INTEGER NOT NULL,
	PRIMARY KEY (source_path, task_text)
);
