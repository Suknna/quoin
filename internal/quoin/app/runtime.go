package app

// Runtime slot HTTP surface: the read-only getRuntimeStatus projection.
// The registration commands (prepare/reveal/retire) were retired with the
// registration era — component identity is the deployment CA-signed mTLS
// client certificate (ADR-0009), so there is nothing to administer.

import (
	"context"

	qruntime "github.com/Suknna/quoin/internal/quoin/runtime"
)

// The browser business is retired (受控浏览器退役): the Lintel slot is no
// longer part of any deployment, so the public projection exposes only the
// Plinth slot. The upgrade drain reads slots through the upgrade reconciler,
// not through this projection.

// runtimeSlotViews projects exactly the slots this deployment exposes, in the
// canonical order.
func (application *apiServer) runtimeSlotViews(ctx context.Context) ([]runtimeSlot, error) {
	view, err := application.runtime.View(ctx, qruntime.SlotPlinth)
	if err != nil {
		return nil, err
	}
	return []runtimeSlot{runtimeSlotProjection(view)}, nil
}
