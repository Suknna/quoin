// Package restore coordinates the two real backend-specific T33 tests in a
// deterministic order. They cannot run concurrently because both intentionally
// use the existing local OCI registry fixture at port 5000.
package restore

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTicket33(t *testing.T) {
	evidence := os.Getenv("QUOIN_EVIDENCE_DIR")
	if evidence == "" {
		t.Skip("QUOIN_EVIDENCE_DIR not set; T33 acceptance evidence run disabled")
	}
	if err := os.MkdirAll(evidence, 0o700); err != nil {
		t.Fatal(err)
	}
	startedAt := time.Now().UTC()
	commit := output(t, "git", "rev-parse", "HEAD")
	status := output(t, "git", "status", "--porcelain")
	commands := []map[string]any{}
	artifacts := []map[string]any{}
	defer func() {
		if t.Failed() {
			writeJSON(t, filepath.Join(evidence, "runtime-evidence.json"), map[string]any{
				"gitCommit": strings.TrimSpace(commit), "dirtyStateDigest": digest([]byte(status)), "startedAt": startedAt.Format(time.RFC3339Nano), "finishedAt": time.Now().UTC().Format(time.RFC3339Nano), "status": "failed", "tools": toolInfo(t), "commands": commands, "artifacts": artifacts,
			})
		}
	}()
	for _, target := range []struct{ name, pkg, test string }{
		{"compose", "./test/release/compose", "TestRestoreComposeTicket33"},
		{"helm", "./test/release/helm", "TestRestoreHelmTicket33"},
	} {
		// -v is required: Go otherwise suppresses a skipped child test and would
		// turn an unavailable Compose/Helm environment into a false acceptance.
		log, code := run(t, evidence, target.name, "go", "test", "-timeout=30m", "-v", target.pkg, "-run", "^"+target.test+"$", "-count=1")
		logPath := filepath.Join(evidence, target.name+".log")
		commands = append(commands, map[string]any{"name": target.name, "args": append([]string(nil), append([]string{"go", "test", "-timeout=30m", "-v", target.pkg, "-run", "^" + target.test + "$", "-count=1"}, []string{}...)...), "exitCode": code, "log": logPath})
		artifacts = append(artifacts, artifact(t, logPath))
		for _, relative := range []string{target.name + "/runtime-evidence.json", target.name + "/restore-observation.json", target.name + "/cleanup.json"} {
			path := filepath.Join(evidence, relative)
			if _, err := os.Stat(path); err == nil {
				artifacts = append(artifacts, artifact(t, path))
			}
		}
		if code != 0 {
			t.Fatalf("%s T33 backend failed (exit=%d):\n%s", target.name, code, log)
		}
		if strings.Contains(log, "--- SKIP:") {
			t.Fatalf("%s T33 backend skipped despite QUOIN_EVIDENCE_DIR being set:\n%s", target.name, log)
		}
	}
	writeJSON(t, filepath.Join(evidence, "runtime-evidence.json"), map[string]any{
		"gitCommit": strings.TrimSpace(commit), "dirtyStateDigest": digest([]byte(status)), "startedAt": startedAt.Format(time.RFC3339Nano), "finishedAt": time.Now().UTC().Format(time.RFC3339Nano), "tools": toolInfo(t), "commands": commands, "artifacts": artifacts,
		"assertions": map[string]string{
			"corruptedBackup":       "expected non-zero rejection before workload stop; see backend restore-observation.json",
			"missingBackup":         "expected non-zero rejection before workload stop; see backend restore-observation.json",
			"foreignBackup":         "expected non-zero same-release rejection before workload stop; see backend restore-observation.json",
			"rootKeyMismatch":       "expected non-zero staged database root-key rejection; see backend restore-observation.json",
			"trustIsolation":        "expected old sessions/credentials and trust-boundary state to be invalidated; see backend restore-observation.json",
			"readiness":             "expected maintenance=false/normal readiness only after checklist completion; see backend restore-observation.json",
			"runtimeAndConnections": "expected runtime revoked and connections revalidation-required; see backend restore-observation.json",
		},
		"proofPoints": map[string]string{
			"compose":       "compose/runtime-evidence.json and compose/restore-observation.json record a real helper install, offline backup, PTY restore, maintenance HTTP repair, normal verification and cleanup",
			"helm":          "helm/runtime-evidence.json and helm/restore-observation.json record the equivalent real Kubernetes/Helm path",
			"missingBackup": "each backend executes an attached-TTY restore for a nonexistent backup before the valid snapshot path",
		},
	})
	writeJSON(t, filepath.Join(evidence, "cleanup.json"), map[string]any{
		"backendCleanup": []string{"compose/cleanup.json", "helm/cleanup.json"},
		"ownedResources": []string{"Compose project/network/volumes/containers", "Helm release/namespace/PVCs/pods", "provider fixture", "temporary credentials"},
		"result":         "each backend test proves its own owned resources were removed before this coordinator succeeds",
	})
}

func run(t *testing.T, directory, name string, argv ...string) (string, int) {
	t.Helper()
	command := exec.Command(argv[0], argv[1:]...)
	command.Dir = repoRoot(t)
	command.Env = os.Environ()
	var combined bytes.Buffer
	command.Stdout, command.Stderr = &combined, &combined
	_ = command.Run()
	code := -1
	if command.ProcessState != nil {
		code = command.ProcessState.ExitCode()
	}
	if err := os.WriteFile(filepath.Join(directory, name+".log"), combined.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return combined.String(), code
}
func output(t *testing.T, argv ...string) string {
	t.Helper()
	value, err := exec.Command(argv[0], argv[1:]...).Output()
	if err != nil {
		t.Fatal(err)
	}
	return string(value)
}
func digest(value []byte) string { sum := sha256.Sum256(value); return hex.EncodeToString(sum[:]) }
func writeJSON(t *testing.T, path string, value any) {
	t.Helper()
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(body, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}
func repoRoot(t *testing.T) string {
	t.Helper()
	root := output(t, "go", "env", "GOMOD")
	return filepath.Dir(strings.TrimSpace(root))
}

func artifact(t *testing.T, path string) map[string]any {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{"path": path, "sha256": digest(body), "bytes": len(body)}
}

func toolInfo(t *testing.T) map[string]string {
	t.Helper()
	info := map[string]string{}
	for name, argv := range map[string][]string{
		"go":      {"go", "version"},
		"docker":  {"docker", "version", "--format", "{{.Server.Version}}"},
		"compose": {"docker", "compose", "version"},
		"helm":    {"helm", "version", "--short"},
		"kubectl": {"kubectl", "version", "--client"},
	} {
		if output, err := exec.Command(argv[0], argv[1:]...).CombinedOutput(); err == nil {
			info[name] = strings.TrimSpace(string(output))
		}
	}
	return info
}
