// Package retrieval computes the semantic search channel (T29): cosine
// similarity between the request's query embedding and the ready vectors of
// exactly ONE embedding generation (DATA-EMBED-002: a single cosine pass
// never mixes generations). The channel is a raw ranking — the program sets
// no threshold and synthesizes no combined score (UI-KNOWLEDGE-002).
package retrieval

import (
	"context"
	"database/sql"
	"math"

	"github.com/Suknna/quoin/internal/quoin/knowledge/embedding"
)

// Hit is one semantic channel result: the raw cosine score plus the
// Knowledge locator fields the dual-channel projection needs.
type Hit struct {
	KnowledgeID int64
	VersionID   int64
	Title       string
	VersionSeq  int64
	RowVersion  int64
	Score       float64
}

// Cursor is the semantic channel keyset boundary (score DESC, version id
// ASC); the composite search cursor binds it with the FTS channel state.
type Cursor struct {
	GenerationID int64   `json:"g"`
	LastScore    float64 `json:"s"`
	LastID       int64   `json:"i"`
}

// Service computes the semantic channel over the shared database.
type Service struct {
	db *sql.DB
}

// NewService builds the semantic channel reader.
func NewService(db *sql.DB) *Service {
	return &Service{db: db}
}

// Semantic returns one page of raw cosine-ordered hits for the query vector
// against the given generation's ready vectors. Only eligible versions
// (current ∧ 未停用 ∧ 来源有效 ∧ 未 exit — expressed by their
// knowledge_search_docs projection row) participate; a hit whose version
// lost eligibility between pages is skipped, never resurrected.
func (service *Service) Semantic(ctx context.Context, generationID int64, queryVector []float32, after *Cursor, limit int) ([]Hit, *Cursor, error) {
	if len(queryVector) == 0 || limit < 1 {
		return []Hit{}, nil, nil
	}
	rows, err := service.db.QueryContext(ctx, `
		SELECT k.id, v.id, v.title, v.version_seq, k.row_version, e.vector
		FROM embeddings e
		JOIN knowledge_versions v ON v.id=e.knowledge_version_id
		JOIN reusable_knowledge k ON k.current_version_id=v.id
		JOIN knowledge_version_retrieval_state s ON s.knowledge_version_id=v.id AND s.exited=0
		WHERE e.embedding_generation_id=? AND e.state='ready'
		  AND EXISTS (SELECT 1 FROM knowledge_search_docs d WHERE d.knowledge_version_id=v.id)
		ORDER BY v.id ASC`, generationID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	hits := make([]Hit, 0, limit+1)
	for rows.Next() {
		var hit Hit
		var blob []byte
		if err := rows.Scan(&hit.KnowledgeID, &hit.VersionID, &hit.Title, &hit.VersionSeq, &hit.RowVersion, &blob); err != nil {
			return nil, nil, err
		}
		vector := embedding.BytesToFloats(blob)
		if len(vector) != len(queryVector) {
			// A dimension drift mid-read can only come from a torn state;
			// skip rather than rank against an incomparable vector.
			continue
		}
		hit.Score = cosine(queryVector, vector)
		hits = append(hits, hit)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	// Raw cosine order: score DESC, then the stable version locator ASC.
	sortHits(hits)
	start := 0
	if after != nil {
		// Resume strictly after the keyset boundary (equal scores resume by
		// the version id half of the key).
		for start = 0; start < len(hits); start++ {
			hit := hits[start]
			if hit.Score < after.LastScore || (hit.Score == after.LastScore && hit.VersionID > after.LastID) {
				break
			}
		}
	}
	if start > len(hits) {
		start = len(hits)
	}
	page := hits[start:]
	if len(page) > limit {
		page = page[:limit]
		boundary := page[len(page)-1]
		return page, &Cursor{GenerationID: generationID, LastScore: boundary.Score, LastID: boundary.VersionID}, nil
	}
	return page, nil, nil
}

func sortHits(hits []Hit) {
	for outer := 1; outer < len(hits); outer++ {
		hit := hits[outer]
		inner := outer - 1
		for inner >= 0 && less(hit, hits[inner]) {
			hits[inner+1] = hits[inner]
			inner--
		}
		hits[inner+1] = hit
	}
}

func less(a, b Hit) bool {
	if a.Score != b.Score {
		return a.Score > b.Score
	}
	return a.VersionID < b.VersionID
}

func cosine(a, b []float32) float64 {
	dot, na, nb := 0.0, 0.0, 0.0
	for index := range a {
		dot += float64(a[index]) * float64(b[index])
		na += float64(a[index]) * float64(a[index])
		nb += float64(b[index]) * float64(b[index])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
