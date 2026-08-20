package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	sharedops "github.com/Suknna/quoin/internal/ops"
	"github.com/Suknna/quoin/internal/quoin/alerts"
	"github.com/danielgtaylor/huma/v2"
)

// alertEventStream owns the /api/v1/alerts/events SSE surface (HTTP-SSE-002).
// Poll and heartbeat intervals are fields so deterministic tests can shorten
// them; production uses the defaults.
type alertEventStream struct {
	application      *apiServer
	pollInterval     time.Duration
	heartbeatEvery   time.Duration
	replayBatchLimit int
}

func newAlertEventStream(application *apiServer) *alertEventStream {
	return &alertEventStream{application: application, pollInterval: 300 * time.Millisecond, heartbeatEvery: 15 * time.Second, replayBatchLimit: 500}
}

// registerAlertStream mounts the raw SSE route on the public mux. It is
// registered directly (not via huma.Register) because the response is a
// long-lived event-stream the handler must frame and flush itself; the route
// and method match the frozen OpenAPI operation streamAlertEvents.
func (application *apiServer) registerAlertStream(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/alerts/events", newAlertEventStream(application).serve)
}

// alertChangeEventJSON mirrors the frozen AlertChangeEvent payload field
// order: {"seq":"…","type":"…","occurrenceId":"…","rowVersion":…}.
type alertChangeEventJSON struct {
	Seq          string `json:"seq"`
	Type         string `json:"type"`
	OccurrenceID string `json:"occurrenceId"`
	RowVersion   int64  `json:"rowVersion"`
}

func (stream *alertEventStream) serve(writer http.ResponseWriter, request *http.Request) {
	// Last-Event-ID (native EventSource reconnect) wins over the after
	// query parameter when present (HTTP-SSE-003).
	cursorText := request.Header.Get("Last-Event-ID")
	if cursorText == "" {
		cursorText = request.URL.Query().Get("after")
	}
	cursor, err := strconv.ParseInt(cursorText, 10, 64)
	if err != nil || cursor < 0 {
		writeStreamProblem(writer, http.StatusBadRequest, "after 必须是十进制 change_seq")
		return
	}
	sessionCookie, ok := findSessionCookie(request)
	if !ok {
		writeStreamProblem(writer, http.StatusUnauthorized, "请重新登录")
		return
	}
	if _, err := stream.application.auth.Authenticate(request.Context(), sessionCookie); err != nil {
		writeStreamProblem(writer, http.StatusUnauthorized, "请重新登录")
		return
	}

	service := stream.application.alerts
	highWater, oldest, err := service.Watermarks(request.Context())
	if err != nil {
		writeStreamProblem(writer, http.StatusInternalServerError, "暂时无法读取告警变更流")
		return
	}
	// Cursor already expired before the response head is written → 410
	// problem+json, no stream (HTTP-SSE-002/003).
	if alerts.CursorExpired(cursor, highWater, oldest) {
		writeStreamProblem(writer, http.StatusGone, "游标已过期，请重新读取完整快照")
		return
	}

	flusher, canFlush := writer.(http.Flusher)
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Cache-Control", "no-cache")
	writer.Header().Set("X-Accel-Buffering", "no")
	writer.WriteHeader(http.StatusOK)
	if canFlush {
		flusher.Flush()
	}

	poll := time.NewTicker(stream.pollInterval)
	defer poll.Stop()
	lastHeartbeat := time.Now()
	for {
		if request.Context().Err() != nil {
			return
		}
		// Q214: revocation takes effect on the live stream — re-check the
		// session each poll tick and close silently when it is no longer
		// valid (the client's EventSource surfaces this as an error).
		if _, err := stream.application.auth.Authenticate(request.Context(), sessionCookie); err != nil {
			return
		}
		// Expiry check runs BEFORE replay on every tick: a cursor that fell
		// behind the retention window (in-stream boundary) must resync, never
		// silently skip GC'd rows (HTTP-SSE-002/DATA-SSE-009).
		highWater, oldest, err = service.Watermarks(request.Context())
		if err != nil {
			return
		}
		if alerts.CursorExpired(cursor, highWater, oldest) {
			fmt.Fprint(writer, "event: resync_required\ndata: {\"type\":\"resync_required\"}\n\n")
			if canFlush {
				flusher.Flush()
			}
			return
		}
		events, err := service.ChangesAfter(request.Context(), cursor, stream.replayBatchLimit)
		if err != nil {
			sharedops.LogEvent("quoin", "error", "alert.stream_read_failed", err.Error())
			return
		}
		for _, event := range events {
			payload, marshalErr := json.Marshal(alertChangeEventJSON{
				Seq: strconv.FormatInt(event.Seq, 10), Type: event.ChangeType,
				OccurrenceID: strconv.FormatInt(event.OccurrenceID, 10), RowVersion: event.RowVersion,
			})
			if marshalErr != nil {
				return
			}
			fmt.Fprintf(writer, "event: change\nid: %d\ndata: %s\n\n", event.Seq, payload)
			cursor = event.Seq
		}
		if len(events) > 0 && canFlush {
			flusher.Flush()
		}
		if time.Since(lastHeartbeat) >= stream.heartbeatEvery {
			// Heartbeats are pure transport comments (HTTP-SSE-005).
			fmt.Fprint(writer, ": heartbeat\n\n")
			if canFlush {
				flusher.Flush()
			}
			lastHeartbeat = time.Now()
		}
		select {
		case <-request.Context().Done():
			return
		case <-poll.C:
		}
	}
}

// writeStreamProblem emits the same RFC 9457 shape huma handlers produce so
// SSE error responses match every other endpoint's contract.
func writeStreamProblem(writer http.ResponseWriter, status int, detail string) {
	problem := huma.ErrorModel{Type: "about:blank", Title: http.StatusText(status), Status: status, Detail: detail}
	body, marshalErr := json.Marshal(problem)
	if marshalErr != nil {
		http.Error(writer, detail, status)
		return
	}
	writer.Header().Set("Content-Type", "application/problem+json")
	writer.WriteHeader(status)
	_, _ = writer.Write(append(body, '\n'))
}
