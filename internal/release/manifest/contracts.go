package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"

	"github.com/Suknna/quoin/internal/lintel/catalog"
)

// DatabaseSchemaVersion mirrors the frozen schema gate in
// internal/quoin/upgrade/schemagate.go, the runtime authority that rejects
// every schema_state value but this one. The digest beside it is measured
// from the frozen schema.sql itself.
const DatabaseSchemaVersion = "v1"

// ContractsEntry carries the nine contract version+digest pairs the
// release manifest freezes (the closed set of release-manifest.schema.json).
type ContractsEntry struct {
	DeploymentConfigVersion  int    `json:"deployment_config_version"`
	DeploymentConfigSHA256   string `json:"deployment_config_sha256"`
	DatabaseSchemaVersion    string `json:"database_schema_version"`
	DatabaseSchemaSHA256     string `json:"database_schema_sha256"`
	RuntimeProtoVersion      string `json:"runtime_proto_version"`
	RuntimeProtoSHA256       string `json:"runtime_proto_sha256"`
	WorkerProtocolVersion    string `json:"worker_protocol_version"`
	WorkerProtocolSHA256     string `json:"worker_protocol_sha256"`
	MetricsVersion           int    `json:"metrics_contract_version"`
	MetricsSHA256            string `json:"metrics_sha256"`
	PlinthWorkerToolsVersion int    `json:"plinth_worker_tools_version"`
	PlinthWorkerToolsSHA256  string `json:"plinth_worker_tools_sha256"`
	ReleaseInputsVersion     int    `json:"release_inputs_version"`
	ReleaseInputsSHA256      string `json:"release_inputs_sha256"`
	ReadinessVersion         int    `json:"readiness_response_version"`
	ReadinessSHA256          string `json:"readiness_response_sha256"`
	JourneyCatalogVersion    string `json:"journey_catalog_version"`
	JourneyCatalogSHA256     string `json:"journey_catalog_sha256"`
}

// Contracts reads every contract version and digest from the frozen
// contracts directory (the single machine authority location). Versions
// are parsed from each document's own version marker; digests are the
// SHA-256 of the exact authority bytes (OPS-RELEASE-001).
func Contracts(contractsDir string) (*ContractsEntry, error) {
	if contractsDir == "" {
		return nil, fmt.Errorf("contracts directory is required")
	}
	entry := &ContractsEntry{}
	read := func(rel string) ([]byte, error) {
		body, err := os.ReadFile(filepath.Join(contractsDir, rel))
		if err != nil {
			return nil, fmt.Errorf("contract %s: %w", rel, err)
		}
		return body, nil
	}
	digest := func(rel string) (string, error) {
		body, err := read(rel)
		if err != nil {
			return "", err
		}
		sum := sha256.Sum256(body)
		return hex.EncodeToString(sum[:]), nil
	}

	var err error
	if entry.DeploymentConfigVersion, err = schemaContractVersion(read, "schemas/deployment-config.schema.json"); err != nil {
		return nil, err
	}
	if entry.DeploymentConfigSHA256, err = digest("schemas/deployment-config.schema.json"); err != nil {
		return nil, err
	}
	schemaSQL, err := read("sql/schema.sql")
	if err != nil {
		return nil, err
	}
	entry.DatabaseSchemaVersion = DatabaseSchemaVersion
	entry.DatabaseSchemaSHA256 = sha256Hex(schemaSQL)

	runtimeProto, err := read("runtime.proto")
	if err != nil {
		return nil, err
	}
	entry.RuntimeProtoVersion, err = protoPackageVersion(runtimeProto, "quoin.runtime.")
	if err != nil {
		return nil, fmt.Errorf("runtime.proto: %w", err)
	}
	entry.RuntimeProtoSHA256 = sha256Hex(runtimeProto)

	workerProto, err := read("quoin/plinth/worker/v1/agent_worker.proto")
	if err != nil {
		return nil, err
	}
	entry.WorkerProtocolVersion, err = protoPackageVersion(workerProto, "quoin.plinth.worker.")
	if err != nil {
		return nil, fmt.Errorf("agent_worker.proto: %w", err)
	}
	entry.WorkerProtocolSHA256 = sha256Hex(workerProto)

	metrics, err := read("metrics.yaml")
	if err != nil {
		return nil, err
	}
	entry.MetricsVersion, err = yamlContractVersion(metrics, "metrics.yaml")
	if err != nil {
		return nil, err
	}
	entry.MetricsSHA256 = sha256Hex(metrics)

	tools, err := read("plinth-worker-tools.yaml")
	if err != nil {
		return nil, err
	}
	entry.PlinthWorkerToolsVersion, err = yamlContractVersion(tools, "plinth-worker-tools.yaml")
	if err != nil {
		return nil, err
	}
	entry.PlinthWorkerToolsSHA256 = sha256Hex(tools)

	releaseInputs, err := read("release-inputs.yaml")
	if err != nil {
		return nil, err
	}
	entry.ReleaseInputsVersion, err = yamlContractVersion(releaseInputs, "release-inputs.yaml")
	if err != nil {
		return nil, err
	}
	entry.ReleaseInputsSHA256 = sha256Hex(releaseInputs)

	if entry.ReadinessVersion, err = schemaContractVersion(read, "schemas/readiness-response.schema.json"); err != nil {
		return nil, err
	}
	if entry.ReadinessSHA256, err = digest("schemas/readiness-response.schema.json"); err != nil {
		return nil, err
	}

	// The Journey Catalog is the build-time JCS artifact both Lintel and
	// Quoin embed byte-for-byte (RUNTIME-CTRL-010); its version constant
	// and bytes are that package's authority.
	entry.JourneyCatalogVersion = catalog.Version
	entry.JourneyCatalogSHA256 = sha256Hex([]byte(catalog.JCS))
	return entry, nil
}

func sha256Hex(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// schemaContractVersion reads the x-quoin-contract-version marker of a
// JSON Schema contract.
func schemaContractVersion(read func(string) ([]byte, error), rel string) (int, error) {
	body, err := read(rel)
	if err != nil {
		return 0, err
	}
	var shape struct {
		Version int `json:"x-quoin-contract-version"`
	}
	if err := json.Unmarshal(body, &shape); err != nil {
		return 0, fmt.Errorf("%s: %w", rel, err)
	}
	if shape.Version < 1 {
		return 0, fmt.Errorf("%s carries no x-quoin-contract-version", rel)
	}
	return shape.Version, nil
}

// yamlContractVersion reads the contract_version marker of a YAML
// contract without importing a YAML decoder for one integer: the marker is
// a fixed top-level key.
func yamlContractVersion(body []byte, rel string) (int, error) {
	pattern := regexp.MustCompile(`(?m)^contract_version:\s*([0-9]+)\s*$`)
	match := pattern.FindSubmatch(body)
	if match == nil {
		return 0, fmt.Errorf("%s carries no contract_version", rel)
	}
	version, err := strconv.Atoi(string(match[1]))
	if err != nil {
		return 0, fmt.Errorf("%s contract_version: %w", rel, err)
	}
	return version, nil
}

// protoPackageVersion derives the contract version from a proto package
// declaration (e.g. "package quoin.runtime.v1;" -> "v1").
func protoPackageVersion(body []byte, prefix string) (string, error) {
	pattern := regexp.MustCompile(`(?m)^package\s+` + regexp.QuoteMeta(prefix) + `(v[0-9]+)\s*;`)
	match := pattern.FindSubmatch(body)
	if match == nil {
		return "", fmt.Errorf("no %s* package declaration", prefix)
	}
	return string(match[1]), nil
}
