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

type renderData struct {
	UID, GID            int
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

func quote(value string) string {
	return strconv.Quote(value)
}
