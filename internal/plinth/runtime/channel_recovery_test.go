package runtime

// T12 recovery-semantics tests for the outbound channel: reliable terminal
// delivery (register → deliver → ack completion), reconcile-report
// composition (pending results flush BEFORE the report), and the physical
// dispatch dedup that never spawns a second worker for one attempt.

import (
	"context"
	"testing"
	"time"

	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
)

// fakeStream captures outbound envelopes without a live gRPC stream.
type fakeStream struct {
	sent []*runtimev1.ControlEnvelope
	fail bool
}

func (stream *fakeStream) Send(envelope *runtimev1.ControlEnvelope) error {
	if stream.fail {
		return errStreamDown
	}
	stream.sent = append(stream.sent, envelope)
	return nil
}

type fakeTaskSupervisor struct {
	dispatched int
	cancels    int
	bindings   []DispatchBinding
}

func (supervisor *fakeTaskSupervisor) HandleDispatchAttempt(ctx context.Context, sink *FrameSink, client runtimev1.RuntimeControlClient, dispatch *runtimev1.DispatchAttempt, binding DispatchBinding, stopTask func(int64) bool) {
	supervisor.dispatched++
	supervisor.bindings = append(supervisor.bindings, binding)
}

func (supervisor *fakeTaskSupervisor) HandleCancelAttempt(ctx context.Context, sink *FrameSink, cancel *runtimev1.CancelAttempt, stopTask func(int64) bool) {
	supervisor.cancels++
}

type downError struct{}

func (downError) Error() string { return "stream down" }

var errStreamDown = downError{}

func newTestChannel(t *testing.T) (*Channel, *fakeStream) {
	t.Helper()
	channel, err := NewChannel(ChannelConfig{Slot: "plinth"})
	if err != nil {
		t.Fatal(err)
	}
	stream := &fakeStream{}
	channel.sendStream = stream
	channel.outboundSeq = 1
	return channel, stream
}

func resultProposal(attemptID int64, binding DispatchBinding) *runtimev1.ResultProposal {
	return &runtimev1.ResultProposal{
		AttemptId: attemptID, BootId: binding.BootID, ConnectionEpoch: binding.Epoch,
		Outcome: runtimev1.AttemptOutcome_ATTEMPT_OUTCOME_SUCCEEDED,
		Payload: &runtimev1.ResultPayload{SchemaKind: "initial_analysis_output_v1", CanonicalJson: []byte(`"结论"`)},
	}
}

func TestPendingResultDeliveredUntilAckSurvives(t *testing.T) {
	channel, stream := newTestChannel(t)
	binding := DispatchBinding{BootID: "boot-a", Epoch: 1}

	ackCh := make(chan *runtimev1.ResultAck, 1)
	go func() {
		ack, err := channel.ProposeResult(context.Background(), resultProposal(7, binding))
		if err == nil {
			ackCh <- ack
		}
	}()
	// The immediate delivery round sends the proposal.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(stream.sent) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(stream.sent) == 0 {
		t.Fatal("proposal never sent")
	}
	if got := stream.sent[0].GetResultProposal(); got == nil || got.GetAttemptId() != 7 || got.GetBootId() != "boot-a" || got.GetConnectionEpoch() != 1 {
		t.Fatalf("proposal envelope wrong: %+v", stream.sent[0])
	}
	// The ack is lost; the retry loop re-delivers the identical proposal
	// (Quoin's idempotent adjudication makes replays safe).
	channel.DeliverPendingResults()
	if len(stream.sent) != 2 {
		t.Fatalf("retry round did not re-deliver: %d frames", len(stream.sent))
	}
	// A surviving ack completes the waiter and clears the entry.
	channel.dispatchServerFrame(context.Background(), &FrameSink{channel: channel}, nil, &runtimev1.ControlEnvelope{
		Msg: &runtimev1.ControlEnvelope_ResultAck{ResultAck: &runtimev1.ResultAck{AttemptId: 7, Accepted: true}},
	})
	select {
	case ack := <-ackCh:
		if !ack.GetAccepted() {
			t.Fatalf("ack=%+v", ack)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("propose result never completed")
	}
	if channel.HasPendingResult(7) {
		t.Fatal("completed entry stayed pending")
	}
	// Duplicate late acks are dropped (no waiter, no panic).
	channel.dispatchServerFrame(context.Background(), &FrameSink{channel: channel}, nil, &runtimev1.ControlEnvelope{
		Msg: &runtimev1.ControlEnvelope_ResultAck{ResultAck: &runtimev1.ResultAck{AttemptId: 7, Accepted: true}},
	})
}

func TestReconcileReportFlushesPendingBeforeReporting(t *testing.T) {
	channel, stream := newTestChannel(t)
	binding := DispatchBinding{BootID: "boot-a", Epoch: 1}
	channel.RegisterResult(resultProposal(9, binding))
	stream.sent = nil
	// One task still executing, one finished with an un-acked result.
	channel.RegisterTask(5, func() {})

	channel.dispatchServerFrame(context.Background(), &FrameSink{channel: channel}, nil, &runtimev1.ControlEnvelope{
		Msg: &runtimev1.ControlEnvelope_ReconcileRequest{ReconcileRequest: &runtimev1.ReconcileRequest{ActiveAttemptIds: []int64{5, 9}}},
	})
	if len(stream.sent) != 2 {
		t.Fatalf("expected proposal + report, got %d frames", len(stream.sent))
	}
	if stream.sent[0].GetResultProposal() == nil || stream.sent[0].GetResultProposal().GetAttemptId() != 9 {
		t.Fatalf("pending result must flush first: %+v", stream.sent[0])
	}
	report := stream.sent[1].GetReconcileReport()
	if report == nil || len(report.GetRunningAttemptIds()) != 1 || report.GetRunningAttemptIds()[0] != 5 {
		t.Fatalf("report must list only the executing attempt: %+v", report)
	}
}

func TestDispatchDedupNeverSpawnsASecondWorker(t *testing.T) {
	channel, stream := newTestChannel(t)
	tasks := &fakeTaskSupervisor{}
	channel.Tasks = tasks
	sink := &FrameSink{channel: channel}
	// The first dispatch spawns the executor; only afterwards is the
	// attempt registered as executing (the registry entry models the
	// running task).
	dispatch := &runtimev1.ControlEnvelope{
		BootId: "boot-a", ConnectionEpoch: 2,
		Msg: &runtimev1.ControlEnvelope_DispatchAttempt{DispatchAttempt: &runtimev1.DispatchAttempt{AttemptId: 11}},
	}
	channel.dispatchServerFrame(context.Background(), sink, nil, dispatch)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && tasks.dispatched == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if tasks.dispatched != 1 {
		t.Fatal("first dispatch never reached the supervisor")
	}
	channel.RegisterTask(11, func() {})
	stream.sent = nil
	channel.dispatchServerFrame(context.Background(), sink, nil, dispatch)
	if tasks.dispatched != 1 {
		t.Fatalf("executing attempt re-ran: dispatched=%d", tasks.dispatched)
	}
	if len(stream.sent) != 1 || stream.sent[0].GetAttemptAccept() == nil || stream.sent[0].GetAttemptAccept().GetAttemptId() != 11 {
		t.Fatalf("re-dispatch must re-ack the accept: %+v", stream.sent)
	}
	// A finished attempt with an un-acked result re-delivers the result
	// instead of re-running.
	channel.stopTask(11)
	stream.sent = nil
	channel.RegisterResult(resultProposal(11, DispatchBinding{BootID: "boot-a", Epoch: 2}))
	stream.sent = nil // RegisterResult already fired its immediate delivery
	channel.dispatchServerFrame(context.Background(), sink, nil, dispatch)
	if tasks.dispatched != 1 {
		t.Fatalf("finished attempt re-ran: dispatched=%d", tasks.dispatched)
	}
	if len(stream.sent) != 1 || stream.sent[0].GetResultProposal() == nil {
		t.Fatalf("re-dispatch must re-deliver the pending result: %+v", stream.sent)
	}
}

func TestDispatchBindingFlowsFromEnvelope(t *testing.T) {
	channel, _ := newTestChannel(t)
	tasks := &fakeTaskSupervisor{}
	channel.Tasks = tasks
	channel.dispatchServerFrame(context.Background(), &FrameSink{channel: channel}, nil, &runtimev1.ControlEnvelope{
		BootId: "boot-frozen", ConnectionEpoch: 4,
		Msg: &runtimev1.ControlEnvelope_DispatchAttempt{DispatchAttempt: &runtimev1.DispatchAttempt{AttemptId: 42}},
	})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(tasks.bindings) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if len(tasks.bindings) != 1 {
		t.Fatal("dispatch never reached the supervisor")
	}
	if tasks.bindings[0] != (DispatchBinding{BootID: "boot-frozen", Epoch: 4}) {
		t.Fatalf("binding=%+v", tasks.bindings[0])
	}
}

func TestDeliverySurvivesStreamOutage(t *testing.T) {
	channel, stream := newTestChannel(t)
	channel.RegisterResult(resultProposal(3, DispatchBinding{BootID: "boot-a", Epoch: 1}))
	// The stream dies: delivery defers, the entry stays registered.
	stream.fail = true
	channel.DeliverPendingResults()
	if !channel.HasPendingResult(3) {
		t.Fatal("entry lost while the stream was down")
	}
	// Reconnect: a fresh stream takes the retried proposal.
	fresh := &fakeStream{}
	channel.outboundMu.Lock()
	channel.sendStream = fresh
	channel.outboundMu.Unlock()
	channel.DeliverPendingResults()
	if len(fresh.sent) != 1 || fresh.sent[0].GetResultProposal().GetAttemptId() != 3 {
		t.Fatalf("reconnected delivery missing: %+v", fresh.sent)
	}
}
