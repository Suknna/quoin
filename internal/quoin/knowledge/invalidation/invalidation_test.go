package invalidation

// DATA-TX-011 unit coverage: the shared writer flips operable candidates to
// SourceInvalid, exits produced versions stickily, deletes the FTS
// projection rows and stays idempotent across repeated events. The
// end-to-end commit-order races live in the investigation package tests.

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	gencontracts "github.com/Suknna/quoin/internal/gen/contracts"
	_ "modernc.org/sqlite"
)

func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/test.db?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(gencontracts.SchemaSQL); err != nil {
		t.Fatal(err)
	}
	return db
}

func testNow() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// seedSourceMaterial inserts one imported material source.
func seedSourceMaterial(t *testing.T, db *sql.DB, name string) (materialID, batchID int64) {
	t.Helper()
	now := testNow()
	material, err := db.Exec(`INSERT INTO source_materials(kind,digest,size_bytes,content,created_at) VALUES('knowledge_import',?,?,?,?)`, strings.Repeat("7", 64), 16, "原文 "+name, now)
	if err != nil {
		t.Fatal(err)
	}
	materialID, _ = material.LastInsertId()
	batch, err := db.Exec(`INSERT INTO knowledge_import_batches(source_material_id,state,generation,created_at) VALUES(?,'Processing',1,?)`, materialID, now)
	if err != nil {
		t.Fatal(err)
	}
	batchID, _ = batch.LastInsertId()
	return materialID, batchID
}

// seedCandidate creates one candidate (optionally already confirmed with an
// eligible version) sourced from the given material.
func seedCandidate(t *testing.T, db *sql.DB, batchID, materialID int64, title, state string) (candidateID, versionID int64) {
	t.Helper()
	now := testNow()
	candidate, err := db.Exec(`INSERT INTO knowledge_candidates(import_batch_id,source_type,source_id,state,original_suggestion_json,draft_title,created_at) VALUES(?,'source_material',?,?,'{}',?,?)`, batchID, materialID, state, title, now)
	if err != nil {
		t.Fatal(err)
	}
	candidateID, _ = candidate.LastInsertId()
	if state != "Confirmed" {
		return candidateID, 0
	}
	knowledge, err := db.Exec(`INSERT INTO reusable_knowledge(created_at) VALUES(?)`, now)
	if err != nil {
		t.Fatal(err)
	}
	knowledgeID, _ := knowledge.LastInsertId()
	if _, err := db.Exec(`UPDATE knowledge_candidates SET confirmed_knowledge_id=?,row_version=row_version+1 WHERE id=?`, knowledgeID, candidateID); err != nil {
		t.Fatal(err)
	}
	version, err := db.Exec(`INSERT INTO knowledge_versions(knowledge_id,version_seq,title,body,source_candidate_id,created_at) VALUES(?,1,?,'正文',?,?)`, knowledgeID, title, candidateID, now)
	if err != nil {
		t.Fatal(err)
	}
	versionID, _ = version.LastInsertId()
	if _, err := db.Exec(`UPDATE reusable_knowledge SET current_version_id=?,row_version=row_version+1 WHERE id=?`, versionID, knowledgeID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO knowledge_version_retrieval_state(knowledge_version_id,updated_at) VALUES(?,?)`, versionID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO knowledge_search_docs(knowledge_version_id,title,body) VALUES(?,?,'正文')`, versionID, title); err != nil {
		t.Fatal(err)
	}
	return candidateID, versionID
}

func TestApplyInvalidatesCandidatesVersionsAndProjection(t *testing.T) {
	db := newTestDB(t)
	materialA, batchA := seedSourceMaterial(t, db, "a")
	materialB, batchB := seedSourceMaterial(t, db, "b")
	pendingA, _ := seedCandidate(t, db, batchA, materialA, "待确认知识", "AwaitingConfirmation")
	_, confirmedA := seedCandidate(t, db, batchA, materialA, "已确认知识", "Confirmed")
	_, confirmedB := seedCandidate(t, db, batchB, materialB, "无关知识", "Confirmed")

	// The invalidation runs inside the caller's transaction; release the
	// single pooled connection before any pool-side reads.
	applyInvalidation := func(sourceID int64) Outcome {
		conn, connErr := db.Conn(context.Background())
		if connErr != nil {
			t.Fatal(connErr)
		}
		defer conn.Close()
		if _, err := conn.ExecContext(context.Background(), `BEGIN IMMEDIATE`); err != nil {
			t.Fatal(err)
		}
		outcome, applyErr := Apply(context.Background(), conn, "source_material", []int64{sourceID}, testNow())
		if applyErr != nil {
			t.Fatal(applyErr)
		}
		if _, err := conn.ExecContext(context.Background(), `COMMIT`); err != nil {
			t.Fatal(err)
		}
		return outcome
	}
	outcome := applyInvalidation(materialA)

	if outcome.CandidatesInvalidated != 1 || outcome.VersionsExited != 1 {
		t.Fatalf("outcome = %+v, want 1 candidate + 1 version", outcome)
	}
	var state string
	if err := db.QueryRow(`SELECT state FROM knowledge_candidates WHERE id=?`, pendingA).Scan(&state); err != nil || state != "SourceInvalid" {
		t.Fatalf("pending candidate state = %s err=%v", state, err)
	}
	var exited int64
	if err := db.QueryRow(`SELECT exited FROM knowledge_version_retrieval_state WHERE knowledge_version_id=?`, confirmedA).Scan(&exited); err != nil || exited != 1 {
		t.Fatalf("confirmed version exited = %d err=%v", exited, err)
	}
	if docs := count(t, db, `SELECT COUNT(*) FROM knowledge_search_docs WHERE knowledge_version_id=?`, confirmedA); docs != 0 {
		t.Fatal("invalidated version kept its projection row")
	}
	if docs := count(t, db, `SELECT COUNT(*) FROM knowledge_search_docs WHERE knowledge_version_id=?`, confirmedB); docs != 1 {
		t.Fatal("unrelated version lost its projection row")
	}

	// Repeated events are idempotent: the sticky row keeps its exit facts.
	applyInvalidation(materialA)
	if rowVersion := count(t, db, `SELECT row_version FROM knowledge_version_retrieval_state WHERE knowledge_version_id=?`, confirmedA); rowVersion != 2 {
		t.Fatalf("repeated invalidation touched the sticky exit row: row_version=%d", rowVersion)
	}
}

func count(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var value int64
	if err := db.QueryRow(query, args...).Scan(&value); err != nil {
		t.Fatal(err)
	}
	return int(value)
}
