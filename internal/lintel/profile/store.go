// Package profile owns Lintel's durable browser-profile volume layout and
// manifest validation. A profile generation is never inferred from Chromium's
// mutable files: the atomically written manifest is the sole publication
// marker used by inventory reconciliation.
package profile

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

const manifestName = "manifest.json"

type Manifest struct {
	IdentityID       int64  `json:"identityId"`
	Generation       uint64 `json:"generation"`
	IdentityRevision int64  `json:"identityRevisionId"`
	ChromiumRevision string `json:"chromiumRevision"`
}

type Store struct{ root string }

func NewStore(stateDirectory string) *Store {
	return &Store{root: filepath.Join(stateDirectory, "profiles")}
}

func (store *Store) GenerationPath(identityID int64, generation uint64) (string, error) {
	if identityID <= 0 || generation == 0 {
		return "", errors.New("profile identity and generation must be positive")
	}
	return filepath.Join(store.root, strconv.FormatInt(identityID, 10), strconv.FormatUint(generation, 10)), nil
}

func (store *Store) manifestPath(identityID int64, generation uint64) (string, error) {
	directory, err := store.GenerationPath(identityID, generation)
	if err != nil {
		return "", err
	}
	return filepath.Join(directory, manifestName), nil
}

// Publish writes the generation manifest atomically after Chromium has stopped
// writing the profile. The returned digest is SHA-256 of exact persisted bytes.
func (store *Store) Publish(manifest Manifest) ([]byte, error) {
	if manifest.IdentityID <= 0 || manifest.Generation == 0 || manifest.IdentityRevision <= 0 || manifest.ChromiumRevision == "" {
		return nil, errors.New("incomplete profile manifest")
	}
	path, err := store.manifestPath(manifest.IdentityID, manifest.Generation)
	if err != nil {
		return nil, err
	}
	return writeManifest(path, manifest)
}

func writeManifest(path string, manifest Manifest) ([]byte, error) {
	body, err := json.Marshal(manifest)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, body, 0o600); err != nil {
		return nil, err
	}
	if err := os.Rename(temporary, path); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(body)
	return sum[:], nil
}

// Install atomically adopts a stopped operation's Chromium profile directory
// as its immutable generation before writing the publication marker.
func (store *Store) Install(source string, manifest Manifest) ([]byte, error) {
	destination, err := store.GenerationPath(manifest.IdentityID, manifest.Generation)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return nil, err
	}
	if _, err := os.Stat(destination); err == nil {
		// Quoin only requests a generation number absent from its durable
		// authority. Therefore an existing directory is an unacknowledged
		// prior Lintel adoption (the result was lost before Quoin committed),
		// not a published history record. Retire it so a fresh manual-login
		// attempt can deterministically publish this generation instead of
		// permanently wedging the identity after a process restart.
		if err := os.RemoveAll(destination); err != nil {
			return nil, fmt.Errorf("retire unacknowledged profile generation: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	staging := destination + ".staging"
	if err := os.RemoveAll(staging); err != nil {
		return nil, err
	}
	if err := os.Rename(source, staging); err != nil {
		return nil, fmt.Errorf("stage Chromium profile adoption: %w", err)
	}
	defer func() { _ = os.RemoveAll(staging) }()
	digest, err := writeManifest(filepath.Join(staging, manifestName), manifest)
	if err != nil {
		return nil, err
	}
	if err := os.Rename(staging, destination); err != nil {
		return nil, fmt.Errorf("publish staged Chromium profile: %w", err)
	}
	return digest, nil
}

func (store *Store) Inspect(identityID int64, generation uint64) (Manifest, []byte, error) {
	var manifest Manifest
	path, err := store.manifestPath(identityID, generation)
	if err != nil {
		return manifest, nil, err
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return manifest, nil, err
	}
	if err := json.Unmarshal(body, &manifest); err != nil {
		return manifest, nil, fmt.Errorf("parse profile manifest: %w", err)
	}
	if manifest.IdentityID != identityID || manifest.Generation != generation || manifest.IdentityRevision <= 0 || manifest.ChromiumRevision == "" {
		return manifest, nil, errors.New("profile manifest does not match its durable location")
	}
	sum := sha256.Sum256(body)
	return manifest, sum[:], nil
}

func DigestHex(digest []byte) string { return hex.EncodeToString(digest) }
