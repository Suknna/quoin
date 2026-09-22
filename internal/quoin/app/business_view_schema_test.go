package app_test

import (
	"net/http"
	"testing"
)

func TestBusinessViewScopeWithAllRoutesRegistered(t *testing.T) {
	server, headers := newEnrichmentHTTPServer(t)
	mustPost(t, server, headers, "/api/v1/business-views", `{"clientCommandId":"view-scope-create","viewKey":"scope-regression","displayName":"Scope regression","description":"","scope":{"labelConditions":{"system_id":"mall-shop"},"alertSourceKeys":["mall-shop-alertmanager"]}}`, http.StatusCreated)
	mustDo(t, server, http.MethodPut, headers, "/api/v1/business-views/scope-regression", `{"clientCommandId":"view-scope-update","displayName":"Scope regression","description":"","scope":{"labelConditions":{"system_id":"mall-shop"},"alertSourceKeys":["mall-shop-alertmanager"]},"expectedRowVersion":1}`, http.StatusOK)
}
