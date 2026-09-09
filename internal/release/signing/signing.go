// Package signing owns the release-closure side of the Sigstore contract:
// the deterministic asset names of the two closure bundles (the final
// Release manifest and the offline archive), the payload decoding needed
// for closure checks, and the exactly-one-external-bundle-per-signed-asset
// verification the publish gate runs (OPS-SUPPLY-001/002, OPS-OFFLINE-001).
//
// This package never signs and never holds keys: production signing is
// keyless cosign in CI; verification belongs to internal/release/supplychain,
// which this package only consumes.
package signing

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/Suknna/quoin/internal/release/subjects"
	"github.com/Suknna/quoin/internal/release/supplychain"
)

// manifestBundleName is the external bundle over the final Release manifest
// bytes (OPS-SUPPLY-001).
const manifestBundleName = "release-manifest.sigstore.json"

// ManifestBundleName returns the closed bundle asset name for the Release
// manifest. It is constant across releases: the subject digest, not the
// name, binds it to one release.
func ManifestBundleName() string {
	return manifestBundleName
}

var bundleNamePattern = regexp.MustCompile(`^[A-Za-z0-9._+-]+\.sigstore\.json$`)

// OfflineBundleName derives the external bundle asset name of the offline
// archive: the archive file name plus the .sigstore.json suffix, kept
// outside the archive so the archive never contains the signature of
// itself (OPS-OFFLINE-001).
func OfflineBundleName(releaseVersion string) (string, error) {
	if _, err := subjects.Names(releaseVersion); err != nil {
		return "", err
	}
	name := "quoin-offline-" + releaseVersion + ".tar.zst.sigstore.json"
	if !bundleNamePattern.MatchString(name) {
		return "", fmt.Errorf("offline bundle name %q malformed", name)
	}
	return name, nil
}

// ClosureNames derives the complete closed bundle-name map of one release:
// the sixteen subject bundles the inventory owns plus the two closure
// bundles. Keys mirror the inventory bundle keys; the closure adds
// "release_manifest" and "offline".
func ClosureNames(releaseVersion string) (map[string]string, error) {
	names := map[string]string{}
	for key, value := range closureSubjectNames() {
		names[key] = value
	}
	offline, err := OfflineBundleName(releaseVersion)
	if err != nil {
		return nil, err
	}
	names["release_manifest"] = manifestBundleName
	names["offline"] = offline
	return names, nil
}

// closureSubjectNames projects the inventory's bundle vocabulary flat; the
// deterministic projection keeps publish-side lookups independent of the
// inventory's map iteration order.
func closureSubjectNames() map[string]string {
	bundles := subjects.NamesForBundles()
	names := map[string]string{
		"kubernetes":                    bundles.Kubernetes,
		"compose":                       bundles.Compose,
		"deployment_helper/linux/amd64": bundles.DeploymentHelper["linux/amd64"],
		"deployment_helper/linux/arm64": bundles.DeploymentHelper["linux/arm64"],
	}
	for _, component := range subjects.Components {
		names["image_indexes/"+component] = bundles.ImageIndexes[component]
		for _, platform := range subjects.Platforms {
			names["image_manifests/"+component+"/"+platform] = bundles.ImageManifests[component][platform]
		}
	}
	return names
}

// envelope is the DSSE envelope inside a Sigstore bundle; only the fields
// the closure checks need are decoded. supplychain owns the cryptographic
// verification of the same bytes.
type envelope struct {
	PayloadType string `json:"payloadType"`
	Payload     string `json:"payload"`
}

type bundleShape struct {
	MediaType            string   `json:"mediaType"`
	Envelope             envelope `json:"dsseEnvelope"`
	VerificationMaterial struct {
		MessageSignature struct {
			MessageDigest struct {
				Digest string `json:"digest"`
			} `json:"messageDigest"`
		} `json:"messageSignature"`
	} `json:"verificationMaterial"`
}

// BundlePayload returns the signed statement payload of a Sigstore bundle:
// the embedded DSSE payload for DSSE bundles, or the external payload
// companion of a cosign messageSignature bundle (sign-blob + bundle
// create). One of the two forms is mandatory.
func BundlePayload(bundleJSON, companion []byte) ([]byte, string, error) {
	var shape bundleShape
	if err := json.Unmarshal(bundleJSON, &shape); err != nil {
		return nil, "", fmt.Errorf("parse bundle: %w", err)
	}
	if shape.MediaType != supplychain.BundleMediaType && !strings.HasPrefix(shape.MediaType, supplychain.BundleMediaType+";") {
		return nil, "", fmt.Errorf("bundle media type %q is not a Sigstore bundle", shape.MediaType)
	}
	if shape.Envelope.PayloadType != "" {
		if shape.Envelope.PayloadType != "application/vnd.in-toto+json" {
			return nil, "", fmt.Errorf("bundle payload type %q is not in-toto", shape.Envelope.PayloadType)
		}
		payload, err := base64.StdEncoding.DecodeString(shape.Envelope.Payload)
		if err != nil {
			return nil, "", fmt.Errorf("bundle payload base64: %w", err)
		}
		return payload, shape.Envelope.PayloadType, nil
	}
	if len(companion) == 0 {
		return nil, "", fmt.Errorf("messageSignature bundle needs its external payload companion")
	}
	return companion, "application/vnd.in-toto+json", nil
}

// TestResultStatement is the closure's minimal projection of an in-toto
// Test Result statement: the verdict, the passed test names and the subject
// digest it binds. The frozen verification-result schema stays the sole
// authority; nothing here re-derives outcomes.
type TestResultStatement struct {
	PredicateType string
	Result        string
	Layer         string
	PassedTests   []string
	SubjectDigest string
}

// ReportStatement is the closure's minimal projection of a signed release
// report statement (the supply-chain gate and the offline-import proof):
// predicateType https://quoin.dev/release-subject/v1 with a typed payload.
type ReportStatement struct {
	Kind   string
	Result string
}

type statementShape struct {
	Subject []struct {
		Name   string            `json:"name"`
		Digest map[string]string `json:"digest"`
	} `json:"subject"`
	PredicateType string `json:"predicateType"`
	Predicate     struct {
		Result      string   `json:"result"`
		PassedTests []string `json:"passedTests"`
		Quoin       struct {
			Layer string `json:"layer"`
		} `json:"quoin"`
		Kind string `json:"kind"`
	} `json:"predicate"`
}

// DecodeTestResult projects a DSSE statement payload into the closure's
// Test Result view.
func DecodeTestResult(payload []byte) (TestResultStatement, error) {
	var shape statementShape
	if err := json.Unmarshal(payload, &shape); err != nil {
		return TestResultStatement{}, fmt.Errorf("parse test result statement: %w", err)
	}
	if len(shape.Subject) != 1 {
		return TestResultStatement{}, fmt.Errorf("statement carries %d subjects, want exactly one", len(shape.Subject))
	}
	return TestResultStatement{
		PredicateType: shape.PredicateType,
		Result:        shape.Predicate.Result,
		Layer:         shape.Predicate.Quoin.Layer,
		PassedTests:   shape.Predicate.PassedTests,
		SubjectDigest: shape.Subject[0].Digest["sha256"],
	}, nil
}

// DecodeReport projects a signed release-report payload. The report body
// is {"kind": ..., "result": ...} typed by the finalize contract.
func DecodeReport(payload []byte) (ReportStatement, error) {
	var shape statementShape
	if err := json.Unmarshal(payload, &shape); err != nil {
		return ReportStatement{}, fmt.Errorf("parse report statement: %w", err)
	}
	return ReportStatement{Kind: shape.Predicate.Kind, Result: shape.Predicate.Result}, nil
}

// Closure is the input of the exactly-one-bundle-per-asset verification:
// the validated subject inventory, the published manifest bytes, the
// offline archive's measured SHA-256, the directory holding the bundles
// and the qualification trust.
type Closure struct {
	Inventory   *subjects.Inventory
	ManifestSHA string
	ArchiveSHA  string
	BundlesDir  string
	Trust       supplychain.Trust
}

// ClosureCheck records one verified bundle-to-asset binding.
type ClosureCheck struct {
	Asset         string `json:"asset"`
	Bundle        string `json:"bundle"`
	SubjectDigest string `json:"subjectDigest"`
	Identity      string `json:"identity,omitempty"`
	Issuer        string `json:"issuer,omitempty"`
}

// ClosureReport is the structured result of VerifyClosure.
type ClosureReport struct {
	Checks []ClosureCheck `json:"checks"`
}

// subjectDigestOf maps one closure asset key to the digest its bundle must
// carry (OPS-SUPPLY-002): OCI subjects bind their OCI digest, blob
// subjects bind the SHA-256 of their published bytes.
func (closure Closure) subjectDigestOf(asset string) (string, error) {
	switch {
	case strings.HasPrefix(asset, "image_indexes/"):
		component := strings.TrimPrefix(asset, "image_indexes/")
		image, ok := closure.Inventory.Images[component]
		if !ok {
			return "", fmt.Errorf("asset %s has no inventory subject", asset)
		}
		return image.IndexDigest, nil
	case strings.HasPrefix(asset, "image_manifests/"):
		rest := strings.TrimPrefix(asset, "image_manifests/")
		component, platform, found := strings.Cut(rest, "/")
		if !found {
			return "", fmt.Errorf("asset key %q malformed", asset)
		}
		image, ok := closure.Inventory.Images[component]
		if !ok {
			return "", fmt.Errorf("asset %s has no inventory subject", asset)
		}
		digest, ok := image.Platforms[platform]
		if !ok {
			return "", fmt.Errorf("asset %s platform %s missing", asset, platform)
		}
		return digest, nil
	case asset == "kubernetes":
		return "sha256:" + closure.Inventory.Kubernetes.SHA256, nil
	case asset == "compose":
		return "sha256:" + closure.Inventory.Compose.SHA256, nil
	case strings.HasPrefix(asset, "deployment_helper/"):
		platform := strings.TrimPrefix(asset, "deployment_helper/")
		helper, ok := closure.Inventory.Helpers[platform]
		if !ok {
			return "", fmt.Errorf("asset %s has no inventory subject", asset)
		}
		return "sha256:" + helper.SHA256, nil
	case asset == "release_manifest":
		return "sha256:" + closure.ManifestSHA, nil
	case asset == "offline":
		return "sha256:" + closure.ArchiveSHA, nil
	}
	return "", fmt.Errorf("unknown closure asset %q", asset)
}

// VerifyClosure proves every signed asset of the release carries exactly
// one external Sigstore bundle with the right subject digest and a
// trusted identity/issuer, that no bundle outside the closed vocabulary
// hides in the bundles directory, and that every bundle verifies offline.
func VerifyClosure(closure Closure) (*ClosureReport, error) {
	if closure.Inventory == nil {
		return nil, fmt.Errorf("closure has no inventory")
	}
	if closure.ManifestSHA == "" || closure.ArchiveSHA == "" {
		return nil, fmt.Errorf("closure needs the manifest and archive digests")
	}
	names, err := ClosureNames(closure.Inventory.ReleaseVersion)
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(names))
	for key := range names {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	entries, err := os.ReadDir(closure.BundlesDir)
	if err != nil {
		return nil, fmt.Errorf("read bundles directory: %w", err)
	}
	present := map[string]bool{}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".sigstore.json") {
			continue
		}
		if !bundleNamePattern.MatchString(name) {
			return nil, fmt.Errorf("bundle %q fails the closed name shape", name)
		}
		present[name] = true
	}

	report := &ClosureReport{Checks: make([]ClosureCheck, 0, len(keys))}
	for _, key := range keys {
		name := names[key]
		if !present[name] {
			return nil, fmt.Errorf("asset %s bundle %s missing", key, name)
		}
		delete(present, name)
		subjectDigest, err := closure.subjectDigestOf(key)
		if err != nil {
			return nil, err
		}
		bundleJSON, err := os.ReadFile(filepath.Join(closure.BundlesDir, name))
		if err != nil {
			return nil, fmt.Errorf("asset %s: %w", key, err)
		}
		var payloadCompanion []byte
		if raw, err := os.ReadFile(filepath.Join(closure.BundlesDir, name+".payload")); err == nil {
			payloadCompanion = raw
		}
		verification, err := supplychain.VerifyBundleWithPayload(bundleJSON, payloadCompanion, subjectDigest, closure.Trust)
		if err != nil {
			return nil, fmt.Errorf("asset %s bundle %s: %w", key, name, err)
		}
		report.Checks = append(report.Checks, ClosureCheck{
			Asset: key, Bundle: name, SubjectDigest: subjectDigest,
			Identity: verification.Identity, Issuer: verification.Issuer,
		})
	}
	if len(present) != 0 {
		extra := make([]string, 0, len(present))
		for name := range present {
			extra = append(extra, name)
		}
		sort.Strings(extra)
		return nil, fmt.Errorf("bundles outside the closed vocabulary: %s", strings.Join(extra, ", "))
	}
	return report, nil
}
