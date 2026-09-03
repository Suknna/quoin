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
	LintelBrowserSlots   int    `json:"lintelBrowserSlots" yaml:"lintelBrowserSlots"`
	LintelShmSizeBytes   int64  `json:"lintelShmSizeBytes,omitempty" yaml:"lintelShmSizeBytes,omitempty"`
}

type QuoinConfig struct {
	Component                 string `json:"component" yaml:"component"`
	PublicOrigin              string `json:"publicOrigin" yaml:"publicOrigin"`
	DataDirectory             string `json:"dataDirectory" yaml:"dataDirectory"`
	BackupDirectory           string `json:"backupDirectory" yaml:"backupDirectory"`
	RootKeyFile               string `json:"rootKeyFile" yaml:"rootKeyFile"`
	RuntimeTLSCertificateFile string `json:"runtimeTlsCertificateFile" yaml:"runtimeTlsCertificateFile"`
	RuntimeTLSPrivateKeyFile  string `json:"runtimeTlsPrivateKeyFile" yaml:"runtimeTlsPrivateKeyFile"`
	SteleServiceTokenFile     string `json:"steleServiceTokenFile" yaml:"steleServiceTokenFile"`
	// DeploymentBinding is frozen by install/upgrade from the release
	// manifest and deployment input bytes. It is absent for local development
	// projections; Deployment Acceptance is then simply unavailable.
	DeploymentBinding *DeploymentBinding `json:"deploymentBinding,omitempty" yaml:"deploymentBinding,omitempty"`
}

// DeploymentBinding is the immutable runtime authority for what this process
// was deployed from: the release manifest bytes (site acceptance subject), the
// deployment input bytes, and the deployment platform. Quoin only reads it.
type DeploymentBinding struct {
	ReleaseVersion          string `json:"releaseVersion" yaml:"releaseVersion"`
	ReleaseSubjectDigest    string `json:"releaseSubjectDigest" yaml:"releaseSubjectDigest"`
	DeploymentConfigDigest  string `json:"deploymentConfigDigest" yaml:"deploymentConfigDigest"`
	Backend                 string `json:"backend" yaml:"backend"`
	Architecture            string `json:"architecture" yaml:"architecture"`
	BrowserChromiumRevision string `json:"browserChromiumRevision" yaml:"browserChromiumRevision"`
}

type PlinthConfig struct {
	Component            string `json:"component" yaml:"component"`
	StateDirectory       string `json:"stateDirectory" yaml:"stateDirectory"`
	WorkspaceDirectory   string `json:"workspaceDirectory" yaml:"workspaceDirectory"`
	QuoinRuntimeEndpoint string `json:"quoinRuntimeEndpoint" yaml:"quoinRuntimeEndpoint"`
	QuoinRuntimeCAFile   string `json:"quoinRuntimeCaFile" yaml:"quoinRuntimeCaFile"`
}

type LintelConfig struct {
	Component            string `json:"component" yaml:"component"`
	StateDirectory       string `json:"stateDirectory" yaml:"stateDirectory"`
	QuoinRuntimeEndpoint string `json:"quoinRuntimeEndpoint" yaml:"quoinRuntimeEndpoint"`
	QuoinRuntimeCAFile   string `json:"quoinRuntimeCaFile" yaml:"quoinRuntimeCaFile"`
	BrowserSlots         int    `json:"browserSlots" yaml:"browserSlots"`
	MinimumShmBytes      int64  `json:"minimumShmBytes" yaml:"minimumShmBytes"`
}

type SteleConfig struct {
	Component            string `json:"component" yaml:"component"`
	QuoinRuntimeEndpoint string `json:"quoinRuntimeEndpoint" yaml:"quoinRuntimeEndpoint"`
	QuoinRuntimeCAFile   string `json:"quoinRuntimeCaFile" yaml:"quoinRuntimeCaFile"`
	ServiceTokenFile     string `json:"serviceTokenFile" yaml:"serviceTokenFile"`
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
