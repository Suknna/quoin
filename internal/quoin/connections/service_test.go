package connections_test

// Deterministic coverage for the T07 connection domain: AEAD tamper and
// binding-mismatch fail-closed, enable fences (single-enabled, model
// provider qualification closure), and the probe attempt/grant closure with
// commit-order discipline. Every user-origin operation runs through the
// shared execution runner, so tests act as the trusted entry point and build
// real execution metadata over a real initialized admin session (ADR-0006:
// no synthetic or anonymous identity).

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
	"github.com/Suknna/quoin/internal/quoin/execution"
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
		RuntimeClientCAFile:       filepath.Join(root, "stele"),
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
	service.SetReader(database.Reader)
	connections.SetReleaseVersion("v0.1.0-dev")
	connections.ProbeContractSource = func() string { return "contract_version: 1" }
	// The qualification/audit FKs reference real users; create the fixture
	// administrator every service test uses as principal 1, fully initialized
	// at auth revision 1, plus the valid admin session the in-transaction
	// recheck (auth.VerifyExecutionSession) verifies.
	if _, err := database.SQL.Exec(`INSERT INTO users(id,username,display_name,role,enabled,initialized,auth_revision,password_phc,row_version,created_at,updated_at) VALUES(1,'admin','Admin','admin',1,1,1,'$argon2id$phc',1,?,?)`, "2026-01-01T00:00:00Z", "2026-01-01T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	seedAdminSession(t, database.SQL, 1, 1)
	return service, database.SQL, config.RootKeyFile
}

// seedAdminSession issues fixture session id sessionID for userID at the
// given auth revision with far-future idle and absolute expiry.
func seedAdminSession(t *testing.T, db *sql.DB, userID, sessionID int64) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.Exec(`INSERT INTO sessions(id,user_id,session_token_digest,auth_revision_at_issue,client_label,created_at,last_active_at,idle_expires_at,absolute_expires_at) VALUES(?,?,randomblob(32),1,'fixture',?,?,?,?)`,
		sessionID, userID, now, now, "2036-09-15T00:00:00Z", "2036-09-22T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
}

// adminContext builds the trusted-entry execution metadata the production
// admission middleware wires for authenticated admin requests: user actor 1
// with its real session proof. Missing metadata fails closed in the service,
// so tests must always carry it.
func adminContext(t *testing.T, correlation string) context.Context {
	t.Helper()
	ctx, err := execution.WithMetadata(context.Background(), execution.Metadata{
		CorrelationID: correlation,
		Actor:         execution.Principal{Kind: execution.PrincipalUser, ID: 1},
		Source:        execution.Source{Kind: execution.SourceHTTP, RequestID: "req-connections"},
		Session:       execution.SessionRef{ID: 1, AuthRevision: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	return ctx
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

func nextCorrelation() string {
	return fmt.Sprintf("corr-connections-%d", seq.Next())
}

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
	ctx := adminContext(t, nextCorrelation())
	created, err := service.Create(ctx, thanosInput("secret-password-1"), 1, "cmd-"+fmt.Sprint(seq.Next()))
	if err != nil {
		t.Fatal(err)
	}
	if created.Enabled || created.RowVersion != 2 {
		// row_version=2: the pointer-wiring UPDATE advances it once after
		// the INSERT (row_version must increase exactly by 1 per UPDATE).
		t.Fatalf("created projection wrong: %+v", created)
	}
	// Decrypt through the actual supervisor grant path: bind the probe to a
	// live stream, then fulfill the frozen grant — the only audited operation
	// by which a sealed secret leaves storage.
	attemptID, err := service.StartProbe(ctx, created.Name, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, grantID, _, ok, err := service.BindQueuedToStream(context.Background(), attemptID, "boot-envelope", 1, 5*time.Minute)
	if err != nil || !ok {
		t.Fatalf("bind envelope probe: %v ok=%v", err, ok)
	}
	payload, err := service.FulfillGrant(context.Background(), grantID, attemptID, "boot-envelope", 1)
	if err != nil {
		t.Fatal(err)
	}
	if payload.Thanos == nil || payload.Thanos.Password != "secret-password-1" {
		t.Fatalf("decrypted secret wrong: %+v", payload)
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
	ctx := adminContext(t, nextCorrelation())
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
	attemptID, err := service.StartProbe(ctx, provider.Name, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, chatGrantID, _, ok, err := service.BindQueuedToStream(context.Background(), attemptID, "chat-only-boot", 1, time.Minute)
	if err != nil || !ok {
		t.Fatalf("bind probe: %v ok=%v", err, ok)
	}
	if err := service.AcceptProbe(context.Background(), attemptID, "chat-only-boot", 1); err != nil {
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
		callID, err := providerledger.Begin(context.Background(), database, service.Reader(), attemptID, chatGrantID, callSeq+1, 0, "chat", "chat-only", fmt.Sprintf("%064x", 2), fmt.Sprintf("%064x", 3), fmt.Sprintf("%064x", 4), fmt.Sprintf("%064x", 5), 1024, 256, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		if err := providerledger.WriteInputLineage(context.Background(), database, service.Reader(), callID, "chat", fmt.Sprintf("%064x", 2), fmt.Sprintf("%064x", 3), attemptID); err != nil {
			t.Fatal(err)
		}
		if err := providerledger.Complete(context.Background(), database, service.Reader(), attemptID, callID, completion); err != nil {
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
	if err := service.CommitProbeResult(context.Background(), attemptID, "chat-only-boot", 1, result, child); err != nil {
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
	ctx := adminContext(t, nextCorrelation())
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
	ctx := adminContext(t, nextCorrelation())
	attemptID, err := service.StartProbe(ctx, summary.Name, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, ok, err := service.BindQueuedToStream(context.Background(), attemptID, boot, epoch, 5*time.Minute); err != nil || !ok {
		t.Fatalf("bind metrics probe: %v ok=%v", err, ok)
	}
	if err := service.AcceptProbe(context.Background(), attemptID, boot, epoch); err != nil {
		t.Fatal(err)
	}
	result := connections.TypedProbeResult{
		Outcome: "passed", ResultDigest: fmt.Sprintf("%064x", attemptID),
		StartedAt: "2026-01-01T00:00:00Z", FinishedAt: "2026-01-01T00:00:01Z",
	}
	child := &connections.TypedChild{Thanos: &connections.ThanosProbeChild{
		Query: "vector(1)", ResponseType: "vector", SampleCount: 1, SampleValue: "1", DetailJSON: `{"kind":"thanos"}`,
	}}
	if err := service.CommitProbeResult(context.Background(), attemptID, boot, epoch, result, child); err != nil {
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
	ctx := adminContext(t, nextCorrelation())
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

func TestProbeClosureCommitOrder(t *testing.T) {
	service, _, _ := newService(t)
	ctx := adminContext(t, nextCorrelation())
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
	if err := service.AcceptProbe(context.Background(), attemptID, "boot-p", 1); err == nil {
		t.Fatal("accept against Queued must fail")
	}
	// One active probe per connection.
	if _, err := service.StartProbe(ctx, created.Name, nil, nil); !errors.Is(err, connections.ErrActiveConflict) {
		t.Fatalf("second active probe must conflict, got %v", err)
	}
}

// TestEnableInTxHookCreatesDefaultPlan (ADR-0004): the enablement-coupled
// hook runs inside the enable transaction — the default basic inspection plan
// commits atomically with the enable, and a hook failure rolls both back.
func TestEnableInTxHookCreatesDefaultPlan(t *testing.T) {
	service, database, _ := newService(t)
	ctx := adminContext(t, nextCorrelation())
	hookInput := thanosInput("")
	hookInput.Name = "hook-thanos"
	created, err := service.Create(ctx, hookInput, 1, "cmd-"+fmt.Sprint(seq.Next()))
	if err != nil {
		t.Fatal(err)
	}
	createdPlans := 0
	seen := map[string]bool{}
	service.SetPostEnableInTx(func(ctx context.Context, conn execution.Executor, name string) error {
		var connectionID int64
		if err := conn.QueryRowContext(ctx, `SELECT id FROM connections WHERE name=?`, name).Scan(&connectionID); err != nil {
			return err
		}
		now := time.Now().UTC().Format(time.RFC3339Nano)
		if _, err := conn.ExecContext(ctx, `INSERT INTO inspection_plans(plan_key,display_name,enabled,connection_id,plugin_id,template_id,template_version,params_json,scope_json,scope_kind,cron,timezone,row_version,created_by,created_at,updated_at)
			VALUES('basic-'||?, '基础巡检', 1, ?, 'thanos', 'promql_instant', '1', '{"expression":"up"}', '{"kind":"integration"}', 'integration', NULL, 'UTC', 1, 1, ?, ?)`,
			name, connectionID, now, now); err != nil {
			return err
		}
		createdPlans++
		seen[name] = true
		return nil
	})
	probe := passedMetricsProbe(t, service, database, created, "boot-hook", 1)
	enabled, err := service.Enable(ctx, created.Name, created.RowVersion, probe, 1)
	if err != nil || !enabled.Enabled {
		t.Fatalf("enable with hook: %v %+v", err, enabled)
	}
	var plans int
	if err := database.QueryRow(`SELECT COUNT(*) FROM inspection_plans WHERE plan_key=?`, "basic-"+created.Name).Scan(&plans); err != nil {
		t.Fatal(err)
	}
	if plans != 1 || createdPlans != 1 || !seen[created.Name] {
		t.Fatalf("hook must commit with the enable: plans=%d calls=%d", plans, createdPlans)
	}
	// Semantic re-enable replays the same hook contract; idempotent hooks keep
	// the single default plan row.
	probe2 := passedMetricsProbe(t, service, database, enabled, "boot-hook2", 2)
	if _, err := service.Enable(ctx, enabled.Name, enabled.RowVersion, probe2, 1); err != nil {
		t.Fatalf("idempotent re-enable: %v", err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM inspection_plans WHERE plan_key=?`, "basic-"+created.Name).Scan(&plans); err != nil {
		t.Fatal(err)
	}
	if plans != 1 {
		t.Fatalf("idempotent hook must keep exactly one default plan, got %d", plans)
	}
	// A hook failure must roll the enable back atomically.
	failingInput := thanosInput("")
	failingInput.Name = "failing-thanos"
	failing, err := service.Create(ctx, failingInput, 1, "cmd-"+fmt.Sprint(seq.Next()))
	if err != nil {
		t.Fatal(err)
	}
	failingProbe := passedMetricsProbe(t, service, database, failing, "boot-hook-fail", 3)
	service.SetPostEnableInTx(func(context.Context, execution.Executor, string) error { return errors.New("hook refused") })
	if _, err := service.Enable(ctx, failing.Name, failing.RowVersion, failingProbe, 1); err == nil {
		t.Fatal("hook failure must fail the enable")
	}
	var enabledCount int
	if err := database.QueryRow(`SELECT enabled FROM connections WHERE id=?`, failing.ID).Scan(&enabledCount); err != nil {
		t.Fatal(err)
	}
	if enabledCount != 0 {
		t.Fatalf("hook failure must roll back the enable, enabled=%d", enabledCount)
	}
}
