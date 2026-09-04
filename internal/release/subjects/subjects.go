// Package subjects owns the immutable subject inventory of one source/tag
// build: the four multi-platform OCI image digests, the Chart, the Compose
// bundle and the two static deployment helpers, each with the deterministic
// asset names and digests the final Release manifest will later reference
// (OPS-RELEASE-001/002/003). The inventory is a build fact document, never
// the evidence-referencing Release manifest itself.
package subjects

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Schema identifies the inventory document.
const Schema = "quoin-release-subjects-v1"

// Platforms is the closed platform set.
var Platforms = []string{"linux/amd64", "linux/arm64"}

// Components is the closed component set.
var Components = []string{"lintel", "plinth", "quoin", "stele"}

// ImageSubject records one component's measured index and per-platform
// manifest digests.
type ImageSubject struct {
	Repository  string            `json:"repository"`
	IndexDigest string            `json:"index_digest"`
	Platforms   map[string]string `json:"platforms"`
	// BuildExecution records how each platform manifest was produced:
	// "native" (native runner) or "emulated" (binfmt build evidence only,
	// VERIFY-EXTERNAL-004). It is provenance, never an architecture
	// qualification claim.
	BuildExecution map[string]string `json:"build_execution"`
	// Attestations records the attestation manifest digests BuildKit
	// attached for each platform manifest (SBOM + provenance).
	Attestations map[string][]string `json:"attestations"`
}

// BlobSubject is one measured release file.
type BlobSubject struct {
	AssetName string `json:"asset_name"`
	SHA256    string `json:"sha256"`
}

// Inventory is the full subject set of one build.
type Inventory struct {
	Schema         string                  `json:"schema"`
	ReleaseVersion string                  `json:"release_version"`
	SourceCommit   string                  `json:"source_commit"`
	GeneratedAt    string                  `json:"generated_at"`
	Images         map[string]ImageSubject `json:"images"`
	Chart          ChartSubject            `json:"chart"`
	Compose        BlobSubject             `json:"compose"`
	Helpers        map[string]BlobSubject  `json:"deployment_helper"`
	Bundles        map[string]string       `json:"sigstore_bundles"`
	Browser        BrowserSubjects         `json:"browser"`
}

// ChartSubject records the packaged Helm chart and its OCI push result.
type ChartSubject struct {
	OCIRepository string `json:"oci_repository"`
	OCIDigest     string `json:"oci_digest"`
	TgzAssetName  string `json:"tgz_asset_name"`
	TgzSHA256     string `json:"tgz_sha256"`
}

// BrowserSubjects records the measured locked browser artifacts baked into
// the lintel image (OPS-IMAGE-002).
type BrowserSubjects struct {
	PlaywrightVersion string                 `json:"playwright_version"`
	ChromiumRevision  string                 `json:"chromium_revision"`
	Artifacts         map[string]BlobSubject `json:"artifacts"`
}

// ChartVersion derives the Helm chart version from the release version: the
// full SemVer after removing the single leading "v" (OPS-RELEASE-001).
func ChartVersion(releaseVersion string) (string, error) {
	if !strings.HasPrefix(releaseVersion, "v") {
		return "", fmt.Errorf("release version %q must start with v", releaseVersion)
	}
	chartVersion := releaseVersion[1:]
	if !semVerPattern.MatchString(chartVersion) {
		return "", fmt.Errorf("release version %q is not v-prefixed SemVer", releaseVersion)
	}
	return chartVersion, nil
}

var semVerPattern = regexp.MustCompile(`^(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$`)

// AssetNames derives the deterministic asset names of one release version.
type AssetNames struct {
	ChartTgz string
	Compose  string
	Helper   map[string]string
}

// Names derives all non-image asset names. Compose keeps the full
// "v<version>"; the chart tgz strips the single leading "v".
func Names(releaseVersion string) (AssetNames, error) {
	chartVersion, err := ChartVersion(releaseVersion)
	if err != nil {
		return AssetNames{}, err
	}
	return AssetNames{
		ChartTgz: "quoin-" + chartVersion + ".tgz",
		Compose:  "quoin-compose-" + releaseVersion + ".tar.gz",
		Helper: map[string]string{
			"linux/amd64": "quoin-deploy-linux-amd64",
			"linux/arm64": "quoin-deploy-linux-arm64",
		},
	}, nil
}

// BundleNames derives the closed Sigstore bundle asset-name vocabulary
// (OPS-SUPPLY-001). "release_manifest" and "offline" belong to the final
// release closure and are intentionally absent here.
type BundleNames struct {
	ImageIndexes     map[string]string
	ImageManifests   map[string]map[string]string
	HelmOCI          string
	Compose          string
	DeploymentHelper map[string]string
}

func NamesForBundles() BundleNames {
	indexes := map[string]string{}
	manifests := map[string]map[string]string{}
	for _, component := range []string{"quoin", "plinth", "lintel", "stele"} {
		indexes[component] = component + "-index.sigstore.json"
		perPlatform := map[string]string{}
		for _, platform := range Platforms {
			perPlatform[platform] = component + "-manifest-" + strings.TrimPrefix(platform, "linux/") + ".sigstore.json"
		}
		manifests[component] = perPlatform
	}
	return BundleNames{
		ImageIndexes:   indexes,
		ImageManifests: manifests,
		HelmOCI:        "quoin-chart-oci.sigstore.json",
		Compose:        "quoin-compose-bundle.sigstore.json",
		DeploymentHelper: map[string]string{
			"linux/amd64": "quoin-deploy-linux-amd64.sigstore.json",
			"linux/arm64": "quoin-deploy-linux-arm64.sigstore.json",
		},
	}
}

var (
	ociDigestPattern  = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	bareSha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
	repositoryPattern = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*(?::[0-9]{1,5})?(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)*$`)
	bundleNamePattern = regexp.MustCompile(`^[A-Za-z0-9._+-]+\.sigstore\.json$`)
)

// Validate asserts the complete closed inventory shape: both platforms of
// all four components with distinct digests, digest-pinned repositories
// without any tag (never "latest"), deterministic asset names, and bundle
// names from the closed vocabulary.
func (inventory *Inventory) Validate() error {
	if inventory.Schema != Schema {
		return fmt.Errorf("inventory schema %q", inventory.Schema)
	}
	names, err := Names(inventory.ReleaseVersion)
	if err != nil {
		return err
	}
	if len(inventory.Images) != len(Components) {
		return fmt.Errorf("inventory has %d image subjects, want %d", len(inventory.Images), len(Components))
	}
	for _, component := range Components {
		image, ok := inventory.Images[component]
		if !ok {
			return fmt.Errorf("image subject %s missing", component)
		}
		if !repositoryPattern.MatchString(image.Repository) || strings.Contains(image.Repository, ":") && !regexpHostPort(image.Repository) {
			return fmt.Errorf("%s repository %q is not a bare repository", component, image.Repository)
		}
		if strings.HasSuffix(image.Repository, ":latest") || strings.Contains(image.Repository, ":latest/") {
			return fmt.Errorf("%s repository references latest", component)
		}
		if err := checkOCIDigest(image.IndexDigest); err != nil {
			return fmt.Errorf("%s index: %w", component, err)
		}
		if len(image.Platforms) != 2 {
			return fmt.Errorf("%s platform set %v", component, keysOf(image.Platforms))
		}
		seen := map[string]bool{image.IndexDigest: true}
		for _, platform := range Platforms {
			digest, ok := image.Platforms[platform]
			if !ok {
				return fmt.Errorf("%s platform %s missing", component, platform)
			}
			if err := checkOCIDigest(digest); err != nil {
				return fmt.Errorf("%s platform %s: %w", component, platform, err)
			}
			if seen[digest] {
				return fmt.Errorf("%s reuses digest %s across index/platform manifests", component, digest)
			}
			seen[digest] = true
			if mode := image.BuildExecution[platform]; mode != "native" && mode != "emulated" {
				return fmt.Errorf("%s platform %s build execution mode %q", component, platform, mode)
			}
		}
	}
	if inventory.Chart.TgzAssetName != names.ChartTgz {
		return fmt.Errorf("chart asset %q want %q", inventory.Chart.TgzAssetName, names.ChartTgz)
	}
	if err := checkBareSHA256(inventory.Chart.TgzSHA256); err != nil {
		return fmt.Errorf("chart tgz: %w", err)
	}
	if err := checkOCIDigest(inventory.Chart.OCIDigest); err != nil {
		return fmt.Errorf("chart oci: %w", err)
	}
	if !repositoryPattern.MatchString(inventory.Chart.OCIRepository) {
		return fmt.Errorf("chart oci repository %q", inventory.Chart.OCIRepository)
	}
	if inventory.Compose.AssetName != names.Compose {
		return fmt.Errorf("compose asset %q want %q", inventory.Compose.AssetName, names.Compose)
	}
	if err := checkBareSHA256(inventory.Compose.SHA256); err != nil {
		return fmt.Errorf("compose bundle: %w", err)
	}
	for _, platform := range Platforms {
		helper, ok := inventory.Helpers[platform]
		if !ok {
			return fmt.Errorf("helper %s missing", platform)
		}
		if helper.AssetName != names.Helper[platform] {
			return fmt.Errorf("helper %s asset %q want %q", platform, helper.AssetName, names.Helper[platform])
		}
		if err := checkBareSHA256(helper.SHA256); err != nil {
			return fmt.Errorf("helper %s: %w", platform, err)
		}
	}
	bundles := NamesForBundles()
	expected := map[string]string{
		"helm_oci": bundles.HelmOCI, "compose": bundles.Compose,
		"deployment_helper/linux/amd64": bundles.DeploymentHelper["linux/amd64"],
		"deployment_helper/linux/arm64": bundles.DeploymentHelper["linux/arm64"],
	}
	for _, component := range Components {
		expected["image_indexes/"+component] = bundles.ImageIndexes[component]
		for _, platform := range Platforms {
			expected["image_manifests/"+component+"/"+platform] = bundles.ImageManifests[component][platform]
		}
	}
	if len(inventory.Bundles) != len(expected) {
		return fmt.Errorf("bundle map has %d entries, want the closed %d", len(inventory.Bundles), len(expected))
	}
	for key, want := range expected {
		got, ok := inventory.Bundles[key]
		if !ok {
			return fmt.Errorf("bundle %s missing", key)
		}
		if got != want {
			return fmt.Errorf("bundle %s name %q want %q", key, got, want)
		}
		if !bundleNamePattern.MatchString(got) {
			return fmt.Errorf("bundle %s name %q malformed", key, got)
		}
	}
	return nil
}

// Marshal renders the canonical inventory JSON.
func (inventory *Inventory) Marshal() ([]byte, error) {
	return json.MarshalIndent(inventory, "", "  ")
}

// Parse loads and validates an inventory document.
func Parse(data []byte) (*Inventory, error) {
	var inventory Inventory
	if err := json.Unmarshal(data, &inventory); err != nil {
		return nil, err
	}
	if err := inventory.Validate(); err != nil {
		return nil, err
	}
	return &inventory, nil
}

func checkOCIDigest(digest string) error {
	if !ociDigestPattern.MatchString(digest) {
		return fmt.Errorf("malformed oci digest %q", digest)
	}
	return nil
}

func checkBareSHA256(digest string) error {
	if !bareSha256Pattern.MatchString(digest) {
		return fmt.Errorf("malformed bare sha256 %q", digest)
	}
	return nil
}

// regexpHostPort reports whether the only colon in a repository is the
// registry host port.
func regexpHostPort(repository string) bool {
	parts := strings.SplitN(repository, "/", 2)
	return strings.Count(repository, ":") == 1 && strings.Contains(parts[0], ":")
}

func keysOf(mapping map[string]string) []string {
	keys := make([]string, 0, len(mapping))
	for key := range mapping {
		keys = append(keys, key)
	}
	return keys
}
