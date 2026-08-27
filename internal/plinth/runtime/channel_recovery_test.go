package runtime

// T12 recovery-semantics tests for the outbound channel: reliable terminal
// delivery (register → deliver → ack completion), reconcile-report
// composition (pending results flush BEFORE the report), and the physical
// dispatch dedup that never spawns a second worker for one attempt.

import (
	"context"
	"sync"
	"testing"
	"time"

	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
)

// fakeStream captures outbound envelopes without a live gRPC stream.
type fakeStream struct {
	mu   sync.Mutex
	sent []*runtimev1.ControlEnvelope
	fail bool
}

func (stream *fakeStream) Send(envelope *runtimev1.ControlEnvelope) error {
	stream.mu.Lock()
	defer stream.mu.Unlock()
	if stream.fail {
		return errStreamDown
	}
	stream.sent = append(stream.sent, envelope)
	return nil
}

func (stream *fakeStream) snapshot() []*runtimev1.ControlEnvelope {
	stream.mu.Lock()
	defer stream.mu.Unlock()
	return append([]*runtimev1.ControlEnvelope(nil), stream.sent...)
}

func (stream *fakeStream) reset() {
	stream.mu.Lock()
	stream.sent = nil
	stream.mu.Unlock()
}

func (stream *fakeStream) setFail(value bool) {
	stream.mu.Lock()
	stream.fail = value
	stream.mu.Unlock()
}

type fakeTaskSupervisor struct {
	mu         sync.Mutex
	dispatched int
	cancels    int
	bindings   []DispatchBinding
}

func (supervisor *fakeTaskSupervisor) HandleDispatchAttempt(ctx context.Context, sink *FrameSink, client runtimev1.RuntimeControlClient, dispatch *runtimev1.DispatchAttempt, binding DispatchBinding, stopTask func(int64) bool) {
	supervisor.mu.Lock()
	supervisor.dispatched++
	supervisor.bindings = append(supervisor.bindings, binding)
	supervisor.mu.Unlock()
}

func (supervisor *fakeTaskSupervisor) HandleCancelAttempt(ctx context.Context, sink *FrameSink, cancel *runtimev1.CancelAttempt, stopTask func(int64) bool) {
	supervisor.mu.Lock()
	supervisor.cancels++
	supervisor.mu.Unlock()
}

func (supervisor *fakeTaskSupervisor) snapshot() (int, int, []DispatchBinding) {
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	return supervisor.dispatched, supervisor.cancels, append([]DispatchBinding(nil), supervisor.bindings...)
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
		if len(stream.snapshot()) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	frames := stream.snapshot()
	if len(frames) == 0 {
		t.Fatal("proposal never sent")
	}
	if got := frames[0].GetResultProposal(); got == nil || got.GetAttemptId() != 7 || got.GetBootId() != "boot-a" || got.GetConnectionEpoch() != 1 {
		t.Fatalf("proposal envelope wrong: %+v", frames[0])
	}
	// The ack is lost; the retry loop re-delivers the identical proposal
	// (Quoin's idempotent adjudication makes replays safe).
	channel.DeliverPendingResults()
	if frames := stream.snapshot(); len(frames) != 2 {
		t.Fatalf("retry round did not re-deliver: %d frames", len(frames))
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
	stream.reset()
	// One task still executing, one finished with an un-acked result.
	channel.RegisterTask(5, func() {})

	channel.dispatchServerFrame(context.Background(), &FrameSink{channel: channel}, nil, &runtimev1.ControlEnvelope{
		Msg: &runtimev1.ControlEnvelope_ReconcileRequest{ReconcileRequest: &runtimev1.ReconcileRequest{ActiveAttemptIds: []int64{5, 9}}},
	})
	frames := stream.snapshot()
	if len(frames) != 2 {
		t.Fatalf("expected proposal + report, got %d frames", len(frames))
	}
	if frames[0].GetResultProposal() == nil || frames[0].GetResultProposal().GetAttemptId() != 9 {
		t.Fatalf("pending result must flush first: %+v", frames[0])
	}
	report := frames[1].GetReconcileReport()
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
	for time.Now().Before(deadline) {
		dispatched, _, _ := tasks.snapshot()
		if dispatched != 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if dispatched, _, _ := tasks.snapshot(); dispatched != 1 {
		t.Fatal("first dispatch never reached the supervisor")
	}
	channel.RegisterTask(11, func() {})
	stream.reset()
	channel.dispatchServerFrame(context.Background(), sink, nil, dispatch)
	if dispatched, _, _ := tasks.snapshot(); dispatched != 1 {
		t.Fatalf("executing attempt re-ran: dispatched=%d", dispatched)
	}
	frames := stream.snapshot()
	if len(frames) != 1 || frames[0].GetAttemptAccept() == nil || frames[0].GetAttemptAccept().GetAttemptId() != 11 {
		t.Fatalf("re-dispatch must re-ack the accept: %+v", frames)
	}
	// An Ack can be lost before the worker goroutine unwinds. A pending
	// terminal result takes priority over the still-present active entry and
	// re-delivers instead of sending AttemptAccept or re-running.
	stream.reset()
	channel.RegisterResult(resultProposal(11, DispatchBinding{BootID: "boot-a", Epoch: 2}))
	stream.reset() // RegisterResult already fired its immediate delivery
	channel.dispatchServerFrame(context.Background(), sink, nil, dispatch)
	if dispatched, _, _ := tasks.snapshot(); dispatched != 1 {
		t.Fatalf("finished attempt re-ran: dispatched=%d", dispatched)
	}
	frames = stream.snapshot()
	if len(frames) != 1 || frames[0].GetResultProposal() == nil {
		t.Fatalf("re-dispatch must re-deliver the pending result: %+v", frames)
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
	for time.Now().Before(deadline) {
		_, _, bindings := tasks.snapshot()
		if len(bindings) != 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	_, _, bindings := tasks.snapshot()
	if len(bindings) != 1 {
		t.Fatal("dispatch never reached the supervisor")
	}
	if bindings[0] != (DispatchBinding{BootID: "boot-frozen", Epoch: 4}) {
		t.Fatalf("binding=%+v", bindings[0])
	}
}

func TestDeliverySurvivesStreamOutage(t *testing.T) {
	channel, stream := newTestChannel(t)
	channel.RegisterResult(resultProposal(3, DispatchBinding{BootID: "boot-a", Epoch: 1}))
	// The stream dies: delivery defers, the entry stays registered.
	stream.setFail(true)
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
	frames := fresh.snapshot()
	if len(frames) != 1 || frames[0].GetResultProposal().GetAttemptId() != 3 {
		t.Fatalf("reconnected delivery missing: %+v", frames)
	}
}

func TestBrowserToolDeliveryRoutesAndAcks(t *testing.T) {
	channel, err := NewChannel(ChannelConfig{Slot: "plinth"})
	if err != nil {
		t.Fatal(err)
	}
	stream := &fakeStream{}
	channel.sendStream = stream
	waiter, err := channel.RegisterBrowserToolResult(41)
	if err != nil {
		t.Fatal(err)
	}
	channel.dispatchServerFrame(context.Background(), &FrameSink{channel: channel}, nil, &runtimev1.ControlEnvelope{
		CorrelationId: 41,
		Msg:           &runtimev1.ControlEnvelope_ToolResultDelivery{ToolResultDelivery: &runtimev1.ToolResultDelivery{ToolCallId: 41, ChildAttemptId: 99, Success: true, Payload: &runtimev1.ResultPayload{SchemaKind: "browser_tool_result_v1", CanonicalJson: []byte(`{"outcome":"success","action":"open"}`)}}},
	})
	select {
	case delivery := <-waiter:
		if delivery.GetChildAttemptId() != 99 || !delivery.GetSuccess() {
			t.Fatalf("delivery=%+v", delivery)
		}
	case <-time.After(time.Second):
		t.Fatal("browser ToolResultDelivery did not reach waiter")
	}
	frames := stream.snapshot()
	if len(frames) != 1 || frames[0].GetToolResultDeliveryAck() == nil || frames[0].GetToolResultDeliveryAck().GetToolCallId() != 41 {
		t.Fatalf("ack=%+v", frames)
	}
}

func TestReleaseBrowserToolResultDropsConsumedCache(t *testing.T) {
	channel, err := NewChannel(ChannelConfig{Slot: "plinth"})
	if err != nil {
		t.Fatal(err)
	}
	channel.deliverBrowserToolResult(&runtimev1.ToolResultDelivery{ToolCallId: 41, ChildAttemptId: 99, Success: true})
	channel.ReleaseBrowserToolResult(41)
	channel.browserMu.Lock()
	defer channel.browserMu.Unlock()
	if channel.browserResults[41] != nil || channel.browserWaiters[41] != nil {
		t.Fatalf("consumed browser delivery remained cached: results=%v waiters=%v", channel.browserResults, channel.browserWaiters)
	}
}

func TestBrowserToolDeliveryRejectsConflictingReplay(t *testing.T) {
	channel, err := NewChannel(ChannelConfig{Slot: "plinth"})
	if err != nil {
		t.Fatal(err)
	}
	stream := &fakeStream{}
	channel.sendStream = stream
	first := &runtimev1.ToolResultDelivery{ToolCallId: 41, ChildAttemptId: 99, Success: true, Payload: &runtimev1.ResultPayload{SchemaKind: "browser_tool_result_v1", CanonicalJson: []byte(`{"action":"open","outcome":"success"}`)}}
	if !channel.deliverBrowserToolResult(first) {
		t.Fatal("first delivery was rejected")
	}
	channel.dispatchServerFrame(context.Background(), &FrameSink{channel: channel}, nil, &runtimev1.ControlEnvelope{CorrelationId: 42, Msg: &runtimev1.ControlEnvelope_ToolResultDelivery{ToolResultDelivery: &runtimev1.ToolResultDelivery{ToolCallId: 41, ChildAttemptId: 99, Success: false}}})
	frames := stream.snapshot()
	if len(frames) != 1 || frames[0].GetToolResultDeliveryAck().GetAccepted() {
		t.Fatalf("conflicting replay was accepted: %+v", frames)
	}
}

func TestBrowserToolDeliveryAfterReleaseDoesNotRebuildCache(t *testing.T) {
	channel, err := NewChannel(ChannelConfig{Slot: "plinth"})
	if err != nil {
		t.Fatal(err)
	}
	waiter, err := channel.RegisterBrowserToolResult(41)
	if err != nil {
		t.Fatal(err)
	}
	first := &runtimev1.ToolResultDelivery{ToolCallId: 41, ChildAttemptId: 99, Success: true}
	if !channel.deliverBrowserToolResult(first) {
		t.Fatal("first delivery rejected")
	}
	<-waiter
	channel.ReleaseBrowserToolResult(41)
	if !channel.deliverBrowserToolResult(first) {
		t.Fatal("late exact replay rejected")
	}
	channel.browserMu.Lock()
	defer channel.browserMu.Unlock()
	if len(channel.browserResults) != 0 || len(channel.browserWaiters) != 0 {
		t.Fatalf("late replay rebuilt released cache: results=%v waiters=%v", channel.browserResults, channel.browserWaiters)
	}
}
