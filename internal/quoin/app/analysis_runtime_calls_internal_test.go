package app

import (
	"context"
	"database/sql"
	"testing"

	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	qruntime "github.com/Suknna/quoin/internal/quoin/runtime"
	_ "modernc.org/sqlite"
)

// TestRuntimeKubernetesResultIngressRejectsUnspecifiedOutcome executes the
// private Runtime ingress rather than duplicating its result-validation logic.
// The outcome fence is deliberately first, so this adversarial frame reaches no
// database-backed attempt or Evidence path.
func TestRuntimeKubernetesResultIngressRejectsUnspecifiedOutcome(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var ack *runtimev1.ControlEnvelope
	service := &RuntimeService{
		Slots: qruntime.NewService(db),
		sendEnvelopeForTest: func(slot string, envelope *runtimev1.ControlEnvelope) error {
			if slot != qruntime.SlotPlinth {
				t.Fatalf("slot=%q, want %q", slot, qruntime.SlotPlinth)
			}
			ack = envelope
			return nil
		},
	}
	service.handleCompleteToolCallRouted(context.Background(), &runtimev1.ControlEnvelope{}, &runtimev1.CompleteToolCall{
		AttemptId:  1,
		ToolCallId: 1,
		Payload: &runtimev1.ResultPayload{
			SchemaKind:    "kubernetes_read_result_v1",
			CanonicalJson: []byte(`{"success":true,"operation":"discovery","results":[]}`),
		},
	})
	if ack == nil || ack.GetCompleteToolCallAck() == nil {
		t.Fatal("runtime ingress did not send CompleteToolCallAck")
	}
	if ack.GetCompleteToolCallAck().GetAccepted() || ack.GetCompleteToolCallAck().GetDetail() != "outcome unspecified" {
		t.Fatalf("ack=%+v, want rejected unspecified outcome", ack.GetCompleteToolCallAck())
	}
}
