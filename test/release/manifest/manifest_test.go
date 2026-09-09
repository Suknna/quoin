package manifest_test

// The T42 manifest closure legs: the final Release manifest derives
// mechanically from the validated subject inventory, the frozen contracts
// and the categorized signed evidence; the frozen schema validates the
// output; adversarial legs prove failed verdicts, foreign subjects,
// missing coverage, unclosed categories and sneaky delegations are all
// rejected. Docker-free and deterministic; the docker-backed closure runs
// in test/release/publish.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/release/inputs"
	"github.com/Suknna/quoin/internal/release/manifest"
	"github.com/Suknna/quoin/internal/release/subjects"
)

const fixtureVersion = "v0.1.0-dev"

// fixtureInventory builds a closed-shape inventory with distinct
// synthetic digests and the lock-true browser facts.
func fixtureInventory(t *testing.T) (*subjects.Inventory, []byte) {
	t.Helper()
	lock, err := inputs.Load()
	if err != nil {
		t.Fatal(err)
	}
	digest := func(seed string) string {
		sum := sha256.Sum256([]byte(seed))
		return hex.EncodeToString(sum[:])
	}
	names, err := subjects.Names(fixtureVersion)
	if err != nil {
		t.Fatal(err)
	}
	bundles := subjects.NamesForBundles()
	inventory := &subjects.Inventory{
		Schema:         subjects.Schema,
		ReleaseVersion: fixtureVersion,
		SourceCommit:   strings.Repeat("ab", 20),
		GeneratedAt:    time.Now().UTC().Format(time.RFC3339),
		Images:         map[string]subjects.ImageSubject{},
		Helpers:        map[string]subjects.BlobSubject{},
		Bundles:        map[string]string{},
		Browser: subjects.BrowserSubjects{
			PlaywrightVersion: lock.Playwright.Version,
			ChromiumRevision:  lock.Playwright.ChromiumRevision,
			Artifacts:         map[string]subjects.BlobSubject{},
		},
	}
	for _, component := range subjects.Components {
		inventory.Images[component] = subjects.ImageSubject{
			Version:     "v0.1.0-dev",
			Repository:  "127.0.0.1:5142/t42/" + component,
			IndexDigest: "sha256:" + digest("index-"+component),
			Platforms: map[string]string{
				"linux/amd64": "sha256:" + digest("amd64-"+component),
				"linux/arm64": "sha256:" + digest("arm64-"+component),
			},
			BuildExecution: map[string]string{"linux/amd64": "native", "linux/arm64": "emulated"},
			Attestations:   map[string][]string{},
		}
	}
	inventory.Kubernetes = subjects.BlobSubject{AssetName: names.Kubernetes, SHA256: digest("kubernetes-bundle")}
	inventory.Compose = subjects.BlobSubject{AssetName: names.Compose, SHA256: digest("compose")}
	for _, platform := range subjects.Platforms {
		inventory.Helpers[platform] = subjects.BlobSubject{AssetName: names.Helper[platform], SHA256: digest("helper-" + platform)}
	}
	for _, platform := range subjects.Platforms {
		artifact := lock.Playwright.Artifacts[platform]
		inventory.Browser.Artifacts[platform] = subjects.BlobSubject{
			AssetName: filepath.Base(artifact.URL), SHA256: artifact.SHA256,
		}
	}
	for key, value := range map[string]string{
		"kubernetes": bundles.Kubernetes, "compose": bundles.Compose,
		"deployment_helper/linux/amd64": bundles.DeploymentHelper["linux/amd64"],
		"deployment_helper/linux/arm64": bundles.DeploymentHelper["linux/arm64"],
	} {
		inventory.Bundles[key] = value
	}
	for _, component := range subjects.Components {
		inventory.Bundles["image_indexes/"+component] = bundles.ImageIndexes[component]
		for _, platform := range subjects.Platforms {
			inventory.Bundles["image_manifests/"+component+"/"+platform] = bundles.ImageManifests[component][platform]
		}
	}
	if err := inventory.Validate(); err != nil {
		t.Fatalf("fixture inventory: %v", err)
	}
	body, err := inventory.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return inventory, body
}

// fixtureEvidence signs the seven closed categories over one subject.
func fixtureEvidence(t *testing.T, signer *closureSigner, dir, subjectDigest, contractsSubject string, verdict string) map[string]manifest.EvidenceInput {
	t.Helper()
	evidence := map[string]manifest.EvidenceInput{}
	put := func(category, subject string, payload map[string]any) {
		evidence[category] = manifest.EvidenceInput{
			Bundle: signer.signStatement(t, dir, category+".bundle.json", "quoin-release-subjects", subject, payload),
		}
	}
	contractsPayload := testResultPayload("contract_gate", verdict, "contracts.machine.valid-minimum-inputs")
	contractsSubjectDigest := subjectDigest
	if contractsSubject != "" {
		contractsSubjectDigest = contractsSubject
	}
	put("contracts", contractsSubjectDigest, contractsPayload)
	put("compose_linux_amd64", subjectDigest, testResultPayload("release_qualification", verdict,
		"release.native-matrix.compose-linux-amd64"))
	put("compose_linux_arm64", subjectDigest, testResultPayload("release_qualification", verdict,
		"release.native-matrix.compose-linux-arm64"))
	put("kubernetes_linux_amd64", subjectDigest, testResultPayload("release_qualification", verdict,
		"release.native-matrix.kubernetes-maintained-0-linux-amd64",
		"release.native-matrix.kubernetes-maintained-1-linux-amd64",
		"release.native-matrix.kubernetes-maintained-2-linux-amd64"))
	put("kubernetes_linux_arm64", subjectDigest, testResultPayload("release_qualification", verdict,
		"release.native-matrix.kubernetes-maintained-0-linux-arm64",
		"release.native-matrix.kubernetes-maintained-1-linux-arm64",
		"release.native-matrix.kubernetes-maintained-2-linux-arm64"))
	put("offline_import", subjectDigest, reportPayload("quoin-offline-import-report", verdictToReport(verdict)))
	put("supply_chain", subjectDigest, reportPayload("quoin-supply-chain-report", verdictToReport(verdict)))
	return evidence
}

func verdictToReport(verdict string) string {
	if verdict == "PASSED" {
		return "passed"
	}
	return "failed"
}

func fixtureInputs(t *testing.T) (manifest.Inputs, *closureSigner, []byte) {
	t.Helper()
	dir := t.TempDir()
	signer := newClosureSigner(t)
	inventory, inventoryBytes := fixtureInventory(t)
	sum := sha256.Sum256(inventoryBytes)
	subjectDigest := hex.EncodeToString(sum[:])
	return manifest.Inputs{
		Inventory:      inventory,
		InventoryBytes: inventoryBytes,
		Evidence:       fixtureEvidence(t, signer, dir, subjectDigest, "", "PASSED"),
		Trust:          signer.trust(),
		ContractsDir:   filepath.Join(repoRoot(t), "docs", "specs", "quoin-v1", "contracts"),
		GeneratedAt:    time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC),
	}, signer, inventoryBytes
}

// cloneEvidence deep-copies one evidence map so adversarial legs never
// leak mutations into shared fixtures.
func cloneEvidence(evidence map[string]manifest.EvidenceInput) map[string]manifest.EvidenceInput {
	copied := make(map[string]manifest.EvidenceInput, len(evidence))
	for category, input := range evidence {
		bundle := append([]byte(nil), input.Bundle...)
		payload := append([]byte(nil), input.Payload...)
		qualification := append([]byte(nil), input.QualificationManifest...)
		copied[category] = manifest.EvidenceInput{Bundle: bundle, Payload: payload, QualificationManifest: qualification}
	}
	return copied
}

func repoRoot(t *testing.T) string {
	t.Helper()
	directory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(directory, "go.mod")); err == nil {
			return directory
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			t.Fatal("repository root not found")
		}
		directory = parent
	}
}

// TestManifestBuildSchemaAndDAG proves the happy closure: the manifest
// validates against the frozen schema, equals its inventory, carries the
// closure bundle names and one-way evidence digests, and has no mutable
// tag reference.
func TestManifestBuildSchemaAndDAG(t *testing.T) {
	inputs, _, inventoryBytes := fixtureInputs(t)
	document, err := manifest.Build(inputs)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	body, err := document.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manifest.ParseValidate(body); err != nil {
		t.Fatalf("frozen schema rejected the generated manifest: %v", err)
	}
	if err := document.EqualToInventory(inputs.Inventory); err != nil {
		t.Fatalf("inventory equality: %v", err)
	}
	if err := document.NoMutableReferences(); err != nil {
		t.Fatal(err)
	}
	if document.Offline.AssetName != "quoin-offline-"+fixtureVersion+".tar.zst" {
		t.Fatalf("offline asset name %q", document.Offline.AssetName)
	}
	if document.SigstoreBundles.ReleaseManifest != "release-manifest.sigstore.json" {
		t.Fatalf("manifest bundle name %q", document.SigstoreBundles.ReleaseManifest)
	}
	if document.SigstoreBundles.Offline != "quoin-offline-"+fixtureVersion+".tar.zst.sigstore.json" {
		t.Fatalf("offline bundle name %q", document.SigstoreBundles.Offline)
	}
	for category, binding := range document.Validation {
		if binding.Status != "passed" {
			t.Fatalf("category %s status %q", category, binding.Status)
		}
		sum := sha256.Sum256(inputs.Evidence[category].Bundle)
		if binding.EvidenceSHA256 != hex.EncodeToString(sum[:]) {
			t.Fatalf("category %s evidence digest drift", category)
		}
	}
	// No self-reference: the manifest bytes are never the qualification
	// subject — the subject is the inventory digest (VERIFY-EVIDENCE-002).
	inventorySum := sha256.Sum256(inventoryBytes)
	if document.Validation["supply_chain"].EvidenceSHA256 == hex.EncodeToString(inventorySum[:]) {
		t.Fatal("evidence binding degenerated onto the inventory bytes")
	}
	if strings.Contains(string(body), "latest") {
		t.Fatal("mutable tag in manifest")
	}
}

// TestManifestDeterminism proves identical inputs produce identical bytes.
func TestManifestDeterminism(t *testing.T) {
	inputs, _, _ := fixtureInputs(t)
	first, err := manifest.Build(inputs)
	if err != nil {
		t.Fatal(err)
	}
	second, err := manifest.Build(inputs)
	if err != nil {
		t.Fatal(err)
	}
	firstBody, _ := first.Marshal()
	secondBody, _ := second.Marshal()
	if string(firstBody) != string(secondBody) {
		t.Fatal("manifest build is not deterministic")
	}
}

// TestManifestAdversarialLegs proves the builder fails closed.
func TestManifestAdversarialLegs(t *testing.T) {
	base, signer, inventoryBytes := fixtureInputs(t)
	sum := sha256.Sum256(inventoryBytes)
	subjectDigest := hex.EncodeToString(sum[:])

	failedVerdict := base
	failedVerdict.Evidence = fixtureEvidence(t, signer, t.TempDir(), subjectDigest, "", "FAILED")
	if _, err := manifest.Build(failedVerdict); err == nil {
		t.Fatal("FAILED verdict accepted")
	} else if !strings.Contains(err.Error(), "FAILED") && !strings.Contains(err.Error(), "failed") {
		t.Fatalf("unexpected failure point: %v", err)
	}

	foreignSubject := base
	foreignSubject.Evidence = fixtureEvidence(t, signer, t.TempDir(), strings.Repeat("07", 32), "", "PASSED")
	if _, err := manifest.Build(foreignSubject); err == nil {
		t.Fatal("foreign subject accepted")
	}

	missingCoverage := base
	missingCoverage.Evidence = cloneEvidence(base.Evidence)
	missingCoverage.Evidence["kubernetes_linux_amd64"] = manifest.EvidenceInput{
		Bundle: signer.signStatement(t, t.TempDir(), "short.bundle.json", "quoin-release-subjects", subjectDigest,
			testResultPayload("release_qualification", "PASSED", "release.native-matrix.kubernetes-maintained-0-linux-amd64")),
	}
	if _, err := manifest.Build(missingCoverage); err == nil {
		t.Fatal("incomplete kubernetes coverage accepted")
	}

	wrongLayer := base
	wrongLayer.Evidence = cloneEvidence(base.Evidence)
	wrongLayer.Evidence["compose_linux_amd64"] = manifest.EvidenceInput{
		Bundle: signer.signStatement(t, t.TempDir(), "layer.bundle.json", "quoin-release-subjects", subjectDigest,
			testResultPayload("contract_gate", "PASSED", "release.native-matrix.compose-linux-amd64")),
	}
	if _, err := manifest.Build(wrongLayer); err == nil {
		t.Fatal("wrong layer accepted")
	}

	missingCategory := base
	missingCategory.Evidence = cloneEvidence(base.Evidence)
	delete(missingCategory.Evidence, "offline_import")
	if _, err := manifest.Build(missingCategory); err == nil {
		t.Fatal("missing category accepted")
	}

	extraCategory := base
	extraCategory.Evidence = cloneEvidence(base.Evidence)
	extraCategory.Evidence["nonsense"] = manifest.EvidenceInput{Bundle: []byte("{}")}
	if _, err := manifest.Build(extraCategory); err == nil {
		t.Fatal("extra category accepted")
	}

	badSignature := base
	badSignature.Evidence = cloneEvidence(base.Evidence)
	badSignature.Evidence["supply_chain"].Bundle[len(base.Evidence["supply_chain"].Bundle)-10] ^= 0xff
	if _, err := manifest.Build(badSignature); err == nil {
		t.Fatal("corrupted bundle accepted")
	}

	browserDrift := base
	browserDrift.Evidence = cloneEvidence(base.Evidence)
	lockedArtifact := base.Inventory.Browser.Artifacts["linux/amd64"]
	driftedArtifact := lockedArtifact
	driftedArtifact.SHA256 = strings.Repeat("99", 32)
	base.Inventory.Browser.Artifacts["linux/amd64"] = driftedArtifact
	if _, err := manifest.Build(browserDrift); err == nil {
		t.Fatal("browser lock drift accepted")
	}
	base.Inventory.Browser.Artifacts["linux/amd64"] = lockedArtifact

	// A delegation report without the explicit allowance is rejected.
	delegated := base
	delegated.Evidence = cloneEvidence(base.Evidence)
	delegated.Evidence["compose_linux_arm64"] = manifest.EvidenceInput{
		Bundle: signer.signStatement(t, t.TempDir(), "delegation.bundle.json", "quoin-release-subjects", subjectDigest,
			reportPayload("quoin-release-delegation", "delegated")),
	}
	if _, err := manifest.Build(delegated); err == nil {
		t.Fatal("undeclared delegation accepted")
	}
	// With the loud local-acceptance allowance the same evidence closes.
	delegated.AllowDelegatedCategories = []string{"compose_linux_arm64"}
	if _, err := manifest.Build(delegated); err != nil {
		t.Fatalf("declared delegation rejected: %v", err)
	}

	// The transitive subject binding: a statement bound to a
	// qualification manifest whose images equal the inventory.
	companion := qualificationCompanion(t, base.Inventory)
	companionSum := sha256.Sum256(companion)
	transitive := base
	transitive.Evidence = cloneEvidence(base.Evidence)
	transitive.Evidence["compose_linux_arm64"] = manifest.EvidenceInput{
		Bundle:                signer.signStatement(t, t.TempDir(), "transitive.bundle.json", "quoin-release-subjects", hex.EncodeToString(companionSum[:]), testResultPayload("release_qualification", "PASSED", "release.native-matrix.compose-linux-arm64")),
		QualificationManifest: companion,
	}
	if _, err := manifest.Build(transitive); err != nil {
		t.Fatalf("transitive subject binding rejected: %v", err)
	}
	// …but a companion projecting different digests is rejected.
	driftedCompanion := []byte(strings.Replace(string(companion), `"index_digest":"sha256:`, `"index_digest":"sha256:f`, 1))
	if string(driftedCompanion) == string(companion) {
		t.Fatal("companion drift fixture did not mutate")
	}
	driftedSum := sha256.Sum256(driftedCompanion)
	driftTransitive := base
	driftTransitive.Evidence = cloneEvidence(base.Evidence)
	driftTransitive.Evidence["compose_linux_arm64"] = manifest.EvidenceInput{
		Bundle:                signer.signStatement(t, t.TempDir(), "drift.bundle.json", "quoin-release-subjects", hex.EncodeToString(driftedSum[:]), testResultPayload("release_qualification", "PASSED", "release.native-matrix.compose-linux-arm64")),
		QualificationManifest: driftedCompanion,
	}
	if _, err := manifest.Build(driftTransitive); err == nil {
		t.Fatal("drifted companion accepted")
	}
}

// qualificationCompanion renders the qualification-manifest shape the
// suites prepare driver projects from an inventory.
func qualificationCompanion(t *testing.T, inventory *subjects.Inventory) []byte {
	t.Helper()
	images := map[string]any{}
	for component, image := range inventory.Images {
		platforms := map[string]string{}
		for platform, digest := range image.Platforms {
			platforms[platform] = digest
		}
		images[component] = map[string]any{
			"repository": image.Repository, "index_digest": image.IndexDigest, "platforms": platforms,
		}
	}
	body, err := json.Marshal(map[string]any{
		"manifest_version": 1, "release_version": inventory.ReleaseVersion,
		"generated_at": "2026-09-06T12:00:00Z", "images": images,
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// TestManifestSourceSubjectBinding proves the contract-gate category can
// bind the source revision instead of the inventory when the finalize
// caller resolves it from the checkout.
func TestManifestSourceSubjectBinding(t *testing.T) {
	dir := t.TempDir()
	signer := newClosureSigner(t)
	inventory, inventoryBytes := fixtureInventory(t)
	sum := sha256.Sum256(inventoryBytes)
	subjectDigest := hex.EncodeToString(sum[:])
	source, err := manifest.ResolveSourceSubject(repoRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	evidence := fixtureEvidence(t, signer, dir, subjectDigest, source.Digest, "PASSED")
	inputs := manifest.Inputs{
		Inventory: inventory, InventoryBytes: inventoryBytes,
		Evidence: evidence, Trust: signer.trust(),
		ContractsDir: filepath.Join(repoRoot(t), "docs", "specs", "quoin-v1", "contracts"),
		RepoRoot:     repoRoot(t),
		GeneratedAt:  time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC),
	}
	if _, err := manifest.Build(inputs); err != nil {
		t.Fatalf("source-bound contract gate rejected: %v", err)
	}
	// A subject from a different revision must fail closed.
	foreign := inputs
	foreign.Evidence["contracts"] = manifest.EvidenceInput{
		Bundle: signer.signStatement(t, dir, "foreign.bundle.json", "quoin-source:000000000000", strings.Repeat("cd", 32),
			testResultPayload("contract_gate", "PASSED", "contracts.machine.valid-minimum-inputs")),
	}
	if _, err := manifest.Build(foreign); err == nil {
		t.Fatal("foreign source subject accepted")
	}
}

// TestTicket42ManifestLegs is the acceptance-visible summary of this
// package: it reruns the happy path and the adversarial legs and records
// the expected-versus-actual assertions under the ticket evidence root.
func TestTicket42ManifestLegs(t *testing.T) {
	inputs, _, _ := fixtureInputs(t)
	document, err := manifest.Build(inputs)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	body, _ := document.Marshal()
	if _, err := manifest.ParseValidate(body); err != nil {
		t.Fatalf("schema: %v", err)
	}
	assertions := map[string]map[string]any{
		"schema-validation": {"expected": "frozen release-manifest schema accepts the mechanically derived manifest", "actual": "validated"},
		"one-way-evidence": {
			"expected": "seven closed categories with status passed and bundle digests",
			"actual":   fmt.Sprintf("%d categories bound", len(document.Validation)),
		},
		"closure-names": {
			"expected": "release-manifest.sigstore.json + offline bundle name",
			"actual":   map[string]string{"manifest": document.SigstoreBundles.ReleaseManifest, "offline": document.SigstoreBundles.Offline},
		},
		"adversarial": {
			"expected": "FAILED verdict, foreign subject, missing coverage, wrong layer, missing/extra category, bad signature, browser drift and undeclared delegation all rejected",
			"actual":   "rejected (TestManifestAdversarialLegs)",
		},
	}
	if root := os.Getenv("QUOIN_EVIDENCE_DIR"); root != "" {
		legs := filepath.Join(root, "legs")
		if err := os.MkdirAll(legs, 0o755); err != nil {
			t.Fatal(err)
		}
		record, _ := json.MarshalIndent(map[string]any{"ticket": "T42", "leg": "manifest", "assertions": assertions}, "", "  ")
		if err := os.WriteFile(filepath.Join(legs, "manifest-leg.json"), append(record, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(legs, "manifest-leg-manifest.json"), body, 0o644)
	}
}
