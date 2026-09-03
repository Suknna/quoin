// Package helm implements the Kubernetes deployment backend. It owns no
// deployment configuration authority: the embedded frozen deployment schema
// validates the human input before this package derives Helm values from it.
package helm

import (
	"fmt"
	"io"

	"github.com/Suknna/quoin/internal/contract"
	deployconfig "github.com/Suknna/quoin/internal/deploy/config"
)

// Request is the stable input surface for both public Helm commands.
type Request struct {
	ConfigPath          string
	ReleaseManifestPath string
	ReportPath          string
	Stdin               io.Reader
	Stdout              io.Writer
	Stderr              io.Writer
}

type pvcInput struct {
	Capacity         string `json:"capacity" yaml:"capacity"`
	AccessMode       string `json:"accessMode" yaml:"accessMode"`
	StorageClassName string `json:"storageClassName,omitempty" yaml:"storageClassName,omitempty"`
}
type ingressInput struct {
	Enabled       bool   `json:"enabled" yaml:"enabled"`
	ClassName     string `json:"className,omitempty" yaml:"className,omitempty"`
	Host          string `json:"host,omitempty" yaml:"host,omitempty"`
	TLSSecretName string `json:"tlsSecretName,omitempty" yaml:"tlsSecretName,omitempty"`
}

type resourcePair struct {
	CPU    string `json:"cpu,omitempty" yaml:"cpu,omitempty"`
	Memory string `json:"memory,omitempty" yaml:"memory,omitempty"`
}

type resourceBlock struct {
	Requests *resourcePair `json:"requests,omitempty" yaml:"requests,omitempty"`
	Limits   *resourcePair `json:"limits,omitempty" yaml:"limits,omitempty"`
}

// installInput is deliberately local: the generated deployment schema remains
// the single authority for this document, while this shape only decodes its
// already-validated values for mechanical chart projection.
type installInput struct {
	Document      string       `json:"document" yaml:"document"`
	PublicOrigin  string       `json:"publicOrigin" yaml:"publicOrigin"`
	PublicIngress ingressInput `json:"publicIngress" yaml:"publicIngress"`
	SteleIngress  ingressInput `json:"steleIngress" yaml:"steleIngress"`
	Storage       struct {
		QuoinData   pvcInput `json:"quoinData" yaml:"quoinData"`
		QuoinBackup pvcInput `json:"quoinBackup" yaml:"quoinBackup"`
		PlinthState pvcInput `json:"plinthState" yaml:"plinthState"`
		LintelState pvcInput `json:"lintelState" yaml:"lintelState"`
	} `json:"storage" yaml:"storage"`
	LintelBrowserSlots    int                      `json:"lintelBrowserSlots" yaml:"lintelBrowserSlots"`
	LintelShmSize         string                   `json:"lintelShmSize,omitempty" yaml:"lintelShmSize,omitempty"`
	MonitorDiscovery      string                   `json:"monitorDiscovery,omitempty" yaml:"monitorDiscovery,omitempty"`
	PrometheusRule        bool                     `json:"prometheusRule" yaml:"prometheusRule"`
	OpsServiceAnnotations map[string]string        `json:"opsServiceAnnotations,omitempty" yaml:"opsServiceAnnotations,omitempty"`
	Resources             map[string]resourceBlock `json:"resources,omitempty" yaml:"resources,omitempty"`
}

type loadedRequest struct {
	input    installInput
	manifest *deployconfig.ReleaseManifest
	stateDir string
	images   map[string]string
	binding  *contract.DeploymentBinding
}

func (loaded *loadedRequest) release() string {
	if loaded.manifest != nil {
		return loaded.manifest.ReleaseVersion
	}
	return "dev"
}

func load(req Request) (*loadedRequest, error) {
	var input installInput
	if err := contract.DecodeFile(req.ConfigPath, &input); err != nil {
		return nil, fmt.Errorf("invalid helm input: %w", err)
	}
	if input.LintelShmSize == "" {
		input.LintelShmSize = "1Gi"
	}
	if input.MonitorDiscovery == "" {
		input.MonitorDiscovery = "none"
	}
	stateDir, err := deployconfig.StateDirectoryFor("helm")
	if err != nil {
		return nil, err
	}
	loaded := &loadedRequest{input: input, stateDir: stateDir, images: map[string]string{}}
	if req.ReleaseManifestPath == "" {
		return loaded, nil
	}
	manifest, err := deployconfig.LoadReleaseManifest(req.ReleaseManifestPath)
	if err != nil {
		return nil, fmt.Errorf("load release manifest: %w", err)
	}
	loaded.manifest = manifest
	binding, err := deployconfig.DeploymentBinding(manifest, req.ConfigPath, req.ReleaseManifestPath, "kubernetes")
	if err != nil {
		return nil, err
	}
	loaded.binding = binding
	for _, component := range deployconfig.Components {
		reference, err := manifest.ImageReference(component)
		if err != nil {
			return nil, err
		}
		loaded.images[component] = reference
	}
	return loaded, nil
}
