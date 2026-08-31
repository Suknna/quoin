// Package config owns the deployment helper's frozen-contract inputs: the
// release manifest (validated against the embedded release-manifest schema)
// and the local install retry-state identity keyed by the frozen
// release_version + backend + config digest + command tuple (OPS-HELPER-002).
package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	gen "github.com/Suknna/quoin/internal/gen/contracts"
	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

// Components is the closed four-component set of a Quoin release.
var Components = []string{"quoin", "plinth", "lintel", "stele"}

type ReleaseManifest struct {
	ManifestVersion int                     `json:"manifest_version"`
	ReleaseVersion  string                  `json:"release_version"`
	SourceCommit    string                  `json:"source_commit"`
	GeneratedAt     string                  `json:"generated_at"`
	Images          map[string]ReleaseImage `json:"images"`
	Browser         json.RawMessage         `json:"browser"`
	Helm            json.RawMessage         `json:"helm"`
	Compose         json.RawMessage         `json:"compose"`
	Helper          json.RawMessage         `json:"deployment_helper"`
	Offline         json.RawMessage         `json:"offline"`
	Sigstore        json.RawMessage         `json:"sigstore_bundles"`
	Contracts       json.RawMessage         `json:"contracts"`
	Validation      json.RawMessage         `json:"validation"`
}

type ReleaseImage struct {
	Repository  string            `json:"repository"`
	IndexDigest string            `json:"index_digest"`
	Platforms   map[string]string `json:"platforms"`
}

// LoadReleaseManifest reads and schema-validates a release manifest. The
// frozen contracts/schemas/release-manifest.schema.json is the only authority;
// no field is hand-parsed beyond the strict decode.
func LoadReleaseManifest(path string) (*ReleaseManifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read release manifest: %w", err)
	}
	var instance any
	if err := json.Unmarshal(data, &instance); err != nil {
		return nil, fmt.Errorf("parse release manifest: %w", err)
	}
	compiler := jsonschema.NewCompiler()
	schemaDocument, err := jsonschema.UnmarshalJSON(bytes.NewReader(gen.ReleaseManifestSchema))
	if err != nil {
		return nil, fmt.Errorf("load release manifest schema: %w", err)
	}
	const schemaURL = "https://github.com/Suknna/quoin/schemas/release-manifest.schema.json"
	if err := compiler.AddResource(schemaURL, schemaDocument); err != nil {
		return nil, err
	}
	schema, err := compiler.Compile(schemaURL)
	if err != nil {
		return nil, fmt.Errorf("compile release manifest schema: %w", err)
	}
	if err := schema.Validate(instance); err != nil {
		return nil, fmt.Errorf("validate release manifest: %w", err)
	}
	var manifest ReleaseManifest
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return nil, fmt.Errorf("decode release manifest: %w", err)
	}
	return &manifest, nil
}

// ImageReference returns the digest-pinned reference for a component
// (repository@index_digest): release manifests are the only image authority
// and never carry a mutable tag (OPS-RELEASE-002).
func (manifest *ReleaseManifest) ImageReference(component string) (string, error) {
	image, ok := manifest.Images[component]
	if !ok {
		return "", fmt.Errorf("release manifest has no image for component %q", component)
	}
	return image.Repository + "@" + image.IndexDigest, nil
}

// PlatformDigest returns the per-platform manifest digest recorded for the
// component, so the verifier can pin the exact platform artifact.
func (manifest *ReleaseManifest) PlatformDigest(component, platform string) (string, error) {
	image, ok := manifest.Images[component]
	if !ok {
		return "", fmt.Errorf("release manifest has no image for component %q", component)
	}
	digest, ok := image.Platforms[platform]
	if !ok {
		return "", fmt.Errorf("release manifest has no %s digest for component %q", platform, component)
	}
	return digest, nil
}

// DigestFile is the stable content identity of a helper input file.
func DigestFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s for digest: %w", path, err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// StateDirectory is the helper-owned local state root for the Compose
// backend. Keep it for compatibility with the existing Compose caller.
func StateDirectory() (string, error) {
	return StateDirectoryFor("compose")
}

// StateDirectoryFor returns a backend-private state root. Install retry state
// carries a backend identity, so sharing its file across backends would make a
// completed Compose stage appear reusable by Helm (or vice versa).
func StateDirectoryFor(backend string) (string, error) {
	if backend != "compose" && backend != "helm" {
		return "", fmt.Errorf("unsupported deployment backend %q", backend)
	}
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("find home directory: %w", err)
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "quoin", backend), nil
}

// InstallStateKey is the frozen retry identity: retries with the same key
// resume from persisted stages; any change refuses to reuse the old state.
type InstallStateKey struct {
	ReleaseVersion string `json:"release_version"`
	Backend        string `json:"backend"`
	ConfigDigest   string `json:"config_digest"`
	Command        string `json:"command"`
	// TargetIdentity fences retry state to one concrete deployment target.
	// Backends that do not need an external target leave it empty; Helm records
	// the current API server, cluster UID, namespace and release.
	TargetIdentity string `json:"target_identity,omitempty"`
}

// InstallState is the persisted last-completed-stage record. It never holds
// secrets or attached-stdin content.
type InstallState struct {
	Key        InstallStateKey `json:"key"`
	StagesDone []string        `json:"stages_completed"`
	FinishedAt string          `json:"finished_at,omitempty"`
}

// LoadInstallState reads the retry state from the helper state directory.
func LoadInstallState(stateDirectory string) (*InstallState, error) {
	data, err := os.ReadFile(filepath.Join(stateDirectory, "install-state.json"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read install state: %w", err)
	}
	var state InstallState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("install state is corrupt: %w", err)
	}
	return &state, nil
}

// WriteInstallState persists the retry state atomically.
func (state *InstallState) WriteInstallState(stateDirectory string) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(stateDirectory, "install-state.json")
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, append(data, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		return err
	}
	return syncDirectory(stateDirectory)
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
