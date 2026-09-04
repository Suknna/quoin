package subjects_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/release/subjects"
)

const (
	releaseVersion = "v0.1.0-dev"
	registryName   = "t39-registry"
	builderName    = "t39-release-subjects"
	chartVersion   = "0.1.0-dev"
)

var registryHost = fmt.Sprintf("127.0.0.1:%s", envOr("QUOIN_T39_REGISTRY_PORT", "5099"))

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

// TestTicket39 proves the T39 release-subject path end to end through real
// tooling: one buildx docker-container builder produces the four component
// images for linux/amd64 (native) and linux/arm64 (binfmt build evidence
// only, never a runtime claim), with BuildKit SBOM and SLSA provenance
// attestations; the merged OCI indexes, the packaged Helm chart, the
// digest-pinned Compose bundle and both static quoin-deploy helpers land in a
// real local registry / work tree; every subject is signed by a local
// Fulcio-shaped qualification authority and the offline gate verifies
// subject digests, certificate identity/issuer and attestation subjects.
// Adversarial legs prove the gate rejects drift, foreign identities and
// missing bundles. Evidence lands under QUOIN_EVIDENCE_DIR.
func TestTicket39(t *testing.T) {
	evidenceDir := os.Getenv("QUOIN_EVIDENCE_DIR")
	if evidenceDir == "" {
		t.Skip("QUOIN_EVIDENCE_DIR not set; T39 acceptance evidence run disabled")
	}
	requireTools(t)
	recorder := newEvidence(t, evidenceDir)
	workRoot := t.TempDir()
	baseline := captureEnvironmentBaseline(t, recorder)
	registryRef := startRegistry(t, recorder)

	// Failure-path safety net: an aborted acceptance must not leave the
	// test-owned registry or a test-created builder behind even when
	// assertions fail first. A pre-existing healthy builder is foreign
	// infrastructure (warm cache) and stays.
	builderCreatedByTest := createBuilder(t, recorder, workRoot)
	t.Cleanup(func() {
		if builderCreatedByTest {
			tolerantRemove("docker", "buildx", "rm", "-f", builderName)
		}
		tolerantRemove("docker", "rm", "-f", registryName)
		// The buildx docker-container driver leaves the buildkitd container's
		// anonymous volume dangling after removal; dispose of exactly the
		// anonymous volumes this run added over the baseline.
		removeNewAnonymousVolumes(recorder, baseline.Volumes)
	})

	work := filepath.Join(workRoot, "work")
	inventoryPath := filepath.Join(work, "subjects-inventory.json")
	started := time.Now()
	recorder.run("build-subjects", nil, 0,
		"go", "run", "./internal/release/build",
		"-registry", registryHost+"/t39",
		"-version", releaseVersion,
		"-chart-oci", registryHost+"/t39/charts",
		"-builder", builderName,
		"-work", work,
		"-out", inventoryPath,
		"-stage", "all",
		"-platform", "linux/amd64=native",
		"-platform", "linux/arm64=emulated",
	)
	recorder.observe("build-timing.json", map[string]any{"seconds": time.Since(started).Seconds()})

	rawInventory, err := os.ReadFile(inventoryPath)
	if err != nil {
		t.Fatal(err)
	}
	inventory, err := subjects.Parse(rawInventory)
	if err != nil {
		t.Fatalf("inventory failed validation: %v", err)
	}
	assertions := runInventoryAssertions(t, recorder, inventory, rawInventory, work)
	assertions["lock-equality"] = runLockEqualityAssertion(t, recorder, inventory)
	artifactAssertions := runArtifactAssertions(t, recorder, inventory, work)
	registryAssertions := runRegistryAssertions(t, recorder, inventory)
	bundleAssertions := runSignatureLegs(t, recorder, inventory, workRoot, work)

	recorder.writeRuntimeEvidence(map[string]any{
		"schema":     "quoin-t39-runtime-evidence",
		"release":    releaseVersion,
		"subject":    "qualification subjects built from one source/tag",
		"assertions": mergeAssertions(assertions, artifactAssertions, registryAssertions, bundleAssertions),
		"qemu": map[string]string{
			"role": "build evidence only (VERIFY-EXTERNAL-004)",
			"declared": "linux/arm64=emulated; no arm64 binary or container was executed; " +
				"native arm64 qualification belongs to the CI native runners and T40",
		},
	})
	cleanupTicketResources(t, recorder, registryRef, baseline, workRoot, builderCreatedByTest)
}

func requireTools(t *testing.T) {
	t.Helper()
	for _, name := range []string{"docker", "go", "helm", "git"} {
		if _, err := lookPath(name); err != nil {
			t.Skipf("%s is not available: %v", name, err)
		}
	}
	// linux/arm64 image builds use binfmt emulation as build evidence.
	if _, err := os.Stat("/proc/sys/fs/binfmt_misc/qemu-aarch64"); err != nil {
		t.Skipf("linux/arm64 build emulation is not available (enable binfmt, e.g. docker run --privileged tonistiigi/binfmt --install arm64): %v", err)
	}
}

func lookPath(name string) (string, error) {
	return execLookPath(name)
}

// captureEnvironmentBaseline snapshots the pre-existing docker inventory so
// cleanup can prove test-owned resources were removed and nothing else moved.
type environmentBaseline struct {
	Images     string
	Containers string
	Volumes    string
	Networks   string
	Builders   string
}

func captureEnvironmentBaseline(t *testing.T, recorder *evidence) environmentBaseline {
	t.Helper()
	baseline := environmentBaseline{
		Images:     recorder.output("docker", "images", "--format", "{{.Repository}}@{{.ID}}"),
		Containers: recorder.output("docker", "ps", "-a", "--format", "{{.Names}}"),
		Volumes:    recorder.output("docker", "volume", "ls", "--format", "{{.Name}}"),
		Networks:   recorder.output("docker", "network", "ls", "--format", "{{.Name}}"),
		Builders:   recorder.output("docker", "buildx", "ls"),
	}
	recorder.observe("environment-baseline.json", baseline)
	return baseline
}

func startRegistry(t *testing.T, recorder *evidence) string {
	t.Helper()
	recorder.run("registry-pull", nil, 0, "docker", "pull", "docker.io/library/registry:2")
	reference := recorder.output("docker", "image", "inspect", "docker.io/library/registry:2", "--format", "{{index .RepoDigests 0}}")
	if !strings.Contains(reference, "@sha256:") {
		t.Fatalf("registry fixture is not digest-pinned: %q", reference)
	}
	_ = execRemoveContainer(registryName)
	recorder.run("registry-run", nil, 0, "docker", "run", "-d", "--name", registryName,
		"-p", registryHost+":5000", reference)
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if err := probeHTTP("http://" + registryHost + "/v2/"); err == nil {
			return reference
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatal("local registry did not become ready")
	return ""
}

// createBuilder provisions the test-owned buildx docker-container builder.
// The BuildKit config mirrors the registries this network can reach and marks
// the plain-HTTP local registry; the CI workflow provisions its own builder
// with direct registry access.
func createBuilder(t *testing.T, recorder *evidence, workRoot string) bool {
	t.Helper()
	config := fmt.Sprintf(`
[registry."docker.io"]
  mirrors = ["docker.m.daocloud.io"]
[registry."gcr.io"]
  mirrors = ["gcr.m.daocloud.io"]
[registry.%q]
  http = true
`, registryHost)
	configPath := filepath.Join(workRoot, "buildkitd.toml")
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	// A healthy pre-existing builder is reused as warm foreign
	// infrastructure: the cleanup contract only disposes resources this run
	// created. An interrupted run's stale builder is disposable.
	if recorder.runTolerant("builder-inspect-existing", "docker", "buildx", "inspect", builderName) == 0 {
		recorder.observe("builder-reused.json", map[string]string{"builder": builderName, "owned": "pre-existing"})
		return false
	}
	recorder.run("builder-create", nil, 0, "docker", "buildx", "create",
		"--name", builderName, "--driver", "docker-container",
		"--driver-opt", "network=host", "--config", configPath)
	recorder.run("builder-bootstrap", nil, 0, "docker", "buildx", "inspect", "--bootstrap", builderName)
	return true
}

// runInventoryAssertions checks the builder's measured inventory against the
// frozen contracts: closed component/platform sets, distinct digests, the
// deterministic asset names, disclosed build-execution provenance, no
// mutable tags anywhere and the locked browser artifacts.
func runInventoryAssertions(t *testing.T, recorder *evidence, inventory *subjects.Inventory, rawInventory []byte, work string) map[string]map[string]any {
	t.Helper()
	assertions := map[string]map[string]any{}
	names, _ := subjects.Names(releaseVersion)
	for _, component := range subjects.Components {
		image := inventory.Images[component]
		entry := map[string]any{
			"expected": "two distinct platform manifests and an index digest, repository@digest only",
			"index":    image.IndexDigest,
			"platforms": map[string]string{
				"linux/amd64": image.Platforms["linux/amd64"],
				"linux/arm64": image.Platforms["linux/arm64"],
			},
			"distinct": image.Platforms["linux/amd64"] != image.Platforms["linux/arm64"] &&
				image.IndexDigest != image.Platforms["linux/amd64"] &&
				image.IndexDigest != image.Platforms["linux/arm64"],
			"build_execution": map[string]string{
				"linux/amd64": image.BuildExecution["linux/amd64"],
				"linux/arm64": image.BuildExecution["linux/arm64"],
			},
		}
		if !entry["distinct"].(bool) {
			t.Fatalf("%s does not carry distinct index/per-platform digests: %+v", component, image)
		}
		if image.BuildExecution["linux/amd64"] != "native" || image.BuildExecution["linux/arm64"] != "emulated" {
			t.Fatalf("%s build execution modes undisclosed: %+v", component, image.BuildExecution)
		}
		if !strings.HasPrefix(image.Repository, registryHost+"/t39/") {
			t.Fatalf("%s repository %q is outside the test registry namespace", component, image.Repository)
		}
		// The bare-repository shape (no tag, digest binding only) is already
		// asserted by subjects.Parse; here the registry namespace is pinned.
		assertions["images/"+component] = entry
	}
	if inventory.Chart.TgzAssetName != names.ChartTgz || inventory.Compose.AssetName != names.Compose {
		t.Fatalf("asset names drifted: chart=%q compose=%q", inventory.Chart.TgzAssetName, inventory.Compose.AssetName)
	}
	assertions["asset-names"] = map[string]any{
		"expected": map[string]string{"chart": names.ChartTgz, "compose": names.Compose,
			"helperAmd64": names.Helper["linux/amd64"], "helperArm64": names.Helper["linux/arm64"]},
		"actual": map[string]string{"chart": inventory.Chart.TgzAssetName, "compose": inventory.Compose.AssetName,
			"helperAmd64": inventory.Helpers["linux/amd64"].AssetName, "helperArm64": inventory.Helpers["linux/arm64"].AssetName},
	}
	if strings.Contains(string(rawInventory), "latest") {
		t.Fatal("inventory mentions latest")
	}
	assertions["no-latest"] = map[string]any{
		"expected": "no mutable tag reference anywhere in the inventory",
		"actual":   "absent",
	}
	for path, subject := range map[string]subjects.BlobSubject{
		work + "/" + names.Compose:               inventory.Compose,
		work + "/" + names.Helper["linux/amd64"]: inventory.Helpers["linux/amd64"],
		work + "/" + names.Helper["linux/arm64"]: inventory.Helpers["linux/arm64"],
	} {
		if err := assertFileSHA256(path, subject.SHA256); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
	}
	chartTgz := work + "/chart/" + inventory.Chart.TgzAssetName
	if err := assertFileSHA256(chartTgz, inventory.Chart.TgzSHA256); err != nil {
		t.Fatalf("chart tgz: %v", err)
	}
	assertions["blob-sha256"] = map[string]any{
		"expected": "compose bundle and both helpers match their recorded SHA-256",
		"actual":   "equal",
	}
	recorder.observe("subjects-inventory.json", json.RawMessage(rawInventory))
	return assertions
}

func mergeAssertions(groups ...map[string]map[string]any) map[string]map[string]any {
	merged := map[string]map[string]any{}
	for _, group := range groups {
		for key, value := range group {
			merged[key] = value
		}
	}
	return merged
}

func mustParseJSON(t *testing.T, data string) map[string]any {
	t.Helper()
	// Tool stdout may carry incidental non-JSON lines around the report;
	// extract the outermost JSON object.
	start := strings.Index(data, "{")
	end := strings.LastIndex(data, "}")
	if start < 0 || end <= start {
		t.Fatalf("no JSON object in output:\n%s", tail(data, 20))
	}
	var document map[string]any
	if err := json.Unmarshal([]byte(data[start:end+1]), &document); err != nil {
		t.Fatal(err)
	}
	return document
}

func mustJSONString(t *testing.T, document map[string]any, path string) string {
	t.Helper()
	segments := strings.Split(path, ".")
	var current any = document
	for _, segment := range segments {
		mapping, ok := current.(map[string]any)
		if !ok {
			t.Fatalf("path %q stopped at %q", path, segment)
		}
		current, ok = mapping[segment]
		if !ok {
			t.Fatalf("path %q missing %q", path, segment)
		}
	}
	text, ok := current.(string)
	if !ok {
		t.Fatalf("path %q is not a string", path)
	}
	return text
}
