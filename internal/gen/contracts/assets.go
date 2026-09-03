// Package contracts exposes generated, embedded projections of Quoin's frozen
// machine contracts. The source of truth remains docs/specs/quoin-v1/contracts.
package contracts

import _ "embed"

var (
	//go:embed schema.sql
	SchemaSQL string

	//go:embed deployment-config.schema.json
	DeploymentConfigSchema []byte

	//go:embed business-system-config.schema.json
	BusinessSystemConfigSchema []byte

	//go:embed label-contract.schema.json
	LabelContractSchema []byte

	//go:embed readiness-response.schema.json
	ReadinessResponseSchema []byte

	//go:embed browser-execution.schema.json
	BrowserExecutionSchema []byte

	//go:embed browser-tool.schema.json
	BrowserToolSchema []byte

	//go:embed release-manifest.schema.json
	ReleaseManifestSchema []byte

	//go:embed plinth-worker-tools.schema.json
	PlinthWorkerToolsSchema []byte

	//go:embed release-inputs.yaml
	ReleaseInputsYAML []byte

	//go:embed metrics.yaml
	MetricsYAML []byte

	//go:embed connection-probes.yaml
	ConnectionProbesYAML []byte

	//go:embed plinth-worker-tools.yaml
	PlinthWorkerToolsYAML []byte

	//go:embed deployment-verification.schema.json
	DeploymentVerificationSchema []byte

	//go:embed verification-result.schema.json
	VerificationResultSchema []byte

	//go:embed verification-catalog.yaml
	VerificationCatalogYAML []byte

	//go:embed verification-result-profile.yaml
	VerificationResultProfileYAML []byte
)
