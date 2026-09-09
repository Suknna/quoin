// Package manifest builds and verifies the final Quoin Release manifest
// (OPS-RELEASE-001/002). Every field is read mechanically from its machine
// authority — the validated subject inventory, the frozen contract
// documents, the locked browser artifacts and the categorized signed
// qualification evidence — and nothing is hand-filled. The manifest binds
// qualification evidence one-way through validation.<category>.evidence_sha256
// and never becomes a qualification subject itself (VERIFY-EVIDENCE-002/004).
package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/Suknna/quoin/internal/release/inputs"
	"github.com/Suknna/quoin/internal/release/signing"
	"github.com/Suknna/quoin/internal/release/subjects"
	"github.com/Suknna/quoin/internal/release/supplychain"
)

// ManifestVersion is the frozen manifest document version.
const ManifestVersion = 1

// Categories is the closed validation-category set of the release manifest;
// each category binds exactly one signed evidence bundle.
var Categories = []string{
	"contracts",
	"compose_linux_amd64",
	"compose_linux_arm64",
	"kubernetes_linux_amd64",
	"kubernetes_linux_arm64",
	"offline_import",
	"supply_chain",
}

// Report predicate kinds of the two report categories: the frozen
// release-subject statement predicate carries a typed report payload.
const (
	ReportPredicate     = "https://quoin.dev/release-subject/v1"
	KindOfflineImport   = "quoin-offline-import-report"
	KindSupplyChainGate = "quoin-supply-chain-report"
	KindDelegation      = "quoin-release-delegation"
)

// TestResultPredicate is the in-toto Test Result predicate type the
// qualification statements must carry.
const TestResultPredicate = "https://in-toto.io/attestation/test-result/v0.1"

// The qualification layers a category's evidence must come from
// (catalog layer vocabulary).
const (
	layerContractGate = "contract_gate"
	layerRelease      = "release_qualification"
)

// requiredCells freezes the catalog test names each qualification category
// must cover (scenario.cell identifiers of verification-catalog.yaml).
var requiredCells = map[string][]string{
	"compose_linux_amd64": {
		"release.native-matrix.compose-linux-amd64",
	},
	"compose_linux_arm64": {
		"release.native-matrix.compose-linux-arm64",
	},
	"kubernetes_linux_amd64": {
		"release.native-matrix.kubernetes-maintained-0-linux-amd64",
		"release.native-matrix.kubernetes-maintained-1-linux-amd64",
		"release.native-matrix.kubernetes-maintained-2-linux-amd64",
	},
	"kubernetes_linux_arm64": {
		"release.native-matrix.kubernetes-maintained-0-linux-arm64",
		"release.native-matrix.kubernetes-maintained-1-linux-arm64",
		"release.native-matrix.kubernetes-maintained-2-linux-arm64",
	},
}

// Document is the typed Release manifest. The frozen
// contracts/schemas/release-manifest.schema.json stays the only shape
// authority; this struct only projects it.
type Document struct {
	ManifestVersion  int                   `json:"manifest_version"`
	ReleaseVersion   string                `json:"release_version"`
	SourceCommit     string                `json:"source_commit"`
	GeneratedAt      string                `json:"generated_at"`
	Images           map[string]ImageEntry `json:"images"`
	Browser          BrowserEntry          `json:"browser"`
	Kubernetes       ComposeEntry          `json:"kubernetes"`
	Compose          ComposeEntry          `json:"compose"`
	DeploymentHelper struct {
		Artifacts map[string]BlobEntry `json:"artifacts"`
	} `json:"deployment_helper"`
	Offline struct {
		AssetName string `json:"asset_name"`
	} `json:"offline"`
	SigstoreBundles struct {
		ImageIndexes     map[string]string            `json:"image_indexes"`
		ImageManifests   map[string]map[string]string `json:"image_manifests"`
		Kubernetes       string                       `json:"kubernetes"`
		ReleaseManifest  string                       `json:"release_manifest"`
		Compose          string                       `json:"compose"`
		DeploymentHelper map[string]string            `json:"deployment_helper"`
		Offline          string                       `json:"offline"`
	} `json:"sigstore_bundles"`
	Contracts  ContractsEntry            `json:"contracts"`
	Validation map[string]PassedEvidence `json:"validation"`
}

type ImageEntry struct {
	Version     string            `json:"version"`
	Repository  string            `json:"repository"`
	IndexDigest string            `json:"index_digest"`
	Platforms   map[string]string `json:"platforms"`
}

type BrowserArtifact struct {
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

type BrowserEntry struct {
	PlaywrightVersion string                     `json:"playwright_version"`
	ChromiumRevision  string                     `json:"chromium_revision"`
	Artifacts         map[string]BrowserArtifact `json:"artifacts"`
}

type ComposeEntry struct {
	AssetName    string `json:"asset_name"`
	BundleSHA256 string `json:"bundle_sha256"`
}

type BlobEntry struct {
	AssetName string `json:"asset_name"`
	SHA256    string `json:"sha256"`
}

// PassedEvidence is the one-way evidence binding of one category
// (VERIFY-EVIDENCE-004).
type PassedEvidence struct {
	Status         string `json:"status"`
	EvidenceSHA256 string `json:"evidence_sha256"`
}

// EvidenceInput is one categorized signed bundle with its two optional
// external files: the signed-statement payload a cosign messageSignature
// bundle needs beside it, and — for qualification statements — the
// qualification manifest whose digest is the statement's subject (the
// transitive binding to the release inventory).
type EvidenceInput struct {
	Bundle                []byte
	Payload               []byte
	QualificationManifest []byte
}

// Inputs assembles everything Build reads. InventoryBytes are the exact
// inventory document bytes the evidence statements must bind (directly or
// transitively through the qualification manifest companion); RepoRoot
// provides the source-revision subject the contract-gate statement binds.
//
// AllowDelegatedCategories names qualification categories whose evidence
// may be a signed delegation report instead of a passing Test Result.
// It exists for the local acceptance mechanism proof only: partial
// sub-proofs stay ticket evidence and the complete atomic scenario
// closure belongs to the release workflow's native matrix (the ticket's
// atomicity rule). The production finalize path never sets it.
type Inputs struct {
	Inventory      *subjects.Inventory
	InventoryBytes []byte
	Evidence       map[string]EvidenceInput
	Trust          supplychain.Trust
	ContractsDir   string
	RepoRoot       string
	GeneratedAt    time.Time

	AllowDelegatedCategories []string
}

// Build derives the complete Release manifest. It fails closed on any
// evidence bundle that does not verify, does not bind the subject
// inventory, is not PASSED, or does not cover its category's frozen
// cells; and on any drift between the locked browser artifacts and the
// inventory.
func Build(inputs Inputs) (*Document, error) {
	if inputs.Inventory == nil || len(inputs.InventoryBytes) == 0 {
		return nil, fmt.Errorf("manifest build needs the parsed inventory and its exact bytes")
	}
	subjectSHA := sha256.Sum256(inputs.InventoryBytes)
	subjectDigest := hex.EncodeToString(subjectSHA[:])
	bundleNames := subjects.NamesForBundles()

	document := &Document{
		ManifestVersion: ManifestVersion,
		ReleaseVersion:  inputs.Inventory.ReleaseVersion,
		SourceCommit:    inputs.Inventory.SourceCommit,
		GeneratedAt:     inputs.GeneratedAt.UTC().Format(time.RFC3339),
		Images:          map[string]ImageEntry{},
	}
	document.DeploymentHelper.Artifacts = map[string]BlobEntry{}
	for _, component := range subjects.Components {
		image := inputs.Inventory.Images[component]
		document.Images[component] = ImageEntry{
			Version:     image.Version,
			Repository:  image.Repository,
			IndexDigest: image.IndexDigest,
			Platforms:   image.Platforms,
		}
	}
	browser, err := browserEntry(inputs.Inventory)
	if err != nil {
		return nil, err
	}
	document.Browser = browser
	document.Kubernetes = ComposeEntry{AssetName: inputs.Inventory.Kubernetes.AssetName, BundleSHA256: inputs.Inventory.Kubernetes.SHA256}
	document.Compose = ComposeEntry{AssetName: inputs.Inventory.Compose.AssetName, BundleSHA256: inputs.Inventory.Compose.SHA256}
	for _, platform := range subjects.Platforms {
		helper := inputs.Inventory.Helpers[platform]
		document.DeploymentHelper.Artifacts[platform] = BlobEntry{AssetName: helper.AssetName, SHA256: helper.SHA256}
	}
	document.Offline.AssetName = "quoin-offline-" + inputs.Inventory.ReleaseVersion + ".tar.zst"
	document.SigstoreBundles.ImageIndexes = bundleNames.ImageIndexes
	document.SigstoreBundles.ImageManifests = bundleNames.ImageManifests
	document.SigstoreBundles.Kubernetes = bundleNames.Kubernetes
	document.SigstoreBundles.ReleaseManifest = signing.ManifestBundleName()
	document.SigstoreBundles.Compose = bundleNames.Compose
	document.SigstoreBundles.DeploymentHelper = bundleNames.DeploymentHelper
	offlineBundle, err := signing.OfflineBundleName(inputs.Inventory.ReleaseVersion)
	if err != nil {
		return nil, err
	}
	document.SigstoreBundles.Offline = offlineBundle

	contracts, err := Contracts(inputs.ContractsDir)
	if err != nil {
		return nil, err
	}
	document.Contracts = *contracts

	var sourceSubject *SourceSubject
	if inputs.RepoRoot != "" {
		subject, err := ResolveSourceSubject(inputs.RepoRoot)
		if err != nil {
			return nil, fmt.Errorf("source subject: %w", err)
		}
		sourceSubject = subject
	}
	delegated := map[string]bool{}
	for _, category := range inputs.AllowDelegatedCategories {
		if requiredCells[category] == nil {
			return nil, fmt.Errorf("delegation allowance %q is not a qualification category", category)
		}
		delegated[category] = true
	}
	validation, err := VerifyEvidenceWithDelegation(inputs.Evidence, subjectDigest, inputs.Inventory, sourceSubject, delegated, inputs.Trust)
	if err != nil {
		return nil, err
	}
	document.Validation = validation
	return document, nil
}

// SourceSubject is the source-revision binding the contract-gate
// statement carries: the exact git commit plus the working-tree status
// digest, mirroring quoin-verify's subject resolution. A clean tree and a
// dirty tree never share a subject digest.
type SourceSubject struct {
	Name   string
	Digest string
}

// ResolveSourceSubject recomputes the source-revision subject of one
// checkout (same formula as the quoin-verify contract gate).
func ResolveSourceSubject(root string) (*SourceSubject, error) {
	commit, err := gitOutput(root, "rev-parse", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("git HEAD: %w", err)
	}
	status, err := gitOutput(root, "status", "--porcelain")
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(commit + "\n" + status))
	return &SourceSubject{
		Name:   "quoin-source:" + commit[:12],
		Digest: hex.EncodeToString(sum[:]),
	}, nil
}

func gitOutput(root string, args ...string) (string, error) {
	command := exec.Command("git", args...)
	command.Dir = root
	body, err := command.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(body)), nil
}

// browserEntry projects the release-locked browser artifacts: the frozen
// release-inputs lock owns revision, per-architecture SHA-256 and byte
// size; the inventory must carry the identical digests (OPS-RELEASE-001).
func browserEntry(inventory *subjects.Inventory) (BrowserEntry, error) {
	lock, err := inputs.Load()
	if err != nil {
		return BrowserEntry{}, fmt.Errorf("release inputs lock: %w", err)
	}
	entry := BrowserEntry{
		PlaywrightVersion: lock.Playwright.Version,
		ChromiumRevision:  lock.Playwright.ChromiumRevision,
		Artifacts:         map[string]BrowserArtifact{},
	}
	if entry.PlaywrightVersion != inventory.Browser.PlaywrightVersion ||
		entry.ChromiumRevision != inventory.Browser.ChromiumRevision {
		return BrowserEntry{}, fmt.Errorf("browser lock drift: lock %s/%s inventory %s/%s",
			entry.PlaywrightVersion, entry.ChromiumRevision,
			inventory.Browser.PlaywrightVersion, inventory.Browser.ChromiumRevision)
	}
	for _, platform := range subjects.Platforms {
		locked, ok := lock.Playwright.Artifacts[platform]
		if !ok {
			return BrowserEntry{}, fmt.Errorf("browser lock has no %s artifact", platform)
		}
		inventoried, ok := inventory.Browser.Artifacts[platform]
		if !ok {
			return BrowserEntry{}, fmt.Errorf("inventory has no %s browser artifact", platform)
		}
		if locked.SHA256 != inventoried.SHA256 {
			return BrowserEntry{}, fmt.Errorf("browser %s digest drift: lock %s inventory %s", platform, locked.SHA256, inventoried.SHA256)
		}
		entry.Artifacts[platform] = BrowserArtifact{SHA256: locked.SHA256, Bytes: locked.Bytes}
	}
	return entry, nil
}

// Marshal renders the manifest JSON deterministically.
func (document *Document) Marshal() ([]byte, error) {
	return json.MarshalIndent(document, "", "  ")
}

// OfflineAssetName returns the offline archive asset name of this release.
func (document *Document) OfflineAssetName() string {
	return document.Offline.AssetName
}
