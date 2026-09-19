package worker

// Frozen-contract pins: the worker's provider tool schema and agent
// version must render byte-identical to the Quoin-side catalog
// (internal/quoin/attempt), so the BeginModelCall tool-schema digest gate
// and the agent-version fence can never drift silently.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/Suknna/quoin/internal/plugins/builtin"
	"github.com/Suknna/quoin/internal/quoin/attempt"
	"github.com/Suknna/quoin/internal/quoin/investigation"
)

func TestToolSchemaMatchesQuoinCatalog(t *testing.T) {
	registry := builtin.Registry()
	enabled, err := registry.ResolveEnabled(nil)
	if err != nil {
		t.Fatal(err)
	}
	catalogs, err := attempt.BuildCatalogs(registry, attempt.Implementations(), enabled)
	if err != nil {
		t.Fatal(err)
	}
	for _, agentVersion := range []string{WorkerAgentVersion, WorkerInvestigationAgentVersion} {
		// The canonical input document freezes the per-attempt catalog
		// (ADR-0004); the worker must render exactly those frozen bytes.
		quoinJSON, catalog, err := attempt.FrozenCatalogJSONForCreation(catalogs, agentVersion)
		if err != nil {
			t.Fatal(err)
		}
		inputJSON, err := json.Marshal(map[string]json.RawMessage{"toolCatalog": quoinJSON})
		if err != nil {
			t.Fatal(err)
		}
		workerJSON, err := ProviderToolsJSONForInput(inputJSON, agentVersion)
		if err != nil {
			t.Fatal(err)
		}
		providerJSON, err := catalog.ProviderToolsJSON()
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(workerJSON, providerJSON) {
			t.Fatalf("%s tool schema drift:\nworker=%s\nquoin =%s", agentVersion, workerJSON, providerJSON)
		}
		workerDigest, err := ProviderToolsDigestForInput(inputJSON, agentVersion)
		if err != nil {
			t.Fatal(err)
		}
		providerSum := sha256.Sum256(providerJSON)
		quoinDigest := hex.EncodeToString(providerSum[:])
		if workerDigest != quoinDigest {
			t.Fatalf("%s tool schema digest drift: worker=%s quoin=%s", agentVersion, workerDigest, quoinDigest)
		}
	}
}

func TestAgentVersionMatchesQuoinContract(t *testing.T) {
	if WorkerAgentVersion != attempt.AgentVersion {
		t.Fatalf("agent version drift: worker=%s quoin=%s", WorkerAgentVersion, attempt.AgentVersion)
	}
	if LegacyInitialAnalysisAgentVersion != attempt.PreviousAgentVersion {
		t.Fatalf("previous initial-analysis identity drift: worker=%s quoin=%s", LegacyInitialAnalysisAgentVersion, attempt.PreviousAgentVersion)
	}
	if KnowledgeExtractionAgentVersion != attempt.KnowledgeAgentVersion {
		t.Fatalf("knowledge identity drift: worker=%s quoin=%s", KnowledgeExtractionAgentVersion, attempt.KnowledgeAgentVersion)
	}
	if InspectionAnalysisAgentVersion != attempt.InspectionAgentVersion {
		t.Fatalf("inspection identity drift: worker=%s quoin=%s", InspectionAnalysisAgentVersion, attempt.InspectionAgentVersion)
	}
	if ReportComplianceInspectionAnalysisAgentVersion != attempt.ReportComplianceInspectionAgentVersion {
		t.Fatalf("report-compliance inspection identity drift: worker=%s quoin=%s", ReportComplianceInspectionAnalysisAgentVersion, attempt.ReportComplianceInspectionAgentVersion)
	}
	if PreviousInspectionAnalysisAgentVersion != attempt.PreviousInspectionAgentVersion {
		t.Fatalf("initial inspection identity drift: worker=%s quoin=%s", PreviousInspectionAnalysisAgentVersion, attempt.PreviousInspectionAgentVersion)
	}
	if WorkerInvestigationAgentVersion != investigation.AgentVersion {
		t.Fatalf("investigation identity drift: worker=%s quoin=%s", WorkerInvestigationAgentVersion, investigation.AgentVersion)
	}
	if PreviousInvestigationAgentVersion != investigation.PreviousAgentVersion {
		t.Fatalf("previous investigation identity drift: worker=%s quoin=%s", PreviousInvestigationAgentVersion, investigation.PreviousAgentVersion)
	}
	if LegacyInvestigationAgentVersion != "investigation-v1" {
		t.Fatalf("legacy investigation identity drift: %s", LegacyInvestigationAgentVersion)
	}
}

func TestReadOnlyRuntimePathsParse(t *testing.T) {
	paths, err := ReadOnlyRuntimePaths()
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, path := range paths {
		found[path] = true
	}
	for _, required := range []string{"/usr/bin", "/usr/lib", "/etc/alternatives", "/etc/ld.so.cache", "/dev/null", "/dev/urandom", "/bin/bash"} {
		if !found[required] {
			t.Fatalf("frozen readonly path %s missing", required)
		}
	}
}

func TestExecutionModeCatalog(t *testing.T) {
	if ExecutionModeFor("bash") != "TOOL_EXECUTION_MODE_WORKER_LOCAL" {
		t.Fatal("bash must be worker_local")
	}
	if ExecutionModeFor("artifact_read") != "TOOL_EXECUTION_MODE_SUPERVISOR_TYPED" {
		t.Fatal("artifact_read must be supervisor_typed")
	}
	_ = sha256.Sum256
	_ = hex.EncodeToString
}
