package app

import "context"

// closeBrowserSessions converges manual-login processes after any auth-service
// session revocation path (logout, user disable, reset, or explicit revoke).
func (application *apiServer) closeBrowserSessions(ctx context.Context) {
	operationIDs, err := application.browsers.CloseRevokedSessions(ctx)
	if err != nil {
		return
	}
	for _, operationID := range operationIDs {
		application.browserTunnels.closeOperation(operationID)
		if application.browserStopDispatchFunc != nil {
			_ = application.browserStopDispatchFunc(ctx, operationID)
		}
	}
}
