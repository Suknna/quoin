package appinvestigation

// The ui-message-stream endpoint (HTTP-STREAM-001..006): POST fetch over a
// raw Huma StreamResponse, `x-vercel-ai-ui-message-stream: v1` framing
// (assistant-stream 0.3.37 chunk types), one flushed SSE event per frame,
// `data: [DONE]` as the only success terminator. The stream attaches to
// the attempt the send command already created — it never re-creates or
// re-dispatches (queued delivery is the runtime reconcile's job); terminal
// attempts replay the committed authoritative state; transport detach only
// removes the observer; a revoked session closes the stream silently.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Suknna/quoin/internal/quoin/investigation"
	"github.com/danielgtaylor/huma/v2"
)

// replayChunkRunes bounds one replay text-delta (rune-safe split so a
// multi-byte character is never cut inside a frame).
const replayChunkRunes = 2000

// sessionCheckInterval mirrors the alert SSE revocation cadence (Q214):
// the live stream re-checks the session each tick and closes silently when
// it is no longer valid (SEC-SESSION-002/003).
const sessionCheckInterval = 15 * time.Second

// streamSource is the minimal service surface serveStream needs (kept as an
// interface so the session-revocation loop is testable with a fake).
var _ streamSource = (*investigation.Service)(nil)

type streamSource interface {
	TerminalViewFor(ctx context.Context, attemptID int64) (*investigation.TerminalEvent, error)
	Subscribe(ctx context.Context, attemptID int64) (<-chan investigation.StreamEvent, func(), error)
}

func (handler *Handler) streamInvestigationMessage(ctx context.Context, input *struct {
	Session         string `cookie:"__Host-quoin-session"`
	InvestigationID string `path:"investigationId"`
	MessageID       string `path:"messageId"`
	Body            struct {
		// CommandBase is inherited by the frozen StreamRequest even though
		// the stream is a read-only attach (never re-executed); the id is
		// accepted and unused.
		ClientCommandID string `json:"clientCommandId" minLength:"8" maxLength:"128" pattern:"^[A-Za-z0-9_-]+$"`
		Protocol        string `json:"protocol"`
		// The remaining fields are the assistant-ui adapter's non-
		// authoritative client view (HTTP-STREAM-004): accepted and
		// ignored — input is rebuilt from the Quoin head only.
		Messages  []json.RawMessage `json:"messages,omitempty"`
		Tools     json.RawMessage   `json:"tools,omitempty"`
		RunConfig json.RawMessage   `json:"runConfig,omitempty"`
		System    string            `json:"system,omitempty"`
		ParentID  string            `json:"parentId,omitempty"`
		ThreadID  string            `json:"threadId,omitempty"`
	}
}) (*huma.StreamResponse, error) {
	principalID, err := handler.principal(ctx, input.Session)
	if err != nil {
		return nil, err
	}
	_ = principalID
	if input.Body.Protocol != "ui-message-stream" {
		return nil, problem(400, "malformed_request", "协议必须为 ui-message-stream。")
	}
	investigationID, err := strconv.ParseInt(input.InvestigationID, 10, 64)
	if err != nil || investigationID <= 0 {
		return nil, problem(404, "not_found", "调查不存在。")
	}
	messageID, err := strconv.ParseInt(input.MessageID, 10, 64)
	if err != nil || messageID <= 0 {
		return nil, problem(404, "not_found", "消息不存在。")
	}
	attemptID, err := handler.Service.MessageAttempt(ctx, investigationID, messageID)
	if err != nil {
		if errors.Is(err, investigation.ErrNotFound) {
			return nil, problem(404, "not_found", "消息不存在或不是该调查的用户消息。")
		}
		return nil, problem(500, "unavailable", "暂时无法打开回复流，请重试。")
	}
	return &huma.StreamResponse{Body: func(humaCtx huma.Context) {
		humaCtx.SetHeader("Content-Type", "text/event-stream")
		humaCtx.SetHeader("Cache-Control", "no-cache")
		humaCtx.SetHeader("X-Vercel-Ai-Ui-Message-Stream", "v1")
		humaCtx.SetHeader("X-Accel-Buffering", "no")
		writer := humaCtx.BodyWriter()
		stream := newFrameStream(writer)
		streamCtx := humaCtx.Context()
		valid := handler.SessionValid
		if valid == nil {
			valid = func(context.Context, string) bool { return true }
		}
		serveStream(streamCtx, handler.Service, stream, attemptID, input.Session, valid, sessionCheckInterval)
	}}, nil
}

// serveStream drives one attempt's frames until the terminal event, the
// client detaches (HTTP-STREAM-006: detach never cancels the task) or the
// session is revoked (SEC-SESSION-002/003: the established stream must not
// outlive the current authorization — Q214 closes it silently like the
// alert SSE).
func serveStream(ctx context.Context, source streamSource, stream *frameStream, attemptID int64, sessionCookie string, sessionValid func(context.Context, string) bool, checkInterval time.Duration) {
	// Terminal attempts replay the committed authoritative state
	// immediately (HTTP-STREAM-004).
	if view, err := source.TerminalViewFor(ctx, attemptID); err == nil && view != nil {
		stream.replayTerminal(*view)
		return
	}
	channel, release, err := source.Subscribe(ctx, attemptID)
	if err != nil {
		stream.emitError("暂时无法打开回复流，请刷新页面重试。")
		return
	}
	defer release()
	check := time.NewTicker(checkInterval)
	defer check.Stop()
	started := false
	streamed := 0
	var carry utf8Carry
	for {
		select {
		case <-ctx.Done():
			// Transport detach: observer removed, attempt keeps running.
			return
		case <-check.C:
			// Q214: a revoked/expired session closes the live stream
			// silently (SEC-SESSION-002/003); the client's decoder treats
			// the EOF-without-[DONE] as an abrupt stop and falls back to
			// the durable projection.
			if !sessionValid(ctx, sessionCookie) {
				return
			}
		case event, ok := <-channel:
			if !ok {
				return
			}
			if event.Delta != nil {
				text := carry.Append(event.Delta.Text)
				if text == "" {
					continue
				}
				if !started {
					stream.emitTextStart()
					started = true
				}
				stream.emitTextDelta(text)
				streamed++
			}
			if event.Terminal != nil {
				if tail := carry.Flush(); tail != "" {
					if !started {
						stream.emitTextStart()
						started = true
					}
					stream.emitTextDelta(tail)
				}
				if event.Terminal.State == "Succeeded" {
					// A subscriber attached after the commit race still
					// receives the committed content exactly once.
					if streamed == 0 && event.Terminal.Content != "" {
						if !started {
							stream.emitTextStart()
						}
						for _, chunk := range chunkRunes(event.Terminal.Content, replayChunkRunes) {
							stream.emitTextDelta(chunk)
						}
					}
					stream.emitTextEnd()
					stream.emitFinish(event.Terminal.InputTokens, event.Terminal.OutputTokens)
				} else {
					stream.emitError(failureText(*event.Terminal))
				}
				stream.emitDone()
				return
			}
		}
	}
}

// frameStream writes the frozen SSE framing: one JSON chunk per
// `data: ` line, flushed per event (HTTP-STREAM-001).
type frameStream struct {
	writer io.Writer
}

func newFrameStream(writer io.Writer) *frameStream {
	return &frameStream{writer: writer}
}

func (stream *frameStream) write(payload string) error {
	if _, err := io.WriteString(stream.writer, "data: "+payload+"\n\n"); err != nil {
		return err
	}
	if flusher, ok := stream.writer.(http.Flusher); ok {
		flusher.Flush()
	}
	return nil
}

func (stream *frameStream) emitTextStart() {
	_ = stream.write(`{"type":"text-start","id":"text-1"}`)
}

func (stream *frameStream) emitTextEnd() {
	_ = stream.write(`{"type":"text-end"}`)
}

func (stream *frameStream) emitTextDelta(text string) {
	encoded, _ := json.Marshal(struct {
		Type      string `json:"type"`
		TextDelta string `json:"textDelta"`
	}{Type: "text-delta", TextDelta: text})
	_ = stream.write(string(encoded))
}

func (stream *frameStream) emitFinish(inputTokens, outputTokens int64) {
	encoded, _ := json.Marshal(struct {
		Type         string `json:"type"`
		FinishReason string `json:"finishReason"`
		Usage        struct {
			InputTokens  int64 `json:"inputTokens"`
			OutputTokens int64 `json:"outputTokens"`
		} `json:"usage"`
	}{Type: "finish", FinishReason: "stop", Usage: struct {
		InputTokens  int64 `json:"inputTokens"`
		OutputTokens int64 `json:"outputTokens"`
	}{InputTokens: inputTokens, OutputTokens: outputTokens}})
	_ = stream.write(string(encoded))
}

func (stream *frameStream) emitError(message string) {
	encoded, _ := json.Marshal(struct {
		Type      string `json:"type"`
		ErrorText string `json:"errorText"`
	}{Type: "error", ErrorText: message})
	_ = stream.write(string(encoded))
}

func (stream *frameStream) emitDone() {
	_ = stream.write("[DONE]")
}

// replayTerminal replays a terminal attempt's authoritative frames
// (HTTP-STREAM-004: completed/failed/cancelled, never re-executed).
func (stream *frameStream) replayTerminal(view investigation.TerminalEvent) {
	if view.State == "Succeeded" {
		stream.emitTextStart()
		for _, chunk := range chunkRunes(view.Content, replayChunkRunes) {
			stream.emitTextDelta(chunk)
		}
		stream.emitTextEnd()
		stream.emitFinish(view.InputTokens, view.OutputTokens)
	} else {
		stream.emitError(failureText(view))
	}
	stream.emitDone()
}

// failureText renders the frozen termination reasons in ordinary language
// (UI-ERROR-004: impact and recovery first; the raw enum stays a
// diagnostic detail only).
func failureText(view investigation.TerminalEvent) string {
	reason := "该轮回复未能生成，你可以重新发送这条消息。"
	switch view.TerminationReason {
	case "timeout":
		reason = "模型调用超时，该轮回复未能生成。请稍后重试。"
	case "rate_limited":
		reason = "模型调用被限流，该轮回复未能生成。请稍后重试。"
	case "provider_unavailable":
		reason = "模型供应商暂时不可用，该轮回复未能生成。请稍后重试。"
	case "context_too_large":
		reason = "上下文超出模型容量，该轮回复未能生成。请缩短消息后重试。"
	case "invalid_response":
		reason = "模型返回无法解析，该轮回复未能生成。请重试。"
	case "cancelled":
		reason = "回复已停止。"
	case "lease_expired", "replaced", "revoked":
		reason = "运行组件连接中断，该轮回复未能生成。请重试。"
	case "sandbox_unavailable", "worker_protocol_error":
		reason = "执行环境异常，该轮回复未能生成。请重试。"
	}
	return reason
}

// chunkRunes splits text into rune-safe chunks (a multi-byte character is
// never cut inside a frame).
func chunkRunes(text string, size int) []string {
	if text == "" {
		return nil
	}
	runes := []rune(text)
	var chunks []string
	for start := 0; start < len(runes); start += size {
		end := start + size
		if end > len(runes) {
			end = len(runes)
		}
		chunks = append(chunks, string(runes[start:end]))
	}
	return chunks
}

// utf8Carry re-assembles visible deltas across stream frames so a
// multi-byte rune split between two deltas is emitted whole: the frozen
// framing never carries invalid UTF-8 inside a JSON line.
type utf8Carry struct {
	pending string
}

func (carry *utf8Carry) Append(text string) string {
	if carry.pending == "" && utf8.ValidString(text) {
		return text
	}
	combined := carry.pending + text
	carry.pending = ""
	validEnd := len(combined)
	for validEnd > 0 && !utf8.ValidString(combined[:validEnd]) {
		validEnd--
	}
	if validEnd == 0 {
		// The head itself starts mid-rune: keep up to one rune's bytes as
		// the salvageable split and emit nothing yet.
		if len(combined) > 3 {
			combined = combined[:3]
		}
		carry.pending = combined
		return ""
	}
	tail := combined[validEnd:]
	if len(tail) > 3 {
		// Not a salvageable split: emit the valid prefix and replace the
		// invalid bytes with replacement runes (never raw garbage).
		return combined[:validEnd] + strings.Repeat("\uFFFD", len(tail))
	}
	carry.pending = tail
	return combined[:validEnd]
}

// Flush emits whatever partial tail remains at terminal time; a leftover
// tail is invalid UTF-8 by construction (well-formed deltas never leave
// one), so each byte becomes one replacement rune.
func (carry *utf8Carry) Flush() string {
	tail := carry.pending
	carry.pending = ""
	if tail == "" {
		return ""
	}
	return strings.Repeat("\uFFFD", len(tail))
}
