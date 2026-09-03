package upgrade_test

// TestTicket36 coordinates every T36 acceptance leg. An evidence run may
// never silently skip a leg: it writes all child output under the
// caller-owned immutable evidence root, aggregates the ticket-mandated
// runtime-evidence.json / cleanup.json, and fails if a leg is unavailable.

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

func TestTicket36(t *testing.T) {
	root := os.Getenv("QUOIN_EVIDENCE_DIR")
	if root == "" {
		t.Skip("QUOIN_EVIDENCE_DIR not set; T36 acceptance evidence run disabled")
	}
	evidence := filepath.Join(root, "upgrade")
	if err := os.MkdirAll(evidence, 0o700); err != nil {
		t.Fatal(err)
	}
	startedAt := time.Now().UTC()
	commit := output(t, "git", "rev-parse", "HEAD")
	status := output(t, "git", "status", "--porcelain")
	commands := []map[string]any{}
	artifacts := []map[string]any{}

	// The real-binary gate legs run first: unsupported-version rejection
	// with a synthetic non-release fixture (mechanism evidence only) and the
	// structural no-Ready-during-migration lock exclusion.
	gates := exec.Command("go", "test", "-timeout=30m", "-v", "./test/release/upgrade", "-run", "^TestSchemaGateRejectsUnsupportedVersion|^TestNoReadyDuringMigrationHoldingExclusiveLock$", "-count=1")
	gates.Dir = repoRoot(t)
	gates.Env = os.Environ()
	started := time.Now()
	body, err := gates.CombinedOutput()
	name := "gates.log"
	if writeErr := os.WriteFile(filepath.Join(evidence, name), body, 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	commands = append(commands, map[string]any{"name": "gates", "args": gates.Args, "exitCode": exitCode(err), "duration": time.Since(started).Round(time.Second).String(), "log": name})
	artifacts = append(artifacts, artifactOf(t, filepath.Join(evidence, name)))
	if err != nil {
		t.Fatalf("T36 gate legs failed (evidence: %s): %v\n%s", name, err, body)
	}

	for _, backend := range []struct{ name, pkg, test string }{
		{"compose", "./test/release/compose", "^TestUpgradeComposeTicket36$"},
		{"helm", "./test/release/helm", "^TestUpgradeHelmTicket36$"},
	} {
		cmd := exec.Command("go", "test", "-timeout=60m", "-v", backend.pkg, "-run", backend.test, "-count=1")
		cmd.Dir = repoRoot(t)
		cmd.Env = os.Environ()
		started := time.Now()
		body, err := cmd.CombinedOutput()
		name := backend.name + ".log"
		if writeErr := os.WriteFile(filepath.Join(evidence, name), body, 0o600); writeErr != nil {
			t.Fatal(writeErr)
		}
		commands = append(commands, map[string]any{"name": backend.name, "args": cmd.Args, "exitCode": exitCode(err), "duration": time.Since(started).Round(time.Second).String(), "log": name})
		artifacts = append(artifacts, artifactOf(t, filepath.Join(evidence, name)))
		if err != nil {
			t.Fatalf("%s T36 backend failed (evidence: %s): %v\n%s", backend.name, name, err, body)
		}
		for _, relative := range []string{
			backend.name + "/runtime-evidence.json",
			backend.name + "/cleanup.json",
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
			"activeTaskDrain":     "each backend drained a real queued connection probe attempt through the frozen upgrade-drain cancel after prepareUpgrade",
			"upgradePrepared":     "quoin_upgrade_prepared flipped to 1 only after the checklist was fully Safe and the pre-upgrade backup verified",
			"unsupportedVersion":  "the real migrate binary rejected a synthetic non-release schema with the stable unsupported_schema_version code (mechanism evidence only, no N-1 migration implied)",
			"noReadyDuringMigration": "the real serve binary fails the exclusive data lock before binding listeners while the migration window holds it, then reaches normal readiness after release",
			"preWriteRollback":    "image-only rollback before the migration commit restarted the old Release without any restore (mechanism evidence only)",
			"reprepare":           "a second prepareUpgrade after the abort froze a new revision and reached prepared again",
			"helperUpgrade":       "quoin-deploy compose|helm upgrade observed the prepared gauge, stopped the stack, offline-verified, migrated and restarted in order",
		},
		"proofPoints": map[string]string{
			"gates":   "upgrade/gates.log records the real-binary schema-gate and lock-exclusion proofs",
			"compose": "compose/runtime-evidence.json records the real Compose coordinated upgrade",
			"helm":    "helm/runtime-evidence.json records the equivalent real Kubernetes path",
		},
	})
	writeJSON(t, filepath.Join(root, "cleanup.json"), map[string]any{
		"backendCleanup": []string{"compose/cleanup.json", "helm/cleanup.json"},
		"ownedResources": []string{"Compose project/network/volumes/containers", "Helm release/namespace/PVCs/pods", "local registries", "release images and OCI chart", "temporary credentials", "gate fixtures under test temporary directories"},
		"result":         "each backend test proves its owned resources were removed before this coordinator succeeds",
	})
}

func writeJSON(t *testing.T, path string, value any) {
	t.Helper()
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
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
