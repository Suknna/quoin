package knowledge

// search.go implements the knowledge search projection over the derived
// FTS5 projection and the embedding cosine channel: browse mode lists
// eligible knowledge by the current version's creation time; query mode
// returns the two raw channels side by side. The program never merges,
// weights or thresholds them (UI-KNOWLEDGE-002, DATA-EMBED-002, the
// HTTP-PAGE-001 query exception). The composite cursor binds the query, the
// index generation and each channel's keyset boundary; the query embedding
// rides in the cursor so pagination never re-embeds and never mixes
// generations.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Suknna/quoin/internal/quoin/knowledge/embedding"
	"github.com/Suknna/quoin/internal/quoin/knowledge/retrieval"
)

// SemanticWait bounds how long one search request waits for its query
// embedding attempt before answering with the FTS channel alone.
const SemanticWait = 20 * time.Second

// RebuildSearchDocs reconciles the derived knowledge_search_docs projection
// with its single authority: a document exists exactly for every current
// version whose retrieval state has not exited. The external-content FTS
// index follows through its triggers, so one rebuild repairs both layers
// after any historical drift.

func (service *Service) RebuildSearchDocs(ctx context.Context) error {
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	if _, err := conn.ExecContext(ctx, `DELETE FROM knowledge_search_docs`); err != nil {
		return err
	}
	// Full reload from the authority (current ∧ not exited), never a
	// merge: a drifted row under the same version id is replaced, not kept.
	if _, err := conn.ExecContext(ctx, `INSERT INTO knowledge_search_docs(knowledge_version_id,title,body)
		SELECT v.id, v.title, v.body FROM reusable_knowledge k
		JOIN knowledge_versions v ON v.id=k.current_version_id
		JOIN knowledge_version_retrieval_state r ON r.knowledge_version_id=v.id AND r.exited=0
	`); err != nil {
		return err
	}
	// The external-content index is a second derived layer: reconcile the
	// content rows, then rebuild knowledge_fts itself so FTS-only drift is
	// repaired too (DATA-DERIVED-001).
	if _, err := conn.ExecContext(ctx, `INSERT INTO knowledge_fts(knowledge_fts) VALUES('rebuild')`); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return err
	}
	committed = true
	return nil
}

// SearchHit is one KnowledgeSearchHit in one channel.
type SearchHit struct {
	Knowledge  KnowledgeSummary `json:"knowledge"`
	Score      float64          `json:"score"`
	IndexState string           `json:"indexState,omitempty"`
}

// QueryResult is the KnowledgeQueryResult projection.
type QueryResult struct {
	Mode             string      `json:"mode"`
	ExactTextMatches []SearchHit `json:"exactTextMatches"`
	SemanticMatches  []SearchHit `json:"semanticMatches"`
	NextCursorValue  string      `json:"-"`
}

// QueryCursor is the composite dual-channel cursor: the FTS keyset, the
// semantic keyset (bound to its generation) and the query embedding itself.
// Carrying the vector keeps pagination deterministic (the provider call
// happens exactly once per search) and keeps one cosine pass inside one
// generation (DATA-EMBED-002).
type QueryCursor struct {
	Query string `json:"q"`
	FTS   struct {
		LastScore float64 `json:"fs"`
		LastID    int64   `json:"fi"`
		Done      bool    `json:"fd"`
	} `json:"f"`
	Semantic struct {
		GenerationID int64   `json:"g"`
		LastScore    float64 `json:"ss"`
		LastID       int64   `json:"si"`
		Done         bool    `json:"sd"`
	} `json:"s"`
	SemanticVector []float32 `json:"v,omitempty"`
}

// Search runs the dual-channel query mode. The FTS channel pages by
// (rank, id); the semantic channel pages by (cosine, version id) inside one
// generation. Either channel may be honestly empty (no provider, index
// rebuilding, or the query embedding attempt is contended) — the program
// never fabricates or thresholds results.
func (service *Service) Search(ctx context.Context, query string, after *QueryCursor, limit int) (QueryResult, *QueryCursor, error) {
	var semanticNext *retrieval.Cursor
	result, next, err := service.searchFTS(ctx, query, after, limit)
	if err != nil {
		return QueryResult{}, nil, err
	}
	if next == nil {
		next = &QueryCursor{Query: query}
		if after != nil {
			next.FTS = after.FTS
			next.Semantic = after.Semantic
			next.SemanticVector = after.SemanticVector
		}
	}
	// A cursor whose semantic channel is already exhausted never re-embeds
	// and never replays earlier semantic results: only the FTS side pages on.
	if after != nil && after.Semantic.Done {
		next.Semantic.Done = true
		// The frozen response shape requires an array even when the
		// channel is exhausted (never a JSON null).
		result.SemanticMatches = []SearchHit{}
	} else {
		var semantic []SearchHit
		var vector []float32
		var err error
		semantic, semanticNext, vector, err = service.searchSemantic(ctx, query, after, limit)
		if err != nil {
			return QueryResult{}, nil, err
		}
		result.SemanticMatches = semantic
		if semanticNext != nil {
			next.Semantic.GenerationID = semanticNext.GenerationID
			next.Semantic.LastScore = semanticNext.LastScore
			next.Semantic.LastID = semanticNext.LastID
			next.SemanticVector = vector
		} else {
			next.Semantic.Done = true
		}
	}
	// The composite cursor is emitted while either channel can continue.
	if next.FTS.Done && next.Semantic.Done {
		return result, nil, nil
	}
	return result, next, nil
}

// searchFTS is the trigram channel (bm25 rank keyset). Short queries below
// the trigram floor return an honest empty channel.
func (service *Service) searchFTS(ctx context.Context, query string, after *QueryCursor, limit int) (QueryResult, *QueryCursor, error) {
	hits := make([]SearchHit, 0, limit)
	cursor := &QueryCursor{Query: query}
	if len([]rune(query)) < 3 {
		// The trigram tokenizer cannot match shorter text: an honestly
		// empty, exhausted FTS channel.
		cursor.FTS.Done = true
		return QueryResult{Mode: "query", ExactTextMatches: hits}, cursor, nil
	}
	if after != nil {
		cursor.FTS = after.FTS
		cursor.Semantic = after.Semantic
		cursor.SemanticVector = after.SemanticVector
	}
	if cursor.FTS.Done {
		return QueryResult{Mode: "query", ExactTextMatches: hits}, cursor, nil
	}
	escaped := `"` + strings.ReplaceAll(query, `"`, `""`) + `"`
	where := ` AND f.knowledge_fts MATCH ?`
	args := []any{escaped}
	if after != nil && !after.FTS.Done {
		where += ` AND (rank > ? OR (rank = ? AND s.knowledge_version_id > ?))`
		args = append(args, after.FTS.LastScore, after.FTS.LastScore, after.FTS.LastID)
	}
	rows, err := service.db.QueryContext(ctx, `
		SELECT s.knowledge_version_id, rank, k.id, v.title, v.version_seq, k.row_version
		FROM knowledge_fts f
		JOIN knowledge_search_docs s ON s.knowledge_version_id = f.rowid
		JOIN knowledge_versions v ON v.id = s.knowledge_version_id
		JOIN reusable_knowledge k ON k.current_version_id = v.id
		WHERE 1=1`+where+`
		ORDER BY rank ASC, s.knowledge_version_id ASC LIMIT ?`, append(args, limit+1)...)
	if err != nil {
		return QueryResult{}, nil, err
	}
	defer rows.Close()
	type edge struct {
		score float64
		id    int64
	}
	edges := make([]edge, 0, limit+1)
	for rows.Next() {
		var versionID, knowledgeID, versionSeq, rowVersion int64
		var title string
		var score float64
		if err := rows.Scan(&versionID, &score, &knowledgeID, &title, &versionSeq, &rowVersion); err != nil {
			return QueryResult{}, nil, err
		}
		if len(hits) < limit {
			hits = append(hits, SearchHit{
				Knowledge: KnowledgeSummary{
					ID: fmt.Sprintf("%d", knowledgeID), Title: title, CurrentVersionID: fmt.Sprintf("%d", versionID),
					CurrentVersionSeq: versionSeq, Eligible: true, RowVersion: rowVersion,
				},
				Score: score,
			})
		}
		edges = append(edges, edge{score: score, id: versionID})
	}
	if err := rows.Err(); err != nil {
		return QueryResult{}, nil, err
	}
	if len(edges) > limit {
		// The keyset boundary is the last EMITTED row.
		boundary := edges[len(hits)-1]
		cursor.FTS.LastScore, cursor.FTS.LastID, cursor.FTS.Done = boundary.score, boundary.id, false
	} else {
		cursor.FTS.Done = true
	}
	return QueryResult{Mode: "query", ExactTextMatches: hits}, cursor, nil
}

// searchSemantic resolves the query embedding (first page embeds once;
// pagination reuses the cursor-bound vector) and pages the cosine channel
// inside that exact generation.
func (service *Service) searchSemantic(ctx context.Context, query string, after *QueryCursor, limit int) ([]SearchHit, *retrieval.Cursor, []float32, error) {
	var vector []float32
	var generation embedding.GenerationView
	if after != nil && len(after.SemanticVector) > 0 {
		vector = after.SemanticVector
		generation = embedding.GenerationView{ID: after.Semantic.GenerationID, VectorDim: int64(len(vector))}
	} else {
		resolved, view, err := service.embeddings.QueryVector(ctx, query, SemanticWait)
		if err != nil {
			// Honest empty channel: no qualified provider, index not
			// serving yet, or the generation scope is busy.
			return []SearchHit{}, nil, nil, nil
		}
		vector, generation = resolved, view
	}
	indexState, err := service.semanticIndexState(ctx, generation)
	if err != nil {
		return []SearchHit{}, nil, nil, nil
	}
	var cursor *retrieval.Cursor
	if after != nil && !after.Semantic.Done && len(after.SemanticVector) > 0 {
		cursor = &retrieval.Cursor{GenerationID: after.Semantic.GenerationID, LastScore: after.Semantic.LastScore, LastID: after.Semantic.LastID}
	}
	hits, next, err := service.semantic.Semantic(ctx, generation.ID, vector, cursor, limit)
	if err != nil {
		return []SearchHit{}, nil, nil, nil
	}
	projected := make([]SearchHit, 0, len(hits))
	for _, hit := range hits {
		projected = append(projected, SearchHit{
			Knowledge: KnowledgeSummary{
				ID: fmt.Sprintf("%d", hit.KnowledgeID), Title: hit.Title, CurrentVersionID: fmt.Sprintf("%d", hit.VersionID),
				CurrentVersionSeq: hit.VersionSeq, Eligible: true, RowVersion: hit.RowVersion,
			},
			Score:      hit.Score,
			IndexState: indexState,
		})
	}
	if next != nil {
		next.GenerationID = generation.ID
	}
	return projected, next, vector, nil
}

// semanticIndexState labels the channel's index relative to the current
// EmbeddingGeneration: ready (serving, nothing building), rebuilding (a
// replacement generation is building) or stale (the cursor-bound generation
// is no longer the serving one — the page still answers from its bound
// generation and never mixes, DATA-EMBED-002).
func (service *Service) semanticIndexState(ctx context.Context, bound embedding.GenerationView) (string, error) {
	building, err := service.embeddings.BuildingExists(ctx)
	if err != nil {
		return "", err
	}
	current, hasCurrent, err := service.embeddings.CurrentGeneration(ctx)
	if err != nil {
		return "", err
	}
	switch {
	case hasCurrent && current.ID != bound.ID:
		return "stale", nil
	case building:
		return "rebuilding", nil
	default:
		return "ready", nil
	}
}

// SearchMode distinguishes the two frozen response shapes.
type SearchMode string

const (
	// SearchModeBrowse lists eligible knowledge without a query.
	SearchModeBrowse SearchMode = "browse"
	// SearchModeQuery runs the dual-channel retrieval.
	SearchModeQuery SearchMode = "query"
)
