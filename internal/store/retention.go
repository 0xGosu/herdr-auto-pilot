package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// LLMPayloadGrace is how long a consult keeps its bulky payloads —
// llm_requests.context_json and llm_decisions.captured_output — after it was
// RAISED, before the sweep blanks them.
//
// It is measured from created_at because neither table has a resolved_at
// column, so "an hour after it was raised" is the guarantee, not "an hour after
// it finished". That is the conservative direction for the hazard it exists
// for: a consult lives seconds, so an hour past its creation is long past any
// reader.
//
// It exists because "the row reached a terminal status" is NOT proof that
// nothing will read the payload again. Neither GetLLMRequest nor
// LLMDecisionByRequest filters on status: the MCP server resolves an explicit
// request_id through the first (mcpserver.resolveRequest) and the adapter reads
// a staged decision back through the second, so an LLM CLI still running — or
// an auto-repair attempt on the same request — would be handed an EMPTY context
// if the payload were cleared the instant the status flipped.
//
// It is deliberately NOT the operator's row-retention window: that one may be
// set to 0 ("keep nothing"), and this grace is a liveness guard rather than a
// storage preference.
const LLMPayloadGrace = time.Hour

// RowRetentionFloor is how far back PruneAgedRows clamps its own cutoff,
// whatever the operator configured.
//
// It is the twin of AuditExcerptDedupMargin, and it exists for the same reason:
// Logging.RowRetention documents 0 as a real setting ("keep nothing finished"),
// which without a floor makes the cutoff `now` — and a row can then be deleted
// in the same second it reached a terminal status. Two paths break there.
// frontend.AwaitAgentAction POLLS a finished agent_actions row to learn whether
// the delivery landed, which is the only channel it has (the control socket
// carries no reply), and the daemon's own delivery bound (actionStaleAfter) is
// two minutes. A terminal llm_requests row is likewise still reachable by
// request_id from the MCP server.
//
// A separate constant from LLMPayloadGrace despite the equal value: that one
// bounds a COLUMN blank against a live reader, this one bounds a ROW delete
// against a poller. Retuning either must not silently retune the other.
const RowRetentionFloor = time.Hour

// PruneAgedRows deletes FINISHED bookkeeping rows older than cutoff and blanks
// the payloads of finished consults.
//
// It is the row-level twin of PruneAuditExcerpts, and it follows the same
// doctrine: the exemptions are the safety control, and every one of them is a
// row some path may still act on rather than a row that merely looks recent.
//
//   - agent_actions: only a TERMINAL status. A pending row is the cross-machine
//     control queue itself, and a running one is claimed by a daemon mid-
//     delivery. Even a terminal row is load-bearing until it is read:
//     frontend.AwaitAgentAction polls AgentActionByID and returns act.Result /
//     act.Error, which is the ONLY way the surface that queued the action learns
//     whether it landed — the control socket carries no reply channel. That poll
//     is what RowRetentionFloor bounds; the configured window alone cannot,
//     since 0 is a supported setting.
//   - llm_requests / llm_decisions: never 'pending'. A pending request is the
//     consult retry guard and a pending decision is one the daemon has not
//     re-gated yet.
//   - corrections: only processed ones, AND only when no agent_action still
//     references them. correction_id is a real reference with no foreign key
//     behind it, and UnprocessedCorrections withholds a correction whose
//     delivery is still queued — deleting the row out from under that pairing
//     would let the next pass resolve an escalation nothing answered.
//   - llm_retries: only processed ones. An unprocessed row is a queued retry,
//     and it is also what PruneAuditExcerpts checks before blanking.
//   - kill_events: never the newest row IN ITS SCOPE. Only the latest decides,
//     and LatestKillEventOn reads it `WHERE node_id = ? AND scope = 'global'`
//     — the table carries a SECOND stream, the full self-prompting toggles
//     frontend.recordFSPToggle writes (including the daemon's own ceiling
//     stand-down). A survivor guard keyed on MAX(id) alone therefore deletes a
//     standing global PAUSE the moment any newer FSP row exists, LatestKillEvent
//     returns nil, and the herd silently resumes — the exact trap
//     LatestKillEventOn's own scope filter was added for. Hence the correlated
//     subquery on (node_id, scope).
//   - task_reservations: only CONFIRMED hand-outs. An unconfirmed row is the
//     ledger entry reclaimStrandedTasks needs to return an item to "[ ]"; delete
//     it and the "[-]" mark is stranded forever, since a "[-]" with no ledger
//     row is treated as somebody else's and never touched.
//
// audit_log and decisions are deliberately NOT swept. The audit trail's own
// design says the row survives its excerpt "so `hap audit` history stays
// complete", and decisions feeds CountDecisionsForSignature, so deleting from
// it would change learned behaviour rather than just reclaim space. Both are
// operator decisions, not cleanup.
//
// Every statement is scoped to this node. Under a shared database another
// machine's daemon owns its own rows and is running this same sweep against
// them; deleting them here would race its writers over rows this node never
// wrote. Each is issued at its own call site rather than from a table of
// queries, because TestEveryNodeOwnedStatementIsNodeScoped flattens a CALL's
// SQL argument: a query reached through a struct field is invisible to it, and
// this is the last file in the repo that should fall outside that guard.
//
// The counts are returned per table so the daemon can log what went.
func (s *Store) PruneAgedRows(ctx context.Context, now, cutoff time.Time) (domain.PruneCounts, error) {
	var c domain.PruneCounts
	// Whatever the operator configured, never reach a row a live poller or the
	// MCP server may still be reading. See RowRetentionFloor.
	if latest := now.Add(-RowRetentionFloor); cutoff.After(latest) {
		cutoff = latest
	}
	at := unix(cutoff)
	payloadAt := unix(now.Add(-LLMPayloadGrace))

	// count runs one statement and adds its row count to dst, naming the table
	// in any error.
	count := func(table string, dst *int64, res sql.Result, err error) error {
		if err != nil {
			return fmt.Errorf("prune %s: %w", table, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("prune %s: %w", table, err)
		}
		*dst += n
		return nil
	}

	// agent_actions first: the corrections delete below sees the smallest
	// possible set of live referrers, and its own NOT EXISTS covers the rest.
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM agent_actions
		  WHERE node_id = ? AND status IN (?, ?) AND updated_at < ?`,
		s.self, string(domain.AgentActionDone), string(domain.AgentActionFailed), at)
	if err := count("agent_actions", &c.AgentActions, res, err); err != nil {
		return c, err
	}

	res, err = s.db.ExecContext(ctx,
		`DELETE FROM llm_requests
		  WHERE node_id = ? AND status != 'pending' AND created_at < ?`,
		s.self, at)
	if err := count("llm_requests", &c.LLMRequests, res, err); err != nil {
		return c, err
	}

	res, err = s.db.ExecContext(ctx,
		`DELETE FROM llm_decisions
		  WHERE node_id = ? AND status != 'pending' AND created_at < ?`,
		s.self, at)
	if err := count("llm_decisions", &c.LLMDecisions, res, err); err != nil {
		return c, err
	}

	res, err = s.db.ExecContext(ctx,
		`DELETE FROM corrections
		  WHERE node_id = ? AND processed = 1 AND created_at < ?
		    AND NOT EXISTS (SELECT 1 FROM agent_actions a
				WHERE a.correction_id = corrections.id)`,
		s.self, at)
	if err := count("corrections", &c.Corrections, res, err); err != nil {
		return c, err
	}

	res, err = s.db.ExecContext(ctx,
		`DELETE FROM llm_retries
		  WHERE node_id = ? AND processed = 1 AND created_at < ?`,
		s.self, at)
	if err := count("llm_retries", &c.LLMRetries, res, err); err != nil {
		return c, err
	}

	// The newest row PER SCOPE survives at any age: LatestKillEventOn reads the
	// 'global' stream only, so a MAX(id) taken across scopes would let an FSP
	// toggle make a standing pause deletable.
	res, err = s.db.ExecContext(ctx,
		`DELETE FROM kill_events
		  WHERE node_id = ? AND created_at < ?
		    AND id < (SELECT MAX(k.id) FROM kill_events k
				WHERE k.node_id = kill_events.node_id AND k.scope = kill_events.scope)`,
		s.self, at)
	if err := count("kill_events", &c.KillEvents, res, err); err != nil {
		return c, err
	}

	res, err = s.db.ExecContext(ctx,
		`DELETE FROM task_reservations
		  WHERE node_id = ? AND confirmed_at != 0 AND confirmed_at < ?`,
		s.self, at)
	if err := count("task_reservations", &c.TaskReservations, res, err); err != nil {
		return c, err
	}

	// Payload blanking, on the liveness grace rather than the retention window.
	// These are UPDATEs, so they are counted apart from the deletions.
	res, err = s.db.ExecContext(ctx,
		`UPDATE llm_requests SET context_json = ''
		  WHERE node_id = ? AND status != 'pending' AND context_json != ''
		    AND created_at < ?`,
		s.self, payloadAt)
	if err := count("llm_requests payload", &c.BlankedPayloads, res, err); err != nil {
		return c, err
	}

	res, err = s.db.ExecContext(ctx,
		`UPDATE llm_decisions SET captured_output = ''
		  WHERE node_id = ? AND status != 'pending' AND captured_output != ''
		    AND created_at < ?`,
		s.self, payloadAt)
	if err := count("llm_decisions payload", &c.BlankedPayloads, res, err); err != nil {
		return c, err
	}

	if !c.Empty() {
		s.noteWrite()
	}
	return c, nil
}
