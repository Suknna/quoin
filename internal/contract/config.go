// Package contract decodes generated deployment configuration against the
// repository's machine authority before component code sees typed values.
package contract

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"

	gen "github.com/Suknna/quoin/internal/gen/contracts"
	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"
)

type ComposeInstall struct {
	Document             string `json:"document" yaml:"document"`
	PublicOrigin         string `json:"publicOrigin" yaml:"publicOrigin"`
	PublishMode          string `json:"publishMode" yaml:"publishMode"`
	QuoinPublicHostPort  int    `json:"quoinPublicHostPort,omitempty" yaml:"quoinPublicHostPort,omitempty"`
	SteleWebhookHostPort int    `json:"steleWebhookHostPort,omitempty" yaml:"steleWebhookHostPort,omitempty"`
	ExternalProxyNetwork string `json:"externalProxyNetwork,omitempty" yaml:"externalProxyNetwork,omitempty"`
	SecretDirectory      string `json:"secretDirectory" yaml:"secretDirectory"`
}

type QuoinConfig struct {
	Component                 string `json:"component" yaml:"component"`
	PublicOrigin              string `json:"publicOrigin" yaml:"publicOrigin"`
	DataDirectory             string `json:"dataDirectory" yaml:"dataDirectory"`
	BackupDirectory           string `json:"backupDirectory" yaml:"backupDirectory"`
	RootKeyFile               string `json:"rootKeyFile" yaml:"rootKeyFile"`
	RuntimeTLSCertificateFile string `json:"runtimeTlsCertificateFile" yaml:"runtimeTlsCertificateFile"`
	RuntimeTLSPrivateKeyFile  string `json:"runtimeTlsPrivateKeyFile" yaml:"runtimeTlsPrivateKeyFile"`
	// RuntimeClientCAFile is the deployment CA that signed the component
	// client certificates; the Runtime gRPC listener verifies mTLS client
	// certificates against it (ADR-0009).
	RuntimeClientCAFile string `json:"runtimeClientCaFile" yaml:"runtimeClientCaFile"`
	// StelePublicURL is the externally reachable Alertmanager receiver endpoint.
	// It is deployment authority, never inferred from an HTTP request host.
	StelePublicURL string `json:"stelePublicURL" yaml:"stelePublicURL"`
	// DeploymentBinding is frozen by install/upgrade from the release
	// manifest and deployment input bytes. It is absent for local development
	// projections; Deployment Acceptance is then simply unavailable.
	DeploymentBinding *DeploymentBinding `json:"deploymentBinding,omitempty" yaml:"deploymentBinding,omitempty"`
	// EnabledPlugins is the deployment's explicit plugin enablement
	// whitelist (ADR-0004, ADR-0011 registry). Absent selects every plugin
	// whose registration defaults to enabled (prometheus/thanos/
	// alertmanager). Unknown IDs fail component startup. The same field
	// drives Quoin's catalog and the frozen model tool directory.
	EnabledPlugins []string `json:"enabledPlugins,omitempty" yaml:"enabledPlugins,omitempty"`
	// Authentication is the optional deployment-side bootstrap for
	// verification-code delivery (authentication design section 8). It
	// exists only when this YAML supplies it: an absent section means the
	// deployment ships no delivery preset and administrators configure
	// delivery at runtime. SecretsFile names the read-only secret
	// reference mapping file; runtime-configured secrets stay root-key
	// encrypted in the database and never live in this file.
	Authentication *QuoinAuthenticationConfig `json:"authentication,omitempty" yaml:"authentication,omitempty"`
	// Audit is the optional audit retention override (ADR-0006). Absent
	// selects the six-calendar-month default; RetentionMonths can only
	// extend it.
	Audit *QuoinAuditConfig `json:"audit,omitempty" yaml:"audit,omitempty"`
}

// QuoinAuthenticationConfig declares the login channels (ADR-0010). The
// local channel is enabled and visible unless explicitly disabled; the OIDC
// channel is disabled unless explicitly enabled. Secrets (the OIDC client
// secret) resolve only through SecretsFile — never inline in this file.
type QuoinAuthenticationConfig struct {
	// Local is the emergency password channel; a nil value means enabled
	// and visible (the deployment default).
	Local *QuoinLocalAuthConfig `json:"local,omitempty" yaml:"local,omitempty"`
	// OIDC is the everyday SSO channel; a nil value or Enabled=false keeps
	// it off.
	OIDC *QuoinOIDCAuthConfig `json:"oidc,omitempty" yaml:"oidc,omitempty"`
	// SecretsFile is the read-only path to a JSON or YAML document mapping
	// secret reference names to values (the OIDC client secret uses the
	// reference name "oidcClientSecret"). Required when OIDC is enabled.
	SecretsFile string `json:"secretsFile,omitempty" yaml:"secretsFile,omitempty"`
}

// QuoinLocalAuthConfig gates the local emergency channel.
type QuoinLocalAuthConfig struct {
	// Enabled defaults to true; false disables local login entirely.
	Enabled *bool `json:"enabled,omitempty" yaml:"enabled,omitempty"`
	// Visible defaults to true; false only hides the login form —
	// POST /api/v1/auth/login and /login?local=1 stay reachable.
	Visible *bool `json:"visible,omitempty" yaml:"visible,omitempty"`
}

// LocalEnabled resolves the effective local enablement.
func (config *QuoinAuthenticationConfig) LocalEnabled() bool {
	return config == nil || config.Local == nil || config.Local.Enabled == nil || *config.Local.Enabled
}

// LocalVisible resolves the effective local visibility.
func (config *QuoinAuthenticationConfig) LocalVisible() bool {
	return config == nil || config.Local == nil || config.Local.Visible == nil || *config.Local.Visible
}

// OIDCEnabled resolves the effective OIDC enablement.
func (config *QuoinAuthenticationConfig) OIDCEnabled() bool {
	return config != nil && config.OIDC != nil && config.OIDC.Enabled
}

// QuoinOIDCAuthConfig is the generic OIDC authorization-code channel.
type QuoinOIDCAuthConfig struct {
	Enabled     bool   `json:"enabled" yaml:"enabled"`
	Issuer      string `json:"issuer" yaml:"issuer"`
	ClientID    string `json:"clientId" yaml:"clientId"`
	RedirectURL string `json:"redirectUrl" yaml:"redirectUrl"`
	Label       string `json:"label,omitempty" yaml:"label,omitempty"`
	IconURL     string `json:"iconUrl,omitempty" yaml:"iconUrl,omitempty"`
}

// QuoinAuditConfig is the deployment audit retention override.
type QuoinAuditConfig struct {
	// RetentionMonths is the audit retention in calendar months; the
	// default and schema minimum is six (ADR-0006).
	RetentionMonths int `json:"retentionMonths" yaml:"retentionMonths"`
}

// DeploymentBinding is the immutable runtime authority for what this process
// was deployed from: the release manifest bytes (site acceptance subject), the
// deployment input bytes, and the deployment platform. Quoin only reads it.
type DeploymentBinding struct {
	ReleaseVersion         string `json:"releaseVersion" yaml:"releaseVersion"`
	ReleaseSubjectDigest   string `json:"releaseSubjectDigest" yaml:"releaseSubjectDigest"`
	DeploymentConfigDigest string `json:"deploymentConfigDigest" yaml:"deploymentConfigDigest"`
	Backend                string `json:"backend" yaml:"backend"`
	Architecture           string `json:"architecture" yaml:"architecture"`
}

type PlinthConfig struct {
	Component            string `json:"component" yaml:"component"`
	StateDirectory       string `json:"stateDirectory" yaml:"stateDirectory"`
	WorkspaceDirectory   string `json:"workspaceDirectory" yaml:"workspaceDirectory"`
	QuoinRuntimeEndpoint string `json:"quoinRuntimeEndpoint" yaml:"quoinRuntimeEndpoint"`
	QuoinRuntimeCAFile   string `json:"quoinRuntimeCaFile" yaml:"quoinRuntimeCaFile"`
	// QuoinRuntimeClientCertificateFile / QuoinRuntimeClientPrivateKeyFile are
	// the deployment CA-signed client identity (CN=plinth) presented during
	// the mTLS handshake; they replace the retired registration tokens
	// (ADR-0009).
	QuoinRuntimeClientCertificateFile string `json:"quoinRuntimeClientCertificateFile" yaml:"quoinRuntimeClientCertificateFile"`
	QuoinRuntimeClientPrivateKeyFile  string `json:"quoinRuntimeClientPrivateKeyFile" yaml:"quoinRuntimeClientPrivateKeyFile"`
	// EnabledPlugins must mirror quoinConfig.enabledPlugins from the same
	// deployment input: the disposable worker renders the provider-facing
	// tool schema from the frozen catalog, and BeginModelCall rejects any
	// digest drift, so a split deployment fails loudly instead of offering
	// divergent tools.
	EnabledPlugins []string `json:"enabledPlugins,omitempty" yaml:"enabledPlugins,omitempty"`
}

type SteleConfig struct {
	Component            string `json:"component" yaml:"component"`
	QuoinRuntimeEndpoint string `json:"quoinRuntimeEndpoint" yaml:"quoinRuntimeEndpoint"`
	QuoinRuntimeCAFile   string `json:"quoinRuntimeCaFile" yaml:"quoinRuntimeCaFile"`
	// QuoinRuntimeClientCertificateFile / QuoinRuntimeClientPrivateKeyFile are
	// the deployment CA-signed client identity (CN=stele) presented during
	// the mTLS handshake; they replace the retired service token (ADR-0009).
	QuoinRuntimeClientCertificateFile string `json:"quoinRuntimeClientCertificateFile" yaml:"quoinRuntimeClientCertificateFile"`
	QuoinRuntimeClientPrivateKeyFile  string `json:"quoinRuntimeClientPrivateKeyFile" yaml:"quoinRuntimeClientPrivateKeyFile"`
	// DataDirectory is the local state root for Stele's SQLite queue and
	// counters (ADR-0011): the outbox/dead-letter reliability substrate behind
	// enqueue-as-ack webhooks. Required, non-empty; the directory is created
	// with 0700 and the database restricted to 0600.
	DataDirectory string `json:"dataDirectory" yaml:"dataDirectory"`
}

func DecodeFile(path string, target any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read deployment configuration: %w", err)
	}
	return Decode(data, target)
}

func Decode(data []byte, target any) error {
	var node yaml.Node
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(false)
	if err := decoder.Decode(&node); err != nil {
		return fmt.Errorf("parse deployment configuration: %w", err)
	}
	if len(node.Content) != 1 {
		return fmt.Errorf("deployment configuration must contain one document")
	}
	if err := rejectUnsafeYAML(node.Content[0]); err != nil {
		return err
	}
	var trailing yaml.Node
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("deployment configuration must contain one document")
		}
		return fmt.Errorf("parse trailing deployment configuration: %w", err)
	}
	var value any
	if err := node.Content[0].Decode(&value); err != nil {
		return fmt.Errorf("decode deployment configuration: %w", err)
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("canonicalize deployment configuration: %w", err)
	}
	var instance any
	if err := json.Unmarshal(canonical, &instance); err != nil {
		return err
	}
	compiler := jsonschema.NewCompiler()
	schemaDocument, err := jsonschema.UnmarshalJSON(bytes.NewReader(gen.DeploymentConfigSchema))
	if err != nil {
		return fmt.Errorf("load deployment schema: %w", err)
	}
	const schemaURL = "https://github.com/Suknna/quoin/schemas/deployment-config.schema.json"
	if err := compiler.AddResource(schemaURL, schemaDocument); err != nil {
		return err
	}
	schema, err := compiler.Compile(schemaURL)
	if err != nil {
		return fmt.Errorf("compile deployment schema: %w", err)
	}
	if err := schema.Validate(instance); err != nil {
		return fmt.Errorf("validate deployment configuration: %w", err)
	}
	if err := json.Unmarshal(canonical, target); err != nil {
		return fmt.Errorf("decode typed deployment configuration: %w", err)
	}
	return nil
}

func rejectUnsafeYAML(node *yaml.Node) error {
	if node.Anchor != "" || node.Alias != nil || node.Kind == yaml.AliasNode {
		return fmt.Errorf("deployment configuration anchors and aliases are not allowed")
	}
	if node.Tag != "" && node.Tag != "!!map" && node.Tag != "!!seq" && node.Tag != "!!str" && node.Tag != "!!int" && node.Tag != "!!bool" && node.Tag != "!!null" {
		return fmt.Errorf("deployment configuration custom tags are not allowed")
	}
	if node.Kind == yaml.MappingNode {
		seen := map[string]struct{}{}
		for index := 0; index < len(node.Content); index += 2 {
			key := node.Content[index]
			if key.Kind != yaml.ScalarNode || key.Tag != "!!str" {
				return fmt.Errorf("deployment configuration keys must be strings")
			}
			if _, exists := seen[key.Value]; exists {
				return fmt.Errorf("duplicate deployment configuration key %q", key.Value)
			}
			seen[key.Value] = struct{}{}
		}
	}
	for _, child := range node.Content {
		if err := rejectUnsafeYAML(child); err != nil {
			return err
		}
	}
	return nil
}
