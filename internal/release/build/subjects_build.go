package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Suknna/quoin/deploy/compose"
	"github.com/Suknna/quoin/internal/contract"
	gen "github.com/Suknna/quoin/internal/gen/contracts"
	"github.com/Suknna/quoin/internal/release/subjects"
)

func bundleNameMap() map[string]string {
	bundles := subjects.NamesForBundles()
	mapping := map[string]string{
		"helm_oci":                      bundles.HelmOCI,
		"compose":                       bundles.Compose,
		"deployment_helper/linux/amd64": bundles.DeploymentHelper["linux/amd64"],
		"deployment_helper/linux/arm64": bundles.DeploymentHelper["linux/arm64"],
	}
	for _, component := range subjects.Components {
		mapping["image_indexes/"+component] = bundles.ImageIndexes[component]
		for _, platform := range subjects.Platforms {
			mapping["image_manifests/"+component+"/"+platform] = bundles.ImageManifests[component][platform]
		}
	}
	return mapping
}

// buildChart packages the frozen Chart with the release SemVer and pushes it
// to the configured OCI registry.
func buildChart(options *options, inventory *subjects.Inventory) error {
	chartVersion, err := subjects.ChartVersion(options.version)
	if err != nil {
		return err
	}
	packageDirectory := filepath.Join(options.work, "chart")
	if err := os.MkdirAll(packageDirectory, 0o755); err != nil {
		return err
	}
	if _, err := command(options, "chart-package", "helm", "package", "deploy/helm/quoin",
		"--version", chartVersion, "--destination", packageDirectory); err != nil {
		return err
	}
	names, err := subjects.Names(options.version)
	if err != nil {
		return err
	}
	tgzPath := filepath.Join(packageDirectory, names.ChartTgz)
	data, err := os.ReadFile(tgzPath)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	pushOutput, err := command(options, "chart-push", "helm", "push", tgzPath, "oci://"+options.chartOCI)
	if err != nil {
		return err
	}
	digest := ""
	for _, line := range strings.Split(pushOutput, "\n") {
		if strings.HasPrefix(line, "Digest: ") {
			digest = strings.TrimSpace(strings.TrimPrefix(line, "Digest: "))
		}
	}
	if digest == "" {
		return fmt.Errorf("chart push reported no digest")
	}
	inventory.Chart = subjects.ChartSubject{
		OCIRepository: options.chartOCI + "/quoin",
		OCIDigest:     digest,
		TgzAssetName:  names.ChartTgz,
		TgzSHA256:     hex.EncodeToString(sum[:]),
	}
	return nil
}

// buildComposeBundle assembles the digest-pinned Compose bundle: the
// canonical compose projection with the measured image digests, the minimal
// input template, the deployment-config schema and the quoin-deploy wizard
// entry (OPS-RELEASE-003). The bundle never contains a release manifest.
func buildComposeBundle(options *options, inventory *subjects.Inventory) error {
	names, err := subjects.Names(options.version)
	if err != nil {
		return err
	}
	renderRoot := filepath.Join(options.work, "compose-render")
	if err := os.MkdirAll(renderRoot, 0o755); err != nil {
		return err
	}
	images := map[string]string{}
	for _, component := range subjects.Components {
		image := inventory.Images[component]
		images[component] = image.Repository + "@" + image.IndexDigest
	}
	input := contract.ComposeInstall{
		Document:             "compose-install",
		PublicOrigin:         "https://quoin.example.com",
		PublishMode:          "loopback",
		QuoinPublicHostPort:  8080,
		SteleWebhookHostPort: 8081,
		SecretDirectory:      "/var/lib/quoin/secrets",
		LintelBrowserSlots:   1,
		LintelShmSizeBytes:   1 << 30,
	}
	projection, err := compose.RenderWithOptions(input, filepath.Join(renderRoot, "state"), compose.Options{Images: images})
	if err != nil {
		return fmt.Errorf("render digest-pinned compose projection: %w", err)
	}
	composeYAML, err := os.ReadFile(projection.ComposeFile)
	if err != nil {
		return err
	}
	if err := assertNoLatest(string(composeYAML)); err != nil {
		return fmt.Errorf("rendered compose.yaml: %w", err)
	}
	example, err := os.ReadFile(filepath.Join(repoRoot(), "deploy", "examples", "compose-install.yaml"))
	if err != nil {
		return err
	}
	entry := []byte(`#!/bin/sh
# quoin-deploy wizard entry (OPS-RELEASE-003/OPS-HELPER-001): the operator
# downloads the platform helper asset (quoin-deploy-linux-amd64 or
# quoin-deploy-linux-arm64) published with this release, names it
# quoin-deploy beside this bundle and invokes:
#   ./quoin-deploy compose install --config install-minimal.yaml
set -eu
here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
if [ ! -x "$here/quoin-deploy" ]; then
	echo "quoin-deploy helper not found next to this bundle." >&2
	echo "Download the platform asset published with this release and place it as $here/quoin-deploy." >&2
	exit 2
fi
exec "$here/quoin-deploy" "$@"
`)
	bundlePath := filepath.Join(options.work, names.Compose)
	if err := writeComposeBundle(bundlePath, map[string][]byte{
		"compose.yaml":                         composeYAML,
		"install-minimal.yaml":                 example,
		"schema/deployment-config.schema.json": gen.DeploymentConfigSchema,
		"quoin-deploy":                         entry,
	}); err != nil {
		return err
	}
	data, err := os.ReadFile(bundlePath)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	inventory.Compose = subjects.BlobSubject{AssetName: names.Compose, SHA256: hex.EncodeToString(sum[:])}
	return nil
}

// writeComposeBundle assembles the deterministic tar.gz (sorted entries,
// fixed mtimes) so the same subjects always produce the same bytes.
func writeComposeBundle(path string, entries map[string][]byte) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()
	writer := gzip.NewWriter(file)
	tarWriter := tar.NewWriter(writer)
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		content := entries[name]
		mode := int64(0o644)
		if strings.HasPrefix(name, "quoin-deploy") && !strings.Contains(name, "/") {
			mode = 0o755
		}
		if err := tarWriter.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(content)), ModTime: time.Unix(0, 0)}); err != nil {
			return err
		}
		if _, err := tarWriter.Write(content); err != nil {
			return err
		}
	}
	if err := tarWriter.Close(); err != nil {
		return err
	}
	return writer.Close()
}

// buildHelpers cross-compiles the static quoin-deploy helpers for both
// architectures regardless of the build host (OPS-HELPER-001).
func buildHelpers(options *options, inventory *subjects.Inventory) error {
	names, err := subjects.Names(options.version)
	if err != nil {
		return err
	}
	for _, platform := range []struct{ arch, goarch string }{{"amd64", "amd64"}, {"arm64", "arm64"}} {
		output := filepath.Join(options.work, names.Helper["linux/"+platform.arch])
		environment := append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+platform.goarch)
		started := time.Now()
		process := exec.Command("go", "build", "-trimpath", "-ldflags=-s -w -buildid=", "-o", output, "./cmd/quoin-deploy")
		process.Dir = repoRoot()
		process.Env = environment
		var buildOutput bytes.Buffer
		process.Stdout, process.Stderr = &buildOutput, &buildOutput
		if err := process.Run(); err != nil {
			os.WriteFile(filepath.Join(options.logs, "helper-"+platform.arch+".log"), buildOutput.Bytes(), 0o644)
			return fmt.Errorf("helper %s: %w", platform.arch, err)
		}
		data, err := os.ReadFile(output)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		os.WriteFile(filepath.Join(options.logs, "helper-"+platform.arch+".json"),
			[]byte(fmt.Sprintf(`{"asset":%q,"sha256":%q,"bytes":%d,"seconds":%.1f}`, names.Helper["linux/"+platform.arch], hex.EncodeToString(sum[:]), len(data), time.Since(started).Seconds())), 0o644)
		inventory.Helpers["linux/"+platform.arch] = subjects.BlobSubject{
			AssetName: names.Helper["linux/"+platform.arch],
			SHA256:    hex.EncodeToString(sum[:]),
		}
	}
	return nil
}

var latestPattern = regexp.MustCompile(`(^|[/:.\s"])latest($|[\s"':])`)

// assertNoLatest rejects any mutable-tag reference in rendered outputs
// (OPS-RELEASE-002).
func assertNoLatest(text string) error {
	if match := latestPattern.FindString(text); match != "" {
		return fmt.Errorf("mutable tag reference %q", strings.TrimSpace(match))
	}
	return nil
}
