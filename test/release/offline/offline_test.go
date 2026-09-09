package offline_test

// The T42 offline-archive legs: a deterministic tar.zst carries the
// manifest, assets, verification materials and the four component OCI
// layouts; verification proves inner digest equality, content-addressed
// layouts and the no-embedded-signature rule; tampering legs fail
// closed; the skopeo roundtrip (docker-gated) proves layout pull, import
// and digest readback through real OCI tooling.

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Suknna/quoin/internal/release/offline"
)

// synthLayout writes one minimal valid OCI layout with two platform
// manifests over a shared layer and config blob, content-addressed.
// It returns the index digest of the layout.
// The fixture OCI documents mirror the OpenContainers Go struct field
// order exactly: c/image re-serializes any non-canonical bytes during
// oci: writes, which would silently change digests; real BuildKit
// producers emit this canonical order, and so does the fixture.
type fixtureDescriptor struct {
	MediaType string           `json:"mediaType"`
	Digest    string           `json:"digest"`
	Size      int              `json:"size"`
	Platform  *fixturePlatform `json:"platform,omitempty"`
}

type fixturePlatform struct {
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
}

type fixtureManifest struct {
	SchemaVersion int                 `json:"schemaVersion"`
	MediaType     string              `json:"mediaType"`
	Config        fixtureDescriptor   `json:"config"`
	Layers        []fixtureDescriptor `json:"layers"`
}

type fixtureIndex struct {
	SchemaVersion int                 `json:"schemaVersion"`
	MediaType     string              `json:"mediaType"`
	Manifests     []fixtureDescriptor `json:"manifests"`
}

// synthLayout writes one minimal valid OCI layout with two distinct
// platform manifests over a shared layer, content-addressed. It returns
// the index digest of the layout.
func synthLayout(t *testing.T, dir, seed string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "blobs", "sha256"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeBlob := func(body []byte) string {
		sum := sha256.Sum256(body)
		name := hex.EncodeToString(sum[:])
		if err := os.WriteFile(filepath.Join(dir, "blobs", "sha256", name), body, 0o644); err != nil {
			t.Fatal(err)
		}
		return "sha256:" + name
	}
	// Real image layers are compressed; c/image converts uncompressed
	// layers in transit (changing digests), so the fixture layer is
	// gzipped like every producer's output.
	var layerBuffer bytes.Buffer
	gzipWriter := gzip.NewWriter(&layerBuffer)
	gzipWriter.Write([]byte("layer:" + seed))
	gzipWriter.Close()
	layerBody := layerBuffer.Bytes()
	layerDigest := writeBlob(layerBody)
	layerSize := len(layerBody)
	manifestOf := func(arch string) (string, int) {
		config := []byte(`{"architecture":"` + arch + `","os":"linux","rootfs":{"type":"layers","diff_ids":[]}}`)
		configDigest := writeBlob(config)
		manifest := fixtureManifest{
			SchemaVersion: 2,
			MediaType:     "application/vnd.oci.image.manifest.v1+json",
			Config:        fixtureDescriptor{MediaType: "application/vnd.oci.image.config.v1+json", Digest: configDigest, Size: len(config)},
			Layers:        []fixtureDescriptor{{MediaType: "application/vnd.oci.image.layer.v1.tar+gzip", Digest: layerDigest, Size: layerSize}},
		}
		body, err := json.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
		return writeBlob(body), len(body)
	}
	amdDigest, amdSize := manifestOf("amd64")
	armDigest, armSize := manifestOf("arm64")
	index := fixtureIndex{
		SchemaVersion: 2,
		MediaType:     "application/vnd.oci.image.index.v1+json",
		Manifests: []fixtureDescriptor{
			{MediaType: "application/vnd.oci.image.manifest.v1+json", Digest: amdDigest, Size: amdSize,
				Platform: &fixturePlatform{Architecture: "amd64", OS: "linux"}},
			{MediaType: "application/vnd.oci.image.manifest.v1+json", Digest: armDigest, Size: armSize,
				Platform: &fixturePlatform{Architecture: "arm64", OS: "linux"}},
		},
	}
	indexBody, err := json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	indexDigest := writeBlob(indexBody)
	indexJSON, err := json.Marshal(fixtureIndex{
		SchemaVersion: 2,
		MediaType:     "application/vnd.oci.image.index.v1+json",
		Manifests:     []fixtureDescriptor{{MediaType: "application/vnd.oci.image.index.v1+json", Digest: indexDigest, Size: len(indexBody)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.json"), indexJSON, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "oci-layout"), []byte(`{"imageLayoutVersion":"1.0.0"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return indexDigest
}

func fixtureContents(t *testing.T, root string) (offline.Contents, offline.Expected, map[string]string) {
	t.Helper()
	layouts := map[string]string{}
	indexDigests := map[string]string{}
	for _, component := range []string{"quoin", "plinth", "lintel", "stele"} {
		dir := filepath.Join(root, "layout-"+component)
		indexDigests[component] = synthLayout(t, dir, component)
		layouts[component] = dir
	}
	manifest := []byte(`{"manifest_version":1,"release_version":"v0.1.0-dev"}` + "\n")
	chart := []byte("chart-tgz-bytes")
	compose := []byte("compose-bundle-bytes")
	helperAmd := []byte("helper-amd64-bytes")
	helperArm := []byte("helper-arm64-bytes")
	sha := func(body []byte) string {
		sum := sha256.Sum256(body)
		return hex.EncodeToString(sum[:])
	}
	contents := offline.Contents{
		Manifest:       manifest,
		KubernetesName: "quoin-0.1.0-dev.tgz",
		Kubernetes:     chart,
		ComposeName:    "quoin-compose-v0.1.0-dev.tar.gz",
		Compose:        compose,
		Helpers:        map[string][]byte{"quoin-deploy-linux-amd64": helperAmd, "quoin-deploy-linux-arm64": helperArm},
		Verification:   map[string][]byte{"subjects-inventory.json": []byte(`{"schema":"quoin-release-subjects-v1"}`)},
		ImageLayouts:   layouts,
	}
	expected := offline.Expected{
		Manifest:         manifest,
		KubernetesName:   contents.KubernetesName,
		KubernetesSHA256: sha(chart),
		ComposeName:      contents.ComposeName,
		ComposeSHA256:    sha(compose),
		HelperNames: map[string]string{
			"quoin-deploy-linux-amd64": sha(helperAmd),
			"quoin-deploy-linux-arm64": sha(helperArm),
		},
		Verification: contents.Verification,
		IndexDigests: indexDigests,
	}
	return contents, expected, indexDigests
}

func requireZstd(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("zstd"); err != nil {
		t.Skipf("zstd unavailable: %v", err)
	}
}

func TestOfflineArchiveBuildVerifyDeterminism(t *testing.T) {
	requireZstd(t)
	contents, expected, _ := fixtureContents(t, t.TempDir())
	runner := offline.ExecRunner{}
	first, err := offline.Build(t.TempDir(), contents, runner)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	second, err := offline.Build(t.TempDir(), contents, runner)
	if err != nil {
		t.Fatal(err)
	}
	if first.SHA256 != second.SHA256 {
		t.Fatal("archive build is not deterministic")
	}
	report, err := offline.Verify(first.Path, expected, runner)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	failed := 0
	for _, check := range report.Checks {
		if check.Result != "passed" {
			failed++
		}
	}
	if failed != 0 {
		t.Fatalf("%d archive checks failed", failed)
	}
	os.RemoveAll(filepath.Dir(report.Extracted))
	os.RemoveAll(strings.TrimSuffix(first.Path, ".tar.zst") + "-verify-parent")
}

func TestOfflineTamperLegs(t *testing.T) {
	requireZstd(t)
	contents, expected, _ := fixtureContents(t, t.TempDir())
	runner := offline.ExecRunner{}

	tamperedManifest := contents
	tamperedManifest.Manifest = append([]byte(nil), contents.Manifest...)
	tamperedManifest.Manifest[len(contents.Manifest)-3] ^= 0x01
	if _, err := offline.Verify(mustArchive(t, runner, tamperedManifest), expected, runner); err == nil {
		t.Fatal("manifest byte drift accepted")
	}

	sidecar := contents
	sidecar.Verification = map[string][]byte{}
	for name, body := range contents.Verification {
		sidecar.Verification[name] = body
	}
	sidecar.Verification["release-manifest.sigstore.json"] = []byte(`{"mediaType":"application/vnd.dev.sigstore.bundle+json"}`)
	if _, err := offline.Verify(mustArchive(t, runner, sidecar), expected, runner); err == nil {
		t.Fatal("embedded signature sidecar accepted")
	}

	// The archive's own signature companion planted at the root is
	// rejected; categorized evidence payloads inside verification/ are
	// legitimate internal materials and stay allowed.
	rootCompanion := contents
	rootCompanion.Verification = map[string][]byte{}
	for name, body := range contents.Verification {
		rootCompanion.Verification[name] = body
	}
	rootCompanion.Verification["../quoin-offline-v0.1.0-dev.tar.zst.sigstore.json.payload"] = []byte("{}")
	if _, err := offline.Verify(mustArchive(t, runner, rootCompanion), expected, runner); err == nil {
		t.Fatal("archive-own payload companion accepted")
	}
	evidenceCompanion := contents
	evidenceCompanion.Verification = map[string][]byte{}
	for name, body := range contents.Verification {
		evidenceCompanion.Verification[name] = body
	}
	evidenceCompanion.Verification["compose_linux_amd64.payload"] = []byte("{}")
	if report, err := offline.Verify(mustArchive(t, runner, evidenceCompanion), expected, runner); err != nil {
		t.Fatalf("categorized evidence payload rejected: %v", err)
	} else {
		os.RemoveAll(filepath.Dir(report.Extracted))
	}

	wrongIndex := expected
	wrongIndex.IndexDigests = map[string]string{}
	for component, digest := range expected.IndexDigests {
		wrongIndex.IndexDigests[component] = digest
	}
	replacement := "sha256:" + strings.Repeat("00", 32)
	wrongIndex.IndexDigests["quoin"] = replacement
	archive := mustArchive(t, runner, contents)
	if _, err := offline.Verify(archive, wrongIndex, runner); err == nil {
		t.Fatal("index digest drift accepted")
	}
}

func mustArchive(t *testing.T, runner offline.Runner, contents offline.Contents) string {
	t.Helper()
	archive, err := offline.Build(t.TempDir(), contents, runner)
	if err != nil {
		t.Fatal(err)
	}
	return archive.Path
}

// TestTicket42OfflineImport proves the real OCI toolchain roundtrip:
// import a synthetic layout into a fresh registry preserving digests,
// read the index and platform digests back, then pull a fresh layout and
// re-verify it. Docker and skopeo are required; otherwise the leg skips.
func TestTicket42OfflineImport(t *testing.T) {
	requireZstd(t)
	for _, tool := range []string{"docker", "skopeo"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s unavailable: %v", tool, err)
		}
	}
	if err := exec.Command("docker", "version", "--format", "{{.Server.Os}}").Run(); err != nil {
		t.Skipf("docker server unreachable: %v", err)
	}
	root := t.TempDir()
	_, _, indexDigests := fixtureContents(t, root)
	runner := offline.ExecRunner{}

	const registryName = "t42-offline-registry"
	removeContainer(registryName)
	output, err := runner.Run("docker", "run", "-d", "--name", registryName, "-p", "127.0.0.1:5149:5000", "docker.io/library/registry:2")
	if err != nil {
		t.Fatalf("registry: %v: %s", err, output)
	}
	t.Cleanup(func() { removeContainer(registryName) })
	awaitHTTP(t, "http://127.0.0.1:5149/v2/")

	assertions := map[string]map[string]any{}
	for _, component := range []string{"quoin", "plinth", "lintel", "stele"} {
		source := filepath.Join(root, "layout-"+component)
		target := "127.0.0.1:5149/t42/" + component
		if err := offline.ImportLayout(runner, source, target, true); err != nil {
			t.Fatalf("%s import: %v", component, err)
		}
		digest, platforms, err := offline.ReadBackIndex(runner, target+"@"+indexDigests[component], true)
		if err != nil {
			t.Fatalf("%s readback: %v", component, err)
		}
		if digest != indexDigests[component] {
			t.Fatalf("%s readback digest %s want %s", component, digest, indexDigests[component])
		}
		if platforms["linux/amd64"] == "" || platforms["linux/arm64"] == "" {
			t.Fatalf("%s platform digests missing: %v", component, platforms)
		}
		pulled := filepath.Join(root, "pulled-"+component)
		if err := offline.PullLayout(runner, target+"@"+indexDigests[component], pulled, true); err != nil {
			t.Fatalf("%s pull: %v", component, err)
			t.Fatalf("%s pull: %v", component, err)
		}
		assertions[component] = map[string]any{
			"expected": "import preserves the index digest; readback lists both platforms; pull re-verifies the layout",
			"actual":   map[string]any{"indexDigest": digest, "platforms": platforms},
		}
	}
	if evidence := os.Getenv("QUOIN_EVIDENCE_DIR"); evidence != "" {
		legs := filepath.Join(evidence, "legs")
		if err := os.MkdirAll(legs, 0o755); err != nil {
			t.Fatal(err)
		}
		body, _ := json.MarshalIndent(map[string]any{"ticket": "T42", "leg": "offline-import", "assertions": assertions}, "", "  ")
		if err := os.WriteFile(filepath.Join(legs, "offline-import-leg.json"), append(body, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func removeContainer(name string) {
	_ = exec.Command("docker", "rm", "-f", name).Run()
}

func awaitHTTP(t *testing.T, url string) {
	t.Helper()
	for attempt := 0; attempt < 60; attempt++ {
		if err := exec.Command("curl", "-sf", url).Run(); err == nil {
			return
		}
		sleepSeconds(1)
	}
	t.Fatalf("%s never became ready", url)
}

func sleepSeconds(seconds int) {
	_ = exec.Command("sleep", "1").Run()
}
