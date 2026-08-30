package knowledge

// T29 search-level dual-channel tests: the composite query mode merges the
// FTS and semantic channels under one cursor, embeds the query exactly once
// per search and pages without re-embedding (the cursor carries the query
// vector and the bound generation).

import (
	"context"
	"encoding/json"
	"strconv"
	"sync"
	"testing"
	"time"
)

// rebuildItem mirrors one frozen rebuild input entry for test driving.
type rebuildItem struct {
	VersionID int64  `json:"versionId"`
	Text      string `json:"text"`
	Digest    string `json:"digest"`
}

type rebuildInput struct {
	SchemaKind   string        `json:"schemaKind"`
	AttemptID    int64         `json:"attemptId"`
	GenerationID int64         `json:"generationId"`
	Mode         string        `json:"mode"`
	Inputs       []rebuildItem `json:"inputs"`
}

// driveEmbedding walks the active rebuild attempt to Running and commits
// deterministic vectors through the real adjudication path.
func driveEmbedding(t *testing.T, f *fixture, vectorFor func(item rebuildItem) []float32) {
	t.Helper()
	ctx := context.Background()
	var attemptID int64
	if err := f.db.QueryRow(`SELECT id FROM execution_attempts WHERE attempt_type='embedding' AND state='Queued' LIMIT 1`).Scan(&attemptID); err != nil {
		t.Fatalf("no queued embedding attempt: %v", err)
	}
	walkToRunning(t, f, attemptID)
	raw, err := f.service.Embeddings().RebuildInput(ctx, attemptID)
	if err != nil {
		t.Fatal(err)
	}
	var input rebuildInput
	if err := json.Unmarshal(raw, &input); err != nil {
		t.Fatal(err)
	}
	vectors := make([]map[string]any, 0, len(input.Inputs))
	for _, item := range input.Inputs {
		vectors = append(vectors, map[string]any{"versionId": item.VersionID, "digest": item.Digest, "values": vectorFor(item)})
	}
	payload, err := json.Marshal(map[string]any{
		"schemaKind": "embedding_generation_result_v1", "attemptId": input.AttemptID,
		"generationId": input.GenerationID, "modelId": "fixture-embed-1", "vectors": vectors,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.Embeddings().CommitResult(ctx, attemptID, "boot", 1, payload); err != nil {
		t.Fatalf("commit rebuild: %v", err)
	}
}

func walkToRunning(t *testing.T, f *fixture, attemptID int64) {
	t.Helper()
	future := time.Now().UTC().Add(5 * time.Minute).Format(time.RFC3339Nano)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := f.db.Exec(`UPDATE execution_attempts SET state='Assigned',runtime_slot='plinth',boot_id='boot',connection_epoch=1,lease_until=?,runtime_release_version='test',row_version=row_version+1 WHERE id=? AND state='Queued'`, future, attemptID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`UPDATE execution_attempts SET state='Running',accepted_at=?,started_at=?,row_version=row_version+1 WHERE id=? AND state='Assigned'`, now, now, attemptID); err != nil {
		t.Fatal(err)
	}
}

// wireQueryDispatcher answers query embedding attempts synchronously with a
// fixed vector through the same real adjudication path.
func wireQueryDispatcher(t *testing.T, f *fixture, vector []float32) func(context.Context, int64) error {
	return func(ctx context.Context, attemptID int64) error {
		var generationID int64
		if err := f.db.QueryRowContext(ctx, `SELECT scope_id FROM execution_attempts WHERE id=?`, attemptID).Scan(&generationID); err != nil {
			return err
		}
		raw, err := f.service.Embeddings().RebuildInput(ctx, attemptID)
		if err != nil {
			return err
		}
		var input struct {
			Query *struct {
				Text   string `json:"text"`
				Digest string `json:"digest"`
			} `json:"query"`
		}
		if err := json.Unmarshal(raw, &input); err != nil || input.Query == nil {
			return err
		}
		walkToRunning(t, f, attemptID)
		payload, err := json.Marshal(map[string]any{
			"schemaKind": "embedding_query_result_v1", "attemptId": attemptID,
			"generationId": generationID, "modelId": "fixture-embed-1",
			"digest": input.Query.Digest, "values": vector,
		})
		if err != nil {
			return err
		}
		return f.service.Embeddings().CommitResult(ctx, attemptID, "boot", 1, payload)
	}
}

// confirmFromSource confirms one knowledge from the fixture's analysis
// output or investigation assistant message and returns the knowledge id.
func confirmFromSource(t *testing.T, f *fixture, suffix string, fromMessage bool) string {
	t.Helper()
	ctx := context.Background()
	var candidateID int64
	var confirmed CandidateSummary
	if fromMessage {
		created, err := f.service.CreateFromInvestigationMessage(ctx, f.userID, "t29-msg-"+suffix, f.investigationID, f.assistantID)
		if err != nil {
			t.Fatal(err)
		}
		candidateID, _ = strconv.ParseInt(created.Candidate.ID, 10, 64)
		confirmed, err = f.service.Confirm(ctx, f.userID, "t29-confirm-"+suffix, candidateID, 0)
		if err != nil {
			t.Fatal(err)
		}
		return confirmed.ConfirmedKnowledgeID
	}
	created, err := f.service.CreateFromAnalysisOutput(ctx, f.userID, "t29-create-"+suffix, f.outputID, f.analysisID)
	if err != nil {
		t.Fatal(err)
	}
	candidateID, _ = strconv.ParseInt(created.Candidate.ID, 10, 64)
	confirmed, err = f.service.Confirm(ctx, f.userID, "t29-confirm-"+suffix, candidateID, 0)
	if err != nil {
		t.Fatal(err)
	}
	return confirmed.ConfirmedKnowledgeID
}

func TestSearchMergesDualChannelsUnderOneCursor(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	// Two confirmed knowledges: only the first shares the FTS query's
	// trigrams; both carry the unit semantic vector, so the semantic channel
	// spans two pages at limit 1 while FTS exhausts on page one.
	first := confirmFromSource(t, f, "dual-a", false)
	second := confirmFromSource(t, f, "dual-b", true)

	// The index builds through the real sweep + commit path.
	if err := f.service.Embeddings().Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	driveEmbedding(t, f, func(item rebuildItem) []float32 {
		vector := make([]float32, 16)
		vector[0] = 1
		return vector
	})

	var mu sync.Mutex
	dispatches := 0
	base := wireQueryDispatcher(t, f, unitQuery())
	f.service.Embeddings().SetDispatcher(func(ctx context.Context, attemptID int64) error {
		mu.Lock()
		dispatches++
		mu.Unlock()
		return base(ctx, attemptID)
	})

	result, next, err := f.service.Search(ctx, "耗尽导致", nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.ExactTextMatches) != 1 || result.ExactTextMatches[0].Knowledge.ID != first {
		t.Fatalf("FTS channel = %+v, want the connection-pool knowledge only", result.ExactTextMatches)
	}
	if len(result.SemanticMatches) != 1 || result.SemanticMatches[0].Knowledge.ID != first {
		t.Fatalf("semantic channel page one = %+v, want the top cosine hit", result.SemanticMatches)
	}
	if result.SemanticMatches[0].IndexState != "ready" {
		t.Fatalf("semantic index state = %q, want ready", result.SemanticMatches[0].IndexState)
	}
	// The same knowledge may legitimately hit both channels; the two raw
	// scores stay separate and comparable only within their channel.
	if next == nil || len(next.SemanticVector) != 16 {
		t.Fatal("cursor does not carry the query embedding")
	}
	if next.Semantic.GenerationID == 0 {
		t.Fatal("cursor does not bind the semantic generation")
	}
	// Pagination continues without re-embedding the query.
	before := dispatches
	page2, next2, err := f.service.Search(ctx, "耗尽导致", next, 1)
	if err != nil {
		t.Fatal(err)
	}
	if dispatches != before {
		t.Fatalf("pagination re-embedded the query (%d -> %d)", before, dispatches)
	}
	if len(page2.SemanticMatches) != 1 || page2.SemanticMatches[0].Knowledge.ID != second {
		t.Fatalf("semantic channel page two = %+v, want the remaining knowledge", page2.SemanticMatches)
	}
	if len(page2.ExactTextMatches) != 0 {
		t.Fatalf("FTS channel fabricated page-two hits: %+v", page2.ExactTextMatches)
	}
	if next2 != nil {
		t.Fatal("both channels exhausted must omit the next cursor")
	}
}

// Without a serving generation the query mode still answers the FTS channel
// and reports an honestly empty semantic channel.
func TestSearchWithoutIndexServesOnlyFTS(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	confirmFromSource(t, f, "ftsonly", false)
	f.service.Embeddings().SetDispatcher(func(context.Context, int64) error { return errNoPlinthForTest })
	result, next, err := f.service.Search(ctx, "连接池", nil, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.ExactTextMatches) != 1 {
		t.Fatalf("FTS channel hits = %d, want 1", len(result.ExactTextMatches))
	}
	if len(result.SemanticMatches) != 0 {
		t.Fatalf("semantic channel hits = %d, want 0", len(result.SemanticMatches))
	}
	if next != nil {
		t.Fatal("cursor emitted with the semantic channel absent and FTS exhausted")
	}
}

var errNoPlinthForTest = &noPlinthError{}

type noPlinthError struct{}

func (*noPlinthError) Error() string { return "plinth not connected (test)" }

func unitQuery() []float32 {
	vector := make([]float32, 16)
	vector[0] = 1
	return vector
}

// When the semantic channel exhausts before the FTS channel, later pages
// must not re-embed the query and must not replay semantic results.
func TestPaginationSkipsExhaustedSemanticChannel(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	poolID := confirmFromSource(t, f, "page-a", false)
	diskID := confirmFromSource(t, f, "page-b", true)
	if err := f.service.Embeddings().Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	driveEmbedding(t, f, func(item rebuildItem) []float32 {
		vector := make([]float32, 16)
		vector[0] = 1
		return vector
	})
	// Force the semantic channel to a single hit while FTS still matches
	// both docs: the disk version's embedding row fails.
	if _, err := f.db.Exec(`UPDATE embeddings SET state='failed',vector=NULL,updated_at=? WHERE knowledge_version_id=(SELECT current_version_id FROM reusable_knowledge WHERE id=?)`, time.Now().UTC().Format(time.RFC3339Nano), parseID(t, diskID)); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	dispatches := 0
	base := wireQueryDispatcher(t, f, unitQuery())
	f.service.Embeddings().SetDispatcher(func(ctx context.Context, attemptID int64) error {
		mu.Lock()
		dispatches++
		mu.Unlock()
		return base(ctx, attemptID)
	})

	_, next, err := f.service.Search(ctx, "连接池", nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	// The single ready vector fills the semantic page exactly, so page
	// one already reports that channel exhausted while FTS continues.
	if next == nil || !next.Semantic.Done || next.FTS.Done {
		t.Fatalf("page one must carry an FTS continuation with the semantic channel exhausted: %+v", next)
	}
	if len(next.SemanticVector) != 0 {
		t.Fatal("exhausted semantic channel must not carry a vector onward")
	}
	// Page two continues FTS-only: no re-embed, no semantic replay.
	mu.Lock()
	embedCount := dispatches
	mu.Unlock()
	page2, next2, err := f.service.Search(ctx, "连接池", next, 1)
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	afterCount := dispatches
	mu.Unlock()
	if afterCount != embedCount {
		t.Fatalf("exhausted semantic channel re-embedded the query (%d -> %d)", embedCount, afterCount)
	}
	if len(page2.SemanticMatches) != 0 {
		t.Fatalf("semantic results replayed after exhaustion: %+v", page2.SemanticMatches)
	}
	if next2 != nil {
		t.Fatal("FTS channel exhausted but cursor continues")
	}
	_ = poolID
	_ = diskID
}
