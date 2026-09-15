package supervisor

// Business operation correlation carrier for dispatched tasks (ADR-0006).
// The typed execution context (WithMetadata/ReplaceMetadata) stays a
// Quoin-side type; the supervisor deliberately keeps this narrow local
// carrier so Plinth never imports Quoin's execution/audit/database stack.
// The correlation is an opaque diagnostics identity: it carries no
// authorization authority, is never accepted back from the runtime, and
// every reply joins attempts by id alone.

import (
	"context"

	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
)

type operationCorrelationKey struct{}

// WithOperationCorrelation attaches the business operation correlation of
// one dispatched attempt to the task context. An empty correlation (legacy
// dispatch) leaves the context untouched; a nil context is passed through.
// Only the dispatch boundary writes the value for one task scope — workers
// inherit it read-only.
func WithOperationCorrelation(ctx context.Context, correlationID string) context.Context {
	if ctx == nil || correlationID == "" {
		return ctx
	}
	return context.WithValue(ctx, operationCorrelationKey{}, correlationID)
}

// OperationCorrelation returns the correlation carried by the task context,
// or "" when the attempt has none.
func OperationCorrelation(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	correlation, _ := ctx.Value(operationCorrelationKey{}).(string)
	return correlation
}

// dispatchContext derives the task context for one dispatched attempt: the
// parent scope with the dispatch's persisted correlation attached.
func dispatchContext(parent context.Context, dispatch *runtimev1.DispatchAttempt) context.Context {
	return WithOperationCorrelation(parent, dispatch.GetOperationCorrelationId())
}
