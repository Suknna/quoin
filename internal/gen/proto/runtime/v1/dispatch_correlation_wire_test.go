package runtimev1

import (
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

// TestDispatchAttemptOperationCorrelationRoundTrip pins the independent
// business operation correlation carrier on DispatchAttempt (ADR-0006). It is
// an opaque propagation identity only: the wire must retain it verbatim while
// the existing per-stream ControlEnvelope.correlation_id message-pair field
// stays a separate, unchanged mechanism.
func TestDispatchAttemptOperationCorrelationRoundTrip(t *testing.T) {
	want := &ControlEnvelope{
		MessageId:       11,
		ConnectionEpoch: 2,
		CorrelationId:   7, // 既有流内请求-响应配对编号，语义不变
		BootId:          "plinth-boot",
		Msg: &ControlEnvelope_DispatchAttempt{DispatchAttempt: &DispatchAttempt{
			AttemptId:              42,
			AttemptType:            AttemptType_ATTEMPT_TYPE_INVESTIGATION,
			OperationCorrelationId: "corr_01JABCDEF0123456789ABCDEFGH",
		}},
	}
	encoded, err := proto.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	got := &ControlEnvelope{}
	if err := proto.Unmarshal(encoded, got); err != nil {
		t.Fatal(err)
	}
	if got.GetCorrelationId() != 7 {
		t.Fatalf("per-stream correlation_id changed: got %d, want 7", got.GetCorrelationId())
	}
	dispatch := got.GetDispatchAttempt()
	if dispatch == nil {
		t.Fatal("dispatch_attempt payload lost")
	}
	if dispatch.GetAttemptId() != 42 {
		t.Fatalf("attempt_id = %d, want 42", dispatch.GetAttemptId())
	}
	if dispatch.GetOperationCorrelationId() != "corr_01JABCDEF0123456789ABCDEFGH" {
		t.Fatalf("operation_correlation_id = %q, want verbatim retention", dispatch.GetOperationCorrelationId())
	}
}

// TestDispatchAttemptOperationCorrelationWireFieldNumber pins the frozen wire
// contract: operation_correlation_id is DispatchAttempt field 11, wire type
// length-delimited (RUNTIME-VERSION-002：字段编号一经分配永不复用).
func TestDispatchAttemptOperationCorrelationWireFieldNumber(t *testing.T) {
	encoded, err := proto.Marshal(&DispatchAttempt{OperationCorrelationId: "corr"})
	if err != nil {
		t.Fatal(err)
	}
	fieldNum, wireType, n := protowire.ConsumeTag(encoded)
	if m := protowire.ConsumeFieldValue(fieldNum, wireType, encoded[n:]); m < 0 {
		t.Fatal("malformed field value")
	}
	if fieldNum != 11 || wireType != protowire.BytesType {
		t.Fatalf("operation_correlation_id wire tag = (%d, %v), want (11, BytesType)", fieldNum, wireType)
	}
}

// TestDispatchAttemptOperationCorrelationEmptyIsDefault keeps "no correlation"
// indistinguishable from absence on the wire: proto3 zero value must not
// allocate bytes, so consumers cannot fake a correlation identity.
func TestDispatchAttemptOperationCorrelationEmptyIsDefault(t *testing.T) {
	encoded, err := proto.Marshal(&DispatchAttempt{AttemptId: 1})
	if err != nil {
		t.Fatal(err)
	}
	got := &DispatchAttempt{}
	if err := proto.Unmarshal(encoded, got); err != nil {
		t.Fatal(err)
	}
	if got.GetOperationCorrelationId() != "" {
		t.Fatalf("absent operation_correlation_id decoded as %q, want empty", got.GetOperationCorrelationId())
	}
}
