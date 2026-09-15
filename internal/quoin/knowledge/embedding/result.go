// result.go: embedding attempt result adjudication (rebuild batches and
// query embeds), failure sealing, and the in-process query registry that
// lets one HTTP search request wait for its own query-embedding attempt.
//
// 结果应用与密封经共享执行器 Execute 以系统主体自动审计运行；操作关联从
// 持久化 Attempt 恢复（attempt.LoadCorrelation），无关联的历史 Attempt 建立
// 显式的独立任务窗口，绝不静默伪装任何身份。
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
	"github.com/Suknna/quoin/internal/quoin/execution"
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

// taskContext restores the persisted operation correlation of an embedding
// attempt so the runtime result path stays linked to the operation that
// created it (ADR-0006 lifecycle correlation). A wired caller context passes
// through untouched; a legacy attempt without a persisted correlation gets an
// explicit fresh task window (never a fabricated identity).
func (service *Service) taskContext(ctx context.Context, attemptID int64) (context.Context, error) {
	if _, ok := execution.FromContext(ctx); ok {
		return ctx, nil
	}
	correlation, found, err := attempt.LoadCorrelation(ctx, service.writer, attemptID)
	if err != nil {
		return nil, err
	}
	actor := execution.Principal{Kind: execution.PrincipalSystem}
	initiator := actor
	source := execution.Source{Kind: execution.SourceTask, RequestID: fmt.Sprintf("embedding-%d", attemptID)}
	correlationID, corrErr := execution.NewCorrelationID()
	if corrErr != nil {
		return nil, corrErr
	}
	if found && correlation.OperationCorrelationID != "" {
		source.RequestID = fmt.Sprintf("task-%s", correlation.OperationCorrelationID)
		switch correlation.InitiatorType {
		case string(execution.PrincipalUser), string(execution.PrincipalService):
			initiator = execution.Principal{Kind: execution.PrincipalKind(correlation.InitiatorType), ID: correlation.InitiatorID}
		}
		return execution.WithMetadata(ctx, execution.Metadata{
			CorrelationID: correlation.OperationCorrelationID,
			Actor:         actor,
			Initiator:     initiator,
			Source:        source,
		})
	}
	return execution.WithMetadata(ctx, execution.Metadata{
		CorrelationID: correlationID,
		Actor:         actor,
		Initiator:     initiator,
		Source:        source,
	})
}

// adjudicate runs one result application step as an audited system operation
// whose correlation is restored from the persisted attempt.
func (service *Service) adjudicate(ctx context.Context, attemptID int64, fn func(tx *execution.Tx) (int64, error)) error {
	ctx, err := service.taskContext(ctx, attemptID)
	if err != nil {
		return err
	}
	_, err = execution.Execute(ctx, service.runner, service.resultTake, fn, func(generationID int64) int64 { return generationID })
	return err
}

// CommitResult adjudicates one embedding ResultProposal payload: identity,
// binding, digests, dimension and version state are re-verified before any
// projection write; late or divergent results are dropped with audit only
// (DATA-TX-012, DATA-EMBED-001).
func (service *Service) CommitResult(ctx context.Context, attemptID int64, bootID string, epoch uint64, raw []byte) error {
	_, mode, err := payloadKind(raw)
	if err != nil {
		return err
	}
	if mode == "query" {
		return service.adjudicate(ctx, attemptID, func(tx *execution.Tx) (int64, error) {
			return service.commitQueryResult(ctx, tx, attemptID, bootID, epoch, raw)
		})
	}
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
	return service.adjudicate(ctx, attemptID, func(tx *execution.Tx) (int64, error) {
		var attemptState, generationState string
		var generationID int64
		var leaseUntil sql.NullString
		err := tx.QueryRowContext(ctx, `
			SELECT a.state, a.scope_id, a.lease_until, g.state FROM execution_attempts a
			JOIN embedding_generations g ON g.id=a.scope_id
			WHERE a.id=? AND a.attempt_type='embedding' AND a.boot_id=? AND a.connection_epoch=?`,
			attemptID, bootID, epoch).Scan(&attemptState, &generationID, &leaseUntil, &generationState)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, attempt.ErrLateResult
		}
		if err != nil {
			return 0, err
		}
		if generationID != proposal.GenerationID {
			return 0, attempt.ErrLateResult
		}
		if generationState != "building" && generationState != "current" {
			return 0, attempt.ErrLateResult
		}
		// The complete envelope is validated before any state branch: identity,
		// the full frozen lineage, digests, dimensions and per-version
		// eligibility hold for a first commit and for a sealed replay alike.
		lineage, err := service.frozenLineage(ctx, tx, attemptID)
		if err != nil {
			return 0, err
		}
		if len(lineage) != len(proposal.Vectors) {
			return 0, fmt.Errorf("%w: vector count %d does not match frozen input %d", ErrInvalidResult, len(proposal.Vectors), len(lineage))
		}
		// DATA-TX-012 also re-checks version state: if any lineage version lost
		// retrieval eligibility (source withdrawn, stopped reuse, superseded)
		// while the batch was in flight, this result is late for the whole
		// batch — the schema's Succeeded closure demands every lineage version
		// ready, so partial writes are not an option.
		for versionID := range lineage {
			var eligible int
			if err := tx.QueryRowContext(ctx, `
				SELECT 1 FROM knowledge_search_docs d
				JOIN reusable_knowledge k ON k.current_version_id=d.knowledge_version_id
				WHERE d.knowledge_version_id=?`, versionID).Scan(&eligible); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return 0, attempt.ErrLateResult
				}
				return 0, err
			}
		}
		var vectorDim int64
		var modelName string
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(vector_dim,0), model_name FROM embedding_generations WHERE id=?`, generationID).Scan(&vectorDim, &modelName); err != nil {
			return 0, err
		}
		if vectorDim < 1 {
			return 0, fmt.Errorf("%w: generation has no validated dimension", ErrInvalidResult)
		}
		if proposal.ModelID != modelName {
			return 0, fmt.Errorf("%w: payload model %q does not match the frozen generation contract %q", ErrInvalidResult, proposal.ModelID, modelName)
		}
		// Every lineage version exactly once: a duplicate id passes a naive
		// count check while silently dropping another version's vector.
		seenVersions := make(map[int64]bool, len(proposal.Vectors))
		for _, vector := range proposal.Vectors {
			if seenVersions[vector.VersionID] {
				return 0, fmt.Errorf("%w: duplicate vector for version %d", ErrInvalidResult, vector.VersionID)
			}
			seenVersions[vector.VersionID] = true
			digest, ok := lineage[vector.VersionID]
			if !ok || digest != vector.Digest {
				return 0, fmt.Errorf("%w: vector digest does not match the frozen input", ErrInvalidResult)
			}
			if int64(len(vector.Values)) != vectorDim {
				return 0, fmt.Errorf("%w: vector dimension %d does not match generation %d", ErrInvalidResult, len(vector.Values), vectorDim)
			}
		}
		if len(seenVersions) != len(lineage) {
			return 0, fmt.Errorf("%w: payload covers %d of %d frozen lineage versions", ErrInvalidResult, len(seenVersions), len(lineage))
		}
		if attemptState == "Succeeded" {
			// Retried delivery of the sealed adjudication: the validated
			// envelope must still match the sealed projection byte-for-byte
			// (the stored ready vectors are the durable seal); divergence is a
			// late result.
			for _, vector := range proposal.Vectors {
				var stored []byte
				var state string
				err := tx.QueryRowContext(ctx, `SELECT vector, state FROM embeddings WHERE knowledge_version_id=? AND embedding_generation_id=?`, vector.VersionID, generationID).Scan(&stored, &state)
				if err != nil || state != "ready" || string(stored) != string(floatsToBytes(vector.Values)) {
					return 0, attempt.ErrLateResult
				}
			}
			return generationID, nil
		}
		if attemptState != "Running" || !leaseUntil.Valid || !validLease(leaseUntil.String, service.nowText()) {
			return 0, attempt.ErrLateResult
		}
		now := service.nowText()
		for _, vector := range proposal.Vectors {
			result, err := tx.ExecContext(ctx, `
				UPDATE embeddings SET state='ready', vector=?, updated_at=?
				WHERE knowledge_version_id=? AND embedding_generation_id=? AND state='pending'`,
				floatsToBytes(vector.Values), now, vector.VersionID, generationID)
			if err != nil {
				return 0, err
			}
			if affected, _ := result.RowsAffected(); affected != 1 {
				return 0, attempt.ErrLateResult
			}
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE execution_attempts SET state='Succeeded', ended_at=?, row_version=row_version+1
			WHERE id=? AND state='Running' AND boot_id=? AND connection_epoch=?`, now, attemptID, bootID, epoch); err != nil {
			return 0, err
		}
		// Settled generations switch atomically in the same transaction.
		if generationState == "building" {
			if _, err := service.finalizeIfSettled(ctx, tx, generationID); err != nil {
				return 0, err
			}
		}
		return generationID, nil
	})
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
	ctx, err := service.taskContext(ctx, attemptID)
	if err != nil {
		return err
	}
	_, execErr := execution.Execute(ctx, service.runner, service.resultSeal, func(tx *execution.Tx) (int64, error) {
		var state string
		var generationID int64
		var frozenBoot sql.NullString
		var frozenEpoch sql.NullInt64
		err := tx.QueryRowContext(ctx, `
			SELECT a.state, a.scope_id, a.boot_id, a.connection_epoch FROM execution_attempts a
			WHERE a.id=? AND a.attempt_type='embedding'`, attemptID).
			Scan(&state, &generationID, &frozenBoot, &frozenEpoch)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, attempt.ErrLateResult
		}
		if err != nil {
			return 0, err
		}
		// When the caller carries the runtime binding it must match the frozen
		// row: attempts never re-bind, so the row identity is itself the fence
		// and stale frames from a previous boot never adjudicate a fresh row.
		if bootID != "" && frozenBoot.Valid && frozenBoot.String != bootID {
			return 0, attempt.ErrLateResult
		}
		if epoch > 0 && frozenEpoch.Valid && uint64(frozenEpoch.Int64) != epoch {
			return 0, attempt.ErrLateResult
		}
		if state == terminal || state == "Succeeded" {
			return generationID, nil
		}
		if state == "Queued" || state == "Assigned" || state == "Running" || state == "Cancelling" {
			// Pending rebuild rows of this attempt's batch flip to failed only
			// for terminal failures; an interrupt leaves them re-attemptable.
			if terminal == "Failed" {
				if _, err := tx.ExecContext(ctx, `
					UPDATE embeddings SET state='failed', updated_at=?
					WHERE state='pending' AND embedding_generation_id=?
					  AND knowledge_version_id IN (
						SELECT i.knowledge_version_id FROM attempt_input_items i
						JOIN attempt_input_snapshots s ON s.id=i.snapshot_id
						WHERE s.attempt_id=? AND i.knowledge_version_id IS NOT NULL)`,
					service.nowText(), generationID, attemptID); err != nil {
					return 0, err
				}
			}
			if _, err := tx.ExecContext(ctx, `
				UPDATE execution_attempts SET state=?, ended_at=?, termination_reason=?, row_version=row_version+1
				WHERE id=? AND state=?`, terminal, service.nowText(), reason, attemptID, state); err != nil {
				return 0, err
			}
		}
		return generationID, nil
	}, func(generationID int64) int64 { return generationID })
	if execErr != nil {
		return execErr
	}
	service.deliverQueryOutcome(attemptID, queryOutcome{err: fmt.Errorf("embedding attempt %s", strings.ToLower(terminal))})
	return nil
}

// CancelQuery cleans up a query attempt whose HTTP request gave up waiting
// (the fence closes Queued directly; Assigned/Running keep their binding and
// converge through the runtime cancel path). The attempt cleanup machine is
// reached through the shared runner's write authority — the controlled raw
// access main's composition owns; no query text or projection state is
// touched outside the registry here.
func (service *Service) CancelQuery(ctx context.Context, attemptID int64) {
	attempts := attempt.NewService(service.writer)
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
func (service *Service) frozenLineage(ctx context.Context, q executor, attemptID int64) (map[int64]string, error) {
	rows, err := q.QueryContext(ctx, `
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
