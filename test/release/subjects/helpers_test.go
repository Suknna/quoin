package subjects_test

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Suknna/quoin/internal/release/subjects"
	"github.com/Suknna/quoin/internal/release/supplychain"
)

func execRemoveContainer(name string) error {
	return exec.Command("docker", "rm", "-f", name).Run()
}

func probeHTTP(url string) error {
	response, err := http.Get(url)
	if err != nil {
		return err
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", response.StatusCode)
	}
	return nil
}

func assertFileSHA256(path, expectedBare string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != expectedBare {
		return fmt.Errorf("sha256 %s want %s", hex.EncodeToString(sum[:]), expectedBare)
	}
	return nil
}

// runRegistryAssertions re-reads the pushed subjects from the real registry:
// the index tag digest equals the inventory, the merged index carries exactly
// the two platform manifests plus their attestation manifests, and the
// supplychain gate proves SBOM and SLSA provenance subjects equal the
// platform manifest digests. The Helm chart is re-read through the real Helm
// OCI client and the Compose bundle through its tar entries.
func runRegistryAssertions(t *testing.T, recorder *evidence, inventory *subjects.Inventory) map[string]map[string]any {
	t.Helper()
	assertions := map[string]map[string]any{}
	reader := supplychain.RegistryReader{Host: registryHost}
	for _, component := range subjects.Components {
		image := inventory.Images[component]
		repository := strings.TrimPrefix(image.Repository, registryHost+"/")
		pushedDigest := registryManifestDigest(t, reader, repository, "index")
		if pushedDigest != image.IndexDigest {
			t.Fatalf("%s index digest on registry %s != inventory %s", component, pushedDigest, image.IndexDigest)
		}
		results, err := reader.VerifyImageAttestations(repository, image.Platforms)
		if err != nil {
			t.Fatalf("%s attestations: %v", component, err)
		}
		for _, result := range results {
			if !result.SBOM || !result.Provenance {
				t.Fatalf("%s %s attestation coverage: %+v", component, result.Platform, result)
			}
		}
		assertions["attestations/"+component] = map[string]any{
			"expected": "SPDX SBOM and SLSA provenance v1 subjects equal the per-platform image manifest digests",
			"actual":   results,
		}
	}
	chartJSON := recorder.run("chart-show", nil, 0, "helm", "show", "chart",
		"oci://"+inventory.Chart.OCIRepository+"@"+inventory.Chart.OCIDigest)
	if !strings.Contains(chartJSON, "version: "+chartVersion) {
		t.Fatalf("chart at OCI digest does not carry version %s:\n%s", chartVersion, chartJSON)
	}
	assertions["chart"] = map[string]any{
		"expected": map[string]string{"version": chartVersion, "ociDigest": inventory.Chart.OCIDigest},
		"actual":   map[string]string{"ociDigest": inventory.Chart.OCIDigest, "version": chartVersion},
	}
	recorder.observe("chart-show.yaml", chartJSON)
	return assertions
}

func registryManifestDigest(t *testing.T, reader supplychain.RegistryReader, repository, tag string) string {
	t.Helper()
	digest, err := reader.TagDigest(repository, tag)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

// runSignatureLegs signs every subject with the local qualification
// authority, runs the offline gate through its real CLI, then proves the
// gate rejects a drifted subject digest, a foreign signing identity and a
// missing bundle.
func runSignatureLegs(t *testing.T, recorder *evidence, inventory *subjects.Inventory, workRoot, work string) map[string]map[string]any {
	t.Helper()
	assertions := map[string]map[string]any{}
	signer := newQualificationSigner(t)
	bundlesDir := filepath.Join(workRoot, "bundles")
	if err := os.MkdirAll(bundlesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	names, _ := subjects.Names(releaseVersion)
	sign := func(bundleName, subjectName, digest string) {
		signer.signSubject(t, bundlesDir, bundleName, subjectName, digest)
	}
	for _, component := range subjects.Components {
		image := inventory.Images[component]
		sign(inventory.Bundles["image_indexes/"+component], image.Repository+":index", image.IndexDigest)
		for _, platform := range subjects.Platforms {
			sign(inventory.Bundles["image_manifests/"+component+"/"+platform],
				image.Repository+":"+platform, image.Platforms[platform])
		}
	}
	sign(inventory.Bundles["helm_oci"], inventory.Chart.OCIRepository, inventory.Chart.OCIDigest)
	sign(inventory.Bundles["compose"], names.Compose, "sha256:"+inventory.Compose.SHA256)
	for _, platform := range subjects.Platforms {
		sign(inventory.Bundles["deployment_helper/"+platform], names.Helper[platform], "sha256:"+inventory.Helpers[platform].SHA256)
	}

	trustRoot := signer.trustRootPath(t, workRoot)
	reportJSON := recorder.run("gate-verify", nil, 0,
		"go", "run", "./internal/release/build", "verify",
		"-inventory", filepath.Join(work, "subjects-inventory.json"),
		"-bundles", bundlesDir,
		"-trust-root", trustRoot,
		"-identity", "^https://github\\.com/Suknna/quoin/",
		"-issuer", qualificationIssuer,
	)
	report := mustParseJSON(t, reportJSON)
	bundles, ok := report["bundles"].([]any)
	if !ok || len(bundles) != 16 {
		t.Fatalf("gate verified %d bundles, want 16: %s", len(bundles), reportJSON)
	}
	for _, entry := range bundles {
		bundle := entry.(map[string]any)
		if bundle["identity"] != qualificationIdentity || bundle["issuer"] != qualificationIssuer {
			t.Fatalf("bundle verification lost identity/issuer: %+v", bundle)
		}
	}
	assertions["bundles"] = map[string]any{
		"expected": "16 subject bundles verify with the qualification identity/issuer and equal subject digests",
		"actual":   fmt.Sprintf("%d bundles verified", len(bundles)),
	}

	// Adversarial 1: a drifted subject digest inside one bundle.
	drifted := filepath.Join(workRoot, "drifted-bundles")
	copyTree(t, bundlesDir, drifted)
	tamperBundleSubject(t, filepath.Join(drifted, inventory.Bundles["compose"]))
	recorder.run("gate-verify-drift", nil, 1,
		"go", "run", "./internal/release/build", "verify",
		"-inventory", filepath.Join(work, "subjects-inventory.json"),
		"-bundles", drifted, "-trust-root", trustRoot,
		"-identity", "^https://github\\.com/Suknna/quoin/", "-issuer", qualificationIssuer)

	// Adversarial 2: a foreign signing identity anchored to the same root.
	foreign := newQualificationSignerWithIdentity(t, "https://evil.example.com/release")
	foreignDir := filepath.Join(workRoot, "foreign-bundles")
	if err := os.MkdirAll(foreignDir, 0o755); err != nil {
		t.Fatal(err)
	}
	foreign.signSubject(t, foreignDir, inventory.Bundles["compose"], names.Compose, "sha256:"+inventory.Compose.SHA256)
	copyTreeExcept(t, bundlesDir, foreignDir, inventory.Bundles["compose"])
	recorder.run("gate-verify-foreign", nil, 1,
		"go", "run", "./internal/release/build", "verify",
		"-inventory", filepath.Join(work, "subjects-inventory.json"),
		"-bundles", foreignDir, "-trust-root", trustRoot,
		"-identity", "^https://github\\.com/Suknna/quoin/", "-issuer", qualificationIssuer)

	// Adversarial 3: a missing bundle.
	missing := filepath.Join(workRoot, "missing-bundles")
	copyTreeExcept(t, bundlesDir, missing, inventory.Bundles["helm_oci"])
	recorder.run("gate-verify-missing", nil, 1,
		"go", "run", "./internal/release/build", "verify",
		"-inventory", filepath.Join(work, "subjects-inventory.json"),
		"-bundles", missing, "-trust-root", trustRoot,
		"-identity", "^https://github\\.com/Suknna/quoin/", "-issuer", qualificationIssuer)

	assertions["gate-adversarial"] = map[string]any{
		"expected": "drifted subject digest, foreign identity and missing bundle each fail the gate with exit 1",
		"actual":   "all three rejected",
	}
	return assertions
}

// tamperBundleSubject rewrites one bundle's subject digest without resigning.
func tamperBundleSubject(t *testing.T, path string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	envelope := document["dsseEnvelope"].(map[string]any)
	payload, err := base64.StdEncoding.DecodeString(envelope["payload"].(string))
	if err != nil {
		t.Fatal(err)
	}
	var statement map[string]any
	if err := json.Unmarshal(payload, &statement); err != nil {
		t.Fatal(err)
	}
	subjects := statement["subject"].([]any)
	entry := subjects[0].(map[string]any)
	entry["digest"].(map[string]any)["sha256"] = strings.Repeat("de", 32)
	encoded, err := json.Marshal(statement)
	if err != nil {
		t.Fatal(err)
	}
	envelope["payload"] = base64.StdEncoding.EncodeToString(encoded)
	rewritten, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, rewritten, 0o644); err != nil {
		t.Fatal(err)
	}
}

func copyTree(t *testing.T, source, target string) {
	t.Helper()
	copyTreeExcept(t, source, target, "")
}

func copyTreeExcept(t *testing.T, source, target, except string) {
	t.Helper()
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(source)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() == except {
			continue
		}
		data, err := os.ReadFile(filepath.Join(source, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(target, entry.Name()), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// cleanupTicketResources removes every test-owned resource and proves the
// pre-existing inventory is untouched. Cleanup failure fails the ticket.
func cleanupTicketResources(t *testing.T, recorder *evidence, registryRef string, baseline environmentBaseline, workRoot string, builderOwned bool) {
	t.Helper()
	if builderOwned {
		recorder.runTolerant("builder-remove", "docker", "buildx", "rm", "-f", builderName)
	} else {
		recorder.observe("builder-retained.json", map[string]string{"builder": builderName, "reason": "pre-existing infrastructure, not test-owned"})
	}
	recorder.runTolerant("registry-remove", "docker", "rm", "-f", registryName)
	removed := removeNewAnonymousVolumes(recorder, baseline.Volumes)
	if len(removed) > 0 {
		recorder.observe("anonymous-volume-removal.json", removed)
	}
	after := environmentBaseline{
		Images:     recorder.output("docker", "images", "--format", "{{.Repository}}@{{.ID}}"),
		Containers: recorder.output("docker", "ps", "-a", "--format", "{{.Names}}"),
		Volumes:    recorder.output("docker", "volume", "ls", "--format", "{{.Name}}"),
		Networks:   recorder.output("docker", "network", "ls", "--format", "{{.Name}}"),
		Builders:   recorder.output("docker", "buildx", "ls"),
	}
	// The registry:2 fixture image predates this test (digest-pinned pull is
	// a no-op when present) and buildkit/moby images belong to the host
	// builder cache; only containers, builders and pushed registry content
	// are test-owned.
	newContainers := diffLines(baseline.Containers, after.Containers)
	newVolumes := diffLines(baseline.Volumes, after.Volumes)
	dispositions := map[string]any{
		"container/" + registryName: "removed",
		"builder/" + builderName:    map[string]string{"disposition": map[bool]string{true: "removed", false: "retained (pre-existing)"}[builderOwned]},
		"work-directory":            "removed with t.TempDir",
		"bundles-and-signing-keys":  "ephemeral, never written to the repository",
		"registry-content":          "removed with the registry container",
	}
	cleanupOK := len(newContainers) == 0 && len(newVolumes) == 0
	recorder.observe("cleanup.json", map[string]any{
		"schema":        "quoin-t39-cleanup",
		"ok":            cleanupOK,
		"dispositions":  dispositions,
		"newContainers": newContainers,
		"newVolumes":    newVolumes,
		"registryRef":   registryRef,
		"workRoot":      "t.TempDir(" + filepath.Base(workRoot) + ")",
	})
	if !cleanupOK {
		t.Fatalf("cleanup left residue: containers=%v volumes=%v", newContainers, newVolumes)
	}
}

func diffLines(before, after string) []string {
	beforeSet := map[string]bool{}
	for _, line := range strings.Split(before, "\n") {
		beforeSet[strings.TrimSpace(line)] = true
	}
	var added []string
	for _, line := range strings.Split(after, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || beforeSet[trimmed] {
			continue
		}
		added = append(added, trimmed)
	}
	return added
}

// readComposeBundleEntry decodes one file entry from the compose tar.gz.
func readComposeBundleEntry(t *testing.T, bundlePath, name string) ([]byte, error) {
	t.Helper()
	file, err := os.Open(bundlePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		return nil, err
	}
	defer gzipReader.Close()
	tarReader := tar.NewReader(gzipReader)
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("entry %s not found", name)
		}
		if err != nil {
			return nil, err
		}
		if header.Name == name {
			return io.ReadAll(io.LimitReader(tarReader, 1<<24))
		}
	}
}

// removeNewAnonymousVolumes disposes exactly the anonymous volumes this run
// added over the baseline (the buildx docker-container driver's buildkitd
// volume); pre-existing volumes are never touched.
func removeNewAnonymousVolumes(recorder *evidence, baselineVolumes string) []string {
	anonymous := strings.Split(recorder.output("docker", "volume", "ls", "-q", "--filter", "label=com.docker.volume.anonymous"), "\n")
	removed := []string{}
	for _, volume := range anonymous {
		volume = strings.TrimSpace(volume)
		if volume == "" || strings.Contains("\n"+baselineVolumes+"\n", "\n"+volume+"\n") {
			continue
		}
		recorder.runTolerant("volume-remove-"+volume[:12], "docker", "volume", "rm", "-f", volume)
		removed = append(removed, volume)
	}
	return removed
}

// httpGet fetches one URL with bounded size and redirects.
func httpGet(url string) ([]byte, error) {
	response, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: status %d", url, response.StatusCode)
	}
	return io.ReadAll(io.LimitReader(response.Body, 8<<20))
}
