package worker

import (
	"bytes"
	"encoding/binary"
	"testing"

	workerv1 "github.com/Suknna/quoin/internal/gen/proto/plinth/worker/v1"
	"google.golang.org/protobuf/proto"
)

func TestFrameRoundTrip(t *testing.T) {
	var buffer bytes.Buffer
	writer := NewFrameWriter(&buffer)
	reader := NewFrameReader(&buffer)
	for index := 1; index <= 3; index++ {
		if err := writer.Send(&workerv1.WorkerEnvelope{
			AttemptId: 7,
			Msg:       &workerv1.WorkerEnvelope_WorkerLog{WorkerLog: &workerv1.WorkerLog{Level: "info", Message: "frame"}},
		}); err != nil {
			t.Fatal(err)
		}
		envelope, err := reader.Read()
		if err != nil {
			t.Fatal(err)
		}
		if envelope.GetAttemptId() != 7 || envelope.GetMessageId() != uint64(index) {
			t.Fatalf("envelope=%v", envelope)
		}
	}
}

func TestFrameSequenceViolationTerminates(t *testing.T) {
	var buffer bytes.Buffer
	envelope := &workerv1.WorkerEnvelope{MessageId: 5, AttemptId: 1, Msg: &workerv1.WorkerEnvelope_WorkerLog{WorkerLog: &workerv1.WorkerLog{Level: "info"}}}
	body, _ := proto.Marshal(envelope)
	var prefix [4]byte
	binary.BigEndian.PutUint32(prefix[:], uint32(len(body)))
	buffer.Write(prefix[:])
	buffer.Write(body)
	if _, err := NewFrameReader(&buffer).Read(); err == nil {
		t.Fatal("regressing message_id must be a protocol violation")
	}
}

func TestFrameOversizedRejected(t *testing.T) {
	var buffer bytes.Buffer
	var prefix [4]byte
	binary.BigEndian.PutUint32(prefix[:], maxFrameBytes+1)
	buffer.Write(prefix[:])
	if _, err := NewFrameReader(&buffer).Read(); err == nil {
		t.Fatal("oversized frame must be rejected")
	}
}

func TestFrameTruncatedRejected(t *testing.T) {
	var buffer bytes.Buffer
	var prefix [4]byte
	binary.BigEndian.PutUint32(prefix[:], 16)
	buffer.Write(prefix[:])
	buffer.Write([]byte("short"))
	if _, err := NewFrameReader(&buffer).Read(); err == nil {
		t.Fatal("truncated frame must be rejected")
	}
}
