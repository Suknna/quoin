package kubernetes

// TestTicket43 is the T43 acceptance coordinator: it proves the
// Kubernetes stack adapter through its real path — release subjects built
// into an invocation-local registry, ordinary digest-pinned YAML applied with
// kubectl, invocation-owned port-forwards carrying the public and webhook
// surfaces, the stack-backed suites' locally-applicable Kubernetes cells
// through the real coordinator (including the disposable namespace lifecycle
// and stdin runtime registration), and teardown of temporary resources.
// A reachable cluster is required; unavailable means the run does not
// pass.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/verification/catalog"
	"github.com/Suknna/quoin/internal/verification/result"
	"github.com/Suknna/quoin/internal/verification/suites"
)

const (
	releaseVersion   = "v0.1.0-dev"
	registryName     = "t43-registry"
	registryHostPort = "127.0.0.1:5142"
	namespace        = "quoin-t43"
	releaseName      = "t43live"
	quoinPort        = 23980
	stelePort        = 23981
)

func TestTicket43(t *testing.T) {
	evidenceRoot := os.Getenv("QUOIN_EVIDENCE_DIR")
	if evidenceRoot == "" {
		t.Skip("QUOIN_EVIDENCE_DIR not set; T43 acceptance evidence run disabled")
	}
	requireKubernetesCell(t)
	recorder := newEvidence(evidenceRoot)
	workRoot := filepath.Join(evidenceRoot, "work")
	if err := os.MkdirAll(workRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	startedAt := time.Now().UTC()
	assertions := map[string]map[string]any{}

	// ------------------------------------------------------------
	// Leg 1 — the release subjects and the helper-shaped manifest carrying
	// the image identities the native manifest applies in this namespace.
	// ------------------------------------------------------------
	ensureRegistry(t, recorder)
	buildSubjects(t, recorder)
	inventory := nativeInventory(t, recorder, workRoot)
	kubernetesConfig := filepath.Join(workRoot, "kubernetes.yaml")
	if err := os.WriteFile(kubernetesConfig, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	manifestPath, err := suites.WriteReleaseManifest(workRoot, releaseVersion, gitCommit(), inventory)
	if err != nil {
		t.Fatal(err)
	}
	recorder.note("kubernetes.yaml", mustRead(t, filepath.Join(repoRoot(), "deploy", "kubernetes", "quoin.yaml")))
	recorder.note("release-manifest.json", mustRead(t, manifestPath))

	// ------------------------------------------------------------
	// Leg 2 — the three stack-backed suites through the real
	// coordinator on this cluster's locally-applicable cells. The
	// Kubernetes Stack owns native manifest apply, port-forwards, exec/logs,
	// stdin registration and the disposable second release.
	// ------------------------------------------------------------
	loaded, err := catalog.LoadAndValidate(filepath.Join(repoRoot(), "docs/specs/quoin-v1/contracts/verification-catalog.yaml"))
	if err != nil {
		t.Fatalf("frozen catalog rejected: %v", err)
	}
	profile, err := result.LoadProfile(filepath.Join(repoRoot(), "docs/specs/quoin-v1/contracts/verification-result-profile.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	adminPassword := "t43-live-cluster-" + strconv.FormatInt(time.Now().UnixNano()&0xffffffff, 36)
	bin := filepath.Join(evidenceRoot, "bin")
	_ = os.MkdirAll(bin, 0o755)
	recorder.run(t, "build-quoin-deploy", nil, 0, "go", "build", "-o", filepath.Join(bin, "quoin-deploy"), "./cmd/quoin-deploy")
	pathEnv := bin + ":" + os.Getenv("PATH")

	serverMinor := clusterMinor(t, recorder)
	_ = serverMinor
	target := suites.Target{Backend: "kubernetes", Architecture: "linux/" + hostArch()}
	coordinator := &suites.Coordinator{
		Catalog: loaded, Profile: profile,
		Target:       target,
		OutputDir:    filepath.Join(evidenceRoot, "invocation"),
		RepoRoot:     repoRoot(),
		InvocationID: "t43-kubernetes-qualification",
		ToolVersion:  "quoin-t43-acceptance",
		Subject:      result.Subject{Name: "quoin-release-subject", Digest: map[string]string{"sha256": sha256OfFile(manifestPath)}},
	}
	env := append(os.Environ(), "PATH="+pathEnv,
		"QUOIN_REPO_ROOT="+repoRoot(),
		"QUOIN_SUITE_WORK_ROOT="+workRoot,
		"QUOIN_SUITE_PROJECT="+releaseName,
		"QUOIN_SUITE_NAMESPACE="+namespace,
		"QUOIN_SUITE_RELEASE="+releaseName,
		"QUOIN_SUITE_QUOIN_PORT="+strconv.Itoa(quoinPort),
		"QUOIN_SUITE_STELE_PORT="+strconv.Itoa(stelePort),
		"QUOIN_SUITE_ADMIN_PASSWORD="+adminPassword,
		"QUOIN_SUITE_CONFIG="+kubernetesConfig,
		"QUOIN_SUITE_RELEASE_MANIFEST="+manifestPath,
	)
	executed := map[string]string{}
	delegated := []string{}
	for _, suite := range []string{suites.SuiteReleaseQualification, suites.SuiteProductionTransport, suites.SuiteMonitoringStack} {
		scenarioID, err := suites.ScenarioID(suite)
		if err != nil {
			t.Fatal(err)
		}
		scenario := loaded.Scenario(scenarioID)
		if scenario == nil {
			t.Fatalf("scenario %q missing", scenarioID)
		}
		cells, err := suites.CellsFor(loaded, suite, target)
		if err != nil {
			t.Fatal(err)
		}
		for index := range cells {
			cell := &cells[index]
			// The maintained-1/2 cells are the workflow matrix's
			// per-minor duplicates of the same deployment; the local
			// run executes the maintained-0 cell natively and records
			// the others as delegated (the full 3x2 matrix is the
			// release workflow's, exactly like the foreign arch).
			if strings.Contains(cell.ID, "kubernetes-maintained-1") || strings.Contains(cell.ID, "kubernetes-maintained-2") {
				delegated = append(delegated, scenarioID+"."+cell.ID)
				continue
			}
			if item, execErr := coordinator.ExecuteCell(scenario, cell, "kubernetes", env); execErr != nil {
				t.Fatalf("%s cell %s: %v", scenarioID, cell.ID, execErr)
			} else if item.Outcome != "passed" {
				t.Fatalf("%s cell %s outcome=%s category=%s", scenarioID, cell.ID, item.Outcome, item.Category)
			} else {
				executed[item.TestName()] = item.Outcome
			}
		}
	}
	// The exact native cell inventory: each of the three suites must
	// have executed its identified locally-applicable cell before the
	// aggregate verdict means anything (a catalog-selection regression
	// that returns zero cells cannot hide behind an empty PASSED).
	requiredCells := []string{
		"release.native-matrix.kubernetes-maintained-0-linux-" + hostArch(),
		"production.transport.kubernetes-linux-" + hostArch(),
		"integration.monitoring-stack.kubernetes-maintained-0-linux-" + hostArch(),
	}
	for _, required := range requiredCells {
		if outcome, ok := executed[required]; !ok {
			t.Fatalf("required native cell %s never executed (catalog selection regression?)", required)
		} else if outcome != "passed" {
			t.Fatalf("required native cell %s outcome=%s", required, outcome)
		}
	}
	statement := coordinator.Statement(
		sha256OfFile(filepath.Join(repoRoot(), "docs/specs/quoin-v1/contracts/verification-catalog.yaml")),
		sha256OfFile(filepath.Join(repoRoot(), "docs/specs/quoin-v1/contracts/verification-result-profile.yaml")))
	if statement.Predicate.Result != "PASSED" {
		t.Fatalf("kubernetes suite verdict %s", statement.Predicate.Result)
	}
	body, _ := json.MarshalIndent(statement, "", "  ")
	recorder.note("kubernetes-statement.json", body)
	assertions["stack-backed-suites-native"] = map[string]any{
		"expected": "the three stack-backed suites' locally-applicable kubernetes cells execute natively through the adapter",
		"actual":   map[string]any{"executedNatively": executed, "delegated": delegated},
	}

	// ------------------------------------------------------------
	// Leg 3 — teardown-zero: the coordinator's environment down leaves
	// no release, no namespace resources and no port-forward.
	// ------------------------------------------------------------
	// The Stack's own Down deletes the native manifest and recorded transports
	// (pidfile-owned port-forwards) die with the deployment.
	teardownStack := &suites.Stack{Backend: "kubernetes", Namespace: namespace, ReleaseName: releaseName, WorkRoot: workRoot, ConfigPath: kubernetesConfig, ManifestPath: manifestPath}
	if _, err := teardownStack.Down(false); err != nil {
		t.Fatalf("teardown down: %v", err)
	}
	// Release deletion is asynchronous at the pod level (volume detach on
	// the local-path provisioner can hold a Terminating pod past the
	// uninstall's own wait); a bounded delete-wait closes it before the
	// zero assertion (VERIFY-CLEANUP-001: teardown before verdict).
	if output, _, err := teardownStack.Kubectl("--namespace", namespace, "wait", "--for=delete",
		"pod", "-l", "app.kubernetes.io/instance="+releaseName, "--timeout=300s"); err != nil {
		t.Logf("teardown pod delete-wait: %v: %s", err, strings.TrimSpace(output))
	}
	assertions["teardown-zero"] = map[string]any{
		"expected": "no native manifest resources, zero kubectl port-forward processes",
		"actual":   proveTeardownZero(t, recorder, namespace, releaseName),
	}

	// The pattern self-match trap: the wrapper shell's own command line
	// contains the literal pattern, so a regex-neutral spelling avoids
	// counting the probe itself.
	forwardCount := strings.TrimSpace(runOutput(t, "sh", "-c", "pgrep -fc 'kubectl.*port-?forward' || true"))
	if forwardCount != "" && forwardCount != "0" {
		t.Fatalf("port-forward processes survived teardown: %s", forwardCount)
	}

	// ------------------------------------------------------------
	// Cleanup and evidence closure.
	// ------------------------------------------------------------
	cleanup(t, recorder)
	if leak := scanTree(evidenceRoot, adminPassword); leak != "" {
		t.Fatalf("sentinel leaked into evidence: %s", leak)
	}
	recorder.observe("runtime-evidence.json", map[string]any{
		"schema":           "quoin-t43-runtime-evidence",
		"ticket":           "T43",
		"issue":            70,
		"gitCommit":        gitCommit(),
		"dirtyStateDigest": dirtyDigest(),
		"startedAt":        startedAt.Format(time.RFC3339Nano),
		"finishedAt":       time.Now().UTC().Format(time.RFC3339Nano),
		"status":           "passed",
		"cluster": map[string]any{
			"serverVersion": clusterVersion(t),
			"minor":         serverMinor,
			"kubectl":       runOutput(t, "kubectl", "version", "--client", "--short"),
		},
		"observedTransitions": map[string]any{
			"install":  "kubectl apply of the ordinary native manifest with digest-pinned subject images",
			"suites":   executed,
			"teardown": assertions["teardown-zero"]["actual"],
		},
		"commands":   recorder.commands,
		"artifacts":  recorder.artifacts,
		"assertions": assertions,
	})
	recorder.observe("cleanup.json", cleanupRecord())
}

// requireKubernetesCell proves the executing environment can run the
// Kubernetes cells require a reachable cluster, kubectl, docker for the
// subject build, and socat for kubectl port-forward.
func requireKubernetesCell(t *testing.T) {
	t.Helper()
	// The acceptance contract is explicit: an unavailable cluster means
	// the run does not pass. With QUOIN_EVIDENCE_DIR set this fails
	// (never skips); without it the cheap default `go test ./...` skips.
	for _, tool := range []string{"kubectl", "docker", "go", "git", "socat"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("%s unavailable in the kubernetes cell: %v", tool, err)
		}
	}
	if err := exec.Command("kubectl", "get", "--raw", "/readyz").Run(); err != nil {
		t.Fatalf("no reachable cluster (run inside the cell with kubeconfig): %v", err)
	}
	// The cluster must resolve its own services (a proxy TUN on the
	// host can hijack ClusterIP DNS; the fix is a policy rule pinned
	// above it — recorded, not worked around).
	if err := verifyClusterDNS(t); err != nil {
		t.Fatalf("cluster DNS hijacked (host proxy policy routing): %v", err)
	}
}

// verifyClusterDNS proves the cluster answers its own service DNS
// instead of an upstream fake-IP resolver.
func verifyClusterDNS(t *testing.T) error {
	t.Helper()
	output, err := exec.Command("kubectl", "run", "t43-dnsprobe", "--rm", "-i", "--restart=Never",
		"--image", "docker.io/library/busybox:1.36", "--", "nslookup", "kubernetes.default.svc.cluster.local").CombinedOutput()
	if err != nil {
		return fmt.Errorf("probe: %v: %s", err, output)
	}
	if !strings.Contains(string(output), "10.43.") && !strings.Contains(string(output), "Name:") {
		return fmt.Errorf("unexpected answer: %s", output)
	}
	if strings.Contains(string(output), "198.18.") {
		return fmt.Errorf("fake-IP answer (hijacked): %s", output)
	}
	return nil
}

func hostArch() string {
	output, err := exec.Command("uname", "-m").Output()
	if err != nil {
		return "amd64"
	}
	switch strings.TrimSpace(string(output)) {
	case "aarch64":
		return "arm64"
	default:
		return "amd64"
	}
}

func clusterMinor(t *testing.T, recorder *ticketEvidence) string {
	t.Helper()
	version := clusterVersion(t)
	parts := strings.SplitN(strings.TrimPrefix(version, "v"), ".", 3)
	if len(parts) < 2 {
		t.Fatalf("cluster version %q unparseable", version)
	}
	recorder.observe("cluster-version.json", map[string]string{"version": version, "minor": parts[1]})
	return parts[1]
}

func clusterVersion(t *testing.T) string {
	t.Helper()
	output, err := exec.Command("kubectl", "version", "-o", "json").Output()
	if err != nil {
		t.Fatalf("cluster version: %v", err)
	}
	var document struct {
		ServerVersion struct {
			GitVersion string `json:"gitVersion"`
		} `json:"serverVersion"`
	}
	if err := json.Unmarshal(output, &document); err != nil {
		t.Fatal(err)
	}
	return document.ServerVersion.GitVersion
}

// proveTeardownZero asserts native manifest resources and the namespace's
// workloads are gone after the suites' teardown.
func proveTeardownZero(t *testing.T, recorder *ticketEvidence, namespace, release string) map[string]any {
	t.Helper()
	_ = release // Resource names are fixed; namespace is the deployment identity.
	deploymentOutput, err := exec.Command("kubectl", "--namespace", namespace, "get", "deployment/quoin").CombinedOutput()
	resourcesGone := err != nil && strings.Contains(string(deploymentOutput), "NotFound")
	podsOutput, _ := exec.Command("kubectl", "--namespace", namespace, "get", "pods",
		"-l", "app.kubernetes.io/part-of=quoin", "--no-headers").CombinedOutput()
	// kubectl prints "No resources found ..." when the selector matches
	// nothing — that IS the empty proof, not residue.
	if strings.Contains(string(podsOutput), "No resources found") {
		podsOutput = nil
	}
	forwards, _ := exec.Command("sh", "-c", "pgrep -af 'kubectl.*port-forward' | grep -v grep || true").CombinedOutput()
	proof := map[string]any{
		"resourcesGone": resourcesGone,
		"releasePods":   strings.TrimSpace(string(podsOutput)),
		"portForwards":  strings.TrimSpace(string(forwards)),
	}
	recorder.observe("teardown-zero.json", proof)
	if !resourcesGone || proof["releasePods"].(string) != "" || proof["portForwards"].(string) != "" {
		t.Fatalf("teardown residue: %+v", proof)
	}
	return proof
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func sha256OfFile(path string) string {
	body, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func runOutput(t *testing.T, argv ...string) string {
	t.Helper()
	output, err := exec.Command(argv[0], argv[1:]...).CombinedOutput()
	if err != nil {
		return strings.TrimSpace(string(output))
	}
	return strings.TrimSpace(string(output))
}

func gitCommit() string {
	output, _ := exec.Command("git", "rev-parse", "HEAD").Output()
	return strings.TrimSpace(string(output))
}

func dirtyDigest() string {
	output, _ := exec.Command("git", "status", "--porcelain").Output()
	sum := sha256.Sum256(output)
	return hex.EncodeToString(sum[:])
}

func repoRoot() string {
	dir, err := os.Getwd()
	if err != nil {
		return "."
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "."
		}
		dir = parent
	}
}
