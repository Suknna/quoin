package testfixture

import (
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"testing"
	"time"
)

// SeedHTTPConnectionPair inserts an enabled HTTP connection with one frozen
// revision/credential pair for plugin orchestration tests. Its dummy encrypted
// bytes are never decrypted: gateway encryption and probe qualification have
// their own domain tests. Keep the seed in one place so every synthetic
// acceptance path exercises the same connection/grant shape.
func SeedHTTPConnectionPair(t *testing.T, db *sql.DB, name, kind string, at time.Time) int64 {
	t.Helper()
	stamp := at.UTC().Format(time.RFC3339Nano)
	config, err := json.Marshal(map[string]string{"type": kind, "baseUrl": "https://synthetic.test", "authType": "none"})
	if err != nil {
		t.Fatal(err)
	}
	connection, err := db.Exec(`INSERT INTO connections(name,type,enabled,created_at) VALUES(?,?,1,?)`, name, kind, stamp)
	if err != nil {
		t.Fatal(err)
	}
	connectionID, err := connection.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	revision, err := db.Exec(`INSERT INTO connection_revisions(connection_id,revision_seq,config_json,created_at) VALUES(?,1,?,?)`, connectionID, string(config), stamp)
	if err != nil {
		t.Fatal(err)
	}
	revisionID, err := revision.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, 12)
	binary.BigEndian.PutUint64(nonce[4:], uint64(connectionID)) // deterministic and unique per test connection
	generation, err := db.Exec(`INSERT INTO credential_generations(connection_id,generation_seq,envelope_version,key_binding_revision,nonce,ciphertext,created_at) VALUES(?,1,1,1,?,?,?)`,
		connectionID, nonce, make([]byte, 32), stamp)
	if err != nil {
		t.Fatal(err)
	}
	generationID, err := generation.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE connections SET current_revision_id=?,current_credential_generation_id=?,row_version=row_version+1 WHERE id=?`, revisionID, generationID, connectionID); err != nil {
		t.Fatal(err)
	}
	return connectionID
}
