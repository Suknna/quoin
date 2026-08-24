package attempt

import "testing"

func TestKubernetesReadArgumentClosure(t *testing.T) {
	tool, ok := LookupToolForAgentVersion("investigation-v1", "kubernetes_read")
	if !ok {
		t.Fatal("kubernetes_read missing")
	}
	valid := []string{
		`{"businessSystem":"payments","operation":"discovery"}`,
		`{"businessSystem":"payments","operation":"pod_list","namespace":"prod"}`,
		`{"businessSystem":"payments","operation":"pod_get","namespace":"prod","name":"api"}`,
		`{"businessSystem":"payments","operation":"events_list","namespace":"prod"}`,
		`{"businessSystem":"payments","operation":"pod_logs","namespace":"prod","name":"api","container":"server"}`,
	}
	for _, arguments := range valid {
		if err := ValidateToolArguments(tool, []byte(arguments)); err != nil {
			t.Fatalf("valid %s: %v", arguments, err)
		}
	}
	invalid := []string{
		`{"businessSystem":"payments","operation":"exec","namespace":"prod","name":"api"}`,
		`{"businessSystem":"payments","operation":"pod_logs","namespace":"prod","name":"api"}`,
		`{"businessSystem":"payments","operation":"discovery","namespace":"prod"}`,
		`{"businessSystem":"payments","operation":"pod_list","namespace":"prod","connectionId":"1"}`,
	}
	for _, arguments := range invalid {
		if err := ValidateToolArguments(tool, []byte(arguments)); err == nil {
			t.Fatalf("invalid %s accepted", arguments)
		}
	}
}

func TestKubernetesReadIsInvestigationOnly(t *testing.T) {
	if _, ok := LookupTool("kubernetes_read"); ok {
		t.Fatal("Initial Analysis unexpectedly exposes kubernetes_read")
	}
	initial, err := CanonicalToolsJSON(AgentVersion)
	if err != nil {
		t.Fatal(err)
	}
	investigation, err := CanonicalToolsJSON("investigation-v1")
	if err != nil {
		t.Fatal(err)
	}
	if string(initial) == string(investigation) {
		t.Fatal("mode-specific catalogs have identical schemas")
	}
}

func TestKubernetesReadResultClosure(t *testing.T) {
	valid := []byte(`{"success":true,"operation":"pod_list","observedAt":"2026-01-01T00:00:00Z","totalBytes":2,"totalLines":1,"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","results":[{"success":true,"output":"{}","truncated":false}]}`)
	if err := ValidateToolResultPayload("kubernetes_read_result_v1", valid); err != nil {
		t.Fatalf("valid result rejected: %v", err)
	}
	for _, invalid := range [][]byte{
		[]byte(`{"success":true,"operation":"pod_list","results":[{"success":true,"output":"{}"}],"unexpected":true}`),
		[]byte(`{"success":false,"operation":"pod_list","results":[]}`),
		[]byte(`{"success":true,"operation":"pod_list","results":[{"success":true,"output":"{}","truncated":false,"url":"https://secret"}]}`),
	} {
		if err := ValidateToolResultPayload("kubernetes_read_result_v1", invalid); err == nil {
			t.Fatalf("invalid result accepted: %s", invalid)
		}
	}
	if !KubernetesReadTool.ProducesEvidence {
		t.Fatal("kubernetes read must produce deterministic evidence")
	}
}
