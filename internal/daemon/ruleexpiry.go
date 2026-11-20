package daemon

import (
	"context"
	"log/slog"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
)

// confidenceDecay assembles the rule-aging policy from config. It MIRRORS the
// front end's helper of the same name, for the reason confidenceThresholds is
// duplicated: config stays decoupled from domain, and this package cannot
// import frontend.
func confidenceDecay(cfg config.Config) domain.ConfidenceDecay {
	grace, span, removal := cfg.Learning.ConfidenceDecay()
	return domain.ConfidenceDecay{Grace: grace, Span: span, Removal: removal}
}

// expiryHistoryLimit is the decision window the sweep scores a rule over. It
// must equal the window every other confidence read uses (50): the sweep
// deletes on "this rule's live confidence is 0", and a different window would
// let it reach that verdict about a rule the Rules tab shows a score for.
const expiryHistoryLimit = 50

// expireStaleRules is the bookkeeping half of confidence aging, run once a day
// from the retention sweep's background goroutine. The aging itself needs no
// sweep — domain.LiveConfidence computes it on every read — so this only does
// the three things a read cannot:
//
//  1. STAMP rules that have no decay anchor (rows from before aging existed, or
//     from an older node on a shared database) with "now", so their clock starts
//     the day they are first seen rather than being back-dated.
//  2. OBSERVE which rules sit at zero confidence, stamping the first sighting
//     and clearing the stamp of a rule that has recovered.
//  3. REMOVE a rule whose stamp is older than the removal window, with its
//     decision history, embedding and snapshot.
//
// It reports whether it deleted anything, so the caller knows a VACUUM may be
// worth it.
//
// Deleting a learned rule cannot be undone, so every doubt here resolves to
// "leave it": an unreadable listing or history skips the pass or the rule, a
// rule is only ever removed on the pass AFTER the one that could have stamped
// it (the stamp it is judged by is the one read from the store, never one
// written a moment ago), and the store re-checks, inside the deleting
// transaction, that no operator has vouched for the rule since it was read.
//
// A zero stamp exists only while a removal window is actually running — aging
// on AND removal on. Whenever either is off the sweep CLEARS every stamp
// instead (ClearZeroSinceStamps) and observes nothing, so that switching the
// feature back on, or enabling removal on a herd that has been aging for
// months, opens a fresh window for every rule. Leaving stamps in place across
// an off period is how a working rule would be deleted on the first sweep
// after it: with aging off the rule fires again and is not at zero, yet its
// stamp goes on getting older.
//
// On a shared database rules are fleet-wide and this policy is per node, so
// the nodes' sweeps answer for each other: a node whose settings still score a
// rule above zero clears the stamp a shorter-policy node wrote (the "recovered"
// branch below), and a node with aging or removal off clears them all. A rule
// is therefore removed only once EVERY running node's settings agree it has
// sat at zero — the conservative reading of a disagreement, since the delete
// cannot be undone. The cost is a write per disputed rule per node per day.
//
// Not gated on the kill switch, a pause or per-agent disable: those stop hap
// ACTING on panes, and this touches no pane. It is retention, and retention
// keeps its cadence whatever the herd is doing.
func (d *Daemon) expireStaleRules(ctx context.Context, now time.Time) bool {
	cfg, _, _ := d.snapshot()
	decay := confidenceDecay(cfg)
	// Optional capability, exactly like RowRetentionPort: a store that cannot
	// expire rules simply never does, so no fake has to grow a stub.
	rx, ok := d.opt.Store.(ports.RuleExpiryPort)
	if !ok {
		return false
	}
	windowRunning := decay.Enabled() && decay.Removal > 0
	if !windowRunning {
		if n, err := rx.ClearZeroSinceStamps(ctx); err != nil {
			slog.Warn("rule aging: clearing zero stamps while removal is off failed", "error", err)
		} else if n > 0 {
			slog.Info("rule aging: removal is off, so recorded stays at zero were forgotten", "rules", n)
		}
	}
	if !decay.Enabled() {
		return false
	}

	// Step 1 needs aging only, not removal: an unstamped rule is never aged,
	// so the clock has to start even when nothing will ever be removed.
	if n, err := rx.StampDecayAnchors(ctx, now); err != nil {
		slog.Warn("rule aging: starting the decay clock on unstamped rules failed", "error", err)
	} else if n > 0 {
		slog.Info("rule aging: started the decay clock on rules that had none", "rules", n)
	}
	if !windowRunning {
		return false
	}

	states, err := d.opt.Store.ListSignatures(ctx, domain.SignatureFilter{})
	if err != nil {
		slog.Warn("rule aging: skipped, the rule list is unreadable", "error", err)
		return false
	}
	if len(states) == 0 {
		return false
	}
	histories, readable := d.expiryHistories(ctx, states)

	var removed []string
	for _, st := range states {
		if ctx.Err() != nil {
			break // shutting down: the next start's sweep picks up where this left off
		}
		if !readable[st.Signature] {
			// NOT the same as "no decisions". An empty history scores 0, and 0
			// is what this sweep deletes on.
			continue
		}
		conf := domain.LiveConfidence(histories[st.Signature], st.DecisionFloorID,
			cfg.Learning.ConfirmationWeight, st.Age(decay, now))
		atZero := conf.Score == 0
		switch {
		case !atZero && !st.ZeroSince.IsZero():
			// Recovered without RestartDecay clearing the stamp itself: the
			// policy got longer, a decision arrived on a rule that had none,
			// or an operator confirmed it on a node whose build writes the
			// decision but not the anchor (domain.EffectiveDecayAnchor). On a
			// shared database this is also a longer-policy node answering a
			// shorter one's stamp. Either way its stay at zero is over.
			if _, err := rx.SetSignatureZeroSince(ctx, st.Signature, st.ZeroSince, time.Time{}); err != nil {
				slog.Warn("rule aging: clearing a recovered rule's zero stamp failed",
					"signature", shortSignature(st.Signature), "error", err)
			}
		case atZero && st.ZeroSince.IsZero():
			if _, err := rx.SetSignatureZeroSince(ctx, st.Signature, time.Time{}, now); err != nil {
				slog.Warn("rule aging: recording a rule at zero confidence failed",
					"signature", shortSignature(st.Signature), "error", err)
			}
		case atZero && decay.RemovalDue(st.ZeroSince, now):
			deleted, decisions, err := rx.DeleteStaleSignature(ctx, st)
			if err != nil {
				slog.Warn("rule aging: removing an expired rule failed",
					"signature", shortSignature(st.Signature), "error", err)
				continue
			}
			if !deleted {
				continue // renewed, or already removed by another node's sweep
			}
			removed = append(removed, st.Signature)
			slog.Info("rule aging: removed a rule that stayed at zero confidence",
				"signature", st.Signature, "situation", string(st.SituationType),
				"mode", string(st.Mode), "top_action", conf.TopAction, "decisions", decisions,
				"unconfirmed_since", st.DecayAnchor.Format(time.RFC3339),
				"at_zero_since", st.ZeroSince.Format(time.RFC3339),
				"removal_window", decay.Removal.String())
			d.emitStream(ctx, domain.StreamRuleDeleted,
				domain.StreamStr("sig", shortSignature(st.Signature)),
				domain.StreamStr("reason", "expired"))
		}
	}
	if len(removed) == 0 {
		return false
	}
	d.forgetRemovedRules(removed)
	return true
}

// expiryHistories reads the decision window of every listed rule and reports,
// per rule, whether the read SUCCEEDED. A rule missing from readable must be
// skipped by the caller; a rule present there with no history genuinely has no
// decisions.
//
// The two are kept apart because the batched read reports a rule with no
// decisions by leaving it out of the map (ports.BatchDecisionReader), which is
// indistinguishable from a failed read unless success is tracked beside it.
func (d *Daemon) expiryHistories(ctx context.Context, states []domain.SignatureState) (map[string][]domain.DecisionRecord, map[string]bool) {
	readable := make(map[string]bool, len(states))
	if batch, ok := d.opt.Store.(ports.BatchDecisionReader); ok {
		sigs := make([]string, len(states))
		for i, st := range states {
			sigs[i] = st.Signature
		}
		histories, err := batch.DecisionsForSignatures(ctx, sigs, expiryHistoryLimit)
		if err == nil {
			for _, sig := range sigs {
				readable[sig] = true
			}
			return histories, readable
		}
		slog.Debug("rule aging: batched history read failed; falling back to per-rule reads", "error", err)
	}
	histories := make(map[string][]domain.DecisionRecord, len(states))
	for _, st := range states {
		history, err := d.opt.Store.DecisionsForSignature(ctx, st.Signature, expiryHistoryLimit)
		if err != nil {
			slog.Debug("rule aging: rule history unreadable; leaving the rule alone",
				"signature", shortSignature(st.Signature), "error", err)
			continue
		}
		histories[st.Signature], readable[st.Signature] = history, true
	}
	return histories, readable
}

// forgetRemovedRules drops the daemon's in-memory traces of rules the sweep
// just deleted — what a front end's delete achieves by nudging KindReload,
// done directly because this runs inside the daemon and a full reload (config
// re-read, embedder check) is far more than a deletion needs.
//
// The snapshot cache entry goes so a re-learned rule re-captures its screen.
// RefreshKnowledge rebuilds the semantic index when the embedding rows changed
// — which is the latency and accuracy half of this feature: an expired rule
// stops being a candidate — and retires every re-ranking verdict that may name
// a rule which no longer exists.
func (d *Daemon) forgetRemovedRules(signatures []string) {
	d.mu.Lock()
	for _, sig := range signatures {
		delete(d.snapshotSaved, sig)
	}
	d.mu.Unlock()
	d.RefreshKnowledge()
}
