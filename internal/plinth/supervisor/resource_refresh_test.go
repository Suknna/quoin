package supervisor

import (
	"encoding/json"
	"testing"
)

func TestResourceDiscoveryInputMatchesPublishedExecutionSnapshot(t *testing.T) {
	raw := []byte(`{"schemaKind":"resource_discovery_execution_v1","attemptId":41,"resourceRefreshRunId":19,"discoveryKey":"web-pods","selector":"up{job=\"web\"}","identityLabels":["job","instance"],"grantId":7}`)
	var input resourceDiscoveryInput
	if err := json.Unmarshal(raw, &input); err != nil {
		t.Fatal(err)
	}
	if input.SchemaKind != resourceDiscoveryExecutionSchemaKind || input.AttemptID != 41 || input.ResourceRefreshRunID != 19 || input.DiscoveryKey != "web-pods" || input.Selector != `up{job="web"}` || input.GrantID != 7 {
		t.Fatalf("published discovery snapshot did not decode: %#v", input)
	}
	if len(input.IdentityLabels) != 2 || input.IdentityLabels[0] != "job" || input.IdentityLabels[1] != "instance" {
		t.Fatalf("identity labels = %#v", input.IdentityLabels)
	}
}

func TestResourceDiscoveryInputRejectsRetiredSchema(t *testing.T) {
	var input resourceDiscoveryInput
	if err := json.Unmarshal([]byte(`{"schemaKind":"resource_refresh_execution_v1"}`), &input); err != nil {
		t.Fatal(err)
	}
	if input.SchemaKind == resourceDiscoveryExecutionSchemaKind {
		t.Fatal("retired schema must not be treated as current discovery execution")
	}
}
