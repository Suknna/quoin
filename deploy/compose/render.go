// Package compose renders the canonical local deployment projection.
package compose

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	deploy "github.com/Suknna/quoin/deploy"
	"github.com/Suknna/quoin/internal/contract"
	"gopkg.in/yaml.v3"
)

const minimumShmBytes = 1 << 30

type Projection struct {
	Directory   string
	ComposeFile string
}

// Options extends the canonical Compose projection. Images carries explicit
// digest-pinned component references (repository@sha256:...) resolved from a
// release manifest; a component without an entry keeps the local dev image
// expression so existing harnesses are unchanged.
type Options struct {
	Images map[string]string
	// DeploymentBinding freezes the release/deployment identity into the
	// generated Quoin component configuration. nil keeps the local development
	// projection unchanged (no Deployment Acceptance subject).
	DeploymentBinding *contract.DeploymentBinding
}

func Render(input contract.ComposeInstall, stateDirectory string) (Projection, error) {
	return RenderWithOptions(input, stateDirectory, Options{})
}

func RenderWithOptions(input contract.ComposeInstall, stateDirectory string, options Options) (Projection, error) {
	if input.Document != "compose-install" {
		return Projection{}, fmt.Errorf("deployment document must be compose-install")
	}
	if input.LintelShmSizeBytes == 0 {
		input.LintelShmSizeBytes = minimumShmBytes
	}
	if input.LintelShmSizeBytes < minimumShmBytes {
		return Projection{}, fmt.Errorf("Lintel shared memory must be at least %d bytes", minimumShmBytes)
	}
	directories := map[string]string{
		"config": filepath.Join(stateDirectory, "generated"), "data": filepath.Join(stateDirectory, "data"),
		"backups": filepath.Join(stateDirectory, "backups"), "plinth": filepath.Join(stateDirectory, "plinth"),
		"workspaces": filepath.Join(stateDirectory, "plinth-workspaces"), "lintel": filepath.Join(stateDirectory, "lintel"),
	}
	for _, directory := range directories {
		if err := ensureDirectory(directory); err != nil {
			return Projection{}, err
		}
	}
	if err := ensureDirectory(input.SecretDirectory); err != nil {
		return Projection{}, fmt.Errorf("prepare configured secret directory: %w", err)
	}
	configDirectory := directories["config"]
	quoinPath := filepath.Join(configDirectory, "quoin.yaml")
	plinthPath := filepath.Join(configDirectory, "plinth.yaml")
	lintelPath := filepath.Join(configDirectory, "lintel.yaml")
	stelePath := filepath.Join(configDirectory, "stele.yaml")
	containerSecrets := "/run/quoin-secrets"
	quoinRuntimeEndpoint := "https://quoin:8443"
	configs := map[string]any{
		quoinPath:  contract.QuoinConfig{Component: "quoin", PublicOrigin: input.PublicOrigin, DataDirectory: "/var/lib/quoin/data", BackupDirectory: "/var/lib/quoin/backups", RootKeyFile: containerSecrets + "/root-key", RuntimeTLSCertificateFile: containerSecrets + "/runtime-tls.crt", RuntimeTLSPrivateKeyFile: containerSecrets + "/runtime-tls.key", SteleServiceTokenFile: containerSecrets + "/stele-service-token", DeploymentBinding: options.DeploymentBinding},
		plinthPath: contract.PlinthConfig{Component: "plinth", StateDirectory: "/var/lib/plinth", WorkspaceDirectory: "/var/lib/plinth/workspaces", QuoinRuntimeEndpoint: quoinRuntimeEndpoint, QuoinRuntimeCAFile: containerSecrets + "/runtime-ca.pem"},
		lintelPath: contract.LintelConfig{Component: "lintel", StateDirectory: "/var/lib/lintel", QuoinRuntimeEndpoint: quoinRuntimeEndpoint, QuoinRuntimeCAFile: containerSecrets + "/runtime-ca.pem", BrowserSlots: input.LintelBrowserSlots, MinimumShmBytes: input.LintelShmSizeBytes},
		stelePath:  contract.SteleConfig{Component: "stele", QuoinRuntimeEndpoint: quoinRuntimeEndpoint, QuoinRuntimeCAFile: containerSecrets + "/runtime-ca.pem", ServiceTokenFile: containerSecrets + "/stele-service-token"},
	}
	for file, config := range configs {
		data, err := yaml.Marshal(config)
		if err != nil {
			return Projection{}, err
		}
		var checked any
		if err := contract.Decode(data, &checked); err != nil {
			return Projection{}, fmt.Errorf("validate generated %s: %w", filepath.Base(file), err)
		}
		if err := writeAtomic(file, data, 0o600); err != nil {
			return Projection{}, err
		}
	}
	inputData, err := yaml.Marshal(input)
	if err != nil {
		return Projection{}, err
	}
	if err := writeAtomic(filepath.Join(configDirectory, "install-input.yaml"), inputData, 0o600); err != nil {
		return Projection{}, err
	}
	// Start from the direct deployment document so generated installs and the
	// user-operated Compose path cannot drift into separate topologies.
	composeData, err := renderAuthoritativeCompose(input, options, map[string]string{
		"quoin": quoinPath, "plinth": plinthPath, "lintel": lintelPath, "stele": stelePath,
	}, directories)
	if err != nil {
		return Projection{}, err
	}
	composePath := filepath.Join(configDirectory, "compose.yaml")
	if err := writeAtomic(composePath, composeData, 0o600); err != nil {
		return Projection{}, err
	}
	return Projection{Directory: configDirectory, ComposeFile: composePath}, nil
}

func ensureDirectory(path string) error {
	if info, err := os.Lstat(path); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s is not a real directory", path)
		}
		return os.Chmod(path, 0o700)
	} else if !os.IsNotExist(err) {
		return err
	}
	return os.MkdirAll(path, 0o700)
}

func writeAtomic(path string, content []byte, mode os.FileMode) error {
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, content, mode); err != nil {
		return err
	}
	file, err := os.OpenFile(temporary, os.O_RDWR, mode)
	if err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

func bind(hostPath, containerPath, mode string) string {
	value := hostPath + ":" + containerPath
	if mode != "" {
		value += ":" + mode
	}
	return value
}

// renderAuthoritativeCompose patches only deploy-specific details into the
// source-controlled six-service Compose document. The document itself remains
// the authority for Caddy routes, TLS behaviour, and service relationships.
func renderAuthoritativeCompose(input contract.ComposeInstall, options Options, configs, directories map[string]string) ([]byte, error) {
	var document yaml.Node
	if err := yaml.Unmarshal(deploy.ComposeTemplate, &document); err != nil {
		return nil, fmt.Errorf("parse authoritative Compose document: %w", err)
	}
	root := document.Content[0]
	services := mappingValue(root, "services")
	if services == nil {
		return nil, fmt.Errorf("authoritative Compose document has no services")
	}
	for _, component := range []string{"gateway", "frontend", "quoin", "plinth", "lintel", "stele"} {
		if mappingValue(services, component) == nil {
			return nil, fmt.Errorf("authoritative Compose document lacks %s service", component)
		}
	}
	for component := range configs {
		setScalar(mappingValue(services, component), "image", imageReference(options, component))
	}
	setScalar(mappingValue(services, "frontend"), "image", imageReference(options, "frontend"))
	// Secret bootstrap produces the Runtime TLS pair. It is also the local
	// gateway's self-signed certificate, avoiding a second credential source.
	gateway := mappingValue(services, "gateway")
	for _, key := range []string{"environment", "command"} {
		if value := mappingValue(gateway, key); value != nil {
			replaceScalar(value, "/etc/caddy/tls/tls.crt", "/etc/caddy/tls/runtime-tls.crt")
			replaceScalar(value, "/etc/caddy/tls/tls.key", "/etc/caddy/tls/runtime-tls.key")
		}
	}
	setScalar(gateway, "user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()))
	setScalar(mappingValue(services, "quoin"), "user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()))
	for _, component := range []string{"plinth", "lintel", "stele"} {
		setScalar(mappingValue(services, component), "user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()))
	}

	secretDirectory := input.SecretDirectory
	setSequence(mappingValue(services, "quoin"), "volumes", []string{
		bind(configs["quoin"], "/etc/quoin/component.yaml", "ro"), bind(directories["data"], "/var/lib/quoin/data", ""), bind(directories["backups"], "/var/lib/quoin/backups", ""), bind(secretDirectory, "/run/quoin-secrets", ""),
	})
	setSequence(mappingValue(services, "plinth"), "volumes", []string{
		bind(configs["plinth"], "/etc/quoin/component.yaml", "ro"), bind(directories["plinth"], "/var/lib/plinth", ""), bind(directories["workspaces"], "/var/lib/plinth/workspaces", ""), bind(filepath.Join(secretDirectory, "runtime-ca.pem"), "/run/quoin-secrets/runtime-ca.pem", "ro"),
	})
	setSequence(mappingValue(services, "lintel"), "volumes", []string{
		bind(configs["lintel"], "/etc/quoin/component.yaml", "ro"), bind(directories["lintel"], "/var/lib/lintel", ""), bind(filepath.Join(secretDirectory, "runtime-ca.pem"), "/run/quoin-secrets/runtime-ca.pem", "ro"),
	})
	setSequence(mappingValue(services, "stele"), "volumes", []string{
		bind(configs["stele"], "/etc/quoin/component.yaml", "ro"), bind(filepath.Join(secretDirectory, "runtime-ca.pem"), "/run/quoin-secrets/runtime-ca.pem", "ro"), bind(filepath.Join(secretDirectory, "stele-service-token"), "/run/quoin-secrets/stele-service-token", "ro"),
	})
	setSequence(mappingValue(services, "gateway"), "volumes", []string{
		bind(secretDirectory, "/etc/caddy/tls", "ro"),
	})
	setScalar(mappingValue(services, "lintel"), "shm_size", strconv.FormatInt(input.LintelShmSizeBytes, 10))
	setSequence(mappingValue(services, "gateway"), "ports", []string{fmt.Sprintf("127.0.0.1:%d:8443", input.QuoinPublicHostPort)})

	addBootstrapServices(services, configs["quoin"], directories, secretDirectory, options)
	for _, component := range []string{"quoin", "plinth", "lintel", "stele"} {
		service := mappingValue(services, component)
		dependsOn := mappingValue(service, "depends_on")
		if dependsOn == nil {
			dependsOn = newMapping()
			setNode(service, "depends_on", dependsOn)
		}
		setNode(dependsOn, "admin-bootstrap", dependency("service_completed_successfully"))
	}
	encoded, err := yaml.Marshal(&document)
	if err != nil {
		return nil, fmt.Errorf("encode generated Compose document: %w", err)
	}
	return encoded, nil
}

func addBootstrapServices(services *yaml.Node, quoinConfig string, directories map[string]string, secretDirectory string, options Options) {
	secretBootstrap := newMapping()
	setScalar(secretBootstrap, "image", imageReference(options, "quoin"))
	setSequence(secretBootstrap, "command", []string{"secrets", "bootstrap", "--config", "/etc/quoin/component.yaml"})
	setScalar(secretBootstrap, "user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()))
	setScalar(secretBootstrap, "read_only", "true")
	setSequence(secretBootstrap, "cap_drop", []string{"ALL"})
	setSequence(secretBootstrap, "security_opt", []string{"no-new-privileges:true"})
	setSequence(secretBootstrap, "volumes", []string{bind(quoinConfig, "/etc/quoin/component.yaml", "ro"), bind(directories["data"], "/var/lib/quoin/data", ""), bind(secretDirectory, "/run/quoin-secrets", "")})
	setSequence(secretBootstrap, "tmpfs", []string{"/tmp"})
	setScalar(secretBootstrap, "restart", "no")
	setNode(services, "secret-bootstrap", secretBootstrap)

	adminBootstrap := newMapping()
	setScalar(adminBootstrap, "image", imageReference(options, "quoin"))
	setSequence(adminBootstrap, "command", []string{"admin", "create", "--config", "/etc/quoin/component.yaml"})
	setScalar(adminBootstrap, "user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()))
	setScalar(adminBootstrap, "read_only", "true")
	setScalar(adminBootstrap, "stdin_open", "true")
	setScalar(adminBootstrap, "tty", "true")
	setSequence(adminBootstrap, "cap_drop", []string{"ALL"})
	setSequence(adminBootstrap, "security_opt", []string{"no-new-privileges:true"})
	setNode(adminBootstrap, "depends_on", newMappingWith("secret-bootstrap", dependency("service_completed_successfully")))
	setSequence(adminBootstrap, "volumes", []string{bind(quoinConfig, "/etc/quoin/component.yaml", "ro"), bind(directories["data"], "/var/lib/quoin/data", ""), bind(directories["backups"], "/var/lib/quoin/backups", ""), bind(secretDirectory, "/run/quoin-secrets", "ro")})
	setSequence(adminBootstrap, "tmpfs", []string{"/tmp"})
	setScalar(adminBootstrap, "restart", "no")
	setNode(services, "admin-bootstrap", adminBootstrap)
}

func mappingValue(mapping *yaml.Node, key string) *yaml.Node {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil
	}
	for index := 0; index < len(mapping.Content); index += 2 {
		if mapping.Content[index].Value == key {
			return mapping.Content[index+1]
		}
	}
	return nil
}

func setScalar(mapping *yaml.Node, key, value string) {
	setNode(mapping, key, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value})
}

// replaceScalar walks a YAML value because Caddy JSON is nested inside the
// gateway environment mapping rather than stored directly as that key's value.
func replaceScalar(node *yaml.Node, old, replacement string) {
	if node.Kind == yaml.ScalarNode {
		node.Value = strings.ReplaceAll(node.Value, old, replacement)
	}
	for _, child := range node.Content {
		replaceScalar(child, old, replacement)
	}
}
func setSequence(mapping *yaml.Node, key string, values []string) {
	sequence := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	for _, value := range values {
		sequence.Content = append(sequence.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value})
	}
	setNode(mapping, key, sequence)
}
func setNode(mapping *yaml.Node, key string, value *yaml.Node) {
	for index := 0; index < len(mapping.Content); index += 2 {
		if mapping.Content[index].Value == key {
			mapping.Content[index+1] = value
			return
		}
	}
	mapping.Content = append(mapping.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, value)
}
func newMapping() *yaml.Node { return &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"} }
func newMappingWith(key string, value *yaml.Node) *yaml.Node {
	mapping := newMapping()
	setNode(mapping, key, value)
	return mapping
}
func dependency(condition string) *yaml.Node {
	return newMappingWith("condition", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: condition})
}

// imageReference projects a component image line: a digest-pinned reference
// from the release manifest when provided, otherwise the local dev
// expression the existing harnesses resolve through the environment.
func imageReference(options Options, component string) string {
	if reference, ok := options.Images[component]; ok && reference != "" {
		return reference
	}
	if component == "frontend" {
		return "${QUOIN_IMAGE_NAMESPACE:-quoin}/web:v0.1.0-dev"
	}
	return "${QUOIN_IMAGE_NAMESPACE:-quoin}/" + component + ":v0.1.0-dev"
}

// RenderVerifyOverlay writes the one-shot in-network verifier service next to
// the canonical projection (OPS-VERIFY-003: the Compose path checks
// host-unpublished ops listeners through a same-network disposable service;
// it holds no product or external credentials).
func RenderVerifyOverlay(projection Projection, options Options) (string, error) {
	overlay := `services:
  quoin-verifier:
    image: ` + imageReference(options, "quoin") + `
    entrypoint: ["/quoin-healthcheck"]
    read_only: true
    cap_drop: [ALL]
    security_opt: [no-new-privileges:true]
    restart: "no"
`
	path := filepath.Join(projection.Directory, "verify.yaml")
	temporary := path + ".tmp"
	file, err := os.OpenFile(temporary, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	if _, err := file.WriteString(overlay); err != nil {
		file.Close()
		os.Remove(temporary)
		return "", err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	return path, os.Rename(temporary, path)
}

func quote(value string) string {
	return strconv.Quote(value)
}
