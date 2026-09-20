package app_test

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/contract"
	gencontracts "github.com/Suknna/quoin/internal/gen/contracts"
	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	"github.com/Suknna/quoin/internal/quoin/alerts"
	"github.com/Suknna/quoin/internal/quoin/app"
	"github.com/Suknna/quoin/internal/quoin/bootstrap"
	"github.com/Suknna/quoin/internal/quoin/connections"
	"github.com/Suknna/quoin/internal/quoin/execution"
	"github.com/Suknna/quoin/test/support"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

// relayTLSFixture boots the deployment-shaped mTLS listener material through
// the real secrets bootstrap (ADR-0009): one Runtime CA, the quoin server
// identity, and the stele/plinth client identities.
type relayTLSFixture struct {
	caPEM         []byte
	steleClient   tls.Certificate
	plinthClient  tls.Certificate
	serverAddress string
}

func startRelayServer(t *testing.T, alertsService *alerts.Service, connectionsService *connections.Service) relayTLSFixture {
	t.Helper()
	root := t.TempDir()
	secrets := root + "/secrets"
	config := contract.QuoinConfig{
		Component: "quoin", PublicOrigin: "https://quoin.test",
		DataDirectory:             root + "/data",
		BackupDirectory:           root + "/backup",
		RootKeyFile:               secrets + "/root-key",
		RuntimeTLSCertificateFile: secrets + "/runtime-tls.crt",
		RuntimeTLSPrivateKeyFile:  secrets + "/runtime-tls.key",
		RuntimeClientCAFile:       secrets + "/runtime-ca.pem",
	}
	if err := support.GenerateDeploymentSecrets(config); err != nil {
		t.Fatal(err)
	}
	fixture := relayTLSFixture{}
	var err error
	if fixture.caPEM, err = os.ReadFile(secrets + "/runtime-ca.pem"); err != nil {
		t.Fatal(err)
	}
	if fixture.steleClient, err = tls.LoadX509KeyPair(secrets+"/stele-client.crt", secrets+"/stele-client.key"); err != nil {
		t.Fatal(err)
	}
	if fixture.plinthClient, err = tls.LoadX509KeyPair(secrets+"/plinth-client.crt", secrets+"/plinth-client.key"); err != nil {
		t.Fatal(err)
	}
	serverCert, err := tls.LoadX509KeyPair(secrets+"/runtime-tls.crt", secrets+"/runtime-tls.key")
	if err != nil {
		t.Fatal(err)
	}
	clientPool := x509.NewCertPool()
	if !clientPool.AppendCertsFromPEM(fixture.caPEM) {
		t.Fatal("bootstrap CA cannot be parsed")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	// TLS terminates inside gRPC exactly like production (grpc.Creds on a raw
	// listener): every handler context then carries the verified client
	// identity (ADR-0009).
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{serverCert}, MinVersion: tls.VersionTLS13,
		ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: clientPool,
	})))
	app.RegisterSteleRelay(server, app.NewSteleRelayServer(alertsService, connectionsService, app.NewSteleGateway()))
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	fixture.serverAddress = listener.Addr().String()
	return fixture
}

func relayClient(t *testing.T, fixture relayTLSFixture, clientCert *tls.Certificate) runtimev1.SteleRelayClient {
	t.Helper()
	rootPool := x509.NewCertPool()
	if !rootPool.AppendCertsFromPEM(fixture.caPEM) {
		t.Fatal("bootstrap CA cannot be parsed")
	}
	tlsConfig := &tls.Config{RootCAs: rootPool, ServerName: "localhost", MinVersion: tls.VersionTLS13}
	if clientCert != nil {
		tlsConfig.Certificates = []tls.Certificate{*clientCert}
	}
	conn, err := grpc.NewClient(fixture.serverAddress, grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return runtimev1.NewSteleRelayClient(conn)
}

// relayTestHarness 装配一次 relay 测试的真实部件：引导数据库、alerts 服务
// 与 connections 服务（root key 来自 bootstrap 材料）。
type relayTestHarness struct {
	database   *bootstrap.Database
	alerts     *alerts.Service
	connection *connections.Service
}

func newRelayHarness(t *testing.T) *relayTestHarness {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	secrets := root + "/secrets"
	config := contract.QuoinConfig{
		Component: "quoin", PublicOrigin: "https://quoin.test",
		DataDirectory:             root + "/data",
		BackupDirectory:           root + "/backup",
		RootKeyFile:               secrets + "/root-key",
		RuntimeTLSCertificateFile: secrets + "/runtime-tls.crt",
		RuntimeTLSPrivateKeyFile:  secrets + "/runtime-tls.key",
		RuntimeClientCAFile:       secrets + "/runtime-ca.pem",
	}
	if err := support.GenerateDeploymentSecrets(config); err != nil {
		t.Fatal(err)
	}
	database, err := bootstrap.OpenDatabase(ctx, config.DataDirectory, config.RootKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	// Credential snapshot and delivery lookups are pure reads on the alerts
	// runner; they fail closed until the bootstrap's real read-only pool is
	// installed at assembly (alerts has no post-construction setter).
	alertsService, err := alerts.NewServiceWithReader(database.SQL, database.Reader, execution.NewRunner(database.SQL, execution.NewRegistry(), nil))
	if err != nil {
		t.Fatal(err)
	}
	rootKey, err := os.ReadFile(config.RootKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	connectionsService := connections.NewService(database.SQL, func() ([]byte, error) {
		return rootKey, nil
	})
	if err := connectionsService.SetReader(database.Reader); err != nil {
		t.Fatal(err)
	}
	// 探测契约源与生产装配一致（app.go 同款接线）。
	connections.ProbeContractSource = func() string { return string(gencontracts.ConnectionProbesYAML) }
	return &relayTestHarness{database: database, alerts: alertsService, connection: connectionsService}
}

// seedAdminSession 创建 relay 测试播种连接所需的管理员会话与执行元数据。
func (h *relayTestHarness) seedAdminContext(t *testing.T, correlation string) context.Context {
	t.Helper()
	now := "2026-09-14T00:00:00Z"
	idle, absolute := "2036-09-14T00:00:00Z", "2036-09-21T00:00:00Z"
	for _, statement := range []string{
		`INSERT INTO users(id,username,display_name,role,enabled,initialized,password_phc,row_version,created_at,updated_at) VALUES(1,'admin','Admin','admin',1,1,'fixture',1,'` + now + `','` + now + `')`,
		`INSERT INTO sessions(id,user_id,session_token_digest,auth_revision_at_issue,client_label,created_at,last_active_at,idle_expires_at,absolute_expires_at) VALUES(1,1,randomblob(32),1,'relay-fixture','` + now + `','` + now + `','` + idle + `','` + absolute + `')`,
	} {
		if _, err := h.database.SQL.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	adminCtx, err := execution.WithMetadata(context.Background(), execution.Metadata{
		CorrelationID: correlation,
		Actor:         execution.Principal{Kind: execution.PrincipalUser, ID: 1},
		Source:        execution.Source{Kind: execution.SourceHTTP, RequestID: "req-" + correlation},
		Session:       execution.SessionRef{ID: 1, AuthRevision: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	return adminCtx
}

// TestSteleRelayMTLSIdentityAndDelivery drives the frozen SteleRelay service
// over a real mTLS listener: the CN=stele client identity authenticates
// GetCredentialSnapshot and batched DeliverEvents, the CN=plinth identity is
// rejected, a certificate-less dial cannot even complete the TLS handshake,
// and a mismatched contract fingerprint never admits a request (ADR-0009,
// ADR-0011).
func TestSteleRelayMTLSIdentityAndDelivery(t *testing.T) {
	ctx := context.Background()
	harness := newRelayHarness(t)

	// Seed a source with a bearer whose digest matches a known value.
	adminCtx := harness.seedAdminContext(t, "stele-relay-seed")
	bearer := "known-test-bearer-0123456789abcdef"
	digest := sha256Sum(bearer)
	result, _, err := harness.alerts.CreateSource(adminCtx, "relay-seed-0001", "test-source", "alertmanager", digest[:])
	if err != nil {
		t.Fatal(err)
	}

	fixture := startRelayServer(t, harness.alerts, harness.connection)

	// The CN=plinth identity must never reach the SteleRelay surface.
	plinthClient := relayClient(t, fixture, &fixture.plinthClient)
	if _, err := plinthClient.GetCredentialSnapshot(ctx, &runtimev1.GetCredentialSnapshotRequest{ContractFingerprint: contract.ProtoAuthorityFingerprint}); err == nil {
		t.Fatal("the plinth client identity must not authorize SteleRelay")
	}

	// A certificate-less dial cannot complete the mandatory-client-cert
	// handshake at all: the RPC fails on transport, before any handler.
	noCertClient := relayClient(t, fixture, nil)
	callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := noCertClient.GetCredentialSnapshot(callCtx, &runtimev1.GetCredentialSnapshotRequest{ContractFingerprint: contract.ProtoAuthorityFingerprint}); err == nil {
		t.Fatal("a dial without a client certificate must fail the mTLS handshake")
	}

	steleClient := relayClient(t, fixture, &fixture.steleClient)
	snapshot, err := steleClient.GetCredentialSnapshot(ctx, &runtimev1.GetCredentialSnapshotRequest{ContractFingerprint: contract.ProtoAuthorityFingerprint})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.GetSnapshotVersion() == 0 || len(snapshot.GetSources()) != 1 {
		t.Fatalf("snapshot=%+v", snapshot)
	}
	found := false
	for _, source := range snapshot.GetSources() {
		if source.GetSourceId() == result.SourceID && len(source.GetCredentials()) == 1 && source.GetCredentials()[0].GetCredentialId() == result.CredentialID {
			found = true
		}
	}
	if !found {
		t.Fatalf("seeded credential missing from snapshot: %+v", snapshot)
	}

	body := []byte(`{"status":"firing","alerts":[{"status":"firing","labels":{"alertname":"CPU","instance":"db-1"},"startsAt":"2026-08-17T10:00:00Z","fingerprint":"` + relayFingerprintHex(map[string]string{"alertname": "CPU", "instance": "db-1"}) + `"}],"truncatedAlerts":0}`)
	deliver := func(eventID string) []runtimev1.EventDeliveryStatus {
		response, err := steleClient.DeliverEvents(ctx, &runtimev1.DeliverEventsRequest{
			ContractFingerprint: contract.ProtoAuthorityFingerprint,
			Events: []*runtimev1.RelayEvent{{
				EventId: eventID, SourceKind: "alertmanager", SourceId: result.SourceID,
				CredentialId: result.CredentialID, CredentialSnapshotVersion: snapshot.GetSnapshotVersion(),
				EventType: "alerts.batch", Payload: body,
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		return response.GetResults()
	}
	first := deliver("relay-e2e-1")
	if len(first) != 1 || first[0] != runtimev1.EventDeliveryStatus_EVENT_DELIVERY_STATUS_ACCEPTED {
		t.Fatalf("first delivery results=%v", first)
	}
	second := deliver("relay-e2e-1")
	if len(second) != 1 || second[0] != runtimev1.EventDeliveryStatus_EVENT_DELIVERY_STATUS_ACCEPTED {
		t.Fatalf("replay delivery results=%v", second)
	}
	var occurrenceCount int
	if err := harness.database.SQL.QueryRowContext(ctx, `SELECT COUNT(*) FROM alert_occurrences`).Scan(&occurrenceCount); err != nil || occurrenceCount != 1 {
		t.Fatalf("occurrence count=%d err=%v", occurrenceCount, err)
	}
	var deliveryCount int
	if err := harness.database.SQL.QueryRowContext(ctx, `SELECT COUNT(*) FROM alert_deliveries`).Scan(&deliveryCount); err != nil || deliveryCount != 1 {
		t.Fatalf("delivery count=%d err=%v", deliveryCount, err)
	}

	// A missing or malformed contract fingerprint must never provide a legacy
	// release-version admission path — even with a valid stele identity.
	if _, err := steleClient.GetCredentialSnapshot(ctx, &runtimev1.GetCredentialSnapshotRequest{ContractFingerprint: "not-a-valid-fingerprint"}); err == nil {
		t.Fatal("invalid contract fingerprint must fail")
	}
	if _, err := steleClient.DeliverEvents(ctx, &runtimev1.DeliverEventsRequest{
		Events: []*runtimev1.RelayEvent{{EventId: "relay-bad-contract", SourceKind: "alertmanager"}},
	}); err == nil {
		t.Fatal("missing contract fingerprint must fail")
	}
}

// TestSteleRelayDeliverEventsPerEventAdjudication 覆盖逐事件裁决：未知
// source_kind 确定性 REJECTED，alerts 服务不可用（delivery 表缺失）映射
// UNAVAILABLE，且批量结果与请求顺序一一对应。
func TestSteleRelayDeliverEventsPerEventAdjudication(t *testing.T) {
	ctx := context.Background()
	harness := newRelayHarness(t)
	adminCtx := harness.seedAdminContext(t, "stele-relay-events")
	bearer := "event-batch-bearer-0123456789"
	digest := sha256Sum(bearer)
	result, _, err := harness.alerts.CreateSource(adminCtx, "relay-events-0001", "events-source", "alertmanager", digest[:])
	if err != nil {
		t.Fatal(err)
	}
	fixture := startRelayServer(t, harness.alerts, harness.connection)
	steleClient := relayClient(t, fixture, &fixture.steleClient)

	body := []byte(`{"status":"firing","alerts":[{"status":"firing","labels":{"alertname":"MEM","instance":"db-2"},"startsAt":"2026-08-17T11:00:00Z","fingerprint":"` + relayFingerprintHex(map[string]string{"alertname": "MEM", "instance": "db-2"}) + `"}],"truncatedAlerts":0}`)
	response, err := steleClient.DeliverEvents(ctx, &runtimev1.DeliverEventsRequest{
		ContractFingerprint: contract.ProtoAuthorityFingerprint,
		Events: []*runtimev1.RelayEvent{
			{EventId: "batch-unknown-kind", SourceKind: "webhook", Payload: []byte(`{}`)},
			{EventId: "batch-empty-id", SourceKind: "alertmanager"},
			{EventId: "batch-good", SourceKind: "alertmanager", SourceId: result.SourceID,
				CredentialId: result.CredentialID, CredentialSnapshotVersion: 1, Payload: body},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	results := response.GetResults()
	if len(results) != 3 {
		t.Fatalf("results=%v, want one per request event", results)
	}
	if results[0] != runtimev1.EventDeliveryStatus_EVENT_DELIVERY_STATUS_REJECTED {
		t.Fatalf("unknown source_kind result=%v, want REJECTED", results[0])
	}
	if results[1] != runtimev1.EventDeliveryStatus_EVENT_DELIVERY_STATUS_REJECTED {
		t.Fatalf("missing event_id result=%v, want REJECTED", results[1])
	}
	if results[2] != runtimev1.EventDeliveryStatus_EVENT_DELIVERY_STATUS_ACCEPTED {
		t.Fatalf("alertmanager result=%v, want ACCEPTED", results[2])
	}

	// 底层投递失败（关闭写库模拟库不可用）必须映射 UNAVAILABLE，供 Stele
	// 稍后重试。sql.DB.Close 幂等，t.Cleanup 的二次关闭无害。
	if err := harness.database.SQL.Close(); err != nil {
		t.Fatal(err)
	}
	retry, err := steleClient.DeliverEvents(ctx, &runtimev1.DeliverEventsRequest{
		ContractFingerprint: contract.ProtoAuthorityFingerprint,
		Events: []*runtimev1.RelayEvent{{
			EventId: "batch-unavailable", SourceKind: "alertmanager", SourceId: result.SourceID,
			CredentialId: result.CredentialID, CredentialSnapshotVersion: 1, Payload: body,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := retry.GetResults(); len(got) != 1 || got[0] != runtimev1.EventDeliveryStatus_EVENT_DELIVERY_STATUS_UNAVAILABLE {
		t.Fatalf("unavailable results=%v, want UNAVAILABLE", got)
	}
}

// TestSteleRelayAcquireConnectionCredential 覆盖按需连接材料投递：thanos
// 连接回传 revision 投影与解密秘密；model_provider 连接被拒绝（模型凭据
// 只走 Plinth grant）；未知连接 id 同样确定性拒绝。
func TestSteleRelayAcquireConnectionCredential(t *testing.T) {
	ctx := context.Background()
	harness := newRelayHarness(t)
	adminCtx := harness.seedAdminContext(t, "stele-relay-acquire")
	created, err := harness.connection.Create(adminCtx, connections.CreateInput{
		Name:          "relay-thanos",
		Type:          connections.TypeThanos,
		NonSecretJSON: []byte(`{"type":"thanos","baseUrl":"https://thanos.example"}`),
		Secret:        []byte(`{"type":"thanos","username":"relay-user","password":"relay-pass"}`),
		SecretPresent: true,
	}, 1, "relay-acquire-thanos")
	if err != nil {
		t.Fatal(err)
	}
	modelProvider, err := harness.connection.Create(adminCtx, connections.CreateInput{
		Name:          "relay-model",
		Type:          connections.TypeModelProvider,
		NonSecretJSON: []byte(`{"type":"model_provider","baseUrl":"https://model.example","chatModelId":"chat-1"}`),
		Secret:        []byte(`{"type":"model_provider","apiKey":"relay-key"}`),
		SecretPresent: true,
	}, 1, "relay-acquire-model")
	if err != nil {
		t.Fatal(err)
	}
	// 启用走完整的 qualification 闭包（probe 结果 + qualification 事件 +
	// 启用翻转），与真实 Enable 命令同一提交语义；model_provider 保持禁用
	// 即可覆盖确定性拒绝分支。
	enableMetricsConnectionFixture(t, harness, created)
	fixture := startRelayServer(t, harness.alerts, harness.connection)
	steleClient := relayClient(t, fixture, &fixture.steleClient)

	acquired, err := steleClient.AcquireConnectionCredential(ctx, &runtimev1.AcquireConnectionCredentialRequest{
		ConnectionId: created.ID, ContractFingerprint: contract.ProtoAuthorityFingerprint,
	})
	if err != nil {
		t.Fatal(err)
	}
	if acquired.GetConnectionType() != connections.TypeThanos ||
		acquired.GetConnectionRevisionId() != created.CurrentRevisionID ||
		acquired.GetCredentialGenerationId() != created.CurrentGenerationID {
		t.Fatalf("acquired=%+v", acquired)
	}
	if secret := acquired.GetThanos(); secret == nil || secret.GetUsername() != "relay-user" || secret.GetPassword() != "relay-pass" || secret.GetBearerToken() != "" {
		t.Fatalf("thanos secret=%+v", secret)
	}
	if len(acquired.GetRevisionConfigJson()) == 0 || !jsonContains(acquired.GetRevisionConfigJson(), "thanos.example") {
		t.Fatalf("revision config=%s", acquired.GetRevisionConfigJson())
	}

	// model_provider 连接确定性拒绝：模型凭据只走 Plinth grant。
	if _, err := steleClient.AcquireConnectionCredential(ctx, &runtimev1.AcquireConnectionCredentialRequest{
		ConnectionId: modelProvider.ID, ContractFingerprint: contract.ProtoAuthorityFingerprint,
	}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("model_provider acquire err=%v, want PermissionDenied", err)
	}
	// 未知连接同样确定性拒绝。
	if _, err := steleClient.AcquireConnectionCredential(ctx, &runtimev1.AcquireConnectionCredentialRequest{
		ConnectionId: 404, ContractFingerprint: contract.ProtoAuthorityFingerprint,
	}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("unknown connection acquire err=%v, want PermissionDenied", err)
	}
}

func jsonContains(body []byte, needle string) bool {
	return len(body) > 0 && (fmt.Sprintf("%s", body) != "" && stringContains(string(body), needle))
}

func stringContains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// enableMetricsConnectionFixture 以原始 SQL 播种一次完整的 metrics 连接启用
// 闭包：Running 探测 Attempt + 冻结 grant + passed 探测结果（含 metrics 子
// 行）+ qualification 事件 + 启用翻转。触发器语义与真实 Enable 命令一致。
func enableMetricsConnectionFixture(t *testing.T, harness *relayTestHarness, summary connections.Summary) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	digest := func(seed string) string {
		sum := sha256.Sum256([]byte(seed))
		return fmt.Sprintf("%x", sum)
	}
	contractDigest, err := connections.ProbeContractDigest()
	if err != nil {
		t.Fatal(err)
	}
	actionSetID, actionSetVersion, err := connections.ActionSet(summary.Type)
	if err != nil {
		t.Fatal(err)
	}
	db := harness.database.SQL
	must := func(statement string, args ...any) {
		t.Helper()
		if _, err := db.Exec(statement, args...); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	attempt, err := db.Exec(`INSERT INTO execution_attempts(attempt_type,scope_type,scope_id,state,quoin_release_version,agent_version,created_at) VALUES('connection_probe','connection',?,'Queued','relay-test',NULL,?)`, summary.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	attemptID, _ := attempt.LastInsertId()
	// 派发就绪触发器要求冻结输入谱系（connection_probe_v1 快照 + 输入项）。
	snapshot, err := db.Exec(`INSERT INTO attempt_input_snapshots(attempt_id,schema_kind,renderer_version,content_digest,created_at) VALUES(?,'connection_probe_v1','connection-probe-renderer-v1',?,?)`,
		attemptID, digest(fmt.Sprintf("relay-probe-input-%d", summary.ID)), now)
	if err != nil {
		t.Fatal(err)
	}
	snapshotID, _ := snapshot.LastInsertId()
	must(`INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,connection_revision_id) VALUES(?,1,'connection_revision',?,?)`,
		snapshotID, digest(fmt.Sprintf("relay-probe-revision-%d", summary.CurrentRevisionID)), summary.CurrentRevisionID)
	var bindingRevision int
	if err := db.QueryRow(`SELECT binding_revision FROM root_key_state WHERE id=1`).Scan(&bindingRevision); err != nil {
		t.Fatal(err)
	}
	must(`INSERT INTO attempt_connection_grants(attempt_id,purpose,connection_id,connection_revision_id,credential_generation_id,created_at) VALUES(?,?,?,?,?,?)`,
		attemptID, "thanos_probe", summary.ID, summary.CurrentRevisionID, summary.CurrentGenerationID, now)
	must(`UPDATE execution_attempts SET state='Assigned',runtime_slot='plinth',boot_id='relay-probe-boot',connection_epoch=1,lease_until=?,runtime_release_version='relay-test',row_version=row_version+1 WHERE id=?`, now, attemptID)
	must(`UPDATE execution_attempts SET state='Running',accepted_at=?,started_at=?,row_version=row_version+1 WHERE id=?`, now, now, attemptID)
	probe, err := db.Exec(`INSERT INTO connection_probe_results(attempt_id,connection_id,connection_type,connection_revision_id,credential_generation_id,root_binding_revision,action_set_id,action_set_version,probe_contract_digest,outcome,result_digest,started_at,finished_at,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		attemptID, summary.ID, summary.Type, summary.CurrentRevisionID, summary.CurrentGenerationID, bindingRevision, actionSetID, actionSetVersion, contractDigest, "passed", digest(fmt.Sprintf("relay-probe-%d", summary.ID)), now, now, now)
	if err != nil {
		t.Fatal(err)
	}
	probeID, _ := probe.LastInsertId()
	must(`INSERT INTO thanos_connection_probe_results(probe_result_id,query,response_type,sample_count,sample_value,detail_json) VALUES(?,?,?,?,?,?)`,
		probeID, "vector(1)", "vector", 1, "1", `{"kind":"thanos"}`)
	must(`UPDATE execution_attempts SET state='Succeeded',ended_at=?,row_version=row_version+1 WHERE id=?`, now, attemptID)
	must(`INSERT INTO connection_enable_qualifications(connection_id,enabled_row_version,probe_result_id,created_by,created_at) VALUES(?,?,?,?,?)`,
		summary.ID, summary.RowVersion+1, probeID, 1, now)
	must(`UPDATE connections SET enabled=1,revalidation_required=0,row_version=row_version+1 WHERE id=?`, summary.ID)
}

func sha256Sum(value string) [32]byte {
	return sha256.Sum256([]byte(value))
}

func relayFingerprintHex(labels map[string]string) string {
	return fmt.Sprintf("%x", binary.BigEndian.Uint64(alerts.FingerprintOf(labels)))
}
