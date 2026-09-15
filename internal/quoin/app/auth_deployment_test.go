package app

// Deployment preset tests for configureAuthenticationDeployment: canonical
// schema in a temp SQLite database, a fake 32-byte root key, and no outbound
// delivery — senders are only constructed by validation, never dialed.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/quoin/execution"

	"github.com/Suknna/quoin/internal/contract"
	gencontracts "github.com/Suknna/quoin/internal/gen/contracts"
	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/secrets"
	_ "modernc.org/sqlite"
)

func newAuthDeploymentTestApplication(t *testing.T) (*apiServer, *sql.DB) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "authdeployment.db")
	db, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(gencontracts.SchemaSQL); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.Exec(`INSERT INTO root_key_state(id,binding_revision,verifier_nonce,verifier_ciphertext,bound_at) VALUES(1,1,randomblob(12),randomblob(32),?)`, now); err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	for index := range key {
		key[index] = byte(0x40 + index)
	}
	application := &apiServer{
		db: db,
		rootKey: func() ([]byte, error) {
			return key, nil
		},
	}
	return application, db
}

// assertAuthDeliverySettingsEmpty fails when any delivery settings row exists.
func assertAuthDeliverySettingsEmpty(t *testing.T, db *sql.DB) {
	t.Helper()
	var one int
	if err := db.QueryRow(`SELECT 1 FROM auth_delivery_settings LIMIT 1`).Scan(&one); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("delivery settings must be empty, got %v", err)
	}
}

func authDeploymentConfig(secretsPath string) contract.QuoinConfig {
	return contract.QuoinConfig{
		Authentication: &contract.QuoinAuthenticationConfig{
			SecretsFile: secretsPath,
			Configuration: &contract.AuthDeliveryDeployment{
				Email: &contract.AuthDeliveryChannel{
					Kind:        "smtp",
					Host:        "smtp.example.com",
					Port:        587,
					From:        "noreply@quoin.example.com",
					Username:    "quoin",
					PasswordRef: "smtp-password",
					TLSMode:     "starttls",
				},
				SMS: &contract.AuthDeliveryChannel{
					Kind:          "webhook",
					URL:           "https://sms-gateway.example.com/send",
					SecretHeaders: map[string]string{"X-Api-Key": "sms-api-key"},
					Encoding:      "json",
				},
			},
		},
	}
}

func writeAuthDeploymentSecrets(t *testing.T, content string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "auth-delivery-secrets.yaml")
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	return path
}

const validAuthDeploymentSecrets = `smtp-password: smtp-secret-1
sms-api-key: sms-secret-1
`

func storedAuthDeliveryRow(t *testing.T, db *sql.DB, key []byte) (source, configuration string, values map[string]string, version int64) {
	t.Helper()
	var nonce, ciphertext []byte
	var binding int
	if err := db.QueryRow(`SELECT source,configuration_json,secret_nonce,secret_ciphertext,root_binding_revision,row_version FROM auth_delivery_settings WHERE id=1`).
		Scan(&source, &configuration, &nonce, &ciphertext, &binding, &version); err != nil {
		t.Fatal(err)
	}
	plain, err := secrets.OpenSetting(key, "auth.delivery", version, binding, &secrets.Envelope{Nonce: nonce, Ciphertext: ciphertext})
	if err != nil {
		t.Fatal(err)
	}
	values = map[string]string{}
	if err := json.Unmarshal(plain, &values); err != nil {
		t.Fatal(err)
	}
	return source, configuration, values, version
}

func countAuthDeliveryAuditEvents(t *testing.T, db *sql.DB) int {
	t.Helper()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action='auth.delivery.deploy' AND actor_type='system' AND actor_id=0`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestConfigureAuthenticationDeploymentAppliesPresetOnce(t *testing.T) {
	application, db := newAuthDeploymentTestApplication(t)
	path := writeAuthDeploymentSecrets(t, validAuthDeploymentSecrets, 0o600)

	// Absent section on a fresh database is a no-op.
	if err := application.configureAuthenticationDeployment(context.Background(), contract.QuoinConfig{}); err != nil {
		t.Fatalf("absent section on fresh database: %v", err)
	}
	assertAuthDeliverySettingsEmpty(t, db)

	config := authDeploymentConfig(path)
	if err := application.configureAuthenticationDeployment(context.Background(), config); err != nil {
		t.Fatalf("apply preset: %v", err)
	}
	key, _ := application.rootKey()
	source, configuration, values, version := storedAuthDeliveryRow(t, db, key)
	if source != "deployment" {
		t.Fatalf("source = %q", source)
	}
	if !strings.Contains(configuration, `"email"`) || !strings.Contains(configuration, "smtp.example.com") {
		t.Fatalf("unexpected stored configuration: %s", configuration)
	}
	if values["smtp-password"] != "smtp-secret-1" || values["sms-api-key"] != "sms-secret-1" {
		t.Fatalf("unexpected stored secrets: %v", values)
	}
	if version != 1 {
		t.Fatalf("row_version = %d, want 1", version)
	}
	if countAuthDeliveryAuditEvents(t, db) != 1 {
		t.Fatal("the deployment write must be audited for the system principal")
	}

	// Re-running the identical preset neither advances row_version nor
	// duplicates the durable change.
	if err := application.configureAuthenticationDeployment(context.Background(), config); err != nil {
		t.Fatalf("re-apply identical preset: %v", err)
	}
	_, _, _, version = storedAuthDeliveryRow(t, db, key)
	if version != 1 {
		t.Fatalf("identical preset advanced row_version to %d", version)
	}
}

func TestConfigureAuthenticationDeploymentDetectsChanges(t *testing.T) {
	application, db := newAuthDeploymentTestApplication(t)
	path := writeAuthDeploymentSecrets(t, validAuthDeploymentSecrets, 0o600)
	config := authDeploymentConfig(path)
	if err := application.configureAuthenticationDeployment(context.Background(), config); err != nil {
		t.Fatal(err)
	}

	config.Authentication.Configuration.Email.Host = "smtp2.example.com"
	path = writeAuthDeploymentSecrets(t, validAuthDeploymentSecrets+"extra-key: extra-key-value\n", 0o600)
	config.Authentication.SecretsFile = path
	config.Authentication.Configuration.SMS.SecretHeaders["X-Extra"] = "extra-key"
	if err := application.configureAuthenticationDeployment(context.Background(), config); err != nil {
		t.Fatalf("apply changed preset: %v", err)
	}
	key, _ := application.rootKey()
	_, configuration, values, version := storedAuthDeliveryRow(t, db, key)
	if version != 2 {
		t.Fatalf("row_version = %d, want 2 after a change", version)
	}
	if !strings.Contains(configuration, "smtp2.example.com") {
		t.Fatalf("configuration change was not stored: %s", configuration)
	}
	if values["extra-key"] != "extra-key-value" || values["smtp-password"] != "smtp-secret-1" {
		t.Fatalf("unexpected stored secrets after change: %v", values)
	}
}

func TestConfigureAuthenticationDeploymentRefusesAdministratorSettings(t *testing.T) {
	application, db := newAuthDeploymentTestApplication(t)
	path := writeAuthDeploymentSecrets(t, validAuthDeploymentSecrets, 0o600)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	envelope, err := secrets.SealSetting(mustTestRootKey(application), "auth.delivery", 3, 1, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO auth_delivery_settings(id,source,configuration_json,secret_nonce,secret_ciphertext,root_binding_revision,row_version,updated_at) VALUES(1,'administrator','{"email":{"kind":"smtp","host":"admin.example.com","port":587,"from":"a@b.c"}}',?,?,1,3,?)`, envelope.Nonce, envelope.Ciphertext, now); err != nil {
		t.Fatal(err)
	}

	err = application.configureAuthenticationDeployment(context.Background(), authDeploymentConfig(path))
	if err == nil || !strings.Contains(err.Error(), "administrator-managed") {
		t.Fatalf("deployment must refuse administrator-managed settings, got %v", err)
	}
	var source string
	if err := db.QueryRow(`SELECT source FROM auth_delivery_settings WHERE id=1`).Scan(&source); err != nil || source != "administrator" {
		t.Fatalf("administrator settings were overwritten: %v %q", err, source)
	}
}

func TestConfigureAuthenticationDeploymentNilSectionNeverOverwritesRuntime(t *testing.T) {
	application, db := newAuthDeploymentTestApplication(t)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	envelope, err := secrets.SealSetting(mustTestRootKey(application), "auth.delivery", 1, 1, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO auth_delivery_settings(id,source,configuration_json,secret_nonce,secret_ciphertext,root_binding_revision,row_version,updated_at) VALUES(1,'administrator','{"email":{"kind":"smtp","host":"admin.example.com","port":587,"from":"a@b.c"}}',?,?,1,1,?)`, envelope.Nonce, envelope.Ciphertext, now); err != nil {
		t.Fatal(err)
	}
	if err := application.configureAuthenticationDeployment(context.Background(), contract.QuoinConfig{}); err != nil {
		t.Fatalf("absent section with administrator settings must be a no-op: %v", err)
	}
	var configuration string
	if err := db.QueryRow(`SELECT configuration_json FROM auth_delivery_settings WHERE id=1`).Scan(&configuration); err != nil || !strings.Contains(configuration, "admin.example.com") {
		t.Fatalf("runtime settings were touched: %v %q", err, configuration)
	}
}

func TestConfigureAuthenticationDeploymentFailsOnRemovedDeploymentSection(t *testing.T) {
	application, db := newAuthDeploymentTestApplication(t)
	path := writeAuthDeploymentSecrets(t, validAuthDeploymentSecrets, 0o600)
	if err := application.configureAuthenticationDeployment(context.Background(), authDeploymentConfig(path)); err != nil {
		t.Fatal(err)
	}

	err := application.configureAuthenticationDeployment(context.Background(), contract.QuoinConfig{})
	if err == nil || !strings.Contains(err.Error(), "no authentication section") {
		t.Fatalf("orphaned deployment settings must fail explicitly, got %v", err)
	}
	var source string
	if err := db.QueryRow(`SELECT source FROM auth_delivery_settings WHERE id=1`).Scan(&source); err != nil || source != "deployment" {
		t.Fatalf("deployment settings were silently dropped: %v %q", err, source)
	}
}

func TestConfigureAuthenticationDeploymentRequiresConfiguration(t *testing.T) {
	application, db := newAuthDeploymentTestApplication(t)
	path := writeAuthDeploymentSecrets(t, validAuthDeploymentSecrets, 0o600)
	config := contract.QuoinConfig{Authentication: &contract.QuoinAuthenticationConfig{SecretsFile: path}}
	if err := application.configureAuthenticationDeployment(context.Background(), config); err == nil {
		t.Fatal("a secrets file without delivery channels must be rejected")
	}
	assertAuthDeliverySettingsEmpty(t, db)
}

func TestConfigureAuthenticationDeploymentValidatesChannelsAndReferences(t *testing.T) {
	missingRef := writeAuthDeploymentSecrets(t, "sms-api-key: sms-secret-1\n", 0o600)
	brokenChannel := authDeploymentConfig(missingRef)
	brokenChannel.Authentication.Configuration.Email.PasswordRef = "absent-ref"
	application, db := newAuthDeploymentTestApplication(t)
	err := application.configureAuthenticationDeployment(context.Background(), brokenChannel)
	if err == nil {
		t.Fatal("missing secret reference must fail validation")
	}
	assertAuthDeliverySettingsEmpty(t, db)

	unreachableChannel := authDeploymentConfig(missingRef)
	unreachableChannel.Authentication.Configuration.Email.Host = ""
	application2, _ := newAuthDeploymentTestApplication(t)
	if err := application2.configureAuthenticationDeployment(context.Background(), unreachableChannel); err == nil {
		t.Fatal("an smtp channel without host must fail validation")
	}
}

func TestConfigureAuthenticationDeploymentSecretsFileRules(t *testing.T) {
	application, _ := newAuthDeploymentTestApplication(t)

	t.Run("accepts json as yaml subset", func(t *testing.T) {
		path := writeAuthDeploymentSecrets(t, `{"smtp-password":"json-secret"}`, 0o600)
		config := authDeploymentConfig(path)
		config.Authentication.Configuration.SMS = nil
		if err := application.configureAuthenticationDeployment(context.Background(), config); err != nil {
			t.Fatalf("json secrets file: %v", err)
		}
	})

	t.Run("rejects wrong mode", func(t *testing.T) {
		path := writeAuthDeploymentSecrets(t, validAuthDeploymentSecrets, 0o644)
		if err := application.configureAuthenticationDeployment(context.Background(), authDeploymentConfig(path)); err == nil || !strings.Contains(err.Error(), "0600") {
			t.Fatalf("mode 0644 must be rejected, got %v", err)
		}
	})

	t.Run("rejects symlink", func(t *testing.T) {
		real := writeAuthDeploymentSecrets(t, validAuthDeploymentSecrets, 0o600)
		link := filepath.Join(t.TempDir(), "link.yaml")
		if err := os.Symlink(real, link); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if err := application.configureAuthenticationDeployment(context.Background(), authDeploymentConfig(link)); err == nil || !strings.Contains(err.Error(), "regular file") {
			t.Fatalf("symlink must be rejected, got %v", err)
		}
	})

	t.Run("rejects oversize file", func(t *testing.T) {
		path := writeAuthDeploymentSecrets(t, "big: "+strings.Repeat("x", 1<<20), 0o600)
		if err := application.configureAuthenticationDeployment(context.Background(), authDeploymentConfig(path)); err == nil || !strings.Contains(err.Error(), "1 MiB") {
			t.Fatalf("oversized secrets file must be rejected, got %v", err)
		}
	})

	t.Run("rejects duplicate keys anchors and non strings", func(t *testing.T) {
		for name, content := range map[string]string{
			"duplicate":     "a: \"1\"\na: \"2\"\n",
			"anchor":        "base: &a secret\nref: *a\n",
			"non-string":    "a:\n  nested: value\n",
			"empty-value":   "a: \"\"\n",
			"not-a-mapping": "- a\n- b\n",
		} {
			path := writeAuthDeploymentSecrets(t, content, 0o600)
			if err := application.configureAuthenticationDeployment(context.Background(), authDeploymentConfig(path)); err == nil {
				t.Fatalf("%s secrets file must be rejected", name)
			}
		}
	})

	t.Run("empty file yields no values", func(t *testing.T) {
		path := writeAuthDeploymentSecrets(t, "", 0o600)
		config := authDeploymentConfig(path)
		config.Authentication.Configuration.SMS = nil
		config.Authentication.Configuration.Email.PasswordRef = ""
		config.Authentication.Configuration.Email.Username = ""
		if err := application.configureAuthenticationDeployment(context.Background(), config); err != nil {
			t.Fatalf("empty secrets file with reference-free channels: %v", err)
		}
	})
}

func mustTestRootKey(application *apiServer) []byte {
	key, err := application.rootKey()
	if err != nil {
		panic(err)
	}
	return key
}

func newAuditRetentionTestDB(t *testing.T) (*sql.DB, *auth.Service) {
	t.Helper()
	dbPath := t.TempDir() + "/retention.db"
	db, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(gencontracts.SchemaSQL); err != nil {
		t.Fatal(err)
	}
	// The auth/audit migration seeds the retention singleton before the
	// bootstrap runs; mirror it exactly.
	if _, err := db.Exec(`INSERT OR IGNORE INTO audit_retention(id,retention_months,cleanup_enabled,row_version) VALUES(1,6,0,1)`); err != nil {
		t.Fatal(err)
	}
	service, err := auth.NewService(db)
	if err != nil {
		t.Fatal(err)
	}
	// Production installs the real read-only pool; the raw harness opens an
	// independent mode=ro/query_only pool over the same file with the same
	// cleanup lifetime.
	reader, err := execution.OpenReadOnly(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	if err := service.SetReader(reader); err != nil {
		t.Fatal(err)
	}
	return db, service
}

func auditRetentionRow(t *testing.T, db *sql.DB) (months, rowVersion int, updatedBy any) {
	t.Helper()
	if err := db.QueryRow(`SELECT retention_months,row_version,updated_by_type FROM audit_retention WHERE id=1`).Scan(&months, &rowVersion, &updatedBy); err != nil {
		t.Fatal(err)
	}
	return months, rowVersion, updatedBy
}

// TestPrepareAuthenticationBootstrapAuditRetention pins the deployment
// retention contract: the value from the deployment configuration is applied
// exactly once, only on a fresh database, and only while the audit_retention
// singleton has never been operator-configured.
func TestPrepareAuthenticationBootstrapAuditRetention(t *testing.T) {
	t.Run("applies deployment value on fresh database", func(t *testing.T) {
		db, service := newAuditRetentionTestDB(t)
		if err := prepareAuthenticationBootstrap(context.Background(), service, db, t.TempDir(), 12); err != nil {
			t.Fatal(err)
		}
		months, rowVersion, updatedBy := auditRetentionRow(t, db)
		if months != 12 || rowVersion != 1 || updatedBy != nil {
			t.Fatalf("retention = %d/%v/%v, want 12/1/<nil>", months, rowVersion, updatedBy)
		}
	})

	t.Run("zero value keeps the schema default", func(t *testing.T) {
		db, service := newAuditRetentionTestDB(t)
		if err := prepareAuthenticationBootstrap(context.Background(), service, db, t.TempDir(), 0); err != nil {
			t.Fatal(err)
		}
		if months, _, _ := auditRetentionRow(t, db); months != 6 {
			t.Fatalf("retention = %d, want the untouched default 6", months)
		}
	})

	t.Run("rejects values outside the schema range", func(t *testing.T) {
		for _, months := range []int{5, 241} {
			db, service := newAuditRetentionTestDB(t)
			err := prepareAuthenticationBootstrap(context.Background(), service, db, t.TempDir(), months)
			if err == nil || !strings.Contains(err.Error(), "six and 240") {
				t.Fatalf("retention %d must be rejected with the range error, got %v", months, err)
			}
		}
	})

	t.Run("never overrides operator configuration", func(t *testing.T) {
		db, service := newAuditRetentionTestDB(t)
		if _, err := db.Exec(`UPDATE audit_retention SET retention_months=9,row_version=2,updated_by_type='user',updated_by_id=1,updated_at=?`, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
		if err := prepareAuthenticationBootstrap(context.Background(), service, db, t.TempDir(), 12); err != nil {
			t.Fatal(err)
		}
		if months, rowVersion, _ := auditRetentionRow(t, db); months != 9 || rowVersion != 2 {
			t.Fatalf("retention = %d/%d, want the operator value 9/2", months, rowVersion)
		}
	})

	t.Run("never reapplies on restart", func(t *testing.T) {
		db, service := newAuditRetentionTestDB(t)
		directory := t.TempDir()
		if err := prepareAuthenticationBootstrap(context.Background(), service, db, directory, 12); err != nil {
			t.Fatal(err)
		}
		// A restart runs against a database that now has the bootstrap
		// administrator: a different deployment value must not apply.
		if err := prepareAuthenticationBootstrap(context.Background(), service, db, directory, 18); err != nil {
			t.Fatalf("restart bootstrap must succeed: %v", err)
		}
		if months, _, _ := auditRetentionRow(t, db); months != 12 {
			t.Fatalf("retention = %d, want the first-bootstrap value 12", months)
		}
	})
}
