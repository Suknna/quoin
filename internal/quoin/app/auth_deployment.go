package app

// Deployment-side bootstrap for verification delivery settings
// (authentication design section 8): main calls
// configureAuthenticationDeployment before configureAuthentication so a
// deployment YAML preset is in place before any administrator flow runs.
//
// Source rules, decided explicitly with no hidden fallback:
//   - YAML section absent: administrator-managed settings stay untouched;
//     but deployment-sourced settings without their YAML section are an
//     orphaned preset and fail startup until the operator restores the
//     section. Restoring the YAML section is the only supported resolution:
//     no adoption endpoint exists and none is promised — an explicit
//     adoption migration may be designed later, until then the startup
//     failure is the contract.
//   - YAML section present: deployment settings are written/updated under
//     the "deployment" source; existing administrator-managed settings are
//     never silently overwritten (explicit conflict error instead).
//   - The stored row_version only advances when the configuration or the
//     decrypted secret values actually changed; comparison happens in
//     memory and secret values never reach errors or logs.
//
// Audit retention YAML integration lives in prepareAuthenticationBootstrap
// (auth_bootstrap.go): main maps config.Audit.RetentionMonths — absent
// selects the six-month default — and passes it as an optional value that is
// applied only while the database is still fresh (no users) and the
// audit_retention singleton is untouched (row_version 1, updated_by_type
// NULL, i.e. never operator-configured). The guarded update therefore applies
// the deployment value at most once, never on a restart, never over an
// operator decision, and the supported 6..240 range matches the
// deployment-config schema.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"time"

	"github.com/Suknna/quoin/internal/contract"
	"github.com/Suknna/quoin/internal/quoin/audit"
	"github.com/Suknna/quoin/internal/quoin/execution"
	"github.com/Suknna/quoin/internal/quoin/secrets"
	"gopkg.in/yaml.v3"
)

// deploymentAuthDeliveryOperation is the audited system operation that stores
// the deployment preset.
const deploymentAuthDeliveryOperation = "auth.delivery.deploy"

const maxAuthDeliverySecretsFileBytes = 1 << 20

// configureAuthenticationDeployment applies the optional deployment preset.
// A nil Authentication section never overwrites runtime settings; see the
// source rules in the file comment.
func (application *apiServer) configureAuthenticationDeployment(ctx context.Context, config contract.QuoinConfig) error {
	if config.Authentication == nil {
		return application.ensureNoOrphanedDeploymentDelivery(ctx)
	}
	if config.Authentication.Configuration == nil {
		return errors.New("deployment authentication delivery requires configuration channels")
	}
	values, err := readAuthDeliverySecretsFile(config.Authentication.SecretsFile)
	if err != nil {
		return err
	}
	deliveryConfig, err := deploymentDeliveryConfiguration(config.Authentication.Configuration)
	if err != nil {
		return fmt.Errorf("decode deployment authentication delivery: %w", err)
	}
	if err := validateAuthDelivery(deliveryConfig, values); err != nil {
		return fmt.Errorf("deployment authentication delivery is invalid: %w", err)
	}
	key, err := application.rootKey()
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(deliveryConfig)
	if err != nil {
		return fmt.Errorf("encode deployment authentication delivery: %w", err)
	}

	registry := execution.NewRegistry()
	op, err := registry.Register(execution.Operation{
		Name:       deploymentAuthDeliveryOperation,
		Class:      execution.ClassWrite,
		ObjectType: "auth_delivery_settings",
		Authorize: func(ctx context.Context, _ *execution.Tx) error {
			meta, ok := execution.FromContext(ctx)
			if !ok || meta.Actor.Kind != execution.PrincipalSystem || meta.Actor.ID != 0 || meta.Source.Kind != execution.SourceInternal {
				return errors.New("deployment authentication delivery is restricted to the system principal")
			}
			return nil
		},
	})
	if err != nil {
		return err
	}
	correlationID, err := execution.NewCorrelationID()
	if err != nil {
		return err
	}
	metaCtx, err := execution.WithMetadata(ctx, execution.Metadata{
		CorrelationID: correlationID,
		Actor:         execution.Principal{Kind: execution.PrincipalSystem, ID: 0},
		Source:        execution.Source{Kind: execution.SourceInternal},
	})
	if err != nil {
		return err
	}
	_, err = execution.Execute(metaCtx, execution.NewRunner(application.db, registry, audit.NewWriter()), op, func(tx *execution.Tx) (struct{ ID int64 }, error) {
		var binding int
		if err := tx.QueryRowContext(metaCtx, `SELECT binding_revision FROM root_key_state WHERE id=1`).Scan(&binding); err != nil {
			return struct{ ID int64 }{}, fmt.Errorf("read root key binding: %w", err)
		}
		var source, storedConfig string
		var nonce, ciphertext []byte
		var storedBinding int
		var version int64
		scanErr := tx.QueryRowContext(metaCtx, `SELECT source,configuration_json,secret_nonce,secret_ciphertext,root_binding_revision,row_version FROM auth_delivery_settings WHERE id=1`).
			Scan(&source, &storedConfig, &nonce, &ciphertext, &storedBinding, &version)
		now := time.Now().UTC().Format(time.RFC3339Nano)
		switch {
		case errors.Is(scanErr, sql.ErrNoRows):
			envelope, err := secrets.SealSetting(key, "auth.delivery", 1, binding, mustJSONSecrets(values))
			if err != nil {
				return struct{ ID int64 }{}, err
			}
			if _, err := tx.ExecContext(metaCtx, `INSERT INTO auth_delivery_settings(id,source,configuration_json,secret_nonce,secret_ciphertext,root_binding_revision,row_version,updated_at) VALUES(1,'deployment',?,?,?,?,1,?)`, string(encoded), envelope.Nonce, envelope.Ciphertext, binding, now); err != nil {
				return struct{ ID int64 }{}, err
			}
			return struct{ ID int64 }{1}, nil
		case scanErr != nil:
			return struct{ ID int64 }{}, scanErr
		}
		if source == "administrator" {
			// Explicit conflict: deployment settings must never silently
			// overwrite administrator-managed runtime settings.
			return struct{ ID int64 }{}, errors.New("deployment authentication delivery conflicts with existing administrator-managed delivery settings; remove the deployment authentication section to keep the runtime configuration")
		}
		configChanged, err := authDeliveryConfigurationChanged(storedConfig, string(encoded))
		if err != nil {
			return struct{ ID int64 }{}, err
		}
		secretsChanged, err := authDeliverySecretsChanged(key, nonce, ciphertext, storedBinding, version, values)
		if err != nil {
			return struct{ ID int64 }{}, err
		}
		if !configChanged && !secretsChanged {
			return struct{ ID int64 }{1}, nil
		}
		envelope, err := secrets.SealSetting(key, "auth.delivery", version+1, binding, mustJSONSecrets(values))
		if err != nil {
			return struct{ ID int64 }{}, err
		}
		result, err := tx.ExecContext(metaCtx, `UPDATE auth_delivery_settings SET configuration_json=?,secret_nonce=?,secret_ciphertext=?,root_binding_revision=?,row_version=?,updated_at=? WHERE id=1 AND source='deployment' AND row_version=?`, string(encoded), envelope.Nonce, envelope.Ciphertext, binding, version+1, now, version)
		if err != nil {
			return struct{ ID int64 }{}, err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return struct{ ID int64 }{}, err
		}
		if affected != 1 {
			return struct{ ID int64 }{}, &execution.Rejection{Code: "row_version_conflict", ObjectID: 1}
		}
		return struct{ ID int64 }{1}, nil
	}, func(v struct{ ID int64 }) int64 { return v.ID })
	if err != nil {
		return fmt.Errorf("store deployment authentication delivery: %w", err)
	}
	return nil
}

// ensureNoOrphanedDeploymentDelivery implements the no-hidden-fallback rule
// for a removed deployment section: administrator settings need nothing;
// deployment-sourced settings without their YAML section fail startup until
// the operator restores the section or explicitly adopts the configuration.
func (application *apiServer) ensureNoOrphanedDeploymentDelivery(ctx context.Context) error {
	var orphaned int
	if err := application.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM auth_delivery_settings WHERE id=1 AND source='deployment'`).Scan(&orphaned); err != nil {
		return fmt.Errorf("read authentication delivery settings: %w", err)
	}
	if orphaned != 0 {
		return errors.New("authentication delivery settings were deployed from a previous deployment configuration, but the current configuration has no authentication section; restore the section to keep the deployed delivery preset (restoring the YAML is the only supported resolution — runtime adoption is not available and is deliberately not promised)")
	}
	return nil
}

// deploymentDeliveryConfiguration maps the contract preset onto the stored
// settings shape through JSON, keeping the two documents wire-compatible.
func deploymentDeliveryConfiguration(deployment *contract.AuthDeliveryDeployment) (authDeliveryConfiguration, error) {
	encoded, err := json.Marshal(deployment)
	if err != nil {
		return authDeliveryConfiguration{}, err
	}
	var config authDeliveryConfiguration
	if err := json.Unmarshal(encoded, &config); err != nil {
		return authDeliveryConfiguration{}, err
	}
	return config, nil
}

// authDeliveryConfigurationChanged compares canonical JSON documents so
// key-order differences never count as changes.
func authDeliveryConfigurationChanged(stored, current string) (bool, error) {
	var left, right any
	if err := json.Unmarshal([]byte(stored), &left); err != nil {
		return false, fmt.Errorf("decode stored authentication delivery configuration: %w", err)
	}
	if err := json.Unmarshal([]byte(current), &right); err != nil {
		return false, fmt.Errorf("decode deployment authentication delivery configuration: %w", err)
	}
	return !reflect.DeepEqual(left, right), nil
}

// authDeliverySecretsChanged compares the decrypted stored values with the
// deployment values in memory only; values never appear in errors.
func authDeliverySecretsChanged(key []byte, nonce, ciphertext []byte, binding int, revision int64, current map[string]string) (bool, error) {
	if len(nonce) == 0 && len(ciphertext) == 0 {
		return len(current) > 0, nil
	}
	plain, err := secrets.OpenSetting(key, "auth.delivery", revision, binding, &secrets.Envelope{Nonce: nonce, Ciphertext: ciphertext})
	if err != nil {
		return false, fmt.Errorf("open stored authentication delivery secrets: %w", err)
	}
	var stored map[string]string
	if err := json.Unmarshal(plain, &stored); err != nil {
		return false, fmt.Errorf("decode stored authentication delivery secrets: %w", err)
	}
	return !reflect.DeepEqual(stored, current), nil
}

func mustJSONSecrets(values map[string]string) []byte {
	if values == nil {
		values = map[string]string{}
	}
	encoded, err := json.Marshal(values)
	if err != nil {
		// map[string]string cannot fail to marshal.
		panic(err)
	}
	return encoded
}

// readAuthDeliverySecretsFile loads the optional secret reference mapping.
// The file must be a regular, non-symlink, mode-0600 file of at most 1 MiB
// containing a strict single-document JSON or YAML mapping of reference
// names to string values.
func readAuthDeliverySecretsFile(path string) (map[string]string, error) {
	if path == "" {
		return map[string]string{}, nil
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("read authentication delivery secrets file: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, errors.New("authentication delivery secrets file must be a regular file, not a symlink")
	}
	if info.Mode().Perm() != 0o600 {
		return nil, fmt.Errorf("authentication delivery secrets file must have mode 0600, got %04o", info.Mode().Perm())
	}
	if info.Size() > maxAuthDeliverySecretsFileBytes {
		return nil, errors.New("authentication delivery secrets file exceeds 1 MiB")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read authentication delivery secrets file: %w", err)
	}
	return decodeAuthDeliverySecretsMap(data)
}

// decodeAuthDeliverySecretsMap parses the secrets mapping strictly: one
// document (JSON is accepted as a YAML subset), no anchors or aliases, no
// duplicate keys, string keys and string values only.
func decodeAuthDeliverySecretsMap(data []byte) (map[string]string, error) {
	var node yaml.Node
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&node); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse authentication delivery secrets file: %w", err)
	}
	if node.Kind == 0 {
		return map[string]string{}, nil
	}
	var trailing yaml.Node
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("authentication delivery secrets file must contain one document")
		}
		return nil, fmt.Errorf("parse authentication delivery secrets file: %w", err)
	}
	// Decode into a Node yields the document; unwrap to the single content
	// node before checking the mapping shape.
	if node.Kind == yaml.DocumentNode {
		if len(node.Content) != 1 {
			return nil, errors.New("authentication delivery secrets file must contain one document")
		}
		content := *node.Content[0]
		node = content
	}
	if node.Anchor != "" || node.Alias != nil {
		return nil, errors.New("authentication delivery secrets file must not use anchors or aliases")
	}
	if node.Kind != yaml.MappingNode {
		return nil, errors.New("authentication delivery secrets file must contain a mapping of reference names to values")
	}
	seen := map[string]struct{}{}
	for index := 0; index < len(node.Content); index += 2 {
		key, value := node.Content[index], node.Content[index+1]
		if key.Kind != yaml.ScalarNode || key.Tag != "!!str" {
			return nil, errors.New("authentication delivery secrets reference names must be strings")
		}
		if _, exists := seen[key.Value]; exists {
			return nil, fmt.Errorf("authentication delivery secrets file defines reference %q twice", key.Value)
		}
		seen[key.Value] = struct{}{}
		if value.Kind != yaml.ScalarNode || value.Tag != "!!str" || value.Value == "" {
			return nil, fmt.Errorf("authentication delivery secret %q must have a non-empty string value", key.Value)
		}
	}
	var values map[string]string
	if err := node.Decode(&values); err != nil {
		return nil, fmt.Errorf("decode authentication delivery secrets file: %w", err)
	}
	return values, nil
}
