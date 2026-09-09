package publish

// TestTicket42 is the T42 acceptance coordinator: it closes the release
// through its real path — dual-platform release subjects through the real
// builder, the offline pre-qualification gate, the contract gate, the
// release suite's native compose cell, the real offline import with
// digest readback, the mechanically derived final Release manifest, the
// signed Sigstore closure, the publish gate, and a concrete-site
// Deployment Acceptance against the published release that provably
// never writes back upstream. Foreign-architecture and maintained-
// Kubernetes categories stay delegation carriers (the atomic scenario
// closure belongs to the release workflow's native matrix); nothing
// local is ever published as a passing foreign cell.

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/release/manifest"
	"github.com/Suknna/quoin/internal/verification/catalog"
	"github.com/Suknna/quoin/internal/verification/result"
	"github.com/Suknna/quoin/internal/verification/suites"
	deploymentacceptance "github.com/Suknna/quoin/test/release/deployment-acceptance"
)

const releaseVersion42 = "v0.1.0-dev"

func TestTicket42(t *testing.T) {
	evidenceRoot := os.Getenv("QUOIN_EVIDENCE_DIR")
	if evidenceRoot == "" {
		t.Skip("QUOIN_EVIDENCE_DIR not set; T42 acceptance evidence run disabled")
	}
	requireNativeCell42(t)
	recorder := newEvidence(evidenceRoot)
	workRoot := filepath.Join(evidenceRoot, "work")
	if err := os.MkdirAll(workRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	startedAt := time.Now().UTC()
	baseline := captureInventory()
	recorder.observe("docker-baseline.json", baseline)

	bin := filepath.Join(evidenceRoot, "bin")
	_ = os.MkdirAll(bin, 0o755)
	recorder.run(t, "build-quoin-deploy", nil, 0, "go", "build", "-o", filepath.Join(bin, "quoin-deploy"), "./cmd/quoin-deploy")
	recorder.run(t, "build-quoin-verify", nil, 0, "go", "build", "-o", filepath.Join(bin, "quoin-verify"), "./cmd/quoin-verify")
	pathEnv := bin + ":" + os.Getenv("PATH")

	// ------------------------------------------------------------
	// Leg 1 — the release subjects: dual-platform application images,
	// Kubernetes and Compose bundles, helpers and the validated inventory.
	// ------------------------------------------------------------
	ensureRegistry(t, recorder)
	builderOwned := ensureBuilder(t, recorder, workRoot)
	// The failure-path safety net: a fataled leg must still remove the
	// owned stacks and registries (VERIFY-CLEANUP-002). The success path
	// runs the same idempotent cleanup inline before the assertions
	// below, so resources never survive a failed closure attempt.
	cleanupOnce := &sync.Once{}
	t.Cleanup(func() {
		cleanupOnce.Do(func() { cleanupTicket42(t, recorder, workRoot, builderOwned) })
	})
	subjects := buildSubjects42(t, recorder, workRoot)
	inventoryBytes, err := os.ReadFile(subjects.inventoryPath)
	if err != nil {
		t.Fatal(err)
	}
	subjectDigest := sha256Hex(inventoryBytes)
	recorder.observe("release-subject.json", map[string]any{
		"inventory":     subjects.inventoryPath,
		"subjectSHA256": subjectDigest,
	})

	// ------------------------------------------------------------
	// Leg 2 — the offline pre-qualification gate (real verify mode):
	// sixteen subject bundles over a local Fulcio-shaped authority,
	// then identity/issuer/subject-digest verification.
	// ------------------------------------------------------------
	signer := newClosureSigner(t)
	bundlesDir := filepath.Join(workRoot, "bundles")
	_ = os.MkdirAll(bundlesDir, 0o755)
	trustRootPath := signer.trustRootPath(t, workRoot)
	if err := signSubjectBundles(t, signer, bundlesDir, inventoryBytes); err != nil {
		t.Fatal(err)
	}
	supplyReport := recorder.run(t, "pre-qualification-gate", nil, 0,
		"go", "run", "./internal/release/build", "verify",
		"-inventory", subjects.inventoryPath,
		"-bundles", bundlesDir,
		"-trust-root", trustRootPath,
		"-identity", closureIdentityPattern,
		"-issuer", closureIssuer)

	evidenceDir := filepath.Join(workRoot, "evidence")
	_ = os.MkdirAll(evidenceDir, 0o755)
	writeEvidence := func(name string, payload []byte) {
		if err := os.WriteFile(filepath.Join(evidenceDir, name), payload, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	reportBundle := func(name string, payload []byte) {
		writeEvidence(name+".payload", payload)
		signer.signStatement(t, evidenceDir, name+".bundle.json", "quoin-release-subjects", subjectDigest,
			map[string]any{"kind": reportKindOf(name), "result": reportResultOf(name), "body": json.RawMessage(payload)})
	}

	// ------------------------------------------------------------
	// Leg 3 — the contract gate through the real quoin-verify binary;
	// its statement binds the source revision.
	// ------------------------------------------------------------
	gateOutput := filepath.Join(evidenceRoot, "contract-gate")
	recorder.run(t, "contract-gate", append(os.Environ(), "PATH="+pathEnv), 0,
		filepath.Join(bin, "quoin-verify"), "run",
		"--layer", "contract_gate", "--output", gateOutput, "--invocation-id", "t42-contract-gate")
	sourceSubject, err := manifest.ResolveSourceSubject(repoRoot())
	if err != nil {
		t.Fatal(err)
	}
	contractsStatement, err := os.ReadFile(filepath.Join(gateOutput, "test-result.json"))
	if err != nil {
		t.Fatal(err)
	}
	recorder.note("contract-gate-statement.json", contractsStatement)
	// The gate's own statement is re-signed as the closure bundle with
	// its verdict and layer preserved verbatim.
	writeEvidence("contracts.payload", contractsStatement)
	signer.signStatement(t, evidenceDir, "contracts.bundle.json", sourceSubject.Name, sourceSubject.Digest,
		repackStatement(t, contractsStatement))

	// ------------------------------------------------------------
	// Leg 4 — the release suite's native compose cell through the real
	// coordinator; its statement binds the release subject.
	// ------------------------------------------------------------
	qualification := runReleaseSuiteCell(t, recorder, evidenceRoot, workRoot, pathEnv, inventoryBytes, subjectDigest)
	writeEvidence("compose_linux_amd64.payload", qualification)
	signer.signStatement(t, evidenceDir, "compose_linux_amd64.bundle.json", "quoin-release-subjects", subjectDigest,
		repackStatement(t, qualification))

	// ------------------------------------------------------------
	// Leg 5 — the real offline import with digest readback over the
	// release subjects (the pre-manifest offline proof; the
	// archive-level import is the publish gate below).
	// ------------------------------------------------------------
	importRegistry := "127.0.0.1:" + envOr42("QUOIN_T42_IMPORT_PORT", "5143")
	importReport := proveOfflineImport(t, recorder, workRoot, inventoryBytes, importRegistry)
	importBody, _ := json.Marshal(importReport)
	reportBundle("offline_import", importBody)

	// ------------------------------------------------------------
	// Leg 6 — the supply-chain gate report and the three delegation
	// carriers (foreign-architecture and maintained-Kubernetes cells
	// close on the release workflow's native matrix; locally they are
	// delegation facts, never claimed passes).
	// ------------------------------------------------------------
	reportBundle("supply_chain", extractJSONObject(supplyReport))
	delegated := []string{"compose_linux_arm64", "kubernetes_linux_amd64", "kubernetes_linux_arm64"}
	for _, category := range delegated {
		payload, _ := json.Marshal(map[string]any{
			"kind":   "quoin-release-delegation",
			"result": "delegated",
			"vehicle": ".github/workflows/release.yml (compose-native arm64 runner " +
				"+ six maintained-Kubernetes native cells)",
			"reason": "foreign native architecture / maintained-Kubernetes cells never execute " +
				"locally (VERIFY-EXTERNAL-004); partial sub-proofs stay ticket evidence",
		})
		reportBundle(category, payload)
	}

	// ------------------------------------------------------------
	// Leg 7 — finalize: the mechanically derived manifest and the
	// packed offline archive through the real closure CLI.
	// ------------------------------------------------------------
	releaseDir := filepath.Join(workRoot, "release")
	recorder.run(t, "finalize-release", nil, 0, "go", "run", "./internal/release/publish", "finalize",
		"-inventory", subjects.inventoryPath,
		"-evidence", evidenceDir,
		"-contracts", filepath.Join(repoRoot(), "docs", "specs", "quoin-v1", "contracts"),
		"-assets", subjects.work,
		"-bundles", bundlesDir,
		"-trust-root", trustRootPath,
		"-identity", closureIdentityPattern,
		"-issuer", closureIssuer,
		"-out", releaseDir,
		"-work", filepath.Join(workRoot, "finalize"),
		"-allow-delegated", strings.Join(delegated, ","),
		"-registry-insecure")
	publishedManifest, err := os.ReadFile(filepath.Join(releaseDir, "release-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	recorder.note("release-manifest.json", publishedManifest)
	publishedDigest := sha256Hex(publishedManifest)

	// ------------------------------------------------------------
	// Leg 8 — sign the two closure assets (the external Sigstore
	// bundles the publish gate demands) and run the gate with a real
	// offline import into a fresh registry.
	// ------------------------------------------------------------
	signClosureAssets(t, recorder, signer, releaseDir, publishedManifest)
	gateRegistry := "127.0.0.1:" + envOr42("QUOIN_T42_GATE_PORT", "5144")
	startImportRegistry(t, recorder, gateRegistry)
	gateReportPath := filepath.Join(evidenceRoot, "publish-gate-report.json")
	recorder.run(t, "publish-gate", nil, 0, "go", "run", "./internal/release/publish", "verify",
		"-release", releaseDir,
		"-contracts", filepath.Join(repoRoot(), "docs", "specs", "quoin-v1", "contracts"),
		"-trust-root", trustRootPath,
		"-identity", closureIdentityPattern,
		"-issuer", closureIssuer,
		"-import-registry", gateRegistry+"/gate",
		"-report", gateReportPath,
		"-allow-delegated", strings.Join(delegated, ","),
		"-registry-insecure")
	if body, err := os.ReadFile(gateReportPath); err == nil {
		recorder.note("publish-gate-report.json", body)
	}

	// ------------------------------------------------------------
	// Leg 9 — the concrete-site Deployment Acceptance against the
	// published release, with the no-write-back proof.
	// ------------------------------------------------------------
	releaseStateBefore := digestTree(releaseDir)
	site := runDeploymentAcceptance(t, recorder, bin, releaseDir, workRoot)
	releaseStateAfter := digestTree(releaseDir)
	assertions := map[string]map[string]any{
		"digest-dag-no-self-reference": {
			"expected": "manifest binds inventory + qualified evidence one-way; compose bundle carries no manifest; archive carries no signature sidecar; no mutable tags",
			"actual":   "publish gate offline.* and sigstore.* checks passed (publish-gate-report.json)",
		},
		"schema-validation": {
			"expected": "the frozen release-manifest schema accepts the published manifest",
			"actual":   "publish gate manifest.schema passed",
		},
		"offline-verification-import": {
			"expected": "archive unpacks, inner digests equal, layouts content-addressed, import preserves index and platform digests",
			"actual":   map[string]any{"subjectImport": importReport, "gateImport": "publish gate offline.import-readback passed"},
		},
		"applicable-required-scenarios": {
			"expected": "contract gate PASSED; release.native-matrix compose-linux-amd64 PASSED; supply-chain gate passed; foreign-arch and k8s categories delegated to the release workflow",
			"actual": map[string]any{
				"contractGate":   "PASSED (statement binds the source revision)",
				"nativeCell":     "release.native-matrix.compose-linux-amd64 PASSED",
				"delegated":      delegated,
				"delegationNote": "partial sub-proofs remain ticket evidence; the release workflow's native matrix is the designated closure vehicle",
			},
		},
		"deployment-acceptance": {
			"expected": "receipt on the published release within eight hours, no upstream write-back",
			"actual": map[string]any{
				"invocation":       site.InvocationID,
				"subject":          site.ReleaseSubjectDigest,
				"manifest":         publishedDigest,
				"outcome":          site.OverallOutcome,
				"withinEightHours": withinEightHours(site),
				"releaseUnchanged": releaseStateBefore == releaseStateAfter,
			},
		},
	}
	if site.ReleaseSubjectDigest != publishedDigest {
		t.Fatalf("site acceptance subject %s does not bind the published manifest %s", site.ReleaseSubjectDigest, publishedDigest)
	}
	if releaseStateBefore != releaseStateAfter {
		t.Fatal("the deployment acceptance wrote back into the published release")
	}
	if !withinEightHours(site) {
		t.Fatal("finalization outside the eight-hour window")
	}
	// The helper verified the real running deployment: its deployment
	// items must have passed (exit 0), leaving the deterministic WARNED
	// overall from the sixteen site-observation items a single local run
	// never claims (VERIFY-OBSERVATION-002 site observations).
	if site.HelperExitCode != 0 {
		t.Fatalf("site helper verify exited %d (want 0: the deployment items must pass)", site.HelperExitCode)
	}
	if site.OverallOutcome != "warned" {
		t.Fatalf("site acceptance outcome %q, want the deterministic warned (unsubmitted site observations)", site.OverallOutcome)
	}

	// ------------------------------------------------------------
	// Cleanup proof — owned-resource zero (VERIFY-CLEANUP-002/003).
	// ------------------------------------------------------------
	cleanupOnce.Do(func() { cleanupTicket42(t, recorder, workRoot, builderOwned) })
	assertOwnedResourceZero42(t, recorder, baseline)
	if leak := scanTree(evidenceRoot, siteAdminPassword); leak != "" {
		t.Fatalf("sentinel leaked into evidence: %s", leak)
	}

	recorder.observe("runtime-evidence.json", map[string]any{
		"schema":           "quoin-t42-runtime-evidence",
		"ticket":           "T42",
		"issue":            65,
		"gitCommit":        gitCommit(),
		"dirtyStateDigest": dirtyDigest(),
		"startedAt":        startedAt.Format(time.RFC3339Nano),
		"finishedAt":       time.Now().UTC().Format(time.RFC3339Nano),
		"status":           "passed",
		"release": map[string]any{
			"version":      releaseVersion42,
			"manifest":     publishedDigest,
			"manifestPath": filepath.Join(releaseDir, "release-manifest.json"),
			"subject":      subjectDigest,
			"archive":      "quoin-offline-" + releaseVersion42 + ".tar.zst",
			"bundles":      "16 subject + release_manifest + offline (signing closure)",
			"toolchain":    map[string]string{"skopeo": skopeoVersion(), "zstd": "zstd CLI"},
		},
		"components": map[string]any{
			"quoinDeploy": pathAndDigest(filepath.Join(bin, "quoin-deploy")),
			"quoinVerify": pathAndDigest(filepath.Join(bin, "quoin-verify")),
			"registry":    t42RegistryHost,
			"signingRoot": "ephemeral Fulcio-shaped CA (no key outlives the test)",
		},
		"observedTransitions": map[string]any{
			"subjects": "dual-platform application images + merged indexes + Kubernetes and Compose bundles + helpers (validated inventory)",
			"finalize": "manifest derived; archive packed; gate passed with offline import",
			"acceptance": map[string]any{
				"invocation": site.InvocationID, "outcome": site.OverallOutcome,
				"subject": site.ReleaseSubjectDigest, "noWriteBack": true,
			},
		},
		"commands":   recorder.commands,
		"artifacts":  recorder.artifacts,
		"assertions": assertions,
	})
	recorder.observe("cleanup.json", cleanupRecord42())
}

// repackStatement lifts an existing Test Result statement's predicate
// into the signing payload shape without touching its verdict.
func repackStatement(t *testing.T, statement []byte) map[string]any {
	t.Helper()
	var shape struct {
		Predicate json.RawMessage `json:"predicate"`
	}
	if err := json.Unmarshal(statement, &shape); err != nil {
		t.Fatalf("repack statement: %v", err)
	}
	var predicate map[string]any
	if err := json.Unmarshal(shape.Predicate, &predicate); err != nil {
		t.Fatal(err)
	}
	return predicate
}

// extractJSONObject pulls the outermost JSON object out of command
// output that may carry incidental lines around it.
func extractJSONObject(text string) []byte {
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start < 0 || end <= start {
		return []byte("{}")
	}
	return []byte(text[start : end+1])
}

// runReleaseSuiteCell executes the release suite's native compose cell
// through the real coordinator and returns its frozen statement bytes.
func runReleaseSuiteCell(t *testing.T, recorder *ticketEvidence, evidenceRoot, workRoot, pathEnv string, inventoryBytes []byte, subjectDigest string) []byte {
	t.Helper()
	loaded, err := catalog.LoadAndValidate(filepath.Join(repoRoot(), "docs", "specs", "quoin-v1", "contracts", "verification-catalog.yaml"))
	if err != nil {
		t.Fatalf("frozen catalog rejected: %v", err)
	}
	profile, err := result.LoadProfile(filepath.Join(repoRoot(), "docs", "specs", "quoin-v1", "contracts", "verification-result-profile.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	// A per-invocation suite project name plus explicit removal of the
	// suites' shared /tmp credential file prevent any cross-run state or
	// credential leakage into this invocation (VERIFY-MATRIX-004).
	suiteProject := "t42-release-suite-" + strconv.FormatInt(time.Now().UnixNano()&0xffffffff, 36)
	_ = os.Remove(filepath.Join(os.TempDir(), "quoin-suite-"+suiteProject+"-admin-password"))
	_ = os.Remove(filepath.Join(os.TempDir(), "quoin-suite-t42-release-suite-admin-password"))
	suiteRoot := filepath.Join(workRoot, "suite")
	images := map[string]suites.SubjectImage{}
	var inventory struct {
		Images map[string]struct {
			Repository  string            `json:"repository"`
			IndexDigest string            `json:"index_digest"`
			Platforms   map[string]string `json:"platforms"`
		} `json:"images"`
	}
	if err := json.Unmarshal(inventoryBytes, &inventory); err != nil {
		t.Fatal(err)
	}
	for component, image := range inventory.Images {
		images[component] = suites.SubjectImage{Repository: image.Repository, Index: image.IndexDigest, Platforms: image.Platforms}
	}
	ports := suites.InstallPorts{Quoin: 22980, Stele: 22981}
	configPath, err := suites.WriteInstallConfig(suiteRoot, ports)
	if err != nil {
		t.Fatal(err)
	}
	manifestPath, err := suites.WriteReleaseManifest(suiteRoot, releaseVersion42, gitCommit(), images)
	if err != nil {
		t.Fatal(err)
	}
	coordinator := &suites.Coordinator{
		Catalog: loaded, Profile: profile,
		Target:       suites.Target{Backend: "compose", Architecture: "linux/amd64"},
		OutputDir:    filepath.Join(evidenceRoot, "invocation"),
		RepoRoot:     repoRoot(),
		InvocationID: "t42-release-qualification",
		ToolVersion:  "quoin-t42-acceptance",
		Subject:      result.Subject{Name: "quoin-release-subjects", Digest: map[string]string{"sha256": subjectDigest}},
	}
	scenario := loaded.Scenario("release.native-matrix")
	if scenario == nil {
		t.Fatal("release.native-matrix missing")
	}
	env := append(os.Environ(), "PATH="+pathEnv,
		"QUOIN_REPO_ROOT="+repoRoot(),
		"QUOIN_SUITE_WORK_ROOT="+suiteRoot,
		"QUOIN_SUITE_PROJECT="+suiteProject,
		"QUOIN_SUITE_QUOIN_PORT="+strconv.Itoa(ports.Quoin),
		"QUOIN_SUITE_STELE_PORT="+strconv.Itoa(ports.Stele),
		"QUOIN_SUITE_ADMIN_PASSWORD="+suiteAdminPassword,
		"QUOIN_SUITE_CONFIG="+configPath,
		"QUOIN_SUITE_RELEASE_MANIFEST="+manifestPath,
	)
	for index := range scenario.Cells {
		cell := &scenario.Cells[index]
		if !coordinator.Target.MatchesCell(loaded, *cell) {
			continue
		}
		if item, execErr := coordinator.ExecuteCell(scenario, cell, "compose", env); execErr != nil {
			t.Fatalf("cell %s: %v", cell.ID, execErr)
		} else if item.Outcome != "passed" {
			t.Fatalf("cell %s outcome=%s category=%s", cell.ID, item.Outcome, item.Category)
		}
	}
	statement := coordinator.Statement(
		sha256OfFileOrEmpty(filepath.Join(repoRoot(), "docs", "specs", "quoin-v1", "contracts", "verification-catalog.yaml")),
		sha256OfFileOrEmpty(filepath.Join(repoRoot(), "docs", "specs", "quoin-v1", "contracts", "verification-result-profile.yaml")))
	if statement.Predicate.Result != "PASSED" {
		t.Fatalf("release suite verdict %s", statement.Predicate.Result)
	}
	body, err := json.MarshalIndent(statement, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	recorder.note("release-suite-statement.json", body)
	return body
}

func sha256OfFileOrEmpty(path string) string {
	body, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return sha256Hex(body)
}

func pathAndDigest(path string) map[string]any {
	body, err := os.ReadFile(path)
	if err != nil {
		return map[string]any{"path": path, "error": err.Error()}
	}
	info, _ := os.Stat(path)
	return map[string]any{"path": path, "sha256": sha256Hex(body), "bytes": info.Size()}
}

func skopeoVersion() string {
	output, err := exec.Command("skopeo", "--version").Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(output))
}

// withinEightHours proves the receipt closed inside the frozen window.
func withinEightHours(site *deploymentacceptance.Receipt) bool {
	started, err := time.Parse(time.RFC3339, strings.ReplaceAll(site.StartedAt, " ", "T"))
	if err != nil {
		return false
	}
	finalized, err := time.Parse(time.RFC3339, strings.ReplaceAll(site.FinalizedAt, " ", "T"))
	if err != nil {
		return false
	}
	return !finalized.Before(started) && finalized.Sub(started) <= 8*time.Hour
}

// digestTree fingerprints every file of the release directory.
func digestTree(root string) string {
	var builder strings.Builder
	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		relative, _ := filepath.Rel(root, path)
		fmt.Fprintf(&builder, "%s %s\n", relative, sha256Hex(body))
		return nil
	})
	return builder.String()
}

func reportKindOf(name string) string {
	switch name {
	case "offline_import":
		return "quoin-offline-import-report"
	case "supply_chain":
		return "quoin-supply-chain-report"
	default:
		return "quoin-release-delegation"
	}
}

func reportResultOf(name string) string {
	if reportKindOf(name) == "quoin-release-delegation" {
		return "delegated"
	}
	return "passed"
}
