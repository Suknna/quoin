package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Suknna/quoin/internal/release/subjects"
	"gopkg.in/yaml.v3"
)

func bundleNameMap() map[string]string {
	bundles := subjects.NamesForBundles()
	mapping := map[string]string{
		"kubernetes":                    bundles.Kubernetes,
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

// buildKubernetesBundle packages the directly applicable native manifests.
// The archive is checksum-bound like Compose and is expanded by the release
// consumer before kubectl apply.
func buildKubernetesBundle(options *options, inventory *subjects.Inventory) error {
	names, err := subjects.Names(options.version)
	if err != nil {
		return err
	}
	manifestDirectory := filepath.Join(repoRoot(), "deploy", "kubernetes")
	manifests := map[string][]byte{}
	err = filepath.WalkDir(manifestDirectory, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") {
			return nil
		}
		manifest, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		pinnedManifest, err := pinKubernetesImages(manifest, inventory.Images)
		if err != nil {
			return fmt.Errorf("pin Kubernetes manifest %s: %w", path, err)
		}
		relative, err := filepath.Rel(manifestDirectory, path)
		if err != nil {
			return err
		}
		manifests[filepath.ToSlash(relative)] = pinnedManifest
		return nil
	})
	if err != nil {
		return err
	}
	if len(manifests) == 0 {
		return fmt.Errorf("no Kubernetes YAML manifests in %s", manifestDirectory)
	}
	bundle := filepath.Join(options.work, names.Kubernetes)
	if err := writeComposeBundle(bundle, manifests); err != nil {
		return err
	}
	data, err := os.ReadFile(bundle)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	inventory.Kubernetes = subjects.BlobSubject{AssetName: names.Kubernetes, SHA256: hex.EncodeToString(sum[:])}
	return nil
}

// pinKubernetesImages patches only workload container image scalar values in
// the copied authoritative YAML. It does not invent deployment configuration:
// every application image must correspond to a measured inventory index, while
// the fixed third-party Caddy image remains untouched.
func pinKubernetesImages(source []byte, images map[string]subjects.ImageSubject) ([]byte, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(source))
	var rendered bytes.Buffer
	encoder := yaml.NewEncoder(&rendered)
	encoder.SetIndent(2)
	for {
		var document yaml.Node
		err := decoder.Decode(&document)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("decode Kubernetes manifest: %w", err)
		}
		if len(document.Content) == 0 {
			continue
		}
		if err := patchWorkloadImages(&document, images); err != nil {
			return nil, err
		}
		if err := encoder.Encode(&document); err != nil {
			return nil, fmt.Errorf("encode Kubernetes manifest: %w", err)
		}
	}
	if err := encoder.Close(); err != nil {
		return nil, err
	}
	return rendered.Bytes(), nil
}

// patchWorkloadImages supports the standard PodSpec carriers shipped by the
// native deployment: Deployment, Job and Pod. Application containers are
// identified by their canonical quoin/<component> image prefix or by their
// container name; stock third-party images such as Caddy and busybox remain
// unchanged. This keeps bootstrap jobs tied to the same measured Quoin image.
func patchWorkloadImages(document *yaml.Node, images map[string]subjects.ImageSubject) error {
	root := document.Content[0]
	if len(document.Content) == 0 || root.Kind != yaml.MappingNode {
		return nil
	}
	var podSpec *yaml.Node
	switch yamlMapValue(root, "kind") {
	case "Deployment", "StatefulSet", "DaemonSet", "Job":
		podSpec = yamlMapNode(yamlMapNode(yamlMapNode(root, "spec"), "template"), "spec")
	case "Pod":
		podSpec = yamlMapNode(root, "spec")
	default:
		return nil
	}
	if podSpec == nil {
		return fmt.Errorf("Kubernetes %s has no pod spec", yamlMapValue(root, "kind"))
	}
	return patchContainerList(yamlMapNode(podSpec, "containers"), images)
}

func patchContainerList(containers *yaml.Node, images map[string]subjects.ImageSubject) error {
	if containers == nil || containers.Kind != yaml.SequenceNode {
		return nil
	}
	for _, container := range containers.Content {
		imageNode := yamlMapNode(container, "image")
		if imageNode == nil || imageNode.Kind != yaml.ScalarNode {
			return fmt.Errorf("container %q has no scalar image", yamlMapValue(container, "name"))
		}
		component := applicationImageComponent(yamlMapValue(container, "name"), imageNode.Value, images)
		if component == "" {
			continue
		}
		image := images[component]
		if image.Repository == "" || image.IndexDigest == "" {
			return fmt.Errorf("application container %q has no measured image subject", component)
		}
		imageNode.Value = image.Repository + "@" + image.IndexDigest
	}
	return nil
}

func applicationImageComponent(name, reference string, images map[string]subjects.ImageSubject) string {
	if _, ok := images[name]; ok {
		return name
	}
	for component := range images {
		if strings.HasPrefix(reference, "quoin/"+component+":") || strings.HasPrefix(reference, "quoin/"+component+"@") {
			return component
		}
	}
	return ""
}

func yamlMapNode(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for index := 0; index+1 < len(node.Content); index += 2 {
		if node.Content[index].Value == key {
			return node.Content[index+1]
		}
	}
	return nil
}

func yamlMapValue(node *yaml.Node, key string) string {
	value := yamlMapNode(node, key)
	if value == nil {
		return ""
	}
	return value.Value
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
	composeYAML, err := os.ReadFile(filepath.Join(repoRoot(), "deploy", "compose.yaml"))
	if err != nil {
		return err
	}
	composeYAML, err = pinComposeImages(composeYAML, inventory.Images)
	if err != nil {
		return err
	}
	if err := assertNoLatest(string(composeYAML)); err != nil {
		return fmt.Errorf("rendered compose.yaml: %w", err)
	}
	entries := map[string][]byte{"compose.yaml": composeYAML}
	configDirectory := filepath.Join(repoRoot(), "deploy", "config")
	configs, err := os.ReadDir(configDirectory)
	if err != nil {
		return err
	}
	for _, config := range configs {
		if config.IsDir() || !strings.HasSuffix(config.Name(), ".yaml") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(configDirectory, config.Name()))
		if err != nil {
			return err
		}
		entries["config/"+config.Name()] = body
	}
	bundlePath := filepath.Join(options.work, names.Compose)
	if err := writeComposeBundle(bundlePath, entries); err != nil {
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

// pinComposeImages projects the canonical direct Compose file into a release
// artifact by replacing only the five application service image scalars with
// immutable measured indexes. Relative config and secrets paths are preserved.
func pinComposeImages(source []byte, images map[string]subjects.ImageSubject) ([]byte, error) {
	var document yaml.Node
	if err := yaml.Unmarshal(source, &document); err != nil {
		return nil, fmt.Errorf("decode compose.yaml: %w", err)
	}
	root := document.Content[0]
	services := yamlMapNode(root, "services")
	if services == nil || services.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("compose.yaml has no services mapping")
	}
	for _, component := range subjects.Components {
		service := yamlMapNode(services, component)
		imageNode := yamlMapNode(service, "image")
		image, ok := images[component]
		if service == nil || imageNode == nil || imageNode.Kind != yaml.ScalarNode || !ok || image.Repository == "" || image.IndexDigest == "" {
			return nil, fmt.Errorf("compose service %q has no measured image subject", component)
		}
		imageNode.Value = image.Repository + "@" + image.IndexDigest
	}
	var rendered bytes.Buffer
	encoder := yaml.NewEncoder(&rendered)
	encoder.SetIndent(2)
	if err := encoder.Encode(&document); err != nil {
		return nil, err
	}
	if err := encoder.Close(); err != nil {
		return nil, err
	}
	return rendered.Bytes(), nil
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
