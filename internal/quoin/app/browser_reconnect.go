package app

import (
	"context"
	"time"
)

// awaitBrowserReconnect converts an unexpected WebSocket loss into the bounded
// durable grace state, then independently fences the physical browser after
// expiry. A later successful reattach makes ExpireReconnect a no-op.
func (application *apiServer) awaitBrowserReconnect(operationID int64) {
	deadline, err := application.browsers.AwaitReconnect(context.Background(), operationID)
	if err != nil {
		return
	}
	wait := time.Until(deadline)
	if wait > 0 {
		time.AfterFunc(wait, func() {
			expired, expireErr := application.browsers.ExpireReconnect(context.Background(), operationID)
			if !expired || expireErr != nil {
				return
			}
			application.browserTunnels.closeOperation(operationID)
			if application.browserStopDispatchFunc != nil {
				_ = application.browserStopDispatchFunc(context.Background(), operationID)
			}
		})
	}
}
