package investigation

import (
	"database/sql"
	"encoding/json"
	"testing"
)

type directChatContext struct {
	key                  string
	configID, contractID int64
}

// seedDirectChatBusinessContext uses only published declaration references;
// tests exercise the same explicit authority closure as the UI command.
func seedDirectChatBusinessContext(t *testing.T, db *sql.DB, key string, metricsConnectionID int64) directChatContext {
	t.Helper()
	now := testNow()
	if _, err := db.Exec(`INSERT INTO label_contract_state(id,row_version,updated_at) VALUES(1,1,?)`, now); err != nil {
		t.Fatal(err)
	}
	contract, err := db.Exec(`INSERT INTO label_contracts(version,yaml_body,contract_json,digest,parser_version,schema_version,state,row_version,created_at) VALUES(1,'fixture',?,?,'fixture','v1','draft',1,?)`, `{"label_contract":{"business_system_label":"business_system"}}`, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", now)
	if err != nil {
		t.Fatal(err)
	}
	contractID, _ := contract.LastInsertId()
	if _, err := db.Exec(`INSERT INTO label_contract_activations(contract_id,expected_target_row_version,expected_state_row_version,items_json,created_at) VALUES(?,1,1,'[]',?)`, contractID, now); err != nil {
		t.Fatal(err)
	}
	system, err := db.Exec(`INSERT INTO business_systems(key,display_name,enabled,created_at) VALUES(?,?,0,?)`, key, "Mall live Prometheus", now)
	if err != nil {
		t.Fatal(err)
	}
	systemID, _ := system.LastInsertId()
	declaration, err := json.Marshal(map[string]any{"systemKey": key, "displayName": "Mall live Prometheus", "MetricsConnectionID": metricsConnectionID, "resources": []any{map[string]any{"name": "default", "displayName": "Default", "matchLabels": map[string]string{"business_system": key}, "discoveryMetric": "up", "identityLabels": []string{"instance"}, "allowedMetrics": []string{"up"}}}})
	if err != nil {
		t.Fatal(err)
	}
	config, err := db.Exec(`INSERT INTO business_system_config_versions(business_system_id,version_seq,state,yaml_body,parser_version,schema_version,label_contract_version_id,declaration_json,journey_catalog_digest,journey_catalog_version,digest,created_at,system_key,display_name,metrics_connection_id,enabled,timezone) VALUES(?,1,'draft','{}','test','test',?,?,'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb',1,?,?,?,?,?,1,'UTC')`, systemID, contractID, string(declaration), "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", now, key, "Mall live Prometheus", metricsConnectionID)
	if err != nil {
		t.Fatal(err)
	}
	configID, _ := config.LastInsertId()
	if _, err := db.Exec(`UPDATE business_systems SET current_config_version_id=?,display_name=?,enabled=1,timezone='UTC',row_version=row_version+1 WHERE id=?`, configID, "Mall live Prometheus", systemID); err != nil {
		t.Fatal(err)
	}
	return directChatContext{key: key, configID: configID, contractID: contractID}
}
