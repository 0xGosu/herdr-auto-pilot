package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// captureAgentAction re-runs the attention pipeline for one parked agent.
//
// The request used to arrive as a control-socket nudge carrying its target
// inside the kind string ("capture:<agent>") — the one place a nudge carried a
// domain payload, against internal/control's own stated contract, with a
// bespoke validator existing only to keep that payload from breaking the
// one-field protocol. As a queued action the target is an ordinary column the
// DAEMON resolves, which is also what took the last live agent listing out of
// the front end's copy of this command.
//
// The target is carried in Target rather than a payload struct: it is exactly
// what that column is for — the operator's spelling of an agent, resolved by
// the only process allowed to list them.
func (d *Daemon) captureAgentAction(ctx context.Context, a domain.AgentAction) (string, error) {
	if a.Target == "" {
		return "", errors.New("the queued capture request names no agent")
	}
	res, err := d.captureLiveAgent(ctx, a.Target)
	if err != nil {
		return "", err
	}
	out, err := json.Marshal(res)
	if err != nil {
		return "", fmt.Errorf("recording the capture result: %w", err)
	}
	return string(out), nil
}
