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
	"time"

	"github.com/Suknna/quoin/internal/contract"
	"github.com/Suknna/quoin/internal/quoin/bootstrap"
	"github.com/Suknna/quoin/internal/quoin/connections"
	providerledger "github.com/Suknna/quoin/internal/quoin/connections/modelprovider"
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

// TestChatOnlyModelProviderProbeClosure reproduces the real capability-probe
// persistence boundary for a provider that omits embeddings. It uses the
// production ledger, so the typed child can close only after real chat-success
// and chat-cancellation facts were persisted.
func TestChatOnlyModelProviderProbeClosure(t *testing.T) {
	service, database, _ := newService(t)
	ctx := context.Background()
	projection, _ := json.Marshal(map[string]any{
		"type": "model_provider", "baseUrl": "https://api.example.com", "chatModelId": "chat-only",
		"contextBudgetTokens": 1024, "maxOutputTokens": 256,
	})
	secret, _ := json.Marshal(map[string]string{"type": "model_provider", "apiKey": "test-api-key"})
	provider, err := service.Create(ctx, connections.CreateInput{
		Name: "chat-only-provider", Type: connections.TypeModelProvider,
		NonSecretJSON: projection, Secret: secret, SecretPresent: true,
	}, 1, "cmd-chat-only-provider")
	if err != nil {
		t.Fatal(err)
	}
	if err := registerPlinthSlot(database); err != nil {
		t.Fatal(err)
	}
	attemptID, err := service.StartProbe(ctx, provider.Name, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, chatGrantID, _, ok, err := service.BindQueuedToStream(ctx, attemptID, "chat-only-boot", 1, time.Minute)
	if err != nil || !ok {
		t.Fatalf("bind probe: %v ok=%v", err, ok)
	}
	if err := service.AcceptProbe(ctx, attemptID, "chat-only-boot", 1); err != nil {
		t.Fatal(err)
	}

	// A successful chat proves the provider can answer the fixed qualification
	// calls. The separate cancelled chat is the persisted cancellation fact the
	// child closure requires; no embedding call is fabricated for this config.
	for callSeq, completion := range []providerledger.Completion{
		{Outcome: "succeeded", ProviderRequestID: "req-chat", InputTokens: 1, OutputTokens: 1, TotalTokens: 2, FinishReason: "stop", ResponseJSON: `{"assistantText":"ready","finishReason":"stop","tool_calls":[]}`, ResponseDigest: fmt.Sprintf("%064x", 1), ResponseComplete: true},
		{Outcome: "cancelled", FailureReason: "cancelled", ProviderRequestID: "req-cancel"},
		// A failed optional action remains a real ledger fact. It must not
		// prevent the chat-only child from closing with no embedding claim.
		{Outcome: "failed", FailureReason: "invalid_response", ProviderRequestID: "req-failed-tool"},
	} {
		callID, err := providerledger.Begin(ctx, database, attemptID, chatGrantID, callSeq+1, 0, "chat", "chat-only", fmt.Sprintf("%064x", 2), fmt.Sprintf("%064x", 3), fmt.Sprintf("%064x", 4), fmt.Sprintf("%064x", 5), 1024, 256, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		if err := providerledger.WriteInputLineage(ctx, database, callID, "chat", fmt.Sprintf("%064x", 2), fmt.Sprintf("%064x", 3), attemptID); err != nil {
			t.Fatal(err)
		}
		if err := providerledger.Complete(ctx, database, attemptID, callID, completion); err != nil {
			t.Fatal(err)
		}
	}
	child := &connections.TypedChild{ModelProvider: &connections.ModelProviderProbeChild{
		ChatModelID: "chat-only", ContextBudgetTokens: 1024, MaxOutputTokens: 256,
		StreamingSupported: true, NativeToolCallingSupported: true, MultiToolCallSupported: true,
		CancellationObserved: true, UsageObserved: true, RequestIDObserved: true,
		EmbeddingSupported: false, DetailJSON: `{"kind":"model_provider","embeddingSupported":false}`,
	}}
	result := connections.TypedProbeResult{Outcome: "passed", ResultDigest: fmt.Sprintf("%064x", 6), StartedAt: "2026-01-01T00:00:00Z", FinishedAt: "2026-01-01T00:00:01Z"}
	if err := service.CommitProbeResult(ctx, attemptID, "chat-only-boot", 1, result, child); err != nil {
		t.Fatalf("chat-only probe must close with persisted chat and cancellation facts: %v", err)
	}
	var state, embeddingModelID string
	var embeddingSupported int
	if err := database.QueryRow(`SELECT a.state,COALESCE(m.embedding_model_id,''),m.embedding_supported FROM execution_attempts a JOIN connection_probe_results p ON p.attempt_id=a.id JOIN model_provider_connection_probe_results m ON m.probe_result_id=p.id WHERE a.id=?`, attemptID).Scan(&state, &embeddingModelID, &embeddingSupported); err != nil {
		t.Fatal(err)
	}
	if state != "Succeeded" || embeddingModelID != "" || embeddingSupported != 0 {
		t.Fatalf("chat-only result persisted wrong: state=%s embedding=%q supported=%d", state, embeddingModelID, embeddingSupported)
	}
	var embeddings, failedChats int
	if err := database.QueryRow(`SELECT COUNT(*),(SELECT COUNT(*) FROM model_calls WHERE attempt_id=? AND operation='chat' AND status='failed') FROM model_calls WHERE attempt_id=? AND operation='embedding'`, attemptID, attemptID).Scan(&embeddings, &failedChats); err != nil {
		t.Fatal(err)
	}
	if embeddings != 0 || failedChats != 1 {
		t.Fatalf("chat-only probe must retain actual failed chat but not manufacture embeddings: embeddings=%d failedChats=%d", embeddings, failedChats)
	}
	var probeResultID int64
	if err := database.QueryRow(`SELECT id FROM connection_probe_results WHERE attempt_id=?`, attemptID).Scan(&probeResultID); err != nil {
		t.Fatal(err)
	}
	// A configured chat-only provider still needs the same persisted real-call
	// qualification, but does not claim or require an embedding capability.
	enabled, err := service.Enable(ctx, provider.Name, provider.RowVersion, probeResultID, 1)
	if err != nil || !enabled.Enabled {
		t.Fatalf("chat-only provider must enable from its exact passed probe: %v %+v", err, enabled)
	}
}

func TestEnableFencesAndModelProviderQualification(t *testing.T) {
	service, database, _ := newService(t)
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
	if _, err := service.Enable(ctx, first.Name, first.RowVersion, 0, 1); !errors.Is(err, connections.ErrValidation) {
		t.Fatalf("metrics enable without a passed probe must be rejected, got %v", err)
	}
	firstProbe := passedMetricsProbe(t, service, database, first, "boot-first", 1)
	enabledFirst, err := service.Enable(ctx, first.Name, first.RowVersion, firstProbe, 1)
	if err != nil || !enabledFirst.Enabled {
		t.Fatalf("enable first after exact passed probe: %v %+v", err, enabledFirst)
	}
	// Metrics connections are independently selectable by business declaration,
	// but each one must independently qualify its own frozen pair.
	secondProbe := passedMetricsProbe(t, service, database, second, "boot-second", 2)
	if secondEnabled, err := service.Enable(ctx, second.Name, second.RowVersion, secondProbe, 1); err != nil || !secondEnabled.Enabled {
		t.Fatalf("second enabled thanos must independently qualify, got %v %+v", err, secondEnabled)
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

// passedMetricsProbe follows the production probe state machine so an enable
// qualification can only reference a real immutable result over this exact
// connection revision and credential generation.
func passedMetricsProbe(t *testing.T, service *connections.Service, database *sql.DB, summary connections.Summary, boot string, epoch uint64) int64 {
	t.Helper()
	ctx := context.Background()
	// The shared fixture registers Plinth once; repeated qualifying probes use
	// the same slot and only advance their independent attempt bindings.
	var registered int
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM runtime_slots WHERE slot='plinth' AND state='registered'`).Scan(&registered); err != nil {
		t.Fatal(err)
	}
	if registered == 0 {
		if err := registerPlinthSlot(database); err != nil {
			t.Fatal(err)
		}
	}
	attemptID, err := service.StartProbe(ctx, summary.Name, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, ok, err := service.BindQueuedToStream(ctx, attemptID, boot, epoch, 5*time.Minute); err != nil || !ok {
		t.Fatalf("bind metrics probe: %v ok=%v", err, ok)
	}
	if err := service.AcceptProbe(ctx, attemptID, boot, epoch); err != nil {
		t.Fatal(err)
	}
	result := connections.TypedProbeResult{
		Outcome: "passed", ResultDigest: fmt.Sprintf("%064x", attemptID),
		StartedAt: "2026-01-01T00:00:00Z", FinishedAt: "2026-01-01T00:00:01Z",
	}
	child := &connections.TypedChild{Thanos: &connections.ThanosProbeChild{
		Query: "vector(1)", ResponseType: "vector", SampleCount: 1, SampleValue: "1", DetailJSON: `{"kind":"thanos"}`,
	}}
	if err := service.CommitProbeResult(ctx, attemptID, boot, epoch, result, child); err != nil {
		t.Fatal(err)
	}
	var probeID int64
	if err := database.QueryRowContext(ctx, `SELECT id FROM connection_probe_results WHERE attempt_id=?`, attemptID).Scan(&probeID); err != nil {
		t.Fatal(err)
	}
	return probeID
}

func TestRotationRequiresAndAcceptsFreshExactProbe(t *testing.T) {
	service, database, _ := newService(t)
	ctx := context.Background()
	created, err := service.Create(ctx, thanosInput("first-secret"), 1, "cmd-"+fmt.Sprint(seq.Next()))
	if err != nil {
		t.Fatal(err)
	}
	oldProbe := passedMetricsProbe(t, service, database, created, "boot-before-rotation", 1)
	enabled, err := service.Enable(ctx, created.Name, created.RowVersion, oldProbe, 1)
	if err != nil || !enabled.Enabled {
		t.Fatalf("initial enable must accept an exact passed probe: %v %+v", err, enabled)
	}

	rotatedInput := thanosInput("replacement-secret")
	rotatedInput.Name = created.Name
	rotated, err := service.Rotate(ctx, created.Name, enabled.RowVersion, rotatedInput, 1, "cmd-"+fmt.Sprint(seq.Next()))
	if err != nil {
		t.Fatal(err)
	}
	if rotated.Enabled || !rotated.RevalidationRequired {
		t.Fatalf("rotated metrics connection must be disabled and require revalidation: %+v", rotated)
	}
	if _, err := service.Enable(ctx, rotated.Name, rotated.RowVersion, oldProbe, 1); !errors.Is(err, connections.ErrActiveConflict) {
		t.Fatalf("a pre-rotation probe must not qualify the new pair, got %v", err)
	}

	freshProbe := passedMetricsProbe(t, service, database, rotated, "boot-after-rotation", 2)
	revalidated, err := service.Enable(ctx, rotated.Name, rotated.RowVersion, freshProbe, 1)
	if err != nil || !revalidated.Enabled || revalidated.RevalidationRequired {
		t.Fatalf("fresh exact passed probe must restore the rotated connection: %v %+v", err, revalidated)
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
