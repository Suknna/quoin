package worker

// Framed stdio protocol (ARCH-WORKER-004/005): 4-byte unsigned big-endian
// payload length + protobuf payload on stdout; stdin carries the mirrored
// direction. Half frames, oversized frames, decode failures and protocol
// order violations terminate the worker; stdout carries nothing but the
// protocol and stderr carries bounded diagnostics only.

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	workerv1 "github.com/Suknna/quoin/internal/gen/proto/plinth/worker/v1"
	"google.golang.org/protobuf/proto"
)

// maxFrameBytes bounds one frame (ARCH-WORKER-005).
const maxFrameBytes = 4 << 20

// ErrProtocol reports a framing or ordering violation that terminates the
// worker (ARCH-WORKER-004).
var ErrProtocol = errors.New("worker protocol violation")

// frameReader streams length-delimited protobuf frames.
type FrameReader struct {
	reader  *bufio.Reader
	nextSeq uint64 // per-direction monotonic sequence (1-based)
}

func NewFrameReader(reader io.Reader) *FrameReader {
	return &FrameReader{reader: bufio.NewReaderSize(reader, 64*1024)}
}

// read returns the next envelope after enforcing framing and sequence
// rules (unknown oneofs and duplicate/regressing message ids terminate).
func (reader *FrameReader) Read() (*workerv1.WorkerEnvelope, error) {
	var prefix [4]byte
	if _, err := io.ReadFull(reader.reader, prefix[:]); err != nil {
		return nil, err
	}
	length := binary.BigEndian.Uint32(prefix[:])
	if length == 0 || length > maxFrameBytes {
		return nil, fmt.Errorf("%w: frame length %d out of bounds", ErrProtocol, length)
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(reader.reader, body); err != nil {
		return nil, fmt.Errorf("%w: truncated frame", ErrProtocol)
	}
	var envelope workerv1.WorkerEnvelope
	if err := proto.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("%w: protobuf decode failed: %v", ErrProtocol, err)
	}
	if envelope.GetMessageId() == 0 {
		return nil, fmt.Errorf("%w: message_id must start at 1", ErrProtocol)
	}
	reader.nextSeq++
	if envelope.GetMessageId() != reader.nextSeq {
		return nil, fmt.Errorf("%w: message_id %d out of sequence (want %d)", ErrProtocol, envelope.GetMessageId(), reader.nextSeq)
	}
	return &envelope, nil
}

// frameWriter writes length-delimited envelopes with its own monotonic
// sequence (the caller sets the payload).
type FrameWriter struct {
	writer  *bufio.Writer
	nextSeq uint64
}

func NewFrameWriter(writer io.Writer) *FrameWriter {
	return &FrameWriter{writer: bufio.NewWriterSize(writer, 64*1024)}
}

// send stamps message_id and flushes one envelope.
func (writer *FrameWriter) Send(envelope *workerv1.WorkerEnvelope) error {
	writer.nextSeq++
	envelope.MessageId = writer.nextSeq
	body, err := proto.Marshal(envelope)
	if err != nil {
		return err
	}
	if len(body) > maxFrameBytes {
		return fmt.Errorf("%w: outbound frame too large", ErrProtocol)
	}
	var prefix [4]byte
	binary.BigEndian.PutUint32(prefix[:], uint32(len(body)))
	if _, err := writer.writer.Write(prefix[:]); err != nil {
		return err
	}
	if _, err := writer.writer.Write(body); err != nil {
		return err
	}
	return writer.writer.Flush()
}
