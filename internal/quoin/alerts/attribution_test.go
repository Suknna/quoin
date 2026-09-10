package alerts

// Attribution tests (T17): occurrence business_system_id is written once,
// inside the creating delivery transaction, under the then-active Label
// Contract; missing/unknown label values route to 未归属 (NULL) and later
// contract/system changes never rewrite historical attribution (CONTEXT
//「标签契约」, DATA-ALERT-007 — alert_change_log admits no attribution change
// type, so a mid-life flip is structurally unsupported).

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

const activeContractProjection = `{"label_contract":{"business_system_label":"business_system"}}`

// activateContract drives the real activation INSERT (items_json=[]) through
// the frozen trigger so the state pointer moves exactly like production.
func activateContract(t *testing.T, service *Service, version int64, projection string) {
	t.Helper()
	ctx := context.Background()
	conn, err := service.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = conn.ExecContext(context.Background(), `ROLLBACK`) }()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := conn.ExecContext(ctx, `INSERT INTO label_contracts(version,yaml_body,contract_json,digest,parser_version,schema_version,state,row_version,created_at) VALUES(?,?,?,?,?,'v1','draft',1,?)`,
		version, "label_contract: fixture", projection, strings.Repeat("a", 64), "v1", now); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO label_contract_activations(contract_id,expected_target_row_version,expected_state_row_version,expected_current_contract_id,items_json,created_at)
		SELECT id,1,1,NULL,'[]',? FROM label_contracts WHERE version=?`, now, version); err != nil {
		t.Fatalf("activation insert: %v", err)
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		t.Fatal(err)
	}
}

// seedBusinessSystem creates the Disabled aggregate row the frozen insert
// trigger demands; attribution matches by stable key regardless of enabled.
func seedBusinessSystem(t *testing.T, service *Service, key string) {
	t.Helper()
	if _, err := service.db.Exec(`INSERT INTO business_systems(key,display_name,enabled,row_version,created_at) VALUES(?,?,0,1,?)`, key, key, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
}

// installAttributionProjections supplies the immutable projections owned by
// the business-configuration migration. The alert package deliberately does
// not own these tables; this fixture keeps the ingestion contract testable
// until that coordinated schema lands in the generated authority.
func installAttributionProjections(t *testing.T, service *Service) {
	t.Helper()
	for _, statement := range []string{
		// These are the production definitions promised by the business owner;
		// keeping the FKs here makes fixture insertion follow the same ownership
		// and immutability relationships as the generated authority.
		`CREATE TABLE IF NOT EXISTS config_alert_source_refs (id INTEGER PRIMARY KEY, config_version_id INTEGER NOT NULL REFERENCES business_system_config_versions(id) ON UPDATE RESTRICT ON DELETE RESTRICT, alert_source_id INTEGER NOT NULL REFERENCES alert_sources(id) ON UPDATE RESTRICT ON DELETE RESTRICT, UNIQUE(config_version_id, alert_source_id)) STRICT`,
		`CREATE TABLE IF NOT EXISTS config_alert_label_conditions (id INTEGER PRIMARY KEY, config_version_id INTEGER NOT NULL REFERENCES business_system_config_versions(id) ON UPDATE RESTRICT ON DELETE RESTRICT, label_name TEXT NOT NULL, label_value TEXT NOT NULL, UNIQUE(config_version_id, label_name)) STRICT`,
	} {
		if _, err := service.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
}

// seedAttributionMetricsConnection creates the required explicit metrics
// reference for an otherwise alert-only configuration fixture. Attribution
// never reads it, but a config version must remain structurally valid.
func seedAttributionMetricsConnection(t *testing.T, service *Service) int64 {
	t.Helper()
	var existing int64
	if err := service.db.QueryRow(`SELECT id FROM connections WHERE name='attribution-metrics'`).Scan(&existing); err == nil {
		return existing
	} else if !errors.Is(err, sql.ErrNoRows) {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := service.db.Exec(`INSERT INTO connections(name,type,enabled,created_at) VALUES('attribution-metrics','thanos',0,?)`, now)
	if err != nil {
		t.Fatal(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// publishAttributionConfig creates the smallest valid immutable configuration
// version and advances the real current-version pointer. Attribution must read
// this pointer, not a draft or an old published row.
func publishAttributionConfig(t *testing.T, service *Service, key string, sourceIDs []int64, conditions map[string]string) {
	t.Helper()
	var systemID, contractID int64
	if err := service.db.QueryRow(`SELECT id FROM business_systems WHERE key=?`, key).Scan(&systemID); err != nil {
		t.Fatal(err)
	}
	if err := service.db.QueryRow(`SELECT current_contract_id FROM label_contract_state WHERE id=1`).Scan(&contractID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	metricsConnectionID := seedAttributionMetricsConnection(t, service)
	var versionSeq int64
	if err := service.db.QueryRow(`SELECT COALESCE(MAX(version_seq), 0) + 1 FROM business_system_config_versions WHERE business_system_id=?`, systemID).Scan(&versionSeq); err != nil {
		t.Fatal(err)
	}
	insert, err := service.db.Exec(`INSERT INTO business_system_config_versions(business_system_id,version_seq,state,yaml_body,parser_version,schema_version,label_contract_version_id,journey_catalog_digest,journey_catalog_version,digest,created_at,system_key,display_name,metrics_connection_id,enabled,timezone) VALUES(?,?,'draft','fixture','fixture','v1',?,?,'fixture',?,? ,?,?,?,0,'UTC')`, systemID, versionSeq, contractID, strings.Repeat("b", 64), strings.Repeat("c", 64), now, key, key, metricsConnectionID)
	if err != nil {
		t.Fatal(err)
	}
	versionID, err := insert.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	for _, sourceID := range sourceIDs {
		if _, err := service.db.Exec(`INSERT INTO config_alert_source_refs(config_version_id,alert_source_id) VALUES(?,?)`, versionID, sourceID); err != nil {
			t.Fatal(err)
		}
	}
	for name, value := range conditions {
		if _, err := service.db.Exec(`INSERT INTO config_alert_label_conditions(config_version_id,label_name,label_value) VALUES(?,?,?)`, versionID, name, value); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := service.db.Exec(`UPDATE business_systems SET display_name=?, enabled=0, timezone='UTC', current_config_version_id=?, row_version=row_version+1 WHERE id=?`, key, versionID, systemID); err != nil {
		t.Fatal(err)
	}
}

func deliverWebhook(t *testing.T, service *Service, relayID string, labels map[string]string, startsAt string) DeliveryResult {
	t.Helper()
	return deliverWebhookFrom(t, service, relayID, 1, 1, labels, startsAt)
}

func deliverWebhookFrom(t *testing.T, service *Service, relayID string, sourceID, credentialID int64, labels map[string]string, startsAt string) DeliveryResult {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"status": "firing",
		"alerts": []map[string]any{{
			"status":   "firing",
			"labels":   labels,
			"startsAt": startsAt,
			"endsAt":   "0001-01-01T00:00:00Z",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Deliver(context.Background(), relayID, sourceID, credentialID, 1, body, time.Now().UTC())
	if err != nil {
		t.Fatalf("deliver %s: %v", relayID, err)
	}
	if !result.Accepted {
		t.Fatalf("deliver %s rejected: %+v", relayID, result)
	}
	return result
}

func occurrenceBusinessID(t *testing.T, service *Service, occurrenceID int64) *int64 {
	t.Helper()
	var value *int64
	if err := service.db.QueryRow(`SELECT business_system_id FROM alert_occurrences WHERE id=?`, occurrenceID).Scan(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestAttributionRoutesUnderActiveContract(t *testing.T) {
	service, _, done := newTestService(t)
	defer done()
	ctx := context.Background()
	seedSource(t, service, ctx, "src")
	seedBusinessSystem(t, service, "payments")

	// Before any contract is active everything routes to 未归属.
	unattributed := deliverWebhook(t, service, "t17-pre-contract", map[string]string{
		"alertname": "PreContract", "business_system": "payments",
	}, "2026-09-01T10:00:00Z")
	if id := occurrenceBusinessID(t, service, unattributed.Occurrences[0].ID); id != nil {
		t.Fatalf("pre-contract occurrence must stay unattributed, got %d", *id)
	}

	activateContract(t, service, 1, activeContractProjection)
	installAttributionProjections(t, service)
	publishAttributionConfig(t, service, "payments", nil, nil)

	// Known value → attributed; unknown value and missing label → 未归属.
	known := deliverWebhook(t, service, "t17-known", map[string]string{
		"alertname": "Known", "business_system": "payments",
	}, "2026-09-01T10:01:00Z")
	if id := occurrenceBusinessID(t, service, known.Occurrences[0].ID); id == nil || *id != 1 {
		t.Fatalf("known value must attribute to business system 1, got %v", id)
	}
	unknownValue := deliverWebhook(t, service, "t17-unknown", map[string]string{
		"alertname": "Unknown", "business_system": "not-a-system",
	}, "2026-09-01T10:02:00Z")
	if id := occurrenceBusinessID(t, service, unknownValue.Occurrences[0].ID); id != nil {
		t.Fatalf("unknown value must stay unattributed, got %d", *id)
	}
	missingLabel := deliverWebhook(t, service, "t17-missing", map[string]string{
		"alertname": "Missing",
	}, "2026-09-01T10:03:00Z")
	if id := occurrenceBusinessID(t, service, missingLabel.Occurrences[0].ID); id != nil {
		t.Fatalf("missing label must stay unattributed, got %d", *id)
	}
}

func TestAttributionRestrictsTheContractSelectedBusiness(t *testing.T) {
	service, _, done := newTestService(t)
	defer done()
	ctx := context.Background()
	matchingSourceID, matchingCredentialID := seedSource(t, service, ctx, "matching-source")
	otherSourceID, otherCredentialID := seedSource(t, service, ctx, "other-source")
	seedBusinessSystem(t, service, "payments")
	activateContract(t, service, 1, activeContractProjection)
	installAttributionProjections(t, service)
	publishAttributionConfig(t, service, "payments", []int64{matchingSourceID}, map[string]string{"environment": "production", "team": "payments"})

	deliverFrom := func(relayID string, sourceID, credentialID int64, labels map[string]string, startsAt string) *int64 {
		t.Helper()
		result, err := service.Deliver(ctx, relayID, sourceID, credentialID, 1, mustBody(t, labels, startsAt), time.Now().UTC())
		if err != nil || !result.Accepted || len(result.Occurrences) != 1 {
			t.Fatalf("delivery %s: result=%+v err=%v", relayID, result, err)
		}
		return occurrenceBusinessID(t, service, result.Occurrences[0].ID)
	}

	// A configured source does not assign alerts without the Label Contract's
	// mandatory business label, and every configured condition must match.
	if id := deliverFrom("source-alone", matchingSourceID, matchingCredentialID, map[string]string{"alertname": "SourceOnly", "environment": "production", "team": "payments"}, "2026-09-01T13:00:00Z"); id != nil {
		t.Fatalf("source alone must not attribute, got %d", *id)
	}
	if id := deliverFrom("wrong-source", otherSourceID, otherCredentialID, map[string]string{"alertname": "WrongSource", "business_system": "payments", "environment": "production", "team": "payments"}, "2026-09-01T13:01:00Z"); id != nil {
		t.Fatalf("unlisted source must stay unattributed, got %d", *id)
	}
	if id := deliverFrom("wrong-label", matchingSourceID, matchingCredentialID, map[string]string{"alertname": "WrongLabel", "business_system": "payments", "environment": "staging", "team": "payments"}, "2026-09-01T13:02:00Z"); id != nil {
		t.Fatalf("mismatched condition must stay unattributed, got %d", *id)
	}
	if id := deliverFrom("matched", matchingSourceID, matchingCredentialID, map[string]string{"alertname": "Matched", "business_system": "payments", "environment": "production", "team": "payments"}, "2026-09-01T13:03:00Z"); id == nil {
		t.Fatal("contract, source, and labels match must attribute")
	}
}

func TestAttributionUsesOnlyCurrentPublishedRestrictions(t *testing.T) {
	service, _, done := newTestService(t)
	defer done()
	ctx := context.Background()
	firstSourceID, firstCredentialID := seedSource(t, service, ctx, "first-source")
	secondSourceID, secondCredentialID := seedSource(t, service, ctx, "second-source")
	seedBusinessSystem(t, service, "payments")
	activateContract(t, service, 1, activeContractProjection)
	installAttributionProjections(t, service)

	// The initial unrestricted declaration attributes through any source. Its
	// attribution is a fact even after the new declaration becomes current.
	publishAttributionConfig(t, service, "payments", nil, nil)
	first := deliverWebhookFrom(t, service, "before-restriction", firstSourceID, firstCredentialID, map[string]string{"alertname": "Before", "business_system": "payments"}, "2026-09-01T14:00:00Z")
	if id := occurrenceBusinessID(t, service, first.Occurrences[0].ID); id == nil {
		t.Fatal("unrestricted current declaration must attribute")
	}

	// Only the pointer-selected published version may govern later first
	// observations. Empty source refs leave sources unrestricted, while the
	// exact condition still narrows matches.
	publishAttributionConfig(t, service, "payments", nil, map[string]string{"environment": "production"})
	if id := occurrenceBusinessID(t, service, first.Occurrences[0].ID); id == nil {
		t.Fatal("later declaration must not rewrite historical attribution")
	}
	if result := deliverWebhookFrom(t, service, "empty-refs-label-mismatch", secondSourceID, secondCredentialID, map[string]string{"alertname": "Mismatch", "business_system": "payments", "environment": "staging"}, "2026-09-01T14:01:00Z"); occurrenceBusinessID(t, service, result.Occurrences[0].ID) != nil {
		t.Fatal("empty refs must not bypass configured exact labels")
	}
	if result := deliverWebhookFrom(t, service, "empty-refs-label-match", secondSourceID, secondCredentialID, map[string]string{"alertname": "Match", "business_system": "payments", "environment": "production"}, "2026-09-01T14:02:00Z"); occurrenceBusinessID(t, service, result.Occurrences[0].ID) == nil {
		t.Fatal("empty refs must accept any source when exact labels match")
	}
}

func TestAttributionIsWriteOnceAtCreation(t *testing.T) {
	service, _, done := newTestService(t)
	defer done()
	ctx := context.Background()
	seedSource(t, service, ctx, "src")

	// Occurrence created with a value no system claims yet.
	early := deliverWebhook(t, service, "t17-early", map[string]string{
		"alertname": "Early", "business_system": "payments",
	}, "2026-09-01T11:00:00Z")
	if id := occurrenceBusinessID(t, service, early.Occurrences[0].ID); id != nil {
		t.Fatalf("no business system exists yet; must be unattributed, got %d", *id)
	}

	activateContract(t, service, 1, activeContractProjection)
	seedBusinessSystem(t, service, "payments")
	installAttributionProjections(t, service)
	publishAttributionConfig(t, service, "payments", nil, nil)

	// A repeat firing for the SAME occurrence must not re-attribute: the
	// frozen alert_change_log CHECK admits only created|state_changed, so a
	// mid-life attribution flip is structurally unsupported (write-once).
	repeat, err := service.Deliver(context.Background(), "t17-early-repeat", 1, 1, 1, mustBody(t, map[string]string{
		"alertname": "Early", "business_system": "payments",
	}, "2026-09-01T11:00:00Z"), time.Now().UTC())
	if err != nil || !repeat.Accepted {
		t.Fatalf("repeat delivery: %+v %v", repeat, err)
	}
	if id := occurrenceBusinessID(t, service, early.Occurrences[0].ID); id != nil {
		t.Fatalf("repeat delivery must not rewrite historical attribution, got %d", *id)
	}

	// A NEW occurrence with the same labels attributes under the contract.
	later := deliverWebhook(t, service, "t17-later", map[string]string{
		"alertname": "Early", "business_system": "payments",
	}, "2026-09-01T11:30:00Z")
	if id := occurrenceBusinessID(t, service, later.Occurrences[0].ID); id == nil {
		t.Fatal("new occurrence after activation must attribute")
	}
}

func TestSnapshotFilterAndDetailKey(t *testing.T) {
	service, _, done := newTestService(t)
	defer done()
	ctx := context.Background()
	seedSource(t, service, ctx, "src")
	seedBusinessSystem(t, service, "payments")
	seedBusinessSystem(t, service, "billing")
	activateContract(t, service, 1, activeContractProjection)
	installAttributionProjections(t, service)
	publishAttributionConfig(t, service, "payments", nil, nil)
	publishAttributionConfig(t, service, "billing", nil, nil)

	deliverWebhook(t, service, "t17-pay-1", map[string]string{"alertname": "Pay1", "business_system": "payments"}, "2026-09-01T12:00:00Z")
	deliverWebhook(t, service, "t17-bill-1", map[string]string{"alertname": "Bill1", "business_system": "billing"}, "2026-09-01T12:01:00Z")
	deliverWebhook(t, service, "t17-none-1", map[string]string{"alertname": "None1"}, "2026-09-01T12:02:00Z")

	unfiltered, err := service.AlertSnapshot(ctx, "Firing", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(unfiltered.Items) != 3 {
		t.Fatalf("unfiltered snapshot must list all three, got %d", len(unfiltered.Items))
	}
	var payID int64
	for _, item := range unfiltered.Items {
		switch item.Labels["alertname"] {
		case "Pay1":
			if item.BusinessSystem == nil || *item.BusinessSystem != "payments" {
				t.Fatalf("Pay1 attribution wrong: %v", item.BusinessSystem)
			}
			payID, _ = strconv.ParseInt(item.ID, 10, 64)
		case "Bill1":
			if item.BusinessSystem == nil || *item.BusinessSystem != "billing" {
				t.Fatalf("Bill1 attribution wrong: %v", item.BusinessSystem)
			}
		case "None1":
			if item.BusinessSystem != nil {
				t.Fatalf("None1 must be unattributed, got %v", *item.BusinessSystem)
			}
		}
	}

	filtered, err := service.AlertSnapshot(ctx, "Firing", "payments")
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered.Items) != 1 || filtered.Items[0].Labels["alertname"] != "Pay1" {
		t.Fatalf("payments filter must return exactly Pay1, got %+v", filtered.Items)
	}

	// An unknown system key is a legitimate empty filter result.
	empty, err := service.AlertSnapshot(ctx, "Firing", "ghost")
	if err != nil {
		t.Fatal(err)
	}
	if len(empty.Items) != 0 {
		t.Fatalf("unknown key must filter to empty, got %d", len(empty.Items))
	}

	detail, err := service.GetOccurrence(ctx, payID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.BusinessSystem == nil || *detail.BusinessSystem != "payments" {
		t.Fatalf("detail must carry businessSystemKey=payments, got %v", detail.BusinessSystem)
	}
}

func mustBody(t *testing.T, labels map[string]string, startsAt string) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"status": "firing",
		"alerts": []map[string]any{{
			"status":   "firing",
			"labels":   labels,
			"startsAt": startsAt,
			"endsAt":   "0001-01-01T00:00:00Z",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}
