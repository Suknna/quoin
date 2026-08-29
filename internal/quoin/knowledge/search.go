package knowledge

// search.go implements the knowledge search projection over the derived
// FTS5 projection: browse mode lists eligible knowledge by the current
// version's creation time; query mode returns the raw FTS5 trigram
// channel (bm25 rank) with the semantic channel empty until the
// embedding subsystem lands (T29). The program never merges or weights
// the two channels (UI-KNOWLEDGE-002, HTTP-PAGE-001 query exception).

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

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

// QueryCursor is the composite query cursor: the FTS keyset plus the
// (currently terminal) semantic channel state.
type QueryCursor struct {
	Query        string  `json:"q"`
	FTSLastScore float64 `json:"ftsLastScore"`
	FTSLastID    int64   `json:"ftsLastId"`
	FTSDone      bool    `json:"ftsDone"`
}

// Search runs the query mode: the FTS channel pages by (rank, id); the
// semantic channel is honestly empty until embeddings exist.
func (service *Service) Search(ctx context.Context, query string, after *QueryCursor, limit int) (QueryResult, *QueryCursor, error) {
	// The trigram tokenizer needs at least three characters; shorter
	// queries cannot match and return an honest empty page.
	hits := make([]SearchHit, 0, limit)
	if len([]rune(query)) < 3 {
		return QueryResult{Mode: "query", ExactTextMatches: hits, SemanticMatches: []SearchHit{}}, nil, nil
	}
	escaped := `"` + strings.ReplaceAll(query, `"`, `""`) + `"`
	where := ` AND f.knowledge_fts MATCH ?`
	args := []any{escaped}
	if after != nil && !after.FTSDone {
		where += ` AND (rank > ? OR (rank = ? AND s.knowledge_version_id > ?))`
		args = append(args, after.FTSLastScore, after.FTSLastScore, after.FTSLastID)
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
		hits = append(hits, SearchHit{
			Knowledge: KnowledgeSummary{
				ID: fmt.Sprintf("%d", knowledgeID), Title: title, CurrentVersionID: fmt.Sprintf("%d", versionID),
				CurrentVersionSeq: versionSeq, Eligible: true, RowVersion: rowVersion,
			},
			Score: score,
		})
		edges = append(edges, edge{score: score, id: versionID})
	}
	if err := rows.Err(); err != nil {
		return QueryResult{}, nil, err
	}
	var next *QueryCursor
	if len(hits) > limit {
		hits = hits[:limit]
		// The keyset boundary is the last EMITTED row.
		boundary := edges[len(hits)-1]
		next = &QueryCursor{Query: query, FTSLastScore: boundary.score, FTSLastID: boundary.id}
	}
	return QueryResult{Mode: "query", ExactTextMatches: hits, SemanticMatches: []SearchHit{}}, next, nil
}

// SearchMode distinguishes the two frozen response shapes.
type SearchMode string

const (
	// SearchModeBrowse lists eligible knowledge without a query.
	SearchModeBrowse SearchMode = "browse"
	// SearchModeQuery runs the dual-channel retrieval.
	SearchModeQuery SearchMode = "query"
)

var _ = sql.ErrNoRows
