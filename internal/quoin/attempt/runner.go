package attempt

// Runner adoption for the standalone lifecycle stages (ADR-0006, plan stage
// 5): every transaction-owning stage (agent model/tool ledger,
// cancel/interrupt/lease-sweep) executes through the shared execution runner.
// The runner owns the BEGIN IMMEDIATE transaction and persists the automatic
// audit row in the same transaction, so a lifecycle write can never commit
// without its record and business code never calls the audit writer directly.
//
// The transaction-composable On variants (CancelFenceOn, InterruptOn,
// CommitResultOn) stay plain DBTX helpers: their callers compose them inside
// their own audited runner operations.

import (
	"context"
	"errors"
	"fmt"

	"github.com/Suknna/quoin/internal/quoin/execution"
)

// registerOperations declares every standalone lifecycle stage. There is no
// bypass: an undeclared stage cannot execute. The operation names are the
// lifecycle audit vocabulary (attempt.<fact>), so the automatic audit rows
// keep the same action identity the manual writes used.
func (service *Service) registerOperations() {
	register := func(op execution.Operation) *execution.Operation {
		declared, err := service.runner.Register(op)
		if err != nil {
			// A declaration conflict is a programming error that must surface
			// at startup, never at first use.
			panic("attempt: register " + op.Name + ": " + err.Error())
		}
		return declared
	}
	agent := func(name, objectType string) *execution.Operation {
		return register(execution.Operation{Name: name, Class: execution.ClassWrite, ObjectType: objectType, Authorize: requireRuntimeAuthority})
	}
	service.opModelCallBegin = agent(auditActionModelCallBegin, auditRefModelCall)
	service.opModelCallComplete = agent(auditActionModelCallComplete, auditRefModelCall)
	service.opToolCallBegin = agent(auditActionToolCallBegin, auditRefToolCall)
	service.opToolCallComplete = agent(auditActionToolCallComplete, auditRefToolCall)
	lifecycle := func(name string) *execution.Operation {
		return agent(name, auditRefExecutionAttempt)
	}
	service.opCancelFence = lifecycle(auditActionCancelFence)
	service.opInterrupt = lifecycle(auditActionInterrupt)
	service.opLeaseSweep = lifecycle(auditActionLeaseSweep)
	service.opToolCallCancel = lifecycle(auditActionToolCallCancel)
	service.opDispatchBind = lifecycle(auditActionDispatchBind)
	service.opDispatchAccept = lifecycle(auditActionDispatchAccept)
	service.opResultCommit = lifecycle(auditActionResultCommit)
	service.opCancelAck = lifecycle(auditActionCancelAck)
}

// requireRuntimeAuthority confines the machine lifecycle stages to the
// system runtime authority. Callers reach the runner only through
// lifecycleAuthority / sweepAuthority, which root the context on the
// attempt's persisted association or an explicit machine scope — a raw user
// or service context can never drive these stages, and there is no session
// fallback to fake.
func requireRuntimeAuthority(ctx context.Context, _ *execution.Tx) error {
	meta, err := execution.Require(ctx)
	if err != nil {
		return err
	}
	if meta.Actor.Kind != execution.PrincipalSystem || meta.Actor.ID != 0 {
		return fmt.Errorf("attempt: lifecycle stage requires the system runtime authority, got %s/%d", meta.Actor.Kind, meta.Actor.ID)
	}
	if meta.Source.Kind == execution.SourceHTTP {
		return errors.New("attempt: lifecycle stage cannot arrive from the http channel")
	}
	return nil
}

// lifecycleAuthority resolves the machine identity of one standalone
// lifecycle stage on the attempt's PERSISTED association (ADR-0006: the
// row-scoped correlation is authoritative and immutable — never the raw
// caller context). The actor is always the system runtime authority; the
// original initiator comes verbatim from the row. A legacy row without a
// persisted association still fails closed into an explicit fresh task
// scope: a deliberate machine operation under its own identity, auditable,
// and never an anonymous "assume the caller" fallback.
//
// The read runs on the pool BEFORE the runner transaction opens; the
// association is immutable once set (PersistCorrelationOn only performs the
// NULL → value transition), so no transaction is needed to trust it.
func (service *Service) lifecycleAuthority(ctx context.Context, attemptID int64) (context.Context, error) {
	correlation, found, err := LoadCorrelation(ctx, service.Reader(), attemptID)
	if err != nil {
		return nil, err
	}
	if found && correlation.OperationCorrelationID != "" {
		initiator := execution.Principal{Kind: execution.PrincipalSystem, ID: 0}
		if correlation.InitiatorType != "" {
			initiator = execution.Principal{Kind: execution.PrincipalKind(correlation.InitiatorType), ID: correlation.InitiatorID}
		}
		return execution.ReplaceMetadata(ctx, execution.Metadata{
			CorrelationID: correlation.OperationCorrelationID,
			Actor:         execution.Principal{Kind: execution.PrincipalSystem, ID: 0},
			Initiator:     initiator,
			Source:        execution.Source{Kind: execution.SourceTask},
		})
	}
	correlationID, err := execution.NewCorrelationID()
	if err != nil {
		return nil, err
	}
	return execution.ReplaceMetadata(ctx, execution.Metadata{
		CorrelationID: correlationID,
		Actor:         execution.Principal{Kind: execution.PrincipalSystem, ID: 0},
		Source:        execution.Source{Kind: execution.SourceTask},
	})
}

// sweepAuthority roots one lease-sweep transition. The attempt's persisted
// association stays the correlation authority and this sweep trigger is only
// the request identity; a legacy attempt without an association is audited
// under the batch's explicit scheduler scope — the trigger correlation
// itself, never the raw caller context.
func (service *Service) sweepAuthority(ctx context.Context, attemptID int64, trigger string) (context.Context, error) {
	meta := execution.Metadata{
		CorrelationID: trigger,
		Actor:         execution.Principal{Kind: execution.PrincipalSystem, ID: 0},
		Initiator:     execution.Principal{Kind: execution.PrincipalSystem, ID: 0},
		Source:        execution.Source{Kind: execution.SourceScheduler, RequestID: trigger},
	}
	correlation, found, err := LoadCorrelation(ctx, service.Reader(), attemptID)
	if err != nil {
		return nil, err
	}
	if found && correlation.OperationCorrelationID != "" {
		meta.CorrelationID = correlation.OperationCorrelationID
		if correlation.InitiatorType != "" {
			meta.Initiator = execution.Principal{Kind: execution.PrincipalKind(correlation.InitiatorType), ID: correlation.InitiatorID}
		}
	}
	return execution.ReplaceMetadata(ctx, meta)
}

// CancelPendingToolCall cancels one still-running tool call of an attempt
// whose parent went terminal pending browser cleanup (the reconcile drain).
// It is the runner authority for the fenced tool_calls transition: the
// runner owns the transaction and the automatic audit row under the
// attempt's persisted-correlation system scope. A tool call that is no
// longer 'running' (a result or cancel won the race) is an
// execution.ErrNoTransition replay and records nothing.
func (service *Service) CancelPendingToolCall(ctx context.Context, attemptID, toolCallID int64, detail string) error {
	authority, err := service.lifecycleAuthority(ctx, attemptID)
	if err != nil {
		return err
	}
	_, err = execution.Execute(authority, service.runner, service.opToolCallCancel,
		func(tx *execution.Tx) (struct{}, error) {
			result, err := tx.ExecContext(authority, `
				UPDATE tool_calls SET status='cancelled', ended_at=?, error_detail=?
				WHERE id=? AND attempt_id=? AND status='running'`,
				service.nowText(), detail, toolCallID, attemptID)
			if err != nil {
				return struct{}{}, err
			}
			if affected, _ := result.RowsAffected(); affected == 0 {
				return struct{}{}, fmt.Errorf("%w: tool call %d is not running", execution.ErrNoTransition, toolCallID)
			}
			return struct{}{}, nil
		},
		func(struct{}) int64 { return toolCallID })
	if errors.Is(err, execution.ErrNoTransition) {
		return nil
	}
	return err
}
