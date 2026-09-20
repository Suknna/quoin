package app

// Secrets-file loading for the login channels (ADR-0010). The OIDC client
// secret lives in the read-only secrets reference file mounted by the
// deployment; it never enters the YAML config, logs or /api/v1/auth/config.
// The loader keeps the strict delivery-era rules: a plain non-symlink file,
// mode 0600, at most 1 MiB, one JSON/YAML document mapping non-empty strings
// to non-empty strings, without anchors, aliases or duplicate keys.

import (
	"encoding/json"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// readAuthSecretsFile returns the reference map of the login-channel secrets
// file. An empty path means no secrets were declared.
func readAuthSecretsFile(path string) (map[string]string, error) {
	if path == "" {
		return nil, nil
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("auth secrets file: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("auth secrets file %s must be a regular file", path)
	}
	if info.Mode().Perm() != 0o600 {
		return nil, fmt.Errorf("auth secrets file %s must have mode 0600", path)
	}
	if info.Size() > 1<<20 {
		return nil, fmt.Errorf("auth secrets file %s exceeds 1 MiB", path)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("auth secrets file: %w", err)
	}
	var parsed map[string]any
	if err := yaml.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("auth secrets file %s must be one YAML document: %w", path, err)
	}
	secrets := make(map[string]string, len(parsed))
	for key, value := range parsed {
		if key == "" {
			return nil, fmt.Errorf("auth secrets file %s has an empty reference name", path)
		}
		text, ok := value.(string)
		if !ok || text == "" {
			return nil, fmt.Errorf("auth secrets file %s: reference %q must map to a non-empty string", path, key)
		}
		secrets[key] = text
	}
	if len(secrets) == 0 {
		return nil, fmt.Errorf("auth secrets file %s is empty", path)
	}
	return secrets, nil
}

// unusedJSON keeps the decode contract honest for JSON files (yaml.v3 is a
// JSON superset, so the single parser above covers both formats).
var _ = json.Unmarshal
