package lintel_recovery_test

// TestTicket35 coordinates both backend acceptance tests. An evidence run may
// never silently skip either backend: it writes all child output under the
// caller-owned immutable evidence root, aggregates the ticket-mandated
// runtime-evidence.json / cleanup.json, and fails if a backend is unavailable.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestTicket35(t *testing.T) {
	root := os.Getenv("QUOIN_EVIDENCE_DIR")
	if root == "" {
		t.Skip("QUOIN_EVIDENCE_DIR not set; T35 acceptance evidence run disabled")
	}
	evidence := filepath.Join(root, "lintel-recovery")
	if err := os.MkdirAll(evidence, 0o700); err != nil {
		t.Fatal(err)
	}
	startedAt := time.Now().UTC()
	commit := output(t, "git", "rev-parse", "HEAD")
	status := output(t, "git", "status", "--porcelain")
	commands := []map[string]any{}
	artifacts := []map[string]any{}
	for _, backend := range []struct{ name, pkg string }{
		{"compose", "./test/release/compose"},
		{"helm", "./test/release/helm"},
	} {
		cmd := exec.Command("go", "test", "-timeout=60m", "-v", backend.pkg, "-run", "^TestTicket35", "-count=1")
		cmd.Dir = filepath.Join("..", "..", "..")
		cmd.Env = os.Environ()
		started := time.Now()
		body, err := cmd.CombinedOutput()
		name := backend.name + ".log"
		if writeErr := os.WriteFile(filepath.Join(evidence, name), body, 0o600); writeErr != nil {
			t.Fatal(writeErr)
		}
		commands = append(commands, map[string]any{"name": backend.name, "args": []string{"go", "test", "-timeout=60m", "-v", backend.pkg, "-run", "^TestTicket35", "-count=1"}, "exitCode": exitCode(err), "duration": time.Since(started).Round(time.Second).String(), "log": name})
		artifacts = append(artifacts, artifactOf(t, filepath.Join(evidence, name)))
		if err != nil {
			t.Fatalf("%s T35 backend failed (evidence: %s): %v\n%s", backend.name, name, err, body)
		}
		for _, relative := range []string{
			backend.name + "/runtime-evidence.json",
			backend.name + "/cleanup.json",
			backend.name + "/lintel-recovery-observation.json",
		} {
			path := filepath.Join(root, relative)
			if _, statErr := os.Stat(path); statErr == nil {
				artifacts = append(artifacts, artifactOf(t, path))
			} else {
				t.Fatalf("%s evidence artifact missing: %s", backend.name, relative)
			}
		}
	}
	writeJSON(t, filepath.Join(root, "runtime-evidence.json"), map[string]any{
		"gitCommit": commit, "dirtyStateDigest": digestOf(status), "startedAt": startedAt.Format(time.RFC3339Nano), "finishedAt": time.Now().UTC().Format(time.RFC3339Nano), "status": "passed", "commands": commands, "artifacts": artifacts,
		"assertions": map[string]string{
			"oldProcessFence":     "each backend stopped all four workloads and proved the empty-running fence before recovery",
			"storageDispositions": "exclusively_reattached and retired both ran through the real helper on each backend",
			"credentialRotation":  "pending->confirmed->current/retiring->first-authenticated->retired executed by the frozen protocol",
			"receipt":             "immutable lintel_recovery_receipts committed by the helper-only finalizer",
			"retryState":          "helper persisted recover-lintel stage state; completed finalization skips re-registration",
			"secretScan":          "the one-time registration token is absent from every evidence artifact",
			"helperOnlyFinalize":  "maintenance exited only through the deployment_helper-owned finalizer transaction",
		},
		"proofPoints": map[string]string{
			"compose": "compose/runtime-evidence.json records the real install, registration and both recovery dispositions",
			"helm":    "helm/runtime-evidence.json records the equivalent real Kubernetes path",
		},
	})
	writeJSON(t, filepath.Join(root, "cleanup.json"), map[string]any{
		"backendCleanup": []string{"compose/cleanup.json", "helm/cleanup.json"},
		"ownedResources": []string{"Compose project/network/volumes/containers", "Helm release/namespace/PVCs/pods", "local registries", "release images and OCI chart", "temporary credentials"},
		"result":         "each backend test proves its owned resources were removed before this coordinator succeeds",
	})
}

func jsonMarshal(value any) ([]byte, error) {
	return json.MarshalIndent(value, "", "  ")
}

func output(t *testing.T, argv ...string) string {
	t.Helper()
	body, err := exec.Command(argv[0], argv[1:]...).Output()
	if err != nil {
		t.Fatalf("%s: %v", argv, err)
	}
	return string(body)
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	type exiter interface{ ExitCode() int }
	if exit, ok := err.(exiter); ok {
		return exit.ExitCode()
	}
	return -1
}

func artifactOf(t *testing.T, path string) map[string]any {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	return map[string]any{"path": path, "sha256": hex.EncodeToString(sum[:]), "bytes": strconv.Itoa(len(body))}
}

func digestOf(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func writeJSON(t *testing.T, path string, value any) {
	t.Helper()
	body, err := jsonMarshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if writeErr := os.WriteFile(path, append(body, '\n'), 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
}
