package retrieval

// Deterministic semantic-channel tests over the real frozen schema: raw
// cosine ordering, keyset pagination, single-generation cosines and the
// eligibility filter (DATA-EMBED-002, DATA-KNOWLEDGE-002/003).

import (
	"context"
	"database/sql"
	"math"
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

// seedVersion creates one eligible knowledge version and returns its ids.
func seedVersion(t *testing.T, db *sql.DB, seq int64, title string) (knowledgeID, versionID int64) {
	t.Helper()
	now := testNow()
	knowledge, err := db.Exec(`INSERT INTO reusable_knowledge(created_by,created_at) VALUES(NULL,?)`, now)
	if err != nil {
		t.Fatal(err)
	}
	knowledgeID, _ = knowledge.LastInsertId()
	materialID := seedImportedSource(t, db, seq)
	candidate, err := db.Exec(`INSERT INTO knowledge_candidates(import_batch_id,source_type,source_id,state,original_suggestion_json,draft_title,created_at) VALUES((SELECT id FROM knowledge_import_batches WHERE source_material_id=?),'source_material',?,'Confirmed','{}',?,?)`, materialID, materialID, title, now)
	if err != nil {
		t.Fatal(err)
	}
	candidateID, _ := candidate.LastInsertId()
	if _, err := db.Exec(`UPDATE knowledge_candidates SET confirmed_knowledge_id=?,row_version=row_version+1 WHERE id=?`, knowledgeID, candidateID); err != nil {
		t.Fatal(err)
	}
	version, err := db.Exec(`INSERT INTO knowledge_versions(knowledge_id,version_seq,title,body,source_candidate_id,created_at) VALUES(?,1,?,?,?,?)`, knowledgeID, title, title+" 正文", candidateID, now)
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
	if _, err := db.Exec(`INSERT INTO knowledge_search_docs(knowledge_version_id,title,body) VALUES(?,?,?)`, versionID, title, title+" 正文"); err != nil {
		t.Fatal(err)
	}
	return knowledgeID, versionID
}

// seedImportedSource inserts one real imported Source Material with its
// Processing batch — the lightest frozen source shape for candidate closure.
func seedImportedSource(t *testing.T, db *sql.DB, seq int64) int64 {
	t.Helper()
	now := testNow()
	material, err := db.Exec(`INSERT INTO source_materials(kind,digest,size_bytes,content,created_at) VALUES('knowledge_import',?,?,?,?)`, strings.Repeat("7", 64), 32, "导入原文 "+string(rune('a'+seq)), now)
	if err != nil {
		t.Fatal(err)
	}
	materialID, _ := material.LastInsertId()
	if _, err := db.Exec(`INSERT INTO knowledge_import_batches(source_material_id,state,generation,created_at) VALUES(?,'Processing',1,?)`, materialID, now); err != nil {
		t.Fatal(err)
	}
	return materialID
}

func seedGeneration(t *testing.T, db *sql.DB, generation int64, state string) int64 {
	t.Helper()
	insert, err := db.Exec(`INSERT INTO embedding_generations(model_name,model_version,generation,state,vector_dim,built_at,validated_at,created_at) VALUES('fixture-embed-1','t',?,'building',2,?,?,?)`, generation, testNow(), testNow(), testNow())
	if err != nil {
		t.Fatal(err)
	}
	id, _ := insert.LastInsertId()
	if state != "building" {
		if _, err := db.Exec(`UPDATE embedding_generations SET state=? WHERE id=?`, state, id); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func seedEmbedding(t *testing.T, db *sql.DB, versionID, generationID int64, state string, vector []float32) {
	t.Helper()
	var blob any
	if state == "ready" {
		encoded := make([]byte, len(vector)*4)
		for index, value := range vector {
			bits := math.Float32bits(value)
			encoded[index*4] = byte(bits)
			encoded[index*4+1] = byte(bits >> 8)
			encoded[index*4+2] = byte(bits >> 16)
			encoded[index*4+3] = byte(bits >> 24)
		}
		blob = encoded
	}
	if _, err := db.Exec(`INSERT INTO embeddings(knowledge_version_id,embedding_generation_id,state,vector,updated_at) VALUES(?,?,?,?,'`+testNow()+`')`, versionID, generationID, state, blob); err != nil {
		t.Fatal(err)
	}
}

func TestSemanticOrdersByRawCosineAndPagesWithoutMixing(t *testing.T) {
	db := newTestDB(t)
	service := NewService(db)
	generation := seedGeneration(t, db, 1, "current")
	_, versionA := seedVersion(t, db, 1, "连接池")
	_, versionB := seedVersion(t, db, 2, "磁盘压力")
	_, versionC := seedVersion(t, db, 3, "网络超时")
	// Query vector [1,0]: A is closest, B orthogonal, C opposite.
	seedEmbedding(t, db, versionA, generation, "ready", []float32{0.9, 0.1})
	seedEmbedding(t, db, versionB, generation, "ready", []float32{0, 1})
	seedEmbedding(t, db, versionC, generation, "ready", []float32{-1, 0})

	ctx := context.Background()
	hits, next, err := service.Semantic(ctx, generation, []float32{1, 0}, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].VersionID != versionA || hits[0].Score <= 0.99 {
		t.Fatalf("first page = %+v, want version A with cosine ~1", hits)
	}
	if next == nil {
		t.Fatal("one-page limit must emit a next cursor")
	}
	seen := []int64{hits[0].VersionID}
	for next != nil {
		var page []Hit
		page, next, err = service.Semantic(ctx, generation, []float32{1, 0}, next, 2)
		if err != nil {
			t.Fatal(err)
		}
		for _, hit := range page {
			seen = append(seen, hit.VersionID)
		}
	}
	if len(seen) != 3 || seen[0] != versionA || seen[1] != versionB || seen[2] != versionC {
		t.Fatalf("pagination order = %v, want A (cos~1), B (cos 0), C (cos -1)", seen)
	}
	for index := 1; index < len(seen); index++ {
		if seen[index] == seen[index-1] {
			t.Fatal("pagination emitted a duplicate")
		}
	}
}

func TestCosineNeverMixesGenerations(t *testing.T) {
	db := newTestDB(t)
	service := NewService(db)
	oldGeneration := seedGeneration(t, db, 1, "retired")
	currentGeneration := seedGeneration(t, db, 2, "current")
	_, oldVersion := seedVersion(t, db, 1, "旧模型版本")
	_, newVersion := seedVersion(t, db, 2, "新模型版本")
	seedEmbedding(t, db, oldVersion, oldGeneration, "ready", []float32{1, 0})
	seedEmbedding(t, db, newVersion, currentGeneration, "ready", []float32{0.5, 0.5})

	hits, _, err := service.Semantic(context.Background(), currentGeneration, []float32{1, 0}, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].VersionID != newVersion {
		t.Fatalf("current-generation cosine returned %+v, want only the new version", hits)
	}
}

func TestExitedVersionNeverParticipates(t *testing.T) {
	db := newTestDB(t)
	service := NewService(db)
	generation := seedGeneration(t, db, 1, "current")
	_, versionA := seedVersion(t, db, 1, "连接池")
	_, versionB := seedVersion(t, db, 2, "磁盘压力")
	seedEmbedding(t, db, versionA, generation, "ready", []float32{1, 0})
	seedEmbedding(t, db, versionB, generation, "ready", []float32{1, 0})
	// versionB's source is withdrawn between pages: the exit and projection
	// deletion happen in one transaction.
	if _, err := db.Exec(`UPDATE knowledge_version_retrieval_state SET exited=1,exited_at=?,exit_reason='source_rejected',updated_at=?,row_version=row_version+1 WHERE knowledge_version_id=? AND exited=0`, testNow(), testNow(), versionB); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM knowledge_search_docs WHERE knowledge_version_id=?`, versionB); err != nil {
		t.Fatal(err)
	}
	hits, _, err := service.Semantic(context.Background(), generation, []float32{1, 0}, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].VersionID != versionA {
		t.Fatalf("exited version leaked into the semantic channel: %+v", hits)
	}
}
