package manifest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	gen "github.com/Suknna/quoin/internal/gen/contracts"
	"github.com/Suknna/quoin/internal/release/signing"
	"github.com/Suknna/quoin/internal/release/subjects"
	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

// SchemaURL is the frozen release-manifest schema identity.
const SchemaURL = "https://github.com/Suknna/quoin/schemas/release-manifest.schema.json"

// ParseValidate loads a manifest document and validates it against the
// frozen release-manifest schema before strict-decoding it.
func ParseValidate(data []byte) (*Document, error) {
	var instance any
	if err := json.Unmarshal(data, &instance); err != nil {
		return nil, fmt.Errorf("parse release manifest: %w", err)
	}
	compiler := jsonschema.NewCompiler()
	schemaDocument, err := jsonschema.UnmarshalJSON(bytes.NewReader(gen.ReleaseManifestSchema))
	if err != nil {
		return nil, fmt.Errorf("load release manifest schema: %w", err)
	}
	if err := compiler.AddResource(SchemaURL, schemaDocument); err != nil {
		return nil, err
	}
	schema, err := compiler.Compile(SchemaURL)
	if err != nil {
		return nil, fmt.Errorf("compile release manifest schema: %w", err)
	}
	if err := schema.Validate(instance); err != nil {
		return nil, fmt.Errorf("validate release manifest: %w", err)
	}
	var document Document
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("decode release manifest: %w", err)
	}
	return &document, nil
}

// EqualToInventory asserts the manifest is the exact mechanical projection
// of the subject inventory: every repository, digest, asset name and
// bundle name must be equal (OPS-RELEASE-001 — read from authority and
// asserted equal, never hand-filled).
func (document *Document) EqualToInventory(inventory *subjects.Inventory) error {
	if document.ReleaseVersion != inventory.ReleaseVersion {
		return fmt.Errorf("release version %q != inventory %q", document.ReleaseVersion, inventory.ReleaseVersion)
	}
	if document.SourceCommit != inventory.SourceCommit {
		return fmt.Errorf("source commit %q != inventory %q", document.SourceCommit, inventory.SourceCommit)
	}
	for _, component := range subjects.Components {
		image, ok := document.Images[component]
		if !ok {
			return fmt.Errorf("manifest image %s missing", component)
		}
		subject := inventory.Images[component]
		if image.Version != subject.Version {
			return fmt.Errorf("%s version %q != inventory %q", component, image.Version, subject.Version)
		}
		if image.Repository != subject.Repository {
			return fmt.Errorf("%s repository %q != inventory %q", component, image.Repository, subject.Repository)
		}
		if image.IndexDigest != subject.IndexDigest {
			return fmt.Errorf("%s index digest %q != inventory %q", component, image.IndexDigest, subject.IndexDigest)
		}
		for _, platform := range subjects.Platforms {
			if image.Platforms[platform] != subject.Platforms[platform] {
				return fmt.Errorf("%s %s digest %q != inventory %q", component, platform, image.Platforms[platform], subject.Platforms[platform])
			}
		}
	}
	if document.Kubernetes.AssetName != inventory.Kubernetes.AssetName ||
		document.Kubernetes.BundleSHA256 != inventory.Kubernetes.SHA256 {
		return fmt.Errorf("kubernetes subject %+v != inventory %+v", document.Kubernetes, inventory.Kubernetes)
	}
	if document.Compose.AssetName != inventory.Compose.AssetName ||
		document.Compose.BundleSHA256 != inventory.Compose.SHA256 {
		return fmt.Errorf("compose subject %+v != inventory %+v", document.Compose, inventory.Compose)
	}
	for _, platform := range subjects.Platforms {
		helper := document.DeploymentHelper.Artifacts[platform]
		if helper.AssetName != inventory.Helpers[platform].AssetName ||
			helper.SHA256 != inventory.Helpers[platform].SHA256 {
			return fmt.Errorf("helper %s subject %+v != inventory %+v", platform, helper, inventory.Helpers[platform])
		}
	}
	// The manifest's bundle map must be the complete closure vocabulary
	// projected over this inventory's bundle map plus the two closure
	// names (OPS-RELEASE-001: read from authority and asserted equal).
	names := subjects.NamesForBundles()
	for _, component := range subjects.Components {
		if document.SigstoreBundles.ImageIndexes[component] != names.ImageIndexes[component] {
			return fmt.Errorf("bundle vocabulary drift in image_indexes/%s", component)
		}
		for _, platform := range subjects.Platforms {
			if document.SigstoreBundles.ImageManifests[component][platform] != names.ImageManifests[component][platform] {
				return fmt.Errorf("bundle vocabulary drift in image_manifests/%s/%s", component, platform)
			}
		}
	}
	if document.SigstoreBundles.Kubernetes != names.Kubernetes ||
		document.SigstoreBundles.Compose != names.Compose ||
		document.SigstoreBundles.DeploymentHelper["linux/amd64"] != names.DeploymentHelper["linux/amd64"] ||
		document.SigstoreBundles.DeploymentHelper["linux/arm64"] != names.DeploymentHelper["linux/arm64"] {
		return fmt.Errorf("bundle vocabulary drift in the blob subject names")
	}
	offlineBundle, err := signing.OfflineBundleName(document.ReleaseVersion)
	if err != nil {
		return err
	}
	if document.SigstoreBundles.ReleaseManifest != signing.ManifestBundleName() ||
		document.SigstoreBundles.Offline != offlineBundle {
		return fmt.Errorf("bundle vocabulary drift in the closure names")
	}
	// The browser block equals the inventory's lock-true facts.
	if document.Browser.PlaywrightVersion != inventory.Browser.PlaywrightVersion ||
		document.Browser.ChromiumRevision != inventory.Browser.ChromiumRevision {
		return fmt.Errorf("browser revision drift against the inventory")
	}
	for _, platform := range subjects.Platforms {
		if document.Browser.Artifacts[platform].SHA256 != inventory.Browser.Artifacts[platform].SHA256 {
			return fmt.Errorf("browser %s digest drift against the inventory", platform)
		}
	}
	return nil
}

// NoMutableReferences asserts no mutable tag reference hides anywhere in
// the manifest bytes (OPS-RELEASE-002).
func (document *Document) NoMutableReferences() error {
	body, err := document.Marshal()
	if err != nil {
		return err
	}
	if strings.Contains(string(body), "latest") {
		return fmt.Errorf("manifest references a mutable tag")
	}
	return nil
}
