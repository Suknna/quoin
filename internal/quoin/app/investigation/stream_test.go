package appinvestigation

// Exact framing tests (HTTP-STREAM-001/002): the frame sequence the
// stream endpoint emits, the rune-boundary-safe replay chunking and the
// UTF-8 carry that re-assembles a multi-byte rune split across deltas.

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/quoin/investigation"
)

func TestFrameSequence(t *testing.T) {
	var buffer bytes.Buffer
	stream := newFrameStream(&buffer)
	stream.emitTextStart()
	stream.emitTextDelta("调查结论：")
	stream.emitTextDelta("（fixture-proof）")
	stream.emitFinish(12, 4)
	stream.emitDone()
	expected := "data: {\"type\":\"text-start\",\"id\":\"text-1\"}\n\n" +
		"data: {\"type\":\"text-delta\",\"textDelta\":\"调查结论：\"}\n\n" +
		"data: {\"type\":\"text-delta\",\"textDelta\":\"（fixture-proof）\"}\n\n" +
		"data: {\"type\":\"finish\",\"finishReason\":\"stop\",\"usage\":{\"inputTokens\":12,\"outputTokens\":4}}\n\n" +
		"data: [DONE]\n\n"
	if buffer.String() != expected {
		t.Fatalf("frame sequence diverged:\n%s\nwant:\n%s", buffer.String(), expected)
	}
}

func TestErrorFrameSequence(t *testing.T) {
	var buffer bytes.Buffer
	stream := newFrameStream(&buffer)
	stream.emitError("模型供应商暂时不可用，该轮回复未能生成。请稍后重试。")
	stream.emitDone()
	body := buffer.String()
	// The producer emits deterministic key order (struct marshaling); the
	// decoder parses the frame, so assert the parsed shape plus [DONE].
	lines := strings.Split(strings.TrimSpace(body), "\n")
	var frames []string
	for _, line := range lines {
		if line != "" {
			frames = append(frames, line)
		}
	}
	if len(frames) != 2 {
		t.Fatalf("error sequence must be one frame + [DONE]: %q", body)
	}
	var frame struct {
		Type      string `json:"type"`
		ErrorText string `json:"errorText"`
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(frames[0], "data: ")), &frame); err != nil {
		t.Fatalf("error frame unparseable: %v", err)
	}
	if frame.Type != "error" || !strings.Contains(frame.ErrorText, "模型供应商暂时不可用") {
		t.Fatalf("error frame wrong: %+v", frame)
	}
	if frames[1] != "data: [DONE]" {
		t.Fatalf("missing [DONE] terminator: %q", body)
	}
}

func TestUTF8CarrySplits(t *testing.T) {
	var carry utf8Carry
	// 中 is E4 B8 AD: split after the first byte.
	first := carry.Append("\xE4")
	if first != "" {
		t.Fatalf("incomplete rune must be held back, got %q", first)
	}
	second := carry.Append("\xB8\xAD后续")
	if second != "中后续" {
		t.Fatalf("reassembled=%q want 中后续", second)
	}
	// Well-formed deltas pass through untouched.
	if got := carry.Append("正常文本"); got != "正常文本" {
		t.Fatalf("pass-through=%q", got)
	}
	if tail := carry.Flush(); tail != "" {
		t.Fatalf("flush with empty carry=%q", tail)
	}
	// An invalid trailing tail flushes as replacement runes, never raw
	// garbage bytes (the frozen framing never carries invalid UTF-8).
	if got := carry.Append("abc\xFF"); got != "abc" {
		t.Fatalf("invalid tail split=%q want abc", got)
	}
	if tail := carry.Flush(); tail != "\uFFFD" {
		t.Fatalf("invalid tail flush=%q want U+FFFD", tail)
	}
}

func TestChunkRunesNeverSplits(t *testing.T) {
	chunks := chunkRunes("一二三四五", 2)
	if len(chunks) != 3 || chunks[0] != "一二" || chunks[1] != "三四" || chunks[2] != "五" {
		t.Fatalf("chunks=%v", chunks)
	}
	for _, chunk := range chunks {
		if !utf8Valid(chunk) {
			t.Fatalf("chunk %q is not valid UTF-8", chunk)
		}
	}
}

// fakeStreamSource is the testable stream source: one buffered delta then
// a never-closing channel (the revocation, not the feed, ends the stream).
type fakeStreamSource struct {
	events chan investigation.StreamEvent
}

func (fake fakeStreamSource) TerminalViewFor(context.Context, int64) (*investigation.TerminalEvent, error) {
	return nil, nil
}

func (fake fakeStreamSource) Subscribe(context.Context, int64) (<-chan investigation.StreamEvent, func(), error) {
	return fake.events, func() {}, nil
}

// TestSessionRevocationClosesStream pins SEC-SESSION-002/003 (Q214): a
// revoked session closes the established stream silently — the frames seen
// so far stand, no [DONE] is emitted, and the release runs.
func TestSessionRevocationClosesStream(t *testing.T) {
	var buffer bytes.Buffer
	stream := newFrameStream(&buffer)
	events := make(chan investigation.StreamEvent, 4)
	events <- investigation.StreamEvent{Delta: &investigation.Delta{Text: "已送达的部分"}}
	done := make(chan struct{})
	go func() {
		serveStream(context.Background(), fakeStreamSource{events: events}, stream, 1, "cookie",
			func(context.Context, string) bool { return false }, 10*time.Millisecond)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not close after session revocation")
	}
	body := buffer.String()
	if !strings.Contains(body, "已送达的部分") {
		t.Fatalf("buffered delta missing: %q", body)
	}
	if strings.Contains(body, "[DONE]") {
		t.Fatalf("a revoked stream must not emit [DONE]: %q", body)
	}
}

func utf8Valid(value string) bool {
	return strings.ToValidUTF8(value, "") == value
}
