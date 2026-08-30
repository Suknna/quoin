// Package compose renders the canonical local deployment projection.
package compose

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"text/template"

	"github.com/Suknna/quoin/internal/contract"
	"gopkg.in/yaml.v3"
)

const minimumShmBytes = 1 << 30

//go:embed compose.yaml.tmpl
var composeTemplate string

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
}

type renderData struct {
	UID, GID            int
	QuoinImage          string
	PlinthImage         string
	LintelImage         string
	SteleImage          string
	QuoinConfigBind     string
	PlinthConfigBind    string
	LintelConfigBind    string
	SteleConfigBind     string
	DataBind            string
	BackupBind          string
	SecretsRWBind       string
	SecretsROBind       string
	RuntimeCABind       string
	SteleTokenBind      string
	PlinthStateBind     string
	PlinthWorkBind      string
	LintelStateBind     string
	LintelShmSize       int64
	Loopback            bool
	QuoinPort           int
	StelePort           int
	ExternalNetwork     bool
	ExternalNetworkName string
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
		quoinPath:  contract.QuoinConfig{Component: "quoin", PublicOrigin: input.PublicOrigin, DataDirectory: "/var/lib/quoin/data", BackupDirectory: "/var/lib/quoin/backups", RootKeyFile: containerSecrets + "/root-key", RuntimeTLSCertificateFile: containerSecrets + "/runtime-tls.crt", RuntimeTLSPrivateKeyFile: containerSecrets + "/runtime-tls.key", SteleServiceTokenFile: containerSecrets + "/stele-service-token"},
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
	values := renderData{
		UID: os.Getuid(), GID: os.Getgid(),
		QuoinImage:          imageReference(options, "quoin"),
		PlinthImage:         imageReference(options, "plinth"),
		LintelImage:         imageReference(options, "lintel"),
		SteleImage:          imageReference(options, "stele"),
		QuoinConfigBind:     bind(quoinPath, "/etc/quoin/component.yaml", "ro"),
		PlinthConfigBind:    bind(plinthPath, "/etc/quoin/component.yaml", "ro"),
		LintelConfigBind:    bind(lintelPath, "/etc/quoin/component.yaml", "ro"),
		SteleConfigBind:     bind(stelePath, "/etc/quoin/component.yaml", "ro"),
		DataBind:            bind(directories["data"], "/var/lib/quoin/data", ""),
		BackupBind:          bind(directories["backups"], "/var/lib/quoin/backups", ""),
		SecretsRWBind:       bind(input.SecretDirectory, containerSecrets, ""),
		SecretsROBind:       bind(input.SecretDirectory, containerSecrets, "ro"),
		RuntimeCABind:       bind(filepath.Join(input.SecretDirectory, "runtime-ca.pem"), containerSecrets+"/runtime-ca.pem", "ro"),
		SteleTokenBind:      bind(filepath.Join(input.SecretDirectory, "stele-service-token"), containerSecrets+"/stele-service-token", "ro"),
		PlinthStateBind:     bind(directories["plinth"], "/var/lib/plinth", ""),
		PlinthWorkBind:      bind(directories["workspaces"], "/var/lib/plinth/workspaces", ""),
		LintelStateBind:     bind(directories["lintel"], "/var/lib/lintel", ""),
		LintelShmSize:       input.LintelShmSizeBytes,
		Loopback:            input.PublishMode == "loopback",
		QuoinPort:           input.QuoinPublicHostPort,
		StelePort:           input.SteleWebhookHostPort,
		ExternalNetwork:     input.PublishMode == "internal-network-only",
		ExternalNetworkName: quote(input.ExternalProxyNetwork),
	}
	parsed, err := template.New("compose").Parse(composeTemplate)
	if err != nil {
		return Projection{}, err
	}
	composePath := filepath.Join(configDirectory, "compose.yaml")
	file, err := os.OpenFile(composePath+".tmp", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return Projection{}, err
	}
	if err := parsed.Execute(file, values); err != nil {
		file.Close()
		os.Remove(file.Name())
		return Projection{}, err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return Projection{}, err
	}
	if err := file.Close(); err != nil {
		return Projection{}, err
	}
	if err := os.Rename(composePath+".tmp", composePath); err != nil {
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
	return quote(value)
}

// imageReference projects a component image line: a digest-pinned reference
// from the release manifest when provided, otherwise the local dev
// expression the existing harnesses resolve through the environment.
func imageReference(options Options, component string) string {
	if reference, ok := options.Images[component]; ok && reference != "" {
		return reference
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
    networks: [internal]
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
