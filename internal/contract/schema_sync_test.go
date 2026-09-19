package contract_test

// Every generated, embedded projection in internal/gen/contracts must stay
// byte-identical to its machine authority under
// docs/specs/quoin-v1/contracts (the single authority location; the release
// manifest digests the AUTHORITY bytes while the binaries validate against
// the EMBEDDED copies). Update the authority first, then byte-copy the file
// into internal/gen/contracts; this table fails until the sync happens.

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	gen "github.com/Suknna/quoin/internal/gen/contracts"
)

const contractsAuthorityRoot = "../../docs/specs/quoin-v1/contracts"

func TestEmbeddedContractsMatchDocsAuthority(t *testing.T) {
	projections := []struct {
		name      string
		embedded  []byte
		authority string // relative to docs/specs/quoin-v1/contracts
	}{
		{"SchemaSQL", []byte(gen.SchemaSQL), "sql/schema.sql"},
		{"DeploymentConfigSchema", gen.DeploymentConfigSchema, "schemas/deployment-config.schema.json"},
		{"BusinessSystemConfigSchema", gen.BusinessSystemConfigSchema, "schemas/business-system-config.schema.json"},
		{"BusinessSystemSchema", gen.BusinessSystemSchema, "schemas/business-system.schema.json"},
		{"ConfigVerificationDiscoveryExecutionSchema", gen.ConfigVerificationDiscoveryExecutionSchema, "schemas/config-verification-discovery-execution.schema.json"},
		{"LabelContractSchema", gen.LabelContractSchema, "schemas/label-contract.schema.json"},
		{"ReadinessResponseSchema", gen.ReadinessResponseSchema, "schemas/readiness-response.schema.json"},
		{"ReleaseManifestSchema", gen.ReleaseManifestSchema, "schemas/release-manifest.schema.json"},
		{"PlinthWorkerToolsSchema", gen.PlinthWorkerToolsSchema, "schemas/plinth-worker-tools.schema.json"},
		{"ReleaseInputsYAML", gen.ReleaseInputsYAML, "release-inputs.yaml"},
		{"MetricsYAML", gen.MetricsYAML, "metrics.yaml"},
		{"ConnectionProbesYAML", gen.ConnectionProbesYAML, "connection-probes.yaml"},
		{"PlinthWorkerToolsYAML", gen.PlinthWorkerToolsYAML, "plinth-worker-tools.yaml"},
		{"DeploymentVerificationSchema", gen.DeploymentVerificationSchema, "schemas/deployment-verification.schema.json"},
		{"VerificationResultSchema", gen.VerificationResultSchema, "schemas/verification-result.schema.json"},
		{"VerificationCatalogYAML", gen.VerificationCatalogYAML, "verification-catalog.yaml"},
		{"VerificationResultProfileYAML", gen.VerificationResultProfileYAML, "verification-result-profile.yaml"},
	}
	for _, projection := range projections {
		t.Run(projection.name, func(t *testing.T) {
			authority, err := os.ReadFile(filepath.Join(contractsAuthorityRoot, projection.authority))
			if err != nil {
				t.Fatalf("read authority %s: %v", projection.authority, err)
			}
			if !bytes.Equal(authority, projection.embedded) {
				t.Fatalf("embedded projection diverged from authority %s; update the authority first, then byte-copy into internal/gen/contracts", projection.authority)
			}
		})
	}
}
