package operations

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/Suknna/quoin/internal/quoin/execution"
)

func TestFlowOrAdminUsesOneCredentialIdentity(t *testing.T) {
	declaration := testDeclarations[5]
	for _, test := range []struct {
		name    string
		session string
		flow    string
		status  int
	}{
		{"operator session with admin initialization", operatorCookie, initFlowCookie, http.StatusOK},
		{"unrelated admin session with admin initialization", sessionCookie, initFlowCookie, http.StatusOK},
		{"bad session storage does not override selected flow", "__Host-quoin-session=broken-session", initFlowCookie, http.StatusOK},
		{"admin session cannot upgrade login flow", sessionCookie, loginFlowCook, http.StatusForbidden},
		{"admin session cannot hide expired flow", sessionCookie, "__Host-quoin-flow=expired-flow", http.StatusUnauthorized},
		{"removed recovery flow not authorized", "", "__Host-quoin-flow=recovery-flow", http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			sink := &fakeSink{}
			server := newTestAPI(t, mustAdmission(t, newTestRegistry(t), sink), declaration, func(ctx context.Context, _ *testInput) (*testOutput, error) {
				meta, err := execution.Require(ctx)
				if err != nil {
					return nil, err
				}
				if meta.Actor.ID != 33 || meta.Session.ID != 0 || meta.CorrelationID != "init-flow-correlation" {
					return nil, fmt.Errorf("mixed identity: %+v", meta)
				}
				return &testOutput{Body: "ok"}, nil
			})
			cookies := []string{test.flow}
			if test.session != "" {
				cookies = append(cookies, test.session)
			}
			response := doGet(t, server, declaration.Path, cookies...)
			response.Body.Close()
			if response.StatusCode != test.status {
				t.Fatalf("status=%d want=%d", response.StatusCode, test.status)
			}
		})
	}
}
