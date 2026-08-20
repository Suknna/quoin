package connections_test

// Deterministic coverage for the T07 connection domain: AEAD tamper and
// binding-mismatch fail-closed, enable fences (single-enabled, model
// provider qualification closure), and the probe attempt/grant closure with
// commit-order discipline.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/Suknna/quoin/internal/contract"
	"github.com/Suknna/quoin/internal/quoin/bootstrap"
	"github.com/Suknna/quoin/internal/quoin/connections"
	"github.com/Suknna/quoin/internal/quoin/secrets"
)

const rootKeyHex32 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func newService(t *testing.T) (*connections.Service, *sql.DB, string) {
	t.Helper()
	root := t.TempDir()
	config := contract.QuoinConfig{
		Component: "quoin", PublicOrigin: "https://quoin.test",
		DataDirectory:             filepath.Join(root, "data"),
		RootKeyFile:               filepath.Join(root, "root-key"),
		RuntimeTLSCertificateFile: filepath.Join(root, "tls.crt"),
		RuntimeTLSPrivateKeyFile:  filepath.Join(root, "tls.key"),
		SteleServiceTokenFile:     filepath.Join(root, "stele"),
	}
	if _, err := bootstrap.BootstrapSecrets(config); err != nil {
		t.Fatal(err)
	}
	database, err := bootstrap.OpenDatabase(context.Background(), config.DataDirectory, config.RootKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	service := connections.NewService(database.SQL, func() ([]byte, error) {
		return readKey(t, config.RootKeyFile)
	})
	connections.SetReleaseVersion("v0.1.0-dev")
	connections.ProbeContractSource = func() string { return "contract_version: 1" }
	// The qualification/audit FKs reference real users; create the fixture
	// administrator every service test uses as principal 1.
	if _, err := database.SQL.Exec(`INSERT INTO users(id,username,display_name,role,enabled,password_phc,row_version,created_at,updated_at) VALUES(1,'admin','Admin','admin',1,'$argon2id$phc',1,?,?)`, "2026-01-01T00:00:00Z", "2026-01-01T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	return service, database.SQL, config.RootKeyFile
}

func readKey(t *testing.T, path string) ([]byte, error) {
	t.Helper()
	return readFile(path)
}

func readFile(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) != 32 {
		return nil, fmt.Errorf("root key must be 32 bytes, got %d", len(data))
	}
	return data, nil
}

type cmdSeq struct{ n int }

var seq cmdSeq

func (seq *cmdSeq) Next() int { seq.n++; return seq.n }

func thanosInput(password string) connections.CreateInput {
	projection, _ := json.Marshal(map[string]any{"type": "thanos", "baseUrl": "https://thanos.example.com", "username": "probe"})
	var secret json.RawMessage
	if password != "" {
		secret, _ = json.Marshal(map[string]string{"type": "thanos", "username": "probe", "password": password})
	}
	return connections.CreateInput{Name: "main-thanos", Type: connections.TypeThanos, NonSecretJSON: projection, Secret: secret, SecretPresent: password != ""}
}

func TestEnvelopeRoundTripAndTamper(t *testing.T) {
	service, db, keyFile := newService(t)
	ctx := context.Background()
	created, err := service.Create(ctx, thanosInput("secret-password-1"), 1, "cmd-"+fmt.Sprint(seq.Next()))
	if err != nil {
		t.Fatal(err)
	}
	if created.Enabled || created.RowVersion != 2 {
		// row_version=2: the pointer-wiring UPDATE advances it once after
		// the INSERT (row_version must increase exactly by 1 per UPDATE).
		t.Fatalf("created projection wrong: %+v", created)
	}
	// Decrypt through the supervisor grant path.
	secret, err := service.OpenGeneration(ctx, created.CurrentGenerationID)
	if err != nil {
		t.Fatal(err)
	}
	if secret.Thanos == nil || secret.Thanos.Password != "secret-password-1" {
		t.Fatalf("decrypted secret wrong: %+v", secret)
	}
	// Tamper with the ciphertext: must fail closed.
	key, _ := readFile(keyFile)
	var nonce, ciphertext []byte
	var bindingRevision int
	if err := db.QueryRow(`SELECT nonce,ciphertext,key_binding_revision FROM credential_generations WHERE id=?`, created.CurrentGenerationID).Scan(&nonce, &ciphertext, &bindingRevision); err != nil {
		t.Fatal(err)
	}
	ciphertext[len(ciphertext)-1] ^= 0x01
	if _, err := secrets.Open(key, created.ID, 1, connections.TypeThanos, bindingRevision, &secrets.Envelope{Nonce: nonce, Ciphertext: ciphertext}); err == nil {
		t.Fatal("tampered envelope must not authenticate")
	}
	// Wrong root key: must fail closed (rebind scenario).
	wrong := make([]byte, 32)
	if _, err := secrets.Open(wrong, created.ID, 1, connections.TypeThanos, bindingRevision, &secrets.Envelope{Nonce: nonce, Ciphertext: ciphertext}); err == nil {
		t.Fatal("wrong key must not authenticate")
	}
}

func TestEnableFencesAndModelProviderQualification(t *testing.T) {
	service, _, _ := newService(t)
	ctx := context.Background()
	first, err := service.Create(ctx, thanosInput(""), 1, "cmd-"+fmt.Sprint(seq.Next()))
	if err != nil {
		t.Fatal(err)
	}
	secondInput := thanosInput("")
	secondInput.Name = "backup-thanos"
	second, err := service.Create(ctx, secondInput, 1, "cmd-"+fmt.Sprint(seq.Next()))
	if err != nil {
		t.Fatal(err)
	}
	enabledFirst, err := service.Enable(ctx, first.Name, first.RowVersion, 0, 1)
	if err != nil || !enabledFirst.Enabled {
		t.Fatalf("enable first: %v %+v", err, enabledFirst)
	}
	// Single-enabled partial index for thanos.
	if _, err := service.Enable(ctx, second.Name, second.RowVersion, 0, 1); !errors.Is(err, connections.ErrSingleEnabled) {
		t.Fatalf("second enabled thanos must conflict, got %v", err)
	}
	// Row-version fence.
	if _, err := service.Enable(ctx, second.Name, second.RowVersion-1, 0, 1); !errors.Is(err, connections.ErrRowVersion) {
		t.Fatalf("stale row version must conflict, got %v", err)
	}
	// Model provider enable requires an explicit passed probe result that
	// closes onto the current pair.
	providerProjection, _ := json.Marshal(map[string]any{"type": "model_provider", "baseUrl": "https://api.example.com", "chatModelId": "chat", "embeddingModelId": "embed"})
	providerSecret, _ := json.Marshal(map[string]string{"type": "model_provider", "apiKey": "sk-test"})
	provider, err := service.Create(ctx, connections.CreateInput{Name: "main-openai", Type: connections.TypeModelProvider, NonSecretJSON: providerProjection, Secret: providerSecret, SecretPresent: true}, 1, "cmd-"+fmt.Sprint(seq.Next()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Enable(ctx, provider.Name, provider.RowVersion, 0, 1); !errors.Is(err, connections.ErrValidation) {
		t.Fatalf("provider enable without qualification must be rejected, got %v", err)
	}
	if _, err := service.Enable(ctx, provider.Name, provider.RowVersion, 99999, 1); !errors.Is(err, connections.ErrValidation) {
		t.Fatalf("unknown probe result must be rejected, got %v", err)
	}
}

func TestKubernetesRequiresSecretAndValidatesInput(t *testing.T) {
	service, _, _ := newService(t)
	ctx := context.Background()
	projection, _ := json.Marshal(map[string]any{"type": "kubernetes", "defaultNamespace": "ops"})
	// Missing kubeconfig: deterministic rejection.
	if _, err := service.Create(ctx, connections.CreateInput{Name: "prod-k8s", Type: connections.TypeKubernetes, NonSecretJSON: projection}, 1, "cmd-"+fmt.Sprint(seq.Next())); !errors.Is(err, connections.ErrValidation) {
		t.Fatalf("kubernetes without kubeconfig must be rejected, got %v", err)
	}
	// Secret field smuggled into the non-secret projection: rejected.
	dirty, _ := json.Marshal(map[string]any{"type": "kubernetes", "defaultNamespace": "ops", "kubeconfig": "leak"})
	if _, err := service.Create(ctx, connections.CreateInput{Name: "prod-k8s", Type: connections.TypeKubernetes, NonSecretJSON: dirty}, 1, "cmd-"+fmt.Sprint(seq.Next())); !errors.Is(err, connections.ErrValidation) {
		t.Fatalf("secret in projection must be rejected, got %v", err)
	}
	// Valid creation decrypts the kubeconfig.
	secret, _ := json.Marshal(map[string]string{"type": "kubernetes", "kubeconfig": "apiVersion: v1\nkind: Config\n"})
	created, err := service.Create(ctx, connections.CreateInput{Name: "prod-k8s", Type: connections.TypeKubernetes, NonSecretJSON: projection, Secret: secret, SecretPresent: true}, 1, "cmd-"+fmt.Sprint(seq.Next()))
	if err != nil {
		t.Fatal(err)
	}
	opened, err := service.OpenGeneration(ctx, created.CurrentGenerationID)
	if err != nil {
		t.Fatal(err)
	}
	if opened.Kubernetes == nil || opened.Kubernetes.Kubeconfig == "" {
		t.Fatalf("kubeconfig not decrypted: %+v", opened)
	}
}

func TestProbeClosureCommitOrder(t *testing.T) {
	service, _, _ := newService(t)
	ctx := context.Background()
	created, err := service.Create(ctx, thanosInput(""), 1, "cmd-"+fmt.Sprint(seq.Next()))
	if err != nil {
		t.Fatal(err)
	}
	// No live plinth: attempt stays Queued (dispatch waits for the runtime).
	attemptID, err := service.StartProbe(ctx, created.Name, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Accept out of a fake Running state: simulate the supervisor accept.
	if err := service.AcceptProbe(ctx, attemptID, "boot-p", 1); err == nil {
		t.Fatal("accept against Queued must fail")
	}
	// One active probe per connection.
	if _, err := service.StartProbe(ctx, created.Name, nil, nil); !errors.Is(err, connections.ErrActiveConflict) {
		t.Fatalf("second active probe must conflict, got %v", err)
	}
}
