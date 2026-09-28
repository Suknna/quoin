package synthetic

// 契约测试（#110 验收）：一枚测试插件声明受信任 HTTP 连接种类、grant 范围
// 的模型工具与巡检模板后，仅经宿主装配（显式 registry + BuildCatalogs）即
// 走通 Agent 工具授权（通用解析/校验/凭据围栏/Evidence 封存）与巡检采集
// grant（声明用途冻结/复核/模式闭合触发器）。本文件不修改任何核心业务流
// ——新增插件所需的全部宿主改动就是本测试所做的装配。

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/plugins"
	"github.com/Suknna/quoin/internal/quoin/attempt"
	"github.com/Suknna/quoin/internal/quoin/audit"
	"github.com/Suknna/quoin/internal/quoin/evidence"
	"github.com/Suknna/quoin/internal/quoin/execution"
	"github.com/Suknna/quoin/internal/quoin/testfixture"
)

// assemblingRegistry mirrors a host's blank-import assembly: one explicit
// registry registering exactly this plugin under default enablement.
func assemblingRegistry(t *testing.T) (*plugins.Registry, []string) {
	t.Helper()
	registry := plugins.NewRegistry()
	if err := registry.Register(Plugin()); err != nil {
		t.Fatalf("synthetic plugin registration: %v", err)
	}
	enabled, err := registry.ResolveEnabled(nil)
	if err != nil {
		t.Fatal(err)
	}
	return registry, enabled
}

// assembledCatalogs is the host assembly: frozen catalogs + implementation
// table + dispatch handlers from ONE registry.
func assembledCatalogs(t *testing.T) (*plugins.Registry, []string, *attempt.Catalogs) {
	t.Helper()
	registry, enabled := assemblingRegistry(t)
	catalogs, err := attempt.BuildCatalogs(registry, enabled)
	if err != nil {
		t.Fatal(err)
	}
	return registry, enabled, catalogs
}

// TestSyntheticDeclarationAssemblesGenericSeams proves the DECLARATION half
// of the seam: scope plan derivation, frozen catalog membership, dispatch
// handlers and the template's collection grant purpose all derive from the
// plugin alone.
func TestSyntheticDeclarationAssemblesGenericSeams(t *testing.T) {
	registry, enabled, catalogs := assembledCatalogs(t)

	// The derived source scope plan: the declared role × connection kind.
	scopes := attempt.SourceScopes(registry, enabled)
	if len(scopes) != 1 || scopes[0].Role != SourceItemRole ||
		len(scopes[0].ConnectionTypes) != 1 || scopes[0].ConnectionTypes[0] != ConnectionKindValue {
		t.Fatalf("scopes=%+v, want exactly %s×[%s]", scopes, SourceItemRole, ConnectionKindValue)
	}
	if scopes[0].IntegrationKind() != "synthetic" {
		t.Fatalf("integration kind=%q, want synthetic", scopes[0].IntegrationKind())
	}

	// The model-visible catalog carries the grant tool (declared plan and
	// evidence projection included) and never the internal collector.
	catalog, err := catalogs.CatalogFor(attempt.AgentVersion)
	if err != nil {
		t.Fatal(err)
	}
	if _, known := catalog.Lookup(QueryToolName); !known {
		t.Fatalf("frozen analysis catalog lacks %s", QueryToolName)
	}
	if _, known := catalog.Lookup(CollectToolName); known {
		t.Fatalf("internal tool %s leaked into the model catalog", CollectToolName)
	}
	entry, ok := catalogs.Handlers[QueryToolName]
	if !ok {
		t.Fatalf("assembled dispatch table lacks %s", QueryToolName)
	}
	if entry.Definition.Grant == nil ||
		entry.Definition.Grant.Purpose != QueryGrantPurpose ||
		entry.Definition.Grant.SourceItemRole != SourceItemRole ||
		entry.Definition.Grant.SourceRefArgument != "sourceRef" ||
		!entry.Definition.Grant.FreezeExecutionArguments {
		t.Fatalf("grant plan=%+v, want the declared authorization plan", entry.Definition.Grant)
	}
	if entry.Definition.EvidenceProjector == nil || !entry.Definition.ProducesEvidence {
		t.Fatalf("evidence projector not declared on %s", QueryToolName)
	}
	// The closed argument contract travels with the definition: unknown
	// fields and missing required arguments are refused before any row.
	if err := attempt.ValidateToolArguments(entry.Definition, []byte(`{"query":"up"}`)); err != nil {
		t.Fatalf("valid arguments rejected: %v", err)
	}
	if err := attempt.ValidateToolArguments(entry.Definition, []byte(`{"query":"up","resourceRef":"x"}`)); err == nil {
		t.Fatal("unknown argument accepted")
	}
	if err := attempt.ValidateToolArguments(entry.Definition, []byte(`{}`)); err == nil {
		t.Fatal("missing required argument accepted")
	}

	// The inspection template declares its collection grant purpose.
	template, ok := registry.InspectionTemplate("synthetic-plugin", TemplateID, TemplateVersion)
	if !ok || template.GrantPurpose != CollectionGrantPurpose {
		t.Fatalf("template=%+v ok=%t, want the declared collection purpose", template, ok)
	}
	if !registry.IsCollectionGrantPurpose(CollectionGrantPurpose) {
		t.Fatal("declared collection purpose not resolvable from the registry")
	}
}

// fakePlatform is the minimal gateway stub: it records the authorized
// request and answers deterministically. It never sees credentials.
type fakePlatform struct {
	request  plugins.PlatformRequest
	response *plugins.PlatformResponse
	err      error
}

func (f *fakePlatform) Call(_ context.Context, req plugins.PlatformRequest) (*plugins.PlatformResponse, error) {
	f.request = req
	if f.err != nil {
		return nil, f.err
	}
	return f.response, nil
}

// TestSyntheticToolExecutesThroughGenericDispatch proves the assembled entry
// executes the typed handler over the gateway seam — no credential, URL or
// TLS material ever reaches the handler.
func TestSyntheticToolExecutesThroughGenericDispatch(t *testing.T) {
	_, _, catalogs := assembledCatalogs(t)
	entry, ok := catalogs.Handlers[QueryToolName]
	if !ok {
		t.Fatal("synthetic_query missing from the dispatch table")
	}
	gateway := &fakePlatform{response: &plugins.PlatformResponse{StatusCode: 200, Body: []byte(`{"value":"ok"}`)}}
	raw, err := entry.Invoke(context.Background(), plugins.ToolExecution{
		Arguments: json.RawMessage(`{"query":"up"}`), Platform: gateway,
	})
	if err != nil {
		t.Fatal(err)
	}
	var result queryResult
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if !result.Success || result.Output != `{"value":"ok"}` {
		t.Fatalf("result=%+v, want the echoed success payload", result)
	}
	if gateway.request.Method != "GET" || gateway.request.Path != "/query" {
		t.Fatalf("request=%+v, want the declared GET /query shape", gateway.request)
	}
	if len(gateway.request.Header) != 0 {
		t.Fatalf("handler injected headers %v — credential material must stay at the gateway", gateway.request.Header)
	}
}

// seedWorld opens the product schema, assembles the registry-driven seams
// and freezes one Running-capable analysis attempt carrying a frozen
// synthetic source lineage item.
type seededWorld struct {
	db                                     *sql.DB
	service                                *attempt.Service
	evidence                               *evidence.Service
	catalogs                               *attempt.Catalogs
	attemptID                              int64
	connectionID, revisionID, generationID int64
	connectionName                         string
}

func seedWorld(t *testing.T) *seededWorld {
	t.Helper()
	world := &seededWorld{connectionName: "synthetic-a"}
	world.db = testfixture.OpenDB(t)
	registry, enabled, catalogs := assembledCatalogs(t)
	_ = registry
	_ = enabled
	world.catalogs = catalogs
	service := attempt.NewService(world.db)
	service.Catalogs = catalogs
	// The composition read seam: a real read-only reader from the same
	// fixture file (never the writable pool).
	var seq int
	var name, path string
	if err := world.db.QueryRow(`PRAGMA database_list`).Scan(&seq, &name, &path); err != nil {
		t.Fatal(err)
	}
	reader, err := execution.OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	if err := service.SetReader(reader); err != nil {
		t.Fatal(err)
	}
	world.service = service
	// The evidence authority registers the DECLARED projectors generically
	// and wires the attempt machine's evidence seam.
	world.evidence = evidence.NewService(world.db)
	world.evidence.RegisterEntryProjectors(catalogs.HandlersTable())
	service.EvidenceWriter = world.evidence.WriteForToolCall

	now := time.Now().UTC().Format(time.RFC3339Nano)
	// Credential root binding (the fence the grant re-checks).
	if _, err := world.db.Exec(`INSERT OR IGNORE INTO root_key_state(id,binding_revision,verifier_nonce,verifier_ciphertext,bound_at) VALUES(1,1,?,?,?)`,
		[]byte(strings.Repeat("e", 12)), []byte(strings.Repeat("f", 16)), now); err != nil {
		t.Fatal(err)
	}
	// One enabled synthetic-hook connection at its current (revision,
	// generation) pair.
	connection, err := world.db.Exec(`INSERT INTO connections(name,type,enabled,created_at) VALUES(?,?,1,?)`, world.connectionName, ConnectionKindValue, now)
	if err != nil {
		t.Fatal(err)
	}
	world.connectionID, _ = connection.LastInsertId()
	revision, err := world.db.Exec(`INSERT INTO connection_revisions(connection_id,revision_seq,config_json,created_at) VALUES(?,1,?,?)`,
		world.connectionID, `{"type":"`+ConnectionKindValue+`","baseUrl":"http://synthetic.test","authType":"none"}`, now)
	if err != nil {
		t.Fatal(err)
	}
	world.revisionID, _ = revision.LastInsertId()
	generation, err := world.db.Exec(`INSERT INTO credential_generations(connection_id,generation_seq,envelope_version,key_binding_revision,nonce,ciphertext,created_at) VALUES(?,1,1,1,?,?,?)`,
		world.connectionID, []byte(strings.Repeat("n", 12)), []byte(strings.Repeat("f", 32)), now)
	if err != nil {
		t.Fatal(err)
	}
	world.generationID, _ = generation.LastInsertId()
	if _, err := world.db.Exec(`UPDATE connections SET current_revision_id=?, current_credential_generation_id=?, row_version=row_version+1 WHERE id=?`,
		world.revisionID, world.generationID, world.connectionID); err != nil {
		t.Fatal(err)
	}
	// One alert occurrence and its Queued analysis attempt with the frozen
	// assembled catalog and the declared source lineage item.
	source, err := world.db.Exec(`INSERT INTO alert_sources(source_key,protocol,enabled,created_at) VALUES('synthetic-source','alertmanager',1,?)`, now)
	if err != nil {
		t.Fatal(err)
	}
	sourceID, _ := source.LastInsertId()
	labels := `{"alertname":"SyntheticAlert","severity":"critical"}`
	labelsDigest := sha256.Sum256([]byte(labels))
	occurrence, err := world.db.Exec(`INSERT INTO alert_occurrences(source_id,fingerprint,starts_at,state,labels_canonical,labels_digest,severity,title,annotations_canonical,resource,first_seen_at,last_state_change_at) VALUES(?,?,?,'Firing',?,?,?,?,?,'node-a',?,?)`,
		sourceID, []byte{0, 0, 0, 0, 0, 0, 0, 9}, now, labels, hex.EncodeToString(labelsDigest[:]), "critical", "SyntheticAlert", "{}", now, now)
	if err != nil {
		t.Fatal(err)
	}
	occurrenceID, _ := occurrence.LastInsertId()
	analysis, err := world.db.Exec(`INSERT INTO initial_analyses(occurrence_id,state,input_snapshot_digest,created_by,created_at) VALUES(?,'Queued',?,NULL,?)`,
		occurrenceID, strings.Repeat("a", 64), now)
	if err != nil {
		t.Fatal(err)
	}
	analysisID, _ := analysis.LastInsertId()
	attemptRow, err := world.db.Exec(`INSERT INTO execution_attempts(attempt_type,scope_type,scope_id,state,quoin_release_version,agent_version,created_at) VALUES('initial_analysis','analysis',?,'Queued','test',?,?)`,
		analysisID, attempt.AgentVersion, now)
	if err != nil {
		t.Fatal(err)
	}
	world.attemptID, _ = attemptRow.LastInsertId()
	catalog, err := catalogs.CatalogFor(attempt.AgentVersion)
	if err != nil {
		t.Fatal(err)
	}
	catalogDoc, err := json.Marshal(catalog)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := world.db.Exec(`INSERT INTO attempt_input_snapshots(attempt_id,schema_kind,renderer_version,content_digest,tool_catalog_json,created_at) VALUES(?,'initial_analysis_v1','synthetic-contract-test',?,?,?)`,
		world.attemptID, strings.Repeat("b", 64), string(catalogDoc), now)
	if err != nil {
		t.Fatal(err)
	}
	snapshotID, _ := snapshot.LastInsertId()
	occurrenceDigest := sha256.Sum256([]byte("occurrence:" + fmt.Sprint(occurrenceID)))
	if _, err := world.db.Exec(`INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,occurrence_id) VALUES(?,1,'user',?,?)`,
		snapshotID, hex.EncodeToString(occurrenceDigest[:]), occurrenceID); err != nil {
		t.Fatal(err)
	}
	// The frozen synthetic_source lineage item — exactly what the derived
	// scope plan freezes at creation (attempt.SourceScopes → hosts).
	revisionDigest := sha256.Sum256([]byte("connection-revision:" + fmt.Sprint(world.revisionID)))
	if _, err := world.db.Exec(`INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,connection_revision_id) VALUES(?,2,?,?,?)`,
		snapshotID, SourceItemRole, hex.EncodeToString(revisionDigest[:]), world.revisionID); err != nil {
		t.Fatal(err)
	}
	// The chat_model grant the dispatch trigger requires: one enabled
	// qualified model provider (shared fixture).
	pConnectionID, pRevisionID, pGenerationID, pProbeResultID := testfixture.SeedModelProviderChain(t, world.db)
	if _, err := world.db.Exec(`INSERT INTO attempt_connection_grants(attempt_id,purpose,connection_id,connection_revision_id,credential_generation_id,qualified_probe_result_id,created_at) VALUES(?,'chat_model',?,?,?,?,?)`,
		world.attemptID, pConnectionID, pRevisionID, pGenerationID, pProbeResultID, now); err != nil {
		t.Fatal(err)
	}
	return world
}

// runRunning moves the seeded attempt to Running (Queued → Assigned →
// Running, one audited row_version hop each).
func (world *seededWorld) runRunning(t *testing.T) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	leaseUntil := time.Now().UTC().Add(5 * time.Minute).Format(time.RFC3339Nano)
	if _, err := world.db.Exec(`UPDATE execution_attempts SET state='Assigned',runtime_slot='plinth',boot_id='synthetic-boot',connection_epoch=1,lease_until=?,runtime_release_version='test',row_version=row_version+1 WHERE id=?`, leaseUntil, world.attemptID); err != nil {
		t.Fatal(err)
	}
	if _, err := world.db.Exec(`UPDATE execution_attempts SET state='Running',accepted_at=?,row_version=row_version+1 WHERE id=?`, now, world.attemptID); err != nil {
		t.Fatal(err)
	}
}

func (world *seededWorld) beginModelCall(t *testing.T) int64 {
	t.Helper()
	catalog, err := world.catalogs.CatalogFor(attempt.AgentVersion)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := catalog.Digest()
	if err != nil {
		t.Fatal(err)
	}
	var snapshotDigest string
	if err := world.db.QueryRow(`SELECT content_digest FROM attempt_input_snapshots WHERE attempt_id=?`, world.attemptID).Scan(&snapshotDigest); err != nil {
		t.Fatal(err)
	}
	callID, err := world.service.BeginModelCall(context.Background(), attempt.BeginCall{
		AttemptID: world.attemptID, CallSeq: 1, ModelID: "fixture-chat-1",
		PromptDigest: strings.Repeat("1", 64), ToolSchemaDigest: digest,
		InputDigest: strings.Repeat("2", 64), RenderedDigest: strings.Repeat("3", 64),
		InputItems: []attempt.ModelInputItem{
			{Sequence: 1, ItemKind: "system_contract", ContentDigest: strings.Repeat("4", 64), Role: "system"},
			{Sequence: 2, ItemKind: "tool_schema", ContentDigest: strings.Repeat("5", 64), Role: "system"},
			{Sequence: 3, ItemKind: "snapshot", ContentDigest: snapshotDigest, Role: "system"},
		},
		ContextBudget: 4096, MaxOutput: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	return callID
}

func (world *seededWorld) proposeQuery(t *testing.T, callID int64, arguments string) []attempt.ToolAuthorization {
	t.Helper()
	sum := sha256.Sum256([]byte(arguments))
	proposed := []attempt.ProposedTool{{
		ProviderIndex: 0, ProviderToolCallID: "synthetic-call-1", ToolName: QueryToolName,
		ArgumentsJSON: []byte(arguments), ArgumentsDigest: hex.EncodeToString(sum[:]),
	}}
	_, responseDigest, err := attempt.CanonicalChatResponseJSON("", proposed)
	if err != nil {
		t.Fatal(err)
	}
	authorizations, err := world.service.CompleteModelCall(context.Background(), attempt.CompleteCall{
		AttemptID: world.attemptID, CallID: callID, Outcome: "succeeded", FinishReason: "tool_calls",
		ProposedTools: proposed, ResponseDigest: responseDigest, ResponseComplete: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return authorizations
}

// TestSyntheticToolGrantResolvesValidatesAndSealsEvidence is the high
// behavior seam: the model proposes the synthetic tool, the GENERIC
// declaration-driven machinery freezes the declared grant on the frozen
// source, stamps the declared execution arguments, re-validates the fence
// at execution and seals deterministic Evidence — with zero per-tool code
// anywhere outside this plugin package.
func TestSyntheticToolGrantResolvesValidatesAndSealsEvidence(t *testing.T) {
	world := seedWorld(t)
	world.runRunning(t)
	callID := world.beginModelCall(t)
	authorizations := world.proposeQuery(t, callID, `{"query":"up"}`)

	if len(authorizations) != 1 {
		t.Fatalf("authorizations=%d, want 1", len(authorizations))
	}
	authorization := authorizations[0]
	if authorization.PreflightCode != "" {
		t.Fatalf("unexpected preflight: %+v", authorization)
	}
	if len(authorization.Grants) != 1 {
		t.Fatalf("grants=%+v, want one declared grant", authorization.Grants)
	}
	// The frozen grant closes onto the declared purpose and the exact
	// (connection, revision, generation) triple.
	var purpose string
	var grantedConnection, grantedRevision, grantedGeneration int64
	if err := world.db.QueryRow(`SELECT purpose,connection_id,connection_revision_id,credential_generation_id FROM attempt_connection_grants WHERE id=?`,
		authorization.Grants[0].GrantID).Scan(&purpose, &grantedConnection, &grantedRevision, &grantedGeneration); err != nil {
		t.Fatal(err)
	}
	if purpose != QueryGrantPurpose || grantedConnection != world.connectionID ||
		grantedRevision != world.revisionID || grantedGeneration != world.generationID {
		t.Fatalf("grant=(%s,%d,%d,%d), want the declared purpose on the frozen pair", purpose, grantedConnection, grantedRevision, grantedGeneration)
	}
	// The execution arguments carry the stamped declared source.
	var executionJSON string
	if err := world.db.QueryRow(`SELECT arguments_json FROM tool_call_execution_inputs WHERE tool_call_id=?`, authorization.ToolCallID).Scan(&executionJSON); err != nil {
		t.Fatal(err)
	}
	if executionJSON != `{"query":"up","sourceRef":"synthetic-a"}` {
		t.Fatalf("execution args=%s, want the proposal with the stamped source", executionJSON)
	}
	if string(authorization.ExecutionArgumentsJSON) != executionJSON {
		t.Fatalf("dispatch carried %s, want the frozen %s", authorization.ExecutionArgumentsJSON, executionJSON)
	}

	// The execution authorization re-check closes before the tool runs.
	if err := world.service.BeginToolCall(context.Background(), world.attemptID, authorization.ToolCallID); err != nil {
		t.Fatalf("begin tool call: %v", err)
	}
	var status string
	if err := world.db.QueryRow(`SELECT status FROM tool_calls WHERE id=?`, authorization.ToolCallID).Scan(&status); err != nil || status != "running" {
		t.Fatalf("tool call status=%q err=%v, want running", status, err)
	}

	// The succeeded execution seals deterministic Evidence through the
	// generically registered projector.
	finishedAt := time.Now().UTC().Format(time.RFC3339Nano)
	payload := fmt.Sprintf(`{"success":true,"startedAt":%q,"finishedAt":%q,"query":"up","output":"ok"}`, finishedAt, finishedAt)
	evidenceIDs, err := world.service.CompleteToolCall(context.Background(), attempt.ToolResult{
		AttemptID: world.attemptID, ToolCallID: authorization.ToolCallID,
		Outcome: "succeeded", ResultJSON: payload,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(evidenceIDs) != 1 {
		t.Fatalf("evidence=%v, want one sealed row", evidenceIDs)
	}
	var paramsJSON, observedAt, integrity, resultJSON string
	var evidenceArtifact sql.NullInt64
	if err := world.db.QueryRow(`SELECT params_json,observed_at,integrity,result_json,artifact_id FROM evidence WHERE id=?`,
		evidenceIDs[0]).Scan(&paramsJSON, &observedAt, &integrity, &resultJSON, &evidenceArtifact); err != nil {
		t.Fatal(err)
	}
	if paramsJSON != `{"query":"up"}` {
		t.Fatalf("evidence params=%s, want the frozen proposal", paramsJSON)
	}
	if observedAt != finishedAt || integrity != "complete" || resultJSON != payload || evidenceArtifact.Valid {
		t.Fatalf("evidence=(%s,%s,%s,artifact=%v), want the complete inline projection", observedAt, integrity, resultJSON, evidenceArtifact)
	}
	var target string
	if err := world.db.QueryRow(`SELECT target_type FROM evidence WHERE id=?`, evidenceIDs[0]).Scan(&target); err != nil || target != "initial_analysis" {
		t.Fatalf("evidence target=%q err=%v, want initial_analysis", target, err)
	}
}

// TestSyntheticCollectionGrantFreezesAndFencesThroughDeclarations proves the
// inspection collection seam: the template-declared purpose freezes the
// attempt grant, the generic fence refuses a stale pair, and the schema
// closure trigger (generic over declared config_ purposes) admits the
// synthetic purpose exactly like the metrics one.
func TestSyntheticCollectionGrantFreezesAndFencesThroughDeclarations(t *testing.T) {
	world := seedWorld(t)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	clock := func() time.Time { return time.Now().UTC() }

	// A dedicated guarded runner: collection grants, like every attempt
	// mutation, compose only on runner-owned transactions (ADR-0006).
	opRegistry := execution.NewRegistry()
	grantOp, err := opRegistry.Register(execution.Operation{
		Name: "test.collection.grant", Class: execution.ClassWrite, ObjectType: "inspection_run",
		Authorize: func(context.Context, *execution.Tx) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	runner := execution.NewRunnerWithClock(world.db, opRegistry, audit.NewWriterWithClock(clock), clock)

	// One manual plan → Running run → frozen check row → Queued collection
	// child: the minimal shape the collection closure trigger admits.
	plan, err := world.db.Exec(`INSERT INTO inspection_plans(plan_key,display_name,enabled,connection_id,plugin_id,template_id,template_version,params_json,scope_kind,scope_json,timezone,created_at,updated_at)
		VALUES('synthetic-plan','Synthetic plan',1,?,'synthetic-plugin',?,?,?,'integration','{"kind":"integration"}','UTC',?,?)`,
		world.connectionID, TemplateID, TemplateVersion, `{"expression":"echo"}`, now, now)
	if err != nil {
		t.Fatal(err)
	}
	planID, _ := plan.LastInsertId()
	run, err := world.db.Exec(`INSERT INTO inspection_runs(plan_key,plan_id,connection_id,plugin_id,template_id,template_version,frozen_params_json,frozen_scope_json,trigger_kind,state,created_at)
		VALUES('synthetic-plan',?,?,'synthetic-plugin',?,?,?, '{"kind":"integration"}','manual','Queued',?)`,
		planID, world.connectionID, TemplateID, TemplateVersion, `{"expression":"echo"}`, now)
	if err != nil {
		t.Fatal(err)
	}
	runID, _ := run.LastInsertId()
	// The run enters Running when collection starts (the frozen state
	// machine: Queued → Running with the evidence instant).
	if _, err := world.db.Exec(`UPDATE inspection_runs SET state='Running',evidence_at=?,row_version=row_version+1 WHERE id=? AND state='Queued'`, now, runID); err != nil {
		t.Fatal(err)
	}
	if _, err := world.db.Exec(`INSERT INTO inspection_run_checks(run_id,check_key,display_name,plugin_id,template_id,template_version,params_json,created_at)
		VALUES(?,'check-1','Synthetic check','synthetic-plugin',?,?, '{"expression":"echo"}',?)`,
		runID, TemplateID, TemplateVersion, now); err != nil {
		t.Fatal(err)
	}
	child, err := world.db.Exec(`INSERT INTO execution_attempts(attempt_type,scope_type,scope_id,check_key,state,quoin_release_version,created_at)
		VALUES('inspection_collection','run_check',?,'check-1','Queued','test',?)`, runID, now)
	if err != nil {
		t.Fatal(err)
	}
	childID, _ := child.LastInsertId()

	// The plan-run freezes the grant under the template's DECLARED purpose.
	ctx, err := execution.WithMetadata(context.Background(), execution.Metadata{
		CorrelationID: "synthetic-collection",
		Actor:         execution.Principal{Kind: execution.PrincipalSystem, ID: 0},
		Source:        execution.Source{Kind: execution.SourceTask},
	})
	if err != nil {
		t.Fatal(err)
	}
	template, ok := assemblingRegistryForLookup(t).InspectionTemplate("synthetic-plugin", TemplateID, TemplateVersion)
	if !ok {
		t.Fatal("template lookup failed")
	}
	var grant attempt.ToolGrant
	_, err = execution.Execute(ctx, runner, grantOp, func(tx *execution.Tx) (int64, error) {
		grant, err = attempt.FreezeConnectionGrant(ctx, tx, childID, world.connectionID, template.GrantPurpose)
		if err != nil {
			return 0, err
		}
		return grant.GrantID, nil
	}, func(id int64) int64 { return id })
	if err != nil {
		t.Fatal(err)
	}
	var purpose string
	var count int
	if err := world.db.QueryRow(`SELECT purpose, COUNT(*) OVER () FROM attempt_connection_grants WHERE attempt_id=?`, childID).Scan(&purpose, &count); err != nil {
		t.Fatal(err)
	}
	if purpose != CollectionGrantPurpose || count != 1 {
		t.Fatalf("grant=(%s,%d rows), want the single declared-purpose collection grant", purpose, count)
	}

	// The generic schema closure trigger applies to the declared purpose:
	// a config_ collection grant is structurally bound to inspection
	// collection attempts — the same refusal a metrics purpose gets, on a
	// non-collection attempt shape.
	stray, err := world.db.Exec(`INSERT INTO execution_attempts(attempt_type,scope_type,scope_id,state,quoin_release_version,created_at)
		VALUES('connection_probe','connection',?,'Queued','test',?)`, world.connectionID, now)
	if err != nil {
		t.Fatal(err)
	}
	strayID, _ := stray.LastInsertId()
	_, strayErr := execution.Execute(ctx, runner, grantOp, func(tx *execution.Tx) (int64, error) {
		if _, err := tx.ExecContext(ctx, `INSERT INTO attempt_connection_grants(attempt_id,purpose,connection_id,connection_revision_id,credential_generation_id,created_at)
			VALUES(?,?,?,?,?,?)`, strayID, CollectionGrantPurpose, world.connectionID, world.revisionID, world.generationID, now); err != nil {
			return 0, err
		}
		return 0, nil
	}, func(id int64) int64 { return id })
	if strayErr == nil ||
		(!strings.Contains(strayErr.Error(), "attempt connection grant must close over the exact active binding, purpose and selected qualification") &&
			!strings.Contains(strayErr.Error(), "declared collection grant requires one Queued Observation or Running Inspection Run collection Attempt")) {
		t.Fatalf("closure trigger err=%v, want the generic collection refusal", strayErr)
	}

	// The generic dispatch fence refuses a stale pair: a committed disable
	// wins the race against the queued collection.
	if _, err := execution.Execute(ctx, runner, grantOp, func(tx *execution.Tx) (int64, error) {
		if err := attempt.ValidateAttemptConnectionGrantsCurrent(ctx, tx, childID); err != nil {
			return 0, err
		}
		return 0, nil
	}, func(id int64) int64 { return id }); err != nil {
		t.Fatalf("current pair must pass: %v", err)
	}
	if _, err := world.db.Exec(`UPDATE connections SET enabled=0,row_version=row_version+1 WHERE id=?`, world.connectionID); err != nil {
		t.Fatal(err)
	}
	rollback := errors.New("test observation complete")
	_, err = execution.Execute(ctx, runner, grantOp, func(tx *execution.Tx) (int64, error) {
		fenceErr := attempt.ValidateAttemptConnectionGrantsCurrent(ctx, tx, childID)
		if !errors.Is(fenceErr, attempt.ErrGrantNotCurrent) {
			t.Fatalf("stale pair err=%v, want ErrGrantNotCurrent", fenceErr)
		}
		if err := attempt.ValidateConnectionGrantRowCurrent(ctx, tx, grant.GrantID); !errors.Is(err, attempt.ErrGrantNotCurrent) {
			t.Fatalf("row fence err=%v, want ErrGrantNotCurrent", err)
		}
		return 0, rollback
	}, func(id int64) int64 { return id })
	if !errors.Is(err, rollback) {
		t.Fatalf("fenced observation did not roll back cleanly: %v", err)
	}
}

func assemblingRegistryForLookup(t *testing.T) *plugins.Registry {
	t.Helper()
	registry := plugins.NewRegistry()
	if err := registry.Register(Plugin()); err != nil {
		t.Fatal(err)
	}
	return registry
}
