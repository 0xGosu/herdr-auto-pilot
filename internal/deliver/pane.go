package deliver

import (
	"context"

	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
)

// ReadVisiblePane returns the pane's current on-screen content, preferring a
// visible-source read (which reflects a standing menu) and falling back to the
// plain recent read when the adapter cannot do visible reads.
func ReadVisiblePane(ctx context.Context, h ports.HerdrPort, paneID string, lines int) (string, error) {
	if vr, ok := h.(ports.VisiblePaneReader); ok {
		return vr.ReadPaneVisible(ctx, paneID, lines)
	}
	return h.ReadPane(ctx, paneID, lines)
}
