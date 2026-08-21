// Package contracts exposes generated, embedded projections of Quoin's frozen
// machine contracts. The source of truth remains docs/specs/quoin-v1/contracts.
package contracts

import _ "embed"

var (
	//go:embed schema.sql
	SchemaSQL string

	//go:embed deployment-config.schema.json
	DeploymentConfigSchema []byte

	//go:embed readiness-response.schema.json
	ReadinessResponseSchema []byte

	//go:embed metrics.yaml
	MetricsYAML []byte

	//go:embed connection-probes.yaml
	ConnectionProbesYAML []byte

	//go:embed plinth-worker-tools.yaml
	PlinthWorkerToolsYAML []byte
)
