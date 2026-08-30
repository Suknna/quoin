// result.go: embedding attempt result adjudication (rebuild batches and
// query embeds), failure sealing, and the in-process query registry that
// lets one HTTP search request wait for its own query-embedding attempt.
package embedding

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"time"

	"github.com/Suknna/quoin/internal/quoin/attempt"
)

// ErrInvalidResult reports a typed proposal that violates the closed
// embedding result contract (mapped to a failed attempt, never a retry).
var ErrInvalidResult = errors.New("invalid embedding result")

// ResultVector is one proposed vector of a rebuild batch.
type ResultVector struct {
	VersionID int64     `json:"versionId"`
	Digest    string    `json:"digest"`
	Values    []float32 `json:"values"`
}

// GenerationResult is the embedding_generation_result_v1 payload.
type GenerationResult struct {
	SchemaKind   string         `json:"schemaKind"`
	AttemptID    int64          `json:"attemptId"`
	GenerationID int64          `json:"generationId"`
	ModelID      string         `json:"modelId"`
	Vectors      []ResultVector `json:"vectors"`
}

// QueryResultPayload is the embedding_query_result_v1 payload.
type QueryResultPayload struct {
	SchemaKind   string    `json:"schemaKind"`
	AttemptID    int64     `json:"attemptId"`
	GenerationID int64     `json:"generationId"`
	ModelID      string    `json:"modelId"`
	Digest       string    `json:"digest"`
	Values       []float32 `json:"values"`
}

// CommitResult adjudicates one embedding ResultProposal payload: identity,
// binding, digests, dimension and version state are re-verified before any
// projection write; late or divergent results are dropped with audit only
// (DATA-TX-012, DATA-EMBED-001).
func (service *Service) CommitResult(ctx context.Context, attemptID int64, bootID string, epoch uint64, raw []byte) error {
	kind, mode, err := payloadKind(raw)
	if err != nil {
		return err
	}
	if mode == "query" {
		return service.commitQueryResult(ctx, attemptID, bootID, epoch, raw)
	}
	_ = kind
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	var proposal GenerationResult
	if err := decoder.Decode(&proposal); err != nil {
		return fmt.Errorf("%w: not valid closed JSON: %v", ErrInvalidResult, err)
	}
	if _, err := decoder.Token(); err != io.EOF {
		return fmt.Errorf("%w: trailing data after the result object", ErrInvalidResult)
	}
	if proposal.SchemaKind != ResultSchemaKind || proposal.AttemptID != attemptID || len(proposal.Vectors) == 0 {
		return fmt.Errorf("%w: invalid identity envelope", ErrInvalidResult)
	}
	for _, vector := range proposal.Vectors {
		if vector.VersionID < 1 || len(vector.Digest) != 64 || len(vector.Values) == 0 {
			return fmt.Errorf("%w: invalid vector entry", ErrInvalidResult)
		}
		for _, value := range vector.Values {
			if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
				return fmt.Errorf("%w: non-finite vector component", ErrInvalidResult)
			}
		}
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
	var attemptState, generationState string
	var generationID int64
	var leaseUntil sql.NullString
	err = conn.QueryRowContext(ctx, `
		SELECT a.state, a.scope_id, a.lease_until, g.state FROM execution_attempts a
		JOIN embedding_generations g ON g.id=a.scope_id
		WHERE a.id=? AND a.attempt_type='embedding' AND a.boot_id=? AND a.connection_epoch=?`,
		attemptID, bootID, epoch).Scan(&attemptState, &generationID, &leaseUntil, &generationState)
	if errors.Is(err, sql.ErrNoRows) {
		return attempt.ErrLateResult
	}
	if err != nil {
		return err
	}
	if generationID != proposal.GenerationID {
		return attempt.ErrLateResult
	}
	if generationState != "building" && generationState != "current" {
		return attempt.ErrLateResult
	}
	// The complete envelope is validated before any state branch: identity,
	// the full frozen lineage, digests, dimensions and per-version
	// eligibility hold for a first commit and for a sealed replay alike.
	lineage, err := service.frozenLineage(ctx, conn, attemptID)
	if err != nil {
		return err
	}
	if len(lineage) != len(proposal.Vectors) {
		return fmt.Errorf("%w: vector count %d does not match frozen input %d", ErrInvalidResult, len(proposal.Vectors), len(lineage))
	}
	// DATA-TX-012 also re-checks version state: if any lineage version lost
	// retrieval eligibility (source withdrawn, stopped reuse, superseded)
	// while the batch was in flight, this result is late for the whole
	// batch — the schema's Succeeded closure demands every lineage version
	// ready, so partial writes are not an option.
	for versionID := range lineage {
		var eligible int
		if err := conn.QueryRowContext(ctx, `
			SELECT 1 FROM knowledge_search_docs d
			JOIN reusable_knowledge k ON k.current_version_id=d.knowledge_version_id
			WHERE d.knowledge_version_id=?`, versionID).Scan(&eligible); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return attempt.ErrLateResult
			}
			return err
		}
	}
	var vectorDim int64
	var modelName string
	if err := conn.QueryRowContext(ctx, `SELECT COALESCE(vector_dim,0), model_name FROM embedding_generations WHERE id=?`, generationID).Scan(&vectorDim, &modelName); err != nil {
		return err
	}
	if vectorDim < 1 {
		return fmt.Errorf("%w: generation has no validated dimension", ErrInvalidResult)
	}
	if proposal.ModelID != modelName {
		return fmt.Errorf("%w: payload model %q does not match the frozen generation contract %q", ErrInvalidResult, proposal.ModelID, modelName)
	}
	// Every lineage version exactly once: a duplicate id passes a naive
	// count check while silently dropping another version's vector.
	seenVersions := make(map[int64]bool, len(proposal.Vectors))
	for _, vector := range proposal.Vectors {
		if seenVersions[vector.VersionID] {
			return fmt.Errorf("%w: duplicate vector for version %d", ErrInvalidResult, vector.VersionID)
		}
		seenVersions[vector.VersionID] = true
		digest, ok := lineage[vector.VersionID]
		if !ok || digest != vector.Digest {
			return fmt.Errorf("%w: vector digest does not match the frozen input", ErrInvalidResult)
		}
		if int64(len(vector.Values)) != vectorDim {
			return fmt.Errorf("%w: vector dimension %d does not match generation %d", ErrInvalidResult, len(vector.Values), vectorDim)
		}
	}
	if len(seenVersions) != len(lineage) {
		return fmt.Errorf("%w: payload covers %d of %d frozen lineage versions", ErrInvalidResult, len(seenVersions), len(lineage))
	}
	if attemptState == "Succeeded" {
		// Retried delivery of the sealed adjudication: the validated
		// envelope must still match the sealed projection byte-for-byte
		// (the stored ready vectors are the durable seal); divergence is a
		// late result.
		for _, vector := range proposal.Vectors {
			var stored []byte
			var state string
			err := conn.QueryRowContext(ctx, `SELECT vector, state FROM embeddings WHERE knowledge_version_id=? AND embedding_generation_id=?`, vector.VersionID, generationID).Scan(&stored, &state)
			if err != nil || state != "ready" || string(stored) != string(floatsToBytes(vector.Values)) {
				return attempt.ErrLateResult
			}
		}
		if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
			return err
		}
		committed = true
		return nil
	}
	if attemptState != "Running" || !leaseUntil.Valid || !validLease(leaseUntil.String, service.nowText()) {
		return attempt.ErrLateResult
	}
	blob, err := json.Marshal(proposal)
	if err != nil {
		return err
	}
	_ = blob
	now := service.nowText()
	for _, vector := range proposal.Vectors {
		result, err := conn.ExecContext(ctx, `
			UPDATE embeddings SET state='ready', vector=?, updated_at=?
			WHERE knowledge_version_id=? AND embedding_generation_id=? AND state='pending'`,
			floatsToBytes(vector.Values), now, vector.VersionID, generationID)
		if err != nil {
			return err
		}
		if affected, _ := result.RowsAffected(); affected != 1 {
			return attempt.ErrLateResult
		}
	}
	if _, err := conn.ExecContext(ctx, `
		UPDATE execution_attempts SET state='Succeeded', ended_at=?, row_version=row_version+1
		WHERE id=? AND state='Running' AND boot_id=? AND connection_epoch=?`, now, attemptID, bootID, epoch); err != nil {
		return err
	}
	// Settled generations switch atomically in the same transaction.
	if generationState == "building" {
		if _, err := service.finalizeIfSettled(ctx, conn, generationID); err != nil {
			return err
		}
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return err
	}
	committed = true
	return nil
}

// Fail seals a running embedding attempt as Failed and marks its pending
// batch rows failed (a failed embedding never revokes the knowledge itself;
// the version stays FTS-retrievable with an honest failed index state).
func (service *Service) Fail(ctx context.Context, attemptID int64, bootID string, epoch uint64, reason string) error {
	return service.sealNonSuccess(ctx, attemptID, bootID, epoch, "Failed", reason)
}

// Interrupt seals an embedding attempt after runtime loss; pending rows stay
// pending so the next sweep re-attempts them.
func (service *Service) Interrupt(ctx context.Context, attemptID int64, bootID string, epoch uint64, reason string) error {
	return service.sealNonSuccess(ctx, attemptID, bootID, epoch, "Interrupted", reason)
}

func (service *Service) sealNonSuccess(ctx context.Context, attemptID int64, bootID string, epoch uint64, terminal, reason string) error {
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
	var state string
	var generationID int64
	var frozenBoot sql.NullString
	var frozenEpoch sql.NullInt64
	err = conn.QueryRowContext(ctx, `
		SELECT a.state, a.scope_id, a.boot_id, a.connection_epoch FROM execution_attempts a
		WHERE a.id=? AND a.attempt_type='embedding'`, attemptID).
		Scan(&state, &generationID, &frozenBoot, &frozenEpoch)
	if errors.Is(err, sql.ErrNoRows) {
		return attempt.ErrLateResult
	}
	if err != nil {
		return err
	}
	// When the caller carries the runtime binding it must match the frozen
	// row: attempts never re-bind, so the row identity is itself the fence
	// and stale frames from a previous boot never adjudicate a fresh row.
	if bootID != "" && frozenBoot.Valid && frozenBoot.String != bootID {
		return attempt.ErrLateResult
	}
	if epoch > 0 && frozenEpoch.Valid && uint64(frozenEpoch.Int64) != epoch {
		return attempt.ErrLateResult
	}
	if state == terminal || state == "Succeeded" {
		if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
			return err
		}
		committed = true
		return nil
	}
	if state == "Queued" || state == "Assigned" || state == "Running" || state == "Cancelling" {
		// Pending rebuild rows of this attempt's batch flip to failed only
		// for terminal failures; an interrupt leaves them re-attemptable.
		if terminal == "Failed" {
			if _, err := conn.ExecContext(ctx, `
				UPDATE embeddings SET state='failed', updated_at=?
				WHERE state='pending' AND embedding_generation_id=?
				  AND knowledge_version_id IN (
					SELECT i.knowledge_version_id FROM attempt_input_items i
					JOIN attempt_input_snapshots s ON s.id=i.snapshot_id
					WHERE s.attempt_id=? AND i.knowledge_version_id IS NOT NULL)`,
				service.nowText(), generationID, attemptID); err != nil {
				return err
			}
		}
		if _, err := conn.ExecContext(ctx, `
			UPDATE execution_attempts SET state=?, ended_at=?, termination_reason=?, row_version=row_version+1
			WHERE id=? AND state=?`, terminal, service.nowText(), reason, attemptID, state); err != nil {
			return err
		}
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return err
	}
	committed = true
	service.deliverQueryOutcome(attemptID, queryOutcome{err: fmt.Errorf("embedding attempt %s", strings.ToLower(terminal))})
	return nil
}

// CancelQuery cleans up a query attempt whose HTTP request gave up waiting
// (the fence closes Queued directly; Assigned/Running keep their binding and
// converge through the runtime cancel path).
func (service *Service) CancelQuery(ctx context.Context, attemptID int64) {
	attempts := attempt.NewService(service.db)
	// Binding-preserving leftovers converge through the lease sweeper.
	_, _ = attempts.CancelFence(ctx, attemptID)
	service.forgetQuery(attemptID)
}

// QueryVector resolves the query embedding through the real attempt path:
// create (or reuse) a Queued query attempt, dispatch it and wait bounded for
// the committed vector.
func (service *Service) QueryVector(ctx context.Context, query string, wait time.Duration) ([]float32, GenerationView, error) {
	if err := service.Sweep(ctx); err != nil && !errors.Is(err, ErrNoProvider) {
		return nil, GenerationView{}, err
	}
	attemptID, generation, err := service.CreateQueryAttempt(ctx, query)
	if err != nil {
		return nil, generation, err
	}
	// CreateQueryAttempt already registered this attempt as live; only the
	// outcome channel is attached here.
	outcome := service.registerQueryWaiter(attemptID, "")
	defer service.forgetQuery(attemptID)
	if service.dispatcher != nil {
		if err := service.dispatcher(ctx, attemptID); err != nil {
			// The attempt stays Queued; the reconnect sweep owns it. The
			// request reports an honestly empty semantic channel instead of
			// blocking on a runtime that is not attached.
			service.CancelQuery(ctx, attemptID)
			return nil, generation, ErrBusy
		}
	} else {
		service.CancelQuery(ctx, attemptID)
		return nil, generation, ErrBusy
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case result := <-outcome:
		if result.err != nil {
			return nil, generation, result.err
		}
		return result.vector, generation, nil
	case <-timer.C:
		service.CancelQuery(ctx, attemptID)
		return nil, generation, ErrBusy
	case <-ctx.Done():
		service.CancelQuery(ctx, attemptID)
		return nil, generation, ErrBusy
	}
}

func (service *Service) registerQueryWaiter(attemptID int64, _ string) chan queryOutcome {
	service.mu.Lock()
	defer service.mu.Unlock()
	outcome := make(chan queryOutcome, 1)
	if service.waiters == nil {
		service.waiters = map[int64]chan queryOutcome{}
	}
	service.waiters[attemptID] = outcome
	return outcome
}

func (service *Service) forgetQuery(attemptID int64) {
	service.mu.Lock()
	defer service.mu.Unlock()
	delete(service.waiters, attemptID)
	delete(service.queryTexts, attemptID)
}

func (service *Service) deliverQueryOutcome(attemptID int64, outcome queryOutcome) {
	service.mu.Lock()
	channel, ok := service.waiters[attemptID]
	service.mu.Unlock()
	if ok {
		select {
		case channel <- outcome:
		default:
		}
	}
}

func (service *Service) queryTextFor(attemptID int64) (string, bool) {
	service.mu.Lock()
	defer service.mu.Unlock()
	text, ok := service.queryTexts[attemptID]
	return text, ok
}

// frozenLineage reads the attempt's frozen input digests by version id.
func (service *Service) frozenLineage(ctx context.Context, conn *sql.Conn, attemptID int64) (map[int64]string, error) {
	rows, err := conn.QueryContext(ctx, `
		SELECT i.knowledge_version_id, i.source_digest FROM attempt_input_items i
		JOIN attempt_input_snapshots s ON s.id=i.snapshot_id
		WHERE s.attempt_id=? AND i.knowledge_version_id IS NOT NULL`, attemptID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	lineage := map[int64]string{}
	for rows.Next() {
		var versionID int64
		var digest string
		if err := rows.Scan(&versionID, &digest); err != nil {
			return nil, err
		}
		lineage[versionID] = digest
	}
	return lineage, rows.Err()
}

// payloadKind peeks the schema kind without full validation.
func payloadKind(raw []byte) (string, string, error) {
	var envelope struct {
		SchemaKind string `json:"schemaKind"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return "", "", fmt.Errorf("%w: not valid JSON", ErrInvalidResult)
	}
	switch envelope.SchemaKind {
	case ResultSchemaKind:
		return envelope.SchemaKind, "rebuild", nil
	case QuerySchemaKind:
		return envelope.SchemaKind, "query", nil
	}
	return "", "", fmt.Errorf("%w: unknown schema kind %q", ErrInvalidResult, envelope.SchemaKind)
}

// floatsToBytes encodes the vector as little-endian float32 (schema: byte
// length = vector_dim * 4).
func floatsToBytes(values []float32) []byte {
	blob := make([]byte, len(values)*4)
	for index, value := range values {
		bits := math.Float32bits(value)
		blob[index*4] = byte(bits)
		blob[index*4+1] = byte(bits >> 8)
		blob[index*4+2] = byte(bits >> 16)
		blob[index*4+3] = byte(bits >> 24)
	}
	return blob
}

// BytesToFloats decodes a stored vector blob.
func BytesToFloats(blob []byte) []float32 {
	values := make([]float32, len(blob)/4)
	for index := range values {
		bits := uint32(blob[index*4]) | uint32(blob[index*4+1])<<8 | uint32(blob[index*4+2])<<16 | uint32(blob[index*4+3])<<24
		values[index] = math.Float32frombits(bits)
	}
	return values
}

func validLease(until, now string) bool {
	deadline, deadlineErr := time.Parse(time.RFC3339Nano, until)
	current, currentErr := time.Parse(time.RFC3339Nano, now)
	return deadlineErr == nil && currentErr == nil && deadline.After(current)
}
