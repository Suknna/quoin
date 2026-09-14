package appinspection

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/danielgtaylor/huma/v2/humatest"
)

func TestPluginPlanRoutesRequireAuthenticationBeforeStorage(t *testing.T) {
	_, api := humatest.New(t)
	handler := &Handler{Authenticate: func(context.Context, string) (int64, error) { return 0, auth.ErrUnauthenticated }}
	handler.Register(api)
	for _, path := range []string{"/api/v1/inspections/plans", "/api/v1/inspections/plans/basic-lab", "/api/v1/inspections/runs"} {
		response := api.Get(path)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("%s: status=%d body=%s", path, response.Code, response.Body.String())
		}
	}
}

func TestInspectionReaderPreservesAdminDenial(t *testing.T) {
	handler := &Handler{Authenticate: func(context.Context, string) (int64, error) {
		return 0, problem(http.StatusForbidden, "forbidden", "admin required")
	}}
	_, err := handler.reader(context.Background(), "session")
	var denied *problemError
	if !errors.As(err, &denied) || denied.GetStatus() != http.StatusForbidden {
		t.Fatalf("expected admin denial, got %v", err)
	}
}
