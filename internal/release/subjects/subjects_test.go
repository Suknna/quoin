package subjects

import (
	"encoding/json"
	"strings"
	"testing"
)

func testInventory() *Inventory {
	names, _ := Names("v0.1.0-dev")
	bundles := NamesForBundles()
	bundleMap := map[string]string{
		"helm_oci":                      bundles.HelmOCI,
		"compose":                       bundles.Compose,
		"deployment_helper/linux/amd64": bundles.DeploymentHelper["linux/amd64"],
		"deployment_helper/linux/arm64": bundles.DeploymentHelper["linux/arm64"],
	}
	for _, component := range Components {
		bundleMap["image_indexes/"+component] = bundles.ImageIndexes[component]
		for _, platform := range Platforms {
			bundleMap["image_manifests/"+component+"/"+platform] = bundles.ImageManifests[component][platform]
		}
	}
	images := map[string]ImageSubject{}
	for i, component := range Components {
		platforms := map[string]string{}
		execution := map[string]string{}
		attestations := map[string][]string{}
		for j, platform := range Platforms {
			platforms[platform] = digestAt(i*3 + j + 1)
			execution[platform] = "native"
			attestations[platform] = []string{digestAt(100 + i*3 + j)}
		}
		images[component] = ImageSubject{
			Repository:     "registry.local/quoin/" + component,
			IndexDigest:    digestAt(i*3 + 3),
			Platforms:      platforms,
			BuildExecution: execution,
			Attestations:   attestations,
		}
	}
	return &Inventory{
		Schema:         Schema,
		ReleaseVersion: "v0.1.0-dev",
		SourceCommit:   strings.Repeat("ab", 20),
		GeneratedAt:    "2026-09-03T00:00:00Z",
		Images:         images,
		Chart: ChartSubject{
			OCIRepository: "registry.local/quoin/charts/quoin",
			OCIDigest:     digestAt(50),
			TgzAssetName:  names.ChartTgz,
			TgzSHA256:     strings.TrimPrefix(digestAt(51), "sha256:"),
		},
		Compose: BlobSubject{AssetName: names.Compose, SHA256: strings.TrimPrefix(digestAt(52), "sha256:")},
		Helpers: map[string]BlobSubject{
			"linux/amd64": {AssetName: names.Helper["linux/amd64"], SHA256: strings.TrimPrefix(digestAt(53), "sha256:")},
			"linux/arm64": {AssetName: names.Helper["linux/arm64"], SHA256: strings.TrimPrefix(digestAt(54), "sha256:")},
		},
		Bundles: bundleMap,
		Browser: BrowserSubjects{
			PlaywrightVersion: "1.62.1",
			ChromiumRevision:  "1234",
			Artifacts: map[string]BlobSubject{
				"linux/amd64": {AssetName: "chromium-linux-amd64.zip", SHA256: strings.TrimPrefix(digestAt(60), "sha256:")},
				"linux/arm64": {AssetName: "chromium-linux-arm64.zip", SHA256: strings.TrimPrefix(digestAt(61), "sha256:")},
			},
		},
	}
}

func digestAt(n int) string {
	hex := make([]byte, 64)
	for i := range hex {
		hex[i] = '0'
	}
	s := "sha256:" + string(hex[:62])
	s += string(rune('0'+n/10)) + string(rune('0'+n%10))
	return s
}

func TestValidateAcceptsClosedInventory(t *testing.T) {
	inventory := testInventory()
	if err := inventory.Validate(); err != nil {
		t.Fatal(err)
	}
	data, err := inventory.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.ReleaseVersion != "v0.1.0-dev" {
		t.Fatal("roundtrip lost version")
	}
}

func TestNamesFollowReleaseContract(t *testing.T) {
	names, err := Names("v1.2.3-rc.1")
	if err != nil {
		t.Fatal(err)
	}
	if names.ChartTgz != "quoin-1.2.3-rc.1.tgz" {
		t.Fatalf("chart tgz %q", names.ChartTgz)
	}
	if names.Compose != "quoin-compose-v1.2.3-rc.1.tar.gz" {
		t.Fatalf("compose %q", names.Compose)
	}
	if names.Helper["linux/arm64"] != "quoin-deploy-linux-arm64" {
		t.Fatalf("helper %q", names.Helper["linux/arm64"])
	}
	if _, err := Names("1.2.3"); err == nil {
		t.Fatal("version without leading v must fail")
	}
	if _, err := Names("vnotsemver"); err == nil {
		t.Fatal("non-SemVer must fail")
	}
}

func TestValidateRejectsLatestAndDrift(t *testing.T) {
	inventory := testInventory()
	image := inventory.Images["quoin"]
	image.Repository = "registry.local/quoin/quoin:latest"
	inventory.Images["quoin"] = image
	if err := inventory.Validate(); err == nil {
		t.Fatal("latest tag must fail")
	}

	inventory = testInventory()
	image = inventory.Images["stele"]
	platforms := map[string]string(image.Platforms)
	platforms["linux/arm64"] = platforms["linux/amd64"]
	inventory.Images["stele"] = ImageSubject{
		Repository: image.Repository, IndexDigest: image.IndexDigest,
		Platforms: platforms, BuildExecution: image.BuildExecution, Attestations: image.Attestations,
	}
	if err := inventory.Validate(); err == nil {
		t.Fatal("duplicate platform digests must fail")
	}

	inventory = testInventory()
	image = inventory.Images["plinth"]
	image.BuildExecution["linux/arm64"] = "qemu-runtime"
	inventory.Images["plinth"] = image
	if err := inventory.Validate(); err == nil {
		t.Fatal("undisclosed build execution mode must fail")
	}

	inventory = testInventory()
	delete(inventory.Bundles, "helm_oci")
	if err := inventory.Validate(); err == nil {
		t.Fatal("missing bundle entry must fail")
	}

	inventory = testInventory()
	inventory.Chart.TgzAssetName = "quoin-9.9.9.tgz"
	if err := inventory.Validate(); err == nil {
		t.Fatal("chart asset name drift must fail")
	}
}

func TestValidateRejectsMalformedDigests(t *testing.T) {
	inventory := testInventory()
	inventory.Compose.SHA256 = "deadbeef"
	if err := inventory.Validate(); err == nil {
		t.Fatal("malformed compose sha must fail")
	}
	data, _ := json.Marshal(inventory)
	if _, err := Parse(data); err == nil {
		t.Fatal("parse must reject the malformed inventory")
	}
}
