package suites

// Release-deployment preparation shared by the local acceptance and the
// CI qualification driver: the strict install config and the
// helper-shaped release manifest every suite consumes.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// InstallPorts is the loopback-published port pair of one qualification
// deployment.
type InstallPorts struct {
	Quoin int
	Stele int
}

// WriteInstallConfig renders the strict compose-install input (the
// frozen T30 shape) with its own secret directory and browser slots.
func WriteInstallConfig(workRoot string, ports InstallPorts) (string, error) {
	secrets := filepath.Join(workRoot, "secrets")
	if err := os.MkdirAll(secrets, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(workRoot, "install.yaml")
	content := fmt.Sprintf("document: compose-install\npublicOrigin: https://quoin.example.com\npublishMode: loopback\nquoinPublicHostPort: %d\nsteleWebhookHostPort: %d\nsecretDirectory: %s\nlintelBrowserSlots: 2\nlintelShmSizeBytes: 1073741824\n",
		ports.Quoin, ports.Stele, secrets)
	return path, os.WriteFile(path, []byte(content), 0o600)
}

// SubjectImage is one digest-pinned release subject.
type SubjectImage struct {
	Repository string
	Index      string
	Platforms  map[string]string
}

// WriteReleaseManifest projects the built subjects into the deployment
// helper's release-manifest shape. The schema freezes the dual-platform
// form; on a single-architecture qualification build the unexecuted
// foreign platform carries this cell's digest and the native-architecture
// evidence records the delegation (the local manifest is a qualification
// input, never a published release subject).
func WriteReleaseManifest(workRoot, releaseVersion, sourceCommit string, images map[string]SubjectImage) (string, error) {
	manifest := map[string]any{
		"manifest_version": 1,
		"release_version":  releaseVersion,
		"source_commit":    sourceCommit,
		"generated_at":     time.Now().UTC().Format(time.RFC3339),
		"browser": map[string]any{"playwright_version": "release-locked", "chromium_revision": "release-locked",
			"artifacts": map[string]any{
				"linux/amd64": map[string]any{"sha256": strings.Repeat("60", 32), "bytes": 1},
				"linux/arm64": map[string]any{"sha256": strings.Repeat("61", 32), "bytes": 1},
			}},
		"helm":    map[string]any{"oci_repository": "t40/charts", "oci_digest": "sha256:" + strings.Repeat("10", 32), "tgz_asset_name": "quoin-0.1.0-t40.tgz", "tgz_sha256": strings.Repeat("10", 32)},
		"compose": map[string]any{"asset_name": "quoin-compose-" + releaseVersion + "-t40.tar.gz", "bundle_sha256": strings.Repeat("20", 32)},
		"deployment_helper": map[string]any{"artifacts": map[string]any{
			"linux/amd64": map[string]any{"asset_name": "quoin-deploy-linux-amd64", "sha256": strings.Repeat("30", 32)},
			"linux/arm64": map[string]any{"asset_name": "quoin-deploy-linux-arm64", "sha256": strings.Repeat("31", 32)},
		}},
		"offline": map[string]any{"asset_name": "quoin-offline-" + releaseVersion + "-t40.tar.zst"},
		"sigstore_bundles": map[string]any{
			"image_indexes": map[string]any{"quoin": "q.sigstore.json", "plinth": "p.sigstore.json", "lintel": "l.sigstore.json", "stele": "s.sigstore.json"},
			"image_manifests": map[string]any{
				"quoin":  map[string]any{"linux/amd64": "qa.sigstore.json", "linux/arm64": "qb.sigstore.json"},
				"plinth": map[string]any{"linux/amd64": "pa.sigstore.json", "linux/arm64": "pb.sigstore.json"},
				"lintel": map[string]any{"linux/amd64": "la.sigstore.json", "linux/arm64": "lb.sigstore.json"},
				"stele":  map[string]any{"linux/amd64": "sa.sigstore.json", "linux/arm64": "sb.sigstore.json"},
			},
			"helm_oci": "h.sigstore.json", "release_manifest": "m.sigstore.json", "compose": "c.sigstore.json",
			"deployment_helper": map[string]any{"linux/amd64": "da.sigstore.json", "linux/arm64": "db.sigstore.json"},
			"offline":           "o.sigstore.json",
		},
		"contracts": map[string]any{
			"deployment_config_version": 1, "database_schema_version": "1", "runtime_proto_version": "1",
			"worker_protocol_version": "1", "metrics_contract_version": 1, "plinth_worker_tools_version": 1,
			"release_inputs_version": 1, "readiness_response_version": 1, "journey_catalog_version": "1",
		},
		"validation": map[string]any{},
	}
	projected := map[string]any{}
	for component, image := range images {
		platforms := map[string]any{}
		for platform, digest := range image.Platforms {
			platforms[platform] = digest
		}
		if _, present := platforms["linux/amd64"]; !present {
			if arm, ok := image.Platforms["linux/arm64"]; ok {
				platforms["linux/amd64"] = arm
			}
		}
		if _, present := platforms["linux/arm64"]; !present {
			if amd, ok := image.Platforms["linux/amd64"]; ok {
				platforms["linux/arm64"] = amd
			}
		}
		projected[component] = map[string]any{
			"repository": image.Repository, "index_digest": image.Index, "platforms": platforms,
		}
	}
	manifest["images"] = projected
	for _, field := range []string{"deployment_config", "database_schema", "runtime_proto", "worker_protocol", "metrics", "plinth_worker_tools", "release_inputs", "readiness_response", "journey_catalog"} {
		manifest["contracts"].(map[string]any)[field+"_sha256"] = strings.Repeat("40", 32)
	}
	for _, cell := range []string{"contracts", "compose_linux_amd64", "compose_linux_arm64", "kubernetes_linux_amd64", "kubernetes_linux_arm64", "offline_import", "supply_chain"} {
		manifest["validation"].(map[string]any)[cell] = map[string]any{"status": "passed", "evidence_sha256": strings.Repeat("50", 32)}
	}
	body, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return "", err
	}
	path := filepath.Join(workRoot, "release-manifest.json")
	return path, os.WriteFile(path, body, 0o600)
}
