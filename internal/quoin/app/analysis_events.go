package app

// Task snapshot and task SSE surface (T10, HTTP-SSE-006..009): the active
// task projection plus the derived task_change_log replay stream. Both are
// read-only; the change log is trigger-derived from the frozen schema and
// can be dropped and rebuilt without touching authority rows.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	sharedops "github.com/Suknna/quoin/internal/ops"
	"github.com/Suknna/quoin/internal/quoin/analysis"
	"github.com/danielgtaylor/huma/v2"
)

// cursorToken encodes the keyset cursor (plain base64url of the last id;
// the analysis list cursors carry no snapshot binding because terminal
// histories are immutable).
func cursorToken(after int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(after, 10)))
}

// decodeCursorToken reverses cursorToken.
func decodeCursorToken(token string) (int64, error) {
	body, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return 0, fmt.Errorf("malformed cursor")
	}
	id, err := strconv.ParseInt(string(body), 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("malformed cursor")
	}
	return id, nil
}

func (application *apiServer) registerTaskSnapshot(api huma.API) {
	huma.Register(api, huma.Operation{Method: "GET", Path: "/api/v1/tasks/snapshot", OperationID: "getTaskSnapshot"}, application.getTaskSnapshot)
}

func (application *apiServer) getTaskSnapshot(ctx context.Context, input *struct {
	Session string `cookie:"__Host-quoin-session"`
	Cursor  string `query:"cursor"`
	Limit   int    `query:"limit" minimum:"1" maximum:"200" default:"50"`
}) (*struct {
	Body analysis.TaskSnapshot `json:"body"`
}, error) {
	if _, err := application.authenticateFull(ctx, input.Session, "读取后台任务"); err != nil {
		return nil, err
	}
	highWater, _, err := application.analyses.TaskWatermarks(ctx)
	if err != nil {
		return nil, huma.Error500InternalServerError("暂时无法读取后台任务", err)
	}
	items, err := application.analyses.ActiveTaskSnapshot(ctx)
	if err != nil {
		return nil, huma.Error500InternalServerError("暂时无法读取后台任务", err)
	}
	snapshot := analysis.TaskSnapshot{SnapshotSeq: highWater, Items: items}
	return &struct {
		Body analysis.TaskSnapshot `json:"body"`
	}{Body: snapshot}, nil
}

// taskEventStream owns the /api/v1/tasks/events SSE surface (HTTP-SSE-006).
// Poll and heartbeat intervals are fields so deterministic tests can
// shorten them; production uses the defaults.
type taskEventStream struct {
	application      *apiServer
	pollInterval     time.Duration
	heartbeatEvery   time.Duration
	replayBatchLimit int
}

func newTaskEventStream(application *apiServer) *taskEventStream {
	return &taskEventStream{application: application, pollInterval: 300 * time.Millisecond, heartbeatEvery: 15 * time.Second, replayBatchLimit: 500}
}

// registerTaskStream mounts the raw SSE route on the public mux (the
// response is a long-lived event-stream framed and flushed by hand; the
// route and method match the frozen streamTaskEvents operation).
func (application *apiServer) registerTaskStream(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/tasks/events", newTaskEventStream(application).serve)
}

// taskChangeEventJSON mirrors the frozen TaskChangeEvent payload field
// order: {"seq":"…","objectType":"…","objectId":"…","changeType":"…","rowVersion":…}.
type taskChangeEventJSON struct {
	Seq        string `json:"seq"`
	ObjectType string `json:"objectType"`
	ObjectID   string `json:"objectId"`
	ChangeType string `json:"changeType"`
	RowVersion int64  `json:"rowVersion"`
}

func (stream *taskEventStream) serve(writer http.ResponseWriter, request *http.Request) {
	// Last-Event-ID (native EventSource reconnect) wins over after when
	// present (HTTP-SSE-003).
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
	service := stream.application.analyses
	highWater, oldest, err := service.TaskWatermarks(request.Context())
	if err != nil {
		writeStreamProblem(writer, http.StatusInternalServerError, "暂时无法读取任务变更流")
		return
	}
	// Cursor expired before the response head is written → 410 problem+json
	// (HTTP-SSE-002/003).
	if analysis.TaskCursorExpired(cursor, highWater, oldest) {
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
		// Session revocation closes the live stream (Q214).
		if _, err := stream.application.auth.Authenticate(request.Context(), sessionCookie); err != nil {
			return
		}
		// Expiry check runs BEFORE replay on every tick (HTTP-SSE-002).
		highWater, oldest, err = service.TaskWatermarks(request.Context())
		if err != nil {
			sharedops.LogEvent("quoin", "error", "analysis.task_stream_read_failed", err.Error())
			return
		}
		if analysis.TaskCursorExpired(cursor, highWater, oldest) {
			fmt.Fprint(writer, "event: resync_required\ndata: {\"type\":\"resync_required\"}\n\n")
			if canFlush {
				flusher.Flush()
			}
			return
		}
		changes, err := service.TaskChangesAfter(request.Context(), cursor, stream.replayBatchLimit)
		if err != nil {
			sharedops.LogEvent("quoin", "error", "analysis.task_stream_read_failed", err.Error())
			return
		}
		for _, change := range changes {
			payload, marshalErr := json.Marshal(taskChangeEventJSON{
				Seq: strconv.FormatInt(change.Seq, 10), ObjectType: change.ObjectType,
				ObjectID: strconv.FormatInt(change.ObjectID, 10), ChangeType: change.ChangeType,
				RowVersion: change.RowVersion,
			})
			if marshalErr != nil {
				return
			}
			fmt.Fprintf(writer, "event: change\nid: %d\ndata: %s\n\n", change.Seq, payload)
			cursor = change.Seq
		}
		if len(changes) > 0 && canFlush {
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
