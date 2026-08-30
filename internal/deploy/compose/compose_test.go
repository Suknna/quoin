package compose_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	deploycompose "github.com/Suknna/quoin/internal/deploy/compose"
	deployconfig "github.com/Suknna/quoin/internal/deploy/config"
	sharedops "github.com/Suknna/quoin/internal/ops"
)

func TestJudgeMetricsEnforcesClosedCatalog(t *testing.T) {
	// The authoritative exposition comes from the real shared ops server, so
	// the judge is proven against exactly what components export.
	server, err := sharedops.New("quoin", "127.0.0.1:0", sharedops.Ready)
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	families := recorder.Body.String()
	if failure := deploycompose.JudgeMetricsForTest("quoin", families); failure != nil {
		t.Fatalf("full catalog exposition must pass: %v", failure)
	}
	// Truncate after the first labeled family: the rest of the catalog is
	// missing, which must fail deterministically.
	cut := strings.Index(families, "quoin_maintenance{")
	partial := families[:cut] + `quoin_maintenance{maintenance_reason="upgrade"} 0` + "\n"
	if failure := deploycompose.JudgeMetricsForTest("quoin", partial); failure == nil || failure.Code != "metrics_family_missing" {
		t.Fatalf("partial exposition must fail with missing families, got %+v", failure)
	}
	outside := families + "quoin_rogue_family 1\n"
	if failure := deploycompose.JudgeMetricsForTest("quoin", outside); failure == nil || failure.Code != "metrics_family_outside_catalog" {
		t.Fatalf("family outside the catalog must fail, got %+v", failure)
	}
	openLabel := strings.Replace(families, `maintenance_reason="root_key_rebind"`, `maintenance_reason="rogue"`, 1)
	if failure := deploycompose.JudgeMetricsForTest("quoin", openLabel); failure == nil || failure.Code != "metrics_label_value_outside_catalog" {
		t.Fatalf("label value outside the closed set must fail, got %+v", failure)
	}
	missingLabel := strings.Replace(families, `maintenance_reason="root_key_rebind"} 0`, `} 0`, 1)
	if failure := deploycompose.JudgeMetricsForTest("quoin", missingLabel); failure == nil || failure.Code != "metrics_label_value_missing" {
		t.Fatalf("missing closed label value must fail, got %+v", failure)
	}
}

func TestJudgeLogsEnforcesFrozenJSONShape(t *testing.T) {
	good := `{"ts":"2026-08-30T00:00:00Z","level":"info","component":"quoin","release":"v0.1.0-dev","code":"x","msg":"y"}`
	if failure := deploycompose.JudgeLogsForTest("quoin", good, "v0.1.0-dev"); failure != nil {
		t.Fatalf("valid JSON line must pass: %v", failure)
	}
	if failure := deploycompose.JudgeLogsForTest("quoin", "not json", "v0.1.0-dev"); failure == nil || failure.Code != "logs_not_json" {
		t.Fatalf("non-JSON line must fail, got %+v", failure)
	}
	missing := `{"ts":"2026-08-30T00:00:00Z","level":"info","component":"quoin","release":"v0.1.0-dev","msg":"y"}`
	if failure := deploycompose.JudgeLogsForTest("quoin", missing, "v0.1.0-dev"); failure == nil || failure.Code != "logs_missing_field" {
		t.Fatalf("missing frozen field must fail, got %+v", failure)
	}
	wrongRelease := strings.Replace(good, "v0.1.0-dev", "v9.9.9", 1)
	if failure := deploycompose.JudgeLogsForTest("quoin", wrongRelease, "v0.1.0-dev"); failure == nil || failure.Code != "logs_identity_mismatch" {
		t.Fatalf("release identity mismatch must fail, got %+v", failure)
	}
}

func TestReleaseManifestValidation(t *testing.T) {
	valid := map[string]any{
		"manifest_version": 1,
		"release_version":  "v1.2.3",
		"source_commit":    "0123456789abcdef0123456789abcdef01234567",
		"generated_at":     "2026-08-30T00:00:00Z",
		"images": map[string]any{
			"quoin": image("127.0.0.1:5000/quoin"), "plinth": image("127.0.0.1:5000/plinth"),
			"lintel": image("127.0.0.1:5000/lintel"), "stele": image("127.0.0.1:5000/stele"),
		},
		"browser":           map[string]any{"playwright_version": "1.62.1", "chromium_revision": "1234", "artifacts": map[string]any{"linux/amd64": artifact(), "linux/arm64": artifact()}},
		"helm":              map[string]any{"oci_repository": "ghcr.io/suknna/quoin-chart", "oci_digest": digest("a"), "tgz_asset_name": "quoin-1.2.3.tgz", "tgz_sha256": bare()},
		"compose":           map[string]any{"asset_name": "quoin-compose-v1.2.3.tar.gz", "bundle_sha256": bare()},
		"deployment_helper": map[string]any{"artifacts": map[string]any{"linux/amd64": blob("quoin-deploy-linux-amd64"), "linux/arm64": blob("quoin-deploy-linux-arm64")}},
		"offline":           map[string]any{"asset_name": "quoin-offline-v1.2.3.tar.zst"},
		"sigstore_bundles": map[string]any{
			"image_indexes": map[string]any{"quoin": bundle(), "plinth": bundle(), "lintel": bundle(), "stele": bundle()},
			"image_manifests": map[string]any{
				"quoin":  map[string]any{"linux/amd64": bundle(), "linux/arm64": bundle()},
				"plinth": map[string]any{"linux/amd64": bundle(), "linux/arm64": bundle()},
				"lintel": map[string]any{"linux/amd64": bundle(), "linux/arm64": bundle()},
				"stele":  map[string]any{"linux/amd64": bundle(), "linux/arm64": bundle()},
			},
			"helm_oci": bundle(), "release_manifest": bundle(), "compose": bundle(),
			"deployment_helper": map[string]any{"linux/amd64": bundle(), "linux/arm64": bundle()},
			"offline":           bundle(),
		},
		"contracts":  contractsFixture(),
		"validation": validationFixture(),
	}
	path := filepath.Join(t.TempDir(), "release-manifest.json")
	writeJSON(t, path, valid)
	manifest, err := deployconfig.LoadReleaseManifest(path)
	if err != nil {
		t.Fatalf("valid manifest rejected: %v", err)
	}
	reference, err := manifest.ImageReference("quoin")
	if err != nil || reference != "127.0.0.1:5000/quoin@"+digest("q") {
		t.Fatalf("image reference wrong: %q %v", reference, err)
	}
	if platform, err := manifest.PlatformDigest("quoin", "linux/amd64"); err != nil || platform != digest("q") {
		t.Fatalf("platform digest wrong: %q %v", platform, err)
	}

	tampered := deepCopy(t, valid)
	tampered["images"].(map[string]any)["quoin"].(map[string]any)["index_digest"] = "sha256:zz"
	writeJSON(t, path, tampered)
	if _, err := deployconfig.LoadReleaseManifest(path); err == nil {
		t.Fatal("malformed digest must fail schema validation")
	}

	unknown := deepCopy(t, valid)
	unknown["unexpected"] = true
	writeJSON(t, path, unknown)
	if _, err := deployconfig.LoadReleaseManifest(path); err == nil {
		t.Fatal("unknown top-level field must fail schema validation")
	}

	missingHelper := deepCopy(t, valid)
	delete(missingHelper["deployment_helper"].(map[string]any)["artifacts"].(map[string]any), "linux/arm64")
	writeJSON(t, path, missingHelper)
	if _, err := deployconfig.LoadReleaseManifest(path); err == nil {
		t.Fatal("missing helper platform must fail schema validation")
	}
}

func TestInstallStateRoundtripAndAtomicWrite(t *testing.T) {
	directory := t.TempDir()
	if state, err := deployconfig.LoadInstallState(directory); err != nil || state != nil {
		t.Fatalf("missing state must be nil, got %v %v", state, err)
	}
	state := &deployconfig.InstallState{Key: deployconfig.InstallStateKey{ReleaseVersion: "v1.2.3", Backend: "compose", ConfigDigest: digest("d"), Command: "install"}, StagesDone: []string{"preflight", "secret-bootstrap"}}
	if err := state.WriteInstallState(directory); err != nil {
		t.Fatal(err)
	}
	loaded, err := deployconfig.LoadInstallState(directory)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Key != state.Key || len(loaded.StagesDone) != 2 || loaded.FinishedAt != "" {
		t.Fatalf("roundtrip mismatch: %+v", loaded)
	}
	data, err := os.ReadFile(filepath.Join(directory, "install-state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "install-state.json.tmp") {
		t.Fatal("atomic write must not leave temporary names in the document")
	}
}

func image(repository string) map[string]any {
	return map[string]any{"repository": repository, "index_digest": digest("q"), "platforms": map[string]any{"linux/amd64": digest("q"), "linux/arm64": digest("q")}}
}

func digest(seed string) string {
	_ = seed
	return "sha256:" + strings.Repeat("ab", 32)
}

func bare() string { return strings.Repeat("ab", 32) }

func artifact() map[string]any {
	return map[string]any{"sha256": strings.TrimPrefix(digest("e"), "sha256:"), "bytes": 1}
}
func blob(name string) map[string]any {
	return map[string]any{"asset_name": name, "sha256": strings.TrimPrefix(digest("f"), "sha256:")}
}
func bundle() string { return "x.sigstore.json" }

func contractsFixture() map[string]any {
	value := map[string]any{}
	for _, field := range []string{"deployment_config", "database_schema", "runtime_proto", "worker_protocol", "metrics_contract", "plinth_worker_tools", "release_inputs", "readiness_response", "journey_catalog"} {
		value[field+"_version"] = 1
	}
	// The frozen schema names the metrics hash metrics_sha256, not
	// metrics_contract_sha256.
	value["metrics_sha256"] = strings.Repeat("ab", 32)
	for _, field := range []string{"deployment_config", "database_schema", "runtime_proto", "worker_protocol", "plinth_worker_tools", "release_inputs", "readiness_response", "journey_catalog"} {
		value[field+"_sha256"] = strings.Repeat("ab", 32)
	}
	value["database_schema_version"] = "1"
	value["runtime_proto_version"] = "1"
	value["worker_protocol_version"] = "1"
	value["journey_catalog_version"] = "1"
	return value
}

func validationFixture() map[string]any {
	value := map[string]any{}
	for _, cell := range []string{"contracts", "compose_linux_amd64", "compose_linux_arm64", "kubernetes_linux_amd64", "kubernetes_linux_arm64", "offline_import", "supply_chain"} {
		value[cell] = map[string]any{"status": "passed", "evidence_sha256": strings.Repeat("cd", 32)}
	}
	return value
}

func writeJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func deepCopy(t *testing.T, value map[string]any) map[string]any {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var copy map[string]any
	if err := json.Unmarshal(data, &copy); err != nil {
		t.Fatal(err)
	}
	return copy
}
