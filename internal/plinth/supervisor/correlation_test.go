package supervisor

// The narrow local correlation carrier for dispatched tasks: typed context
// propagation from the dispatch frame, safe on nil/empty inputs, and with
// explicitly no authorization authority (ADR-0006).

import (
	"context"
	"testing"

	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
)

func TestDispatchContextCarriesOperationCorrelation(t *testing.T) {
	parent := context.Background()
	ctx := dispatchContext(parent, &runtimev1.DispatchAttempt{
		AttemptId:              7,
		OperationCorrelationId: "corr-01ABCDEF",
	})
	if got := OperationCorrelation(ctx); got != "corr-01ABCDEF" {
		t.Fatalf("task context correlation = %q, want the dispatch identity", got)
	}
	// The parent scope itself must stay untouched: only the derived task
	// scope carries the value.
	if got := OperationCorrelation(parent); got != "" {
		t.Fatalf("parent context correlation = %q, want untouched parent", got)
	}
}

func TestDispatchContextWithoutCorrelationStaysUntouched(t *testing.T) {
	// Legacy dispatch (empty correlation) must not wrap the context: the
	// task scope stays identical so consumers see absence, never "" as a
	// fabricated identity.
	parent := context.WithValue(context.Background(), operationCorrelationKey{}, "sentinel")
	ctx := dispatchContext(parent, &runtimev1.DispatchAttempt{AttemptId: 7})
	if ctx != parent {
		t.Fatal("empty correlation must return the parent context unchanged")
	}
}

func TestOperationCorrelationAbsentAndNilSafe(t *testing.T) {
	if got := OperationCorrelation(context.Background()); got != "" {
		t.Fatalf("plain context correlation = %q, want empty", got)
	}
	if got := OperationCorrelation(nil); got != "" {
		t.Fatalf("nil context correlation = %q, want empty", got)
	}
	// A wrong-typed value under the private key can never happen through
	// the typed setter; the accessor must still fail closed.
	if got := OperationCorrelation(context.WithValue(context.Background(), operationCorrelationKey{}, 42)); got != "" {
		t.Fatalf("non-string value surfaced as %q, want empty", got)
	}
}

func TestWithOperationCorrelationNilContext(t *testing.T) {
	if got := WithOperationCorrelation(nil, "corr"); got != nil {
		t.Fatalf("nil context input returned %v, want nil", got)
	}
}
