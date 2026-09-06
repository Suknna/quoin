// Package offline owns the release offline archive (OPS-OFFLINE-001/002):
// one deterministic .tar.zst per release carrying the four component OCI
// image layouts, the Chart, the Compose bundle, the final Release manifest
// and the internal verification materials. The archive's own signature and
// Sigstore bundle are external sidecars and never enter the archive, so
// the archive never contains the signature of itself.
package offline

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Runner is the external-tool seam: zstd compression and the skopeo OCI
// tooling run through it, keeping the archive logic testable offline.
type Runner interface {
	Run(name string, argv ...string) (string, error)
}

// ExecRunner runs external commands for real.
type ExecRunner struct{}

// Run executes one command and returns its combined output.
func (ExecRunner) Run(name string, argv ...string) (string, error) {
	output, err := commandOutput(name, argv...)
	return output, err
}

// Contents is everything the offline archive carries. ImageLayouts maps a
// component to a directory holding a complete OCI layout of its merged
// multi-platform index; Verification maps file names to the signed
// evidence materials (subject inventory and categorized bundles).
type Contents struct {
	Manifest     []byte
	ChartName    string
	Chart        []byte
	ComposeName  string
	Compose      []byte
	Helpers      map[string][]byte // asset name -> bytes (both architectures)
	Verification map[string][]byte
	ImageLayouts map[string]string // component -> OCI layout directory
}

// Archive is the built offline archive and its measured digest.
type Archive struct {
	Path   string
	SHA256 string
	Bytes  int64
}

// Build packs the deterministic tar and compresses it with zstd. The tar
// entries are sorted, carry fixed metadata and regular files only, so the
// same contents under the same zstd version always produce the same
// archive bytes; the compressor version is part of the frozen release
// toolchain, and the archive integrity itself is bound by its external
// signature, not by cross-version byte reproducibility.
func Build(dir string, contents Contents, runner Runner) (*Archive, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	tarPath := filepath.Join(dir, "offline.tar")
	if err := writeTar(tarPath, contents); err != nil {
		return nil, err
	}
	defer os.Remove(tarPath)
	archivePath := filepath.Join(dir, "quoin-offline.tar.zst")
	if output, err := runner.Run("zstd", "-q", "-T0", "-19", "-f", "-o", archivePath, tarPath); err != nil {
		return nil, fmt.Errorf("zstd: %v: %s", err, output)
	}
	data, err := os.ReadFile(archivePath)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	return &Archive{Path: archivePath, SHA256: hex.EncodeToString(sum[:]), Bytes: int64(len(data))}, nil
}

// writeTar streams the sorted deterministic tar.
func writeTar(path string, contents Contents) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()
	writer := tar.NewWriter(file)

	type entry struct {
		name    string
		mode    int64
		body    []byte
		fromDir string // when set, a regular file copied from disk
	}
	entries := []entry{
		{name: "release-manifest.json", mode: 0o644, body: contents.Manifest},
		{name: "assets/chart/" + contents.ChartName, mode: 0o644, body: contents.Chart},
		{name: "assets/compose/" + contents.ComposeName, mode: 0o644, body: contents.Compose},
	}
	helperNames := make([]string, 0, len(contents.Helpers))
	for name := range contents.Helpers {
		helperNames = append(helperNames, name)
	}
	sort.Strings(helperNames)
	for _, name := range helperNames {
		entries = append(entries, entry{name: "assets/deployment_helper/" + name, mode: 0o755, body: contents.Helpers[name]})
	}
	verificationNames := make([]string, 0, len(contents.Verification))
	for name := range contents.Verification {
		verificationNames = append(verificationNames, name)
	}
	sort.Strings(verificationNames)
	for _, name := range verificationNames {
		entries = append(entries, entry{name: "verification/" + name, mode: 0o644, body: contents.Verification[name]})
	}
	components := make([]string, 0, len(contents.ImageLayouts))
	for component := range contents.ImageLayouts {
		components = append(components, component)
	}
	sort.Strings(components)
	for _, component := range components {
		root := contents.ImageLayouts[component]
		err := filepath.WalkDir(root, func(current string, item os.DirEntry, walkErr error) error {
			if walkErr != nil || item.IsDir() {
				return walkErr
			}
			relative, err := filepath.Rel(root, current)
			if err != nil {
				return err
			}
			entries = append(entries, entry{
				name:    "images/" + component + "/" + filepath.ToSlash(relative),
				mode:    0o644,
				fromDir: current,
			})
			return nil
		})
		if err != nil {
			return fmt.Errorf("layout %s: %w", component, err)
		}
	}
	sort.Slice(entries, func(left, right int) bool { return entries[left].name < entries[right].name })

	for _, item := range entries {
		var body []byte
		if item.fromDir != "" {
			body, err = os.ReadFile(item.fromDir)
			if err != nil {
				return err
			}
		} else {
			body = item.body
		}
		if err := writer.WriteHeader(&tar.Header{
			Name: item.name, Mode: item.mode, Size: int64(len(body)),
			ModTime: timeEpoch, Format: tar.FormatPAX,
		}); err != nil {
			return err
		}
		if _, err := writer.Write(body); err != nil {
			return err
		}
	}
	if err := writer.Close(); err != nil {
		return err
	}
	return file.Sync()
}

// Expected is the verification-side projection of what a published
// offline archive must contain: the exact manifest bytes, the asset bytes
// the manifest pins, the verification materials and the component index
// digests.
type Expected struct {
	Manifest      []byte
	ChartName     string
	ChartSHA256   string
	ComposeName   string
	ComposeSHA256 string
	HelperNames   map[string]string // asset name -> sha256
	Verification  map[string][]byte // name -> exact bytes
	IndexDigests  map[string]string // component -> expected OCI index digest
}

// Check is one archive verification result with a stable code.
type Check struct {
	Code    string `json:"code"`
	Subject string `json:"subject"`
	Result  string `json:"result"`
	Error   string `json:"error,omitempty"`
}

// Report is the structured archive verification result.
type Report struct {
	Checks    []Check `json:"checks"`
	Extracted string  `json:"extractedDir"`
}

// Verify unpacks the archive with zstd and proves every inner fact: the
// manifest bytes are bit-equal to the published manifest, every asset
// digest equals its pinned SHA-256, no signature sidecar hides inside,
// and every component OCI layout carries the pinned index digest with
// content-addressed blobs.
func Verify(archivePath string, expected Expected, runner Runner) (*Report, error) {
	root, err := os.MkdirTemp(filepath.Dir(archivePath), "offline-verify-")
	if err != nil {
		return nil, err
	}
	tarPath := filepath.Join(root, "offline.tar")
	if output, err := runner.Run("zstd", "-q", "-d", "-f", "-o", tarPath, archivePath); err != nil {
		return nil, fmt.Errorf("zstd decompress: %v: %s", err, output)
	}
	if err := untar(tarPath, filepath.Join(root, "x")); err != nil {
		return nil, err
	}
	report := &Report{Extracted: filepath.Join(root, "x")}
	pass := func(code, subject string) {
		report.Checks = append(report.Checks, Check{Code: code, Subject: subject, Result: "passed"})
	}
	fail := func(code, subject string, err error) error {
		report.Checks = append(report.Checks, Check{Code: code, Subject: subject, Result: "failed", Error: err.Error()})
		return err
	}

	// The archive's own signature stays external (OPS-OFFLINE-001): no
	// Sigstore bundle may appear anywhere inside, and payload companions
	// are confined to the verification materials they belong to.
	var sidecars []string
	verificationRoot := filepath.Join(report.Extracted, "verification") + string(os.PathSeparator)
	filepath.WalkDir(report.Extracted, func(current string, item os.DirEntry, walkErr error) error {
		if walkErr != nil || item.IsDir() {
			return nil
		}
		if strings.HasSuffix(item.Name(), ".sigstore.json") {
			sidecars = append(sidecars, current)
			return nil
		}
		if strings.HasSuffix(item.Name(), ".payload") && !strings.HasPrefix(current, verificationRoot) {
			sidecars = append(sidecars, current)
		}
		return nil
	})
	if len(sidecars) > 0 {
		return report, fail("archive.no-embedded-signature", strings.Join(sidecars, ","), fmt.Errorf("signature sidecars inside the archive"))
	}
	pass("archive.no-embedded-signature", "external sidecars only")

	inner, err := os.ReadFile(filepath.Join(report.Extracted, "release-manifest.json"))
	if err != nil {
		return report, fail("archive.manifest-bytes", "release-manifest.json", err)
	}
	if !bytes.Equal(inner, expected.Manifest) {
		return report, fail("archive.manifest-bytes", "release-manifest.json", fmt.Errorf("inner manifest bytes differ from the published manifest"))
	}
	pass("archive.manifest-bytes", "release-manifest.json")

	for asset, digest := range map[string]string{
		"assets/chart/" + expected.ChartName:     expected.ChartSHA256,
		"assets/compose/" + expected.ComposeName: expected.ComposeSHA256,
	} {
		body, err := os.ReadFile(filepath.Join(report.Extracted, filepath.FromSlash(asset)))
		if err != nil {
			return report, fail("archive.asset-digest", asset, err)
		}
		sum := sha256.Sum256(body)
		if hex.EncodeToString(sum[:]) != digest {
			return report, fail("archive.asset-digest", asset, fmt.Errorf("sha256 %s want %s", hex.EncodeToString(sum[:]), digest))
		}
		pass("archive.asset-digest", asset)
	}
	for name, digest := range expected.HelperNames {
		asset := "assets/deployment_helper/" + name
		body, err := os.ReadFile(filepath.Join(report.Extracted, asset))
		if err != nil {
			return report, fail("archive.asset-digest", asset, err)
		}
		sum := sha256.Sum256(body)
		if hex.EncodeToString(sum[:]) != digest {
			return report, fail("archive.asset-digest", asset, fmt.Errorf("sha256 %s want %s", hex.EncodeToString(sum[:]), digest))
		}
		pass("archive.asset-digest", asset)
	}

	for name, want := range expected.Verification {
		asset := "verification/" + name
		body, err := os.ReadFile(filepath.Join(report.Extracted, asset))
		if err != nil {
			return report, fail("archive.verification-bytes", asset, err)
		}
		if !bytes.Equal(body, want) {
			return report, fail("archive.verification-bytes", asset, fmt.Errorf("verification material bytes differ"))
		}
		pass("archive.verification-bytes", asset)
	}

	components := make([]string, 0, len(expected.IndexDigests))
	for component := range expected.IndexDigests {
		components = append(components, component)
	}
	sort.Strings(components)
	for _, component := range components {
		if err := VerifyLayout(filepath.Join(report.Extracted, "images", component), expected.IndexDigests[component]); err != nil {
			return report, fail("archive.image-layout", "images/"+component, err)
		}
		pass("archive.image-layout", "images/"+component)
	}
	return report, nil
}

// untar extracts a plain tar into dest (regular files only).
func untar(tarPath, dest string) error {
	file, err := os.Open(tarPath)
	if err != nil {
		return err
	}
	defer file.Close()
	reader := tar.NewReader(file)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if header.Typeflag != tar.TypeReg {
			return fmt.Errorf("archive entry %q is not a regular file", header.Name)
		}
		target := filepath.Join(dest, filepath.FromSlash(header.Name))
		if !strings.HasPrefix(target, filepath.Clean(dest)+string(os.PathSeparator)) {
			return fmt.Errorf("archive entry %q escapes the extraction root", header.Name)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		body, err := io.ReadAll(reader)
		if err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if header.Mode&0o111 != 0 {
			mode = 0o755
		}
		if err := os.WriteFile(target, body, mode); err != nil {
			return err
		}
	}
}

// indexDocumentShape is the minimal OCI index projection for layout checks.
type indexDocumentShape struct {
	Manifests []struct {
		Digest   string `json:"digest"`
		Platform *struct {
			OS           string `json:"os"`
			Architecture string `json:"architecture"`
		} `json:"platform"`
	} `json:"manifests"`
}

// VerifyLayout proves an extracted OCI layout is the pinned index: the
// layout's single top descriptor equals the expected digest and every
// stored blob is content-addressed.
func VerifyLayout(layoutDir, expectedIndexDigest string) error {
	layoutFile := filepath.Join(layoutDir, "oci-layout")
	if _, err := os.Stat(layoutFile); err != nil {
		return fmt.Errorf("oci-layout marker: %w", err)
	}
	body, err := os.ReadFile(filepath.Join(layoutDir, "index.json"))
	if err != nil {
		return fmt.Errorf("index.json: %w", err)
	}
	var shape indexDocumentShape
	if err := json.Unmarshal(body, &shape); err != nil {
		return fmt.Errorf("index.json: %w", err)
	}
	if len(shape.Manifests) != 1 {
		return fmt.Errorf("layout index carries %d descriptors, want exactly the pinned one", len(shape.Manifests))
	}
	if shape.Manifests[0].Digest != expectedIndexDigest {
		return fmt.Errorf("layout descriptor %s is not the pinned index %s", shape.Manifests[0].Digest, expectedIndexDigest)
	}
	blobsDir := filepath.Join(layoutDir, "blobs", "sha256")
	entries, err := os.ReadDir(blobsDir)
	if err != nil {
		return fmt.Errorf("blobs: %w", err)
	}
	for _, item := range entries {
		if item.IsDir() {
			continue
		}
		blob, err := os.ReadFile(filepath.Join(blobsDir, item.Name()))
		if err != nil {
			return err
		}
		sum := sha256.Sum256(blob)
		if hex.EncodeToString(sum[:]) != item.Name() {
			return fmt.Errorf("blob %s fails content addressing", item.Name())
		}
	}
	return nil
}

// PlatformDigestsOf parses the platform manifests out of a raw OCI index
// body, mapping linux platform identifiers to their manifest digests.
// Attestation manifests (unknown/unknown) are provenance, not platforms,
// and stay out of the map.
func PlatformDigestsOf(rawIndex []byte) (map[string]string, error) {
	var shape indexDocumentShape
	if err := json.Unmarshal(rawIndex, &shape); err != nil {
		return nil, err
	}
	platforms := map[string]string{}
	for _, manifest := range shape.Manifests {
		if manifest.Platform == nil {
			continue
		}
		platform := manifest.Platform.OS + "/" + manifest.Platform.Architecture
		if manifest.Platform.OS == "unknown" {
			continue
		}
		platforms[platform] = manifest.Digest
	}
	return platforms, nil
}
