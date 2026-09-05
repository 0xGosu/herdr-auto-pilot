package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
)

// focusAgent brings the herdr UI to an agent's exact pane.
//
// It is the lightest of the queued kinds and the only one that types nothing:
// it moves the OPERATOR's view, not the agent's state. That is why it takes
// none of the guards the delivering kinds do — no terminal-identity compare,
// no staleness bound, no side-effect marker. Focusing a pane that has been
// recycled shows the operator the wrong window for a moment; focusing one that
// is gone is a failure the daemon log records. Neither can reach an agent.
//
// It is queued at all for the same reason the rest are: the front ends do not
// own a herdr adapter, and giving one back for a UI convenience would put the
// whole surface within one line of driving a pane again.
func (d *Daemon) focusAgent(ctx context.Context, a domain.AgentAction) (string, error) {
	var p domain.FocusPayload
	if err := json.Unmarshal([]byte(a.Payload), &p); err != nil {
		return "", fmt.Errorf("the queued focus request could not be read: %w", err)
	}
	if p.TabID == "" || p.PaneID == "" {
		return "", errors.New("the queued focus request names no tab and pane to focus")
	}
	fp, ok := d.opt.Herdr.(ports.FocusPort)
	if !ok {
		return "", errors.New("this herdr adapter cannot focus a pane")
	}
	if err := fp.FocusPane(ctx, p.TabID, p.PaneID); err != nil {
		return "", fmt.Errorf("focusing pane %s: %w", p.PaneID, err)
	}
	return "", nil
}
