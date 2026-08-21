package worker

// Frozen-contract pins: the worker's provider tool schema and agent
// version must render byte-identical to the Quoin-side catalog
// (internal/quoin/attempt), so the BeginModelCall tool-schema digest gate
// and the agent-version fence can never drift silently.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/Suknna/quoin/internal/quoin/attempt"
)

func TestToolSchemaMatchesQuoinCatalog(t *testing.T) {
	workerJSON, err := ProviderToolsJSON()
	if err != nil {
		t.Fatal(err)
	}
	quoinJSON, err := attempt.CanonicalToolsJSON()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(workerJSON, quoinJSON) {
		t.Fatalf("tool schema drift:\nworker=%s\nquoin =%s", workerJSON, quoinJSON)
	}
	workerDigest, err := ProviderToolsDigest()
	if err != nil {
		t.Fatal(err)
	}
	quoinDigest, err := attempt.CanonicalToolsDigest()
	if err != nil {
		t.Fatal(err)
	}
	if workerDigest != quoinDigest {
		t.Fatalf("tool schema digest drift: worker=%s quoin=%s", workerDigest, quoinDigest)
	}
}

func TestAgentVersionMatchesQuoinContract(t *testing.T) {
	if WorkerAgentVersion != attempt.AgentVersion {
		t.Fatalf("agent version drift: worker=%s quoin=%s", WorkerAgentVersion, attempt.AgentVersion)
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
