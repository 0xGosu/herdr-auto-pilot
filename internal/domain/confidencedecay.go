package domain

import (
	"fmt"
	"math"
	"time"
)

// ConfidenceDecay is the time-based aging policy for learned rules: how fast a
// rule's confidence drains while no operator re-confirms it, and how long a
// rule may sit at zero before it is removed.
//
// It exists because the agreement ratio (Confidence) is timeless. A rule the
// operator confirmed twice a year ago scores exactly what it scored that day,
// forever, so nothing ever asks whether the answer is still right — and a rule
// whose screen no longer occurs stays in the table, in the semantic index and
// in every candidate set for good. Aging answers both with one clock: an
// in-use rule drifts under its threshold and escalates with its learned answer
// as the suggestion, where one confirmation restores it; an unused rule drains
// to zero and is eventually removed.
//
// The decay is SUBTRACTIVE and linear: after Grace, a rule loses 1/Span of
// confidence per unit time, floored at 0. So a 1.00 reaches zero Grace+Span
// after its anchor, and a contested 0.60 sooner — the less the operator agreed
// with it, the less runway it has.
//
// The zero value is the policy "never age": every method below then leaves a
// score untouched and reports no expiry, which is what keeps a caller that has
// no config (a test, a tool) on the pre-aging behaviour.
type ConfidenceDecay struct {
	// Grace is how long after its anchor a rule keeps its full score.
	Grace time.Duration
	// Span is how long a score of 1.00 takes to drain to 0 once Grace has
	// passed. <= 0 disables aging outright — removal included, since there is
	// then no way for a rule to reach the zero that removal waits on.
	Span time.Duration
	// Removal is how long a rule must have been at zero before RemovalDue says
	// to delete it. <= 0 keeps zero-scored rules forever.
	Removal time.Duration
}

// Enabled reports whether this policy ages anything at all.
func (p ConfidenceDecay) Enabled() bool { return p.Span > 0 }

// Penalty is the confidence a rule anchored at anchor has lost by now, in
// [0, 1].
//
// Both unknowns read as "no penalty", which is the direction this must fail:
// a zero anchor is a row no build has stamped yet (see SignatureState
// .DecayAnchor), and an anchor in the future is another node's clock running
// ahead. Guessing an age for either would demote rules on no evidence.
func (p ConfidenceDecay) Penalty(anchor, now time.Time) float64 {
	if !p.Enabled() || anchor.IsZero() || now.IsZero() {
		return 0
	}
	aged := now.Sub(anchor) - max(p.Grace, 0)
	if aged <= 0 {
		return 0
	}
	if aged >= p.Span {
		return 1
	}
	return float64(aged) / float64(p.Span)
}

// Apply ages a raw agreement score. The result is exactly 0 once the penalty
// has consumed the score, never a small negative or a denormal remainder —
// "at zero" is a state the expiry sweep and the views both test for exactly.
func (p ConfidenceDecay) Apply(raw float64, anchor, now time.Time) float64 {
	if math.IsNaN(raw) || raw <= 0 {
		return 0
	}
	aged := raw - p.Penalty(anchor, now)
	if aged <= 0 {
		return 0
	}
	return aged
}

// ZeroAt PROJECTS the instant a rule with this raw score reaches zero if nobody
// renews it, and whether that instant is knowable. It is for telling the
// operator what is coming ("reaches 0 on …"); nothing is ever deleted on a
// projection — removal waits on the OBSERVED SignatureState.ZeroSince.
//
// A rule that scores nothing to begin with — a reset rule, whose post-floor
// history is empty — is already there: the grace period protects a score, and
// there is none. Unknowable with aging off or the anchor unstamped.
func (p ConfidenceDecay) ZeroAt(raw float64, anchor time.Time) (time.Time, bool) {
	if !p.Enabled() || anchor.IsZero() {
		return time.Time{}, false
	}
	if math.IsNaN(raw) || raw <= 0 {
		return anchor, true
	}
	raw = math.Min(raw, 1)
	return anchor.Add(max(p.Grace, 0) + time.Duration(raw*float64(p.Span))), true
}

// RemovalAt is when a rule that has been at zero since zeroSince becomes
// removable. ok is false whenever removal can never happen: aging is off, the
// removal window is off, or the rule has not been seen at zero.
func (p ConfidenceDecay) RemovalAt(zeroSince time.Time) (time.Time, bool) {
	if !p.Enabled() || p.Removal <= 0 || zeroSince.IsZero() {
		return time.Time{}, false
	}
	return zeroSince.Add(p.Removal), true
}

// RemovalDue reports that a rule has sat at zero for the whole removal window.
//
// zeroSince is SignatureState.ZeroSince — when a running daemon first SAW the
// rule at zero — and deliberately not the instant ZeroAt computes. A computed
// instant moves with the policy: turn aging on after a year off, or shorten
// Span, and every old rule's "zero since" lands in the distant past, so the
// very first sweep would delete a working rule set that had never once been
// displayed at zero. An observed instant can only ever be as old as the
// operator's chance to notice it, which is what makes the window a real
// warning period rather than arithmetic.
//
// Every unknown is "not due": deleting a learned rule and its decision history
// cannot be undone, so the sweep acts only on a positive answer.
func (p ConfidenceDecay) RemovalDue(zeroSince, now time.Time) bool {
	at, ok := p.RemovalAt(zeroSince)
	return ok && !now.IsZero() && !now.Before(at)
}

// ConfidenceAge is one rule's position on the decay clock: the policy, the
// rule's anchor and the reading's "now". The zero value ages nothing, so a
// signature with no state row yet (nothing learned, nothing to age) is passed
// as ConfidenceAge{}.
type ConfidenceAge struct {
	Decay  ConfidenceDecay
	Anchor time.Time
	Now    time.Time
}

// Age positions this rule on the decay clock for LiveConfidence.
func (s SignatureState) Age(decay ConfidenceDecay, now time.Time) ConfidenceAge {
	return ConfidenceAge{Decay: decay, Anchor: s.DecayAnchor, Now: now}
}

// EffectiveDecayAnchor is the instant a rule is actually aged from: its stored
// anchor, or the newest operator decision in its scored history when that is
// later.
//
// The stored anchor alone is not enough on a shared database. A node still on
// a build from before aging records an operator's confirmation as a decision
// row but never writes decay_anchor_at, so by the column alone a rule the
// operator confirms every week on that machine would drain to zero and be
// deleted, with those confirmations, by an upgraded node. The decision row is
// the one piece of evidence every build writes, so it floors the anchor. It
// also covers the crash window between applyCorrection's decision write and
// its state write.
//
// It can only move the anchor LATER, and only on an operator-sourced decision
// — the same act RestartDecay is called for — so it never renews a rule on an
// LLM answer or the rule firing. An UNSTAMPED rule stays unstamped: deriving an
// anchor from its old decisions is exactly the back-dating StampDecayAnchors
// exists to avoid.
//
// post is the scored (post-floor) history, as LiveConfidence holds it.
func EffectiveDecayAnchor(anchor time.Time, post []DecisionRecord) time.Time {
	if anchor.IsZero() {
		return anchor
	}
	for _, d := range post {
		if d.Source == SourceOperator && d.CreatedAt.After(anchor) {
			anchor = d.CreatedAt
		}
	}
	return anchor
}

// RestartDecay restarts a rule's decay clock at now. It is the ONE writer of
// DecayAnchor after creation, and only an explicit operator act may call it:
//
//   - a processed correction — the operator confirmed or corrected the rule's
//     answer on a live escalation (daemon.applyCorrection). "Operator" here
//     is whoever a correction may be authored by, the orchestrator included:
//     its confirmations already count toward graduation as operator decisions
//     (SourceOperator carries no author), so a rule it may graduate it may
//     also renew, and EffectiveDecayAnchor could not tell the two apart;
//   - a RAISE of the streak (`hap signatures confirm`, the Rules tab's `+`),
//     the operator vouching for the rule without waiting for its screen;
//   - a reset, so a rule sent back to re-earn trust gets a full removal window
//     to do it in rather than the remainder of one that was already running.
//
// Never a LOWER (`-`), which would raise a decayed score on the key that means
// "trust this less"; and never anything automatic — an LLM answer, a timed or
// full-self-prompting auto-accept, or the rule firing on its own. Those are
// the machine agreeing with itself, and a rule that renewed its own lease by
// being used would never come back to the operator at all, which is the point
// of aging it.
//
// It also clears ZeroSince: the removal window measures one unbroken stay at
// zero, and an operator vouching for the rule ends that stay even when the
// score it leaves behind is still 0 (a reset). The sweep re-stamps it from the
// next time it looks, so such a rule gets a full window, not the remainder of
// an old one.
func RestartDecay(state SignatureState, now time.Time) SignatureState {
	state.DecayAnchor = now
	state.ZeroSince = time.Time{}
	return state
}

// AgedNote explains a score that aging lowered, for an escalation rationale or
// a rule line: "aged from 0.95, unconfirmed 47d". Empty when aging took
// nothing off, so callers can append it unconditionally.
func (c ConfidenceResult) AgedNote(now time.Time) string {
	if c.RawScore <= c.Score || c.AgedSince.IsZero() || now.IsZero() {
		return ""
	}
	return fmt.Sprintf("aged from %.2f, unconfirmed %s", c.RawScore, DaysLabel(now.Sub(c.AgedSince)))
}

// AgedOutOf reports that aging ALONE is why this score does not clear
// threshold: the operator's decisions still agree strongly enough to pass, and
// only the calendar took the rule under. It is what separates "this rule needs
// a human to vouch for it again" from "this rule is contested", which the
// confidence gate answers differently (see Decide).
func (c ConfidenceResult) AgedOutOf(threshold float64) bool {
	return c.Score <= threshold && c.RawScore > threshold
}

// DaysLabel renders a duration in whole days ("47d"), the only resolution the
// decay clock is configured in. Under a day reads "<1d" rather than "0d",
// which would look like "no time at all".
func DaysLabel(d time.Duration) string {
	if d < 24*time.Hour {
		return "<1d"
	}
	return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
}
