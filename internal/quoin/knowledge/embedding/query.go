package embedding

// query.go: the query-embedding adjudication half of the result path. A
// query attempt serves one HTTP search request; its vector is
// request-scoped (never persisted) and is delivered to the live waiter
// through the in-process registry.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/Suknna/quoin/internal/quoin/attempt"
)

// commitQueryResult seals a query attempt and delivers the vector to its
// live waiter (the vector itself is request-scoped, never persisted).
func (service *Service) commitQueryResult(ctx context.Context, attemptID int64, bootID string, epoch uint64, raw []byte) error {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	var proposal QueryResultPayload
	if err := decoder.Decode(&proposal); err != nil {
		return fmt.Errorf("%w: not valid closed JSON: %v", ErrInvalidResult, err)
	}
	if _, err := decoder.Token(); err != io.EOF {
		return fmt.Errorf("%w: trailing data after the result object", ErrInvalidResult)
	}
	if proposal.SchemaKind != QuerySchemaKind || proposal.AttemptID != attemptID || len(proposal.Values) == 0 {
		return fmt.Errorf("%w: invalid identity envelope", ErrInvalidResult)
	}
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
	var attemptState string
	var generationID, vectorDim int64
	var leaseUntil sql.NullString
	err = conn.QueryRowContext(ctx, `
		SELECT a.state, a.scope_id, a.lease_until, COALESCE(g.vector_dim,0) FROM execution_attempts a
		JOIN embedding_generations g ON g.id=a.scope_id
		WHERE a.id=? AND a.attempt_type='embedding' AND a.boot_id=? AND a.connection_epoch=?`,
		attemptID, bootID, epoch).Scan(&attemptState, &generationID, &leaseUntil, &vectorDim)
	if errors.Is(err, sql.ErrNoRows) {
		return attempt.ErrLateResult
	}
	if err != nil {
		return err
	}
	if attemptState == "Succeeded" {
		// A retried delivery replays the identical physical envelope; its
		// digest must still match the frozen input (a divergent envelope is
		// a late result, never a silent second answer).
		var frozenDigest string
		if err := conn.QueryRowContext(ctx, `
			SELECT i.source_digest FROM attempt_input_items i
			JOIN attempt_input_snapshots s ON s.id=i.snapshot_id
			WHERE s.attempt_id=? AND i.item_role='user'`, attemptID).Scan(&frozenDigest); err != nil {
			return err
		}
		if frozenDigest != proposal.Digest {
			return attempt.ErrLateResult
		}
		if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
			return err
		}
		committed = true
		service.deliverQueryOutcome(attemptID, queryOutcome{vector: proposal.Values})
		return nil
	}
	if attemptState != "Running" || !leaseUntil.Valid || !validLease(leaseUntil.String, service.nowText()) {
		return attempt.ErrLateResult
	}
	if generationID != proposal.GenerationID || int64(len(proposal.Values)) != vectorDim {
		return fmt.Errorf("%w: query result does not match the frozen generation contract", ErrInvalidResult)
	}
	var queryDigest string
	if err := conn.QueryRowContext(ctx, `
		SELECT i.source_digest FROM attempt_input_items i
		JOIN attempt_input_snapshots s ON s.id=i.snapshot_id
		WHERE s.attempt_id=? AND i.item_role='user'`, attemptID).Scan(&queryDigest); err != nil {
		return err
	}
	if queryDigest != proposal.Digest {
		return fmt.Errorf("%w: query digest does not match the frozen input", ErrInvalidResult)
	}
	if _, err := conn.ExecContext(ctx, `
		UPDATE execution_attempts SET state='Succeeded', ended_at=?, row_version=row_version+1
		WHERE id=? AND state='Running' AND boot_id=? AND connection_epoch=?`, service.nowText(), attemptID, bootID, epoch); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return err
	}
	committed = true
	service.deliverQueryOutcome(attemptID, queryOutcome{vector: proposal.Values})
	return nil
}
