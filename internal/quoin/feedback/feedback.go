package feedback

// Diagnosis feedback ledger (T27, DATA-KNOWLEDGE-008): an append-only
// timeline bound to exactly the three immutable diagnosis outputs —
// Initial Analysis outputs, Inspection Report versions and assistant
// Investigation messages. Every event carries a closed value and an
// optional bounded note; the read side returns the full paginated history
// plus a latestValue projection derived from the last committed event.
// A `rejected` event applies DATA-TX-011 in the same transaction: the
// source's still-operable candidates become SourceInvalid and every
// KnowledgeVersion produced from that source exits retrieval (with the
// FTS5 projection row deleted by the schema trigger).

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/knowledge/invalidation"
)

// Target types (diagnosis_feedback.target_type CHECK).
const (
	TargetAnalysisOutput = "initial_analysis_output"
	TargetReport         = "inspection_report"
	TargetMessage        = "investigation_message"
)

// Feedback values (diagnosis_feedback.value CHECK).
const (
	ValueAdopted           = "adopted"
	ValueExecuted          = "executed"
	ValueVerifiedEffective = "verified_effective"
	ValueRejected          = "rejected"
)

const noteLimit = 4096

var (
	// ErrNotFound maps to 404: the target object does not exist.
	ErrNotFound = errors.New("feedback target not found")
	// ErrInvalidTarget maps to 422: the shape is not one of the three
	// immutable diagnosis outputs (for example a user message).
	ErrInvalidTarget = errors.New("feedback target must be an immutable diagnosis output or assistant message")
	// ErrInvalidValue maps to 422: a value outside the closed set.
	ErrInvalidValue = errors.New("feedback value is not one of the closed set")
	// ErrCommandReused maps to 409 command_id_reused (HTTP-COMMAND-003).
	ErrCommandReused = errors.New("client command id reused with a different request")
	// ErrNoteTooLong maps to 422.
	ErrNoteTooLong = errors.New("feedback note exceeds 4096 characters")
)

// Target is one immutable diagnosis output.
type Target struct {
	Type string
	ID   int64
}

// Event is one append-only feedback record (FeedbackSummary). Locator ids
// serialize as the frozen string LocatorId shape.
type Event struct {
	ID         string `json:"id"`
	TargetType string `json:"targetType"`
	TargetID   string `json:"targetId"`
	Value      string `json:"value"`
	Note       string `json:"note,omitempty"`
	CreatedBy  string `json:"createdBy,omitempty"`
	CreatedAt  string `json:"createdAt"`
}

// Cursor is the keyset cursor for the timeline (createdAt DESC, id DESC).
type Cursor struct {
	CreatedAt string
	ID        int64
}

// Timeline is the read projection: full history page plus the latestValue
// derived from the last committed event (never a second write source).
type Timeline struct {
	LatestValue string
	Items       []Event
	Next        *Cursor
}

// Service owns the diagnosis feedback ledger.
type Service struct {
	db  *sql.DB
	now func() time.Time
}

// NewService builds the ledger over the shared database handle.
func NewService(db *sql.DB) *Service {
	return &Service{db: db, now: time.Now}
}

func (service *Service) nowText() string {
	return service.now().UTC().Format(time.RFC3339Nano)
}

func validTargetType(targetType string) bool {
	switch targetType {
	case TargetAnalysisOutput, TargetReport, TargetMessage:
		return true
	}
	return false
}

func validValue(value string) bool {
	switch value {
	case ValueAdopted, ValueExecuted, ValueVerifiedEffective, ValueRejected:
		return true
	}
	return false
}

// targetExists resolves the closed target shape: "missing" (404) for an
// absent row and "invalid" (422) for a user message (DATA-KNOWLEDGE-008).
func targetExists(ctx context.Context, conn *sql.Conn, target Target) (string, error) {
	switch target.Type {
	case TargetAnalysisOutput:
		found, err := exists(ctx, conn, `SELECT 1 FROM initial_analysis_outputs WHERE id=?`, target.ID)
		return presence(found), err
	case TargetReport:
		found, err := exists(ctx, conn, `SELECT 1 FROM inspection_reports WHERE id=?`, target.ID)
		return presence(found), err
	case TargetMessage:
		row := conn.QueryRowContext(ctx, `SELECT role FROM investigation_messages WHERE id=?`, target.ID)
		var role string
		if err := row.Scan(&role); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return "missing", nil
			}
			return "", err
		}
		if role != "assistant" {
			return "invalid", nil
		}
		return "present", nil
	}
	return "invalid", nil
}

func presence(found bool) string {
	if found {
		return "present"
	}
	return "missing"
}

func exists(ctx context.Context, conn *sql.Conn, query string, id int64) (bool, error) {
	row := conn.QueryRowContext(ctx, query, id)
	var one int
	if err := row.Scan(&one); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// Append adds one event to the immutable timeline (HTTP-COMMAND-003
// replay through the durable command ledger). A rejected event applies
// the DATA-TX-011 source invalidation inside the same transaction.
func (service *Service) Append(ctx context.Context, principalID int64, commandID string, target Target, value, note string) (Event, error) {
	if !validTargetType(target.Type) {
		return Event{}, ErrInvalidTarget
	}
	if !validValue(value) {
		return Event{}, ErrInvalidValue
	}
	if utf8.RuneCountInString(note) > noteLimit {
		return Event{}, ErrNoteTooLong
	}
	digest := auth.DigestCommand("diagnosis_feedback.append", map[string]any{
		"targetType": target.Type,
		"targetId":   target.ID,
		"value":      value,
		"note":       note,
	})
	if record, ok, err := auth.LookupCommand(ctx, service.db, principalID, commandID); err != nil {
		return Event{}, err
	} else if ok {
		return service.replay(ctx, record, digest)
	}
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return Event{}, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return Event{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	// The serialized-writer recheck makes a concurrent same-key replay
	// deterministic (HTTP-COMMAND-007). The lookup must stay on this
	// connection: the pool has other waiters during the open transaction.
	if record, ok, err := auth.LookupCommandOn(ctx, conn, principalID, commandID); err != nil {
		return Event{}, err
	} else if ok {
		event, replayErr := replayRecord(ctx, conn, record, digest)
		if replayErr == nil {
			if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
				return Event{}, err
			}
			committed = true
		}
		return event, replayErr
	}
	state, err := targetExists(ctx, conn, target)
	if err != nil {
		return Event{}, err
	}
	if state != "present" {
		if err := rejectCommand(ctx, conn, principalID, commandID, digest, target, value); err != nil {
			return Event{}, err
		}
		if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
			return Event{}, err
		}
		committed = true
		if state == "invalid" {
			return Event{}, ErrInvalidTarget
		}
		return Event{}, ErrNotFound
	}
	now := service.nowText()
	result, err := conn.ExecContext(ctx, `INSERT INTO diagnosis_feedback(target_type,target_id,value,note,created_by,created_at) VALUES(?,?,?,?,?,?)`,
		target.Type, target.ID, value, nullableNote(note), principalID, now)
	if err != nil {
		return Event{}, err
	}
	eventID, err := result.LastInsertId()
	if err != nil {
		return Event{}, err
	}
	if value == ValueRejected {
		if err := service.invalidateSource(ctx, conn, target, now); err != nil {
			return Event{}, err
		}
	}
	if err := recordAudit(ctx, conn, principalID, "diagnosis_feedback.append", target.Type, target.ID, now); err != nil {
		return Event{}, err
	}
	// The durable ledger carries the original result payload so a replay
	// returns the committed outcome even after later appends (HTTP-COMMAND-003).
	var event Event
	row := conn.QueryRowContext(ctx, `
		SELECT id,target_type,target_id,value,COALESCE(note,''),COALESCE(created_by,0),created_at
		FROM diagnosis_feedback WHERE id=?`, eventID)
	if event, err = scanEvent(row.Scan); err != nil {
		return Event{}, err
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return Event{}, err
	}
	if err := auth.RecordCommand(ctx, conn, principalID, commandID, "diagnosis_feedback.append", digest, auth.OutcomeCommitted, "diagnosis_feedback", eventID, string(payload)); err != nil {
		return Event{}, err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return Event{}, err
	}
	committed = true
	return event, nil
}

// replay reconstructs the deterministic outcome for an already-recorded
// command key: the committed payload replays verbatim, a rejected_known
// outcome replays its recorded rejection (HTTP-COMMAND-003/004).
func (service *Service) replay(ctx context.Context, record auth.CommandRecord, digest string) (Event, error) {
	if record.RequestDigest != digest {
		return Event{}, ErrCommandReused
	}
	if record.ResultObjectType != "diagnosis_feedback" {
		return Event{}, ErrCommandReused
	}
	switch record.Outcome {
	case auth.OutcomeCommitted:
		var event Event
		if err := json.Unmarshal([]byte(record.ResultPayload), &event); err != nil {
			return Event{}, err
		}
		return event, nil
	default:
		return Event{}, ErrNotFound
	}
}

// replayRecord resolves a ledger hit through an open transaction
// connection (the pool may be fully occupied by that handle).
func replayRecord(ctx context.Context, conn *sql.Conn, record auth.CommandRecord, digest string) (Event, error) {
	if record.RequestDigest != digest {
		return Event{}, ErrCommandReused
	}
	if record.Outcome != auth.OutcomeCommitted || record.ResultObjectType != "diagnosis_feedback" {
		return Event{}, ErrCommandReused
	}
	var event Event
	if err := json.Unmarshal([]byte(record.ResultPayload), &event); err != nil {
		return Event{}, err
	}
	return event, nil
}

func rejectCommand(ctx context.Context, conn *sql.Conn, principalID int64, commandID, digest string, target Target, value string) error {
	if err := auth.RecordCommand(ctx, conn, principalID, commandID, "diagnosis_feedback.append", digest, auth.OutcomeRejectedKnown, "", 0, `{"status":404}`); err != nil {
		return err
	}
	return recordAudit(ctx, conn, principalID, "diagnosis_feedback.append_rejected", target.Type, target.ID, "")
}

func nullableNote(note string) any {
	if note == "" {
		return nil
	}
	return note
}

// invalidateSource applies DATA-TX-011 for a rejected event through the
// single shared invalidation authority (the same writer the investigation
// undo path uses): operable candidates of the source become SourceInvalid
// and every version the source produced exits retrieval permanently, with
// the FTS projection following through the schema triggers.
func (service *Service) invalidateSource(ctx context.Context, conn *sql.Conn, target Target, now string) error {
	_, err := invalidation.Apply(ctx, conn, target.Type, []int64{target.ID}, now)
	return err
}

func (service *Service) eventByID(ctx context.Context, eventID int64) (Event, error) {
	row := service.db.QueryRowContext(ctx, `
		SELECT id,target_type,target_id,value,COALESCE(note,''),COALESCE(created_by,0),created_at
		FROM diagnosis_feedback WHERE id=?`, eventID)
	return scanEvent(row.Scan)
}

func scanEvent(scan func(dest ...any) error) (Event, error) {
	var id, targetID, createdBy int64
	var event Event
	if err := scan(&id, &event.TargetType, &targetID, &event.Value, &event.Note, &createdBy, &event.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Event{}, ErrNotFound
		}
		return Event{}, err
	}
	event.ID = strconv.FormatInt(id, 10)
	event.TargetID = strconv.FormatInt(targetID, 10)
	if createdBy != 0 {
		event.CreatedBy = strconv.FormatInt(createdBy, 10)
	}
	return event, nil
}

// List returns one page of the append-only history (createdAt DESC, id
// DESC keyset) and the latestValue projection of the last committed event.
func (service *Service) List(ctx context.Context, target Target, after *Cursor, limit int) (Timeline, error) {
	if !validTargetType(target.Type) {
		return Timeline{}, ErrInvalidTarget
	}
	var (
		rows *sql.Rows
		err  error
	)
	nextLimit := limit + 1
	if after == nil {
		rows, err = service.db.QueryContext(ctx, `
			SELECT id,target_type,target_id,value,COALESCE(note,''),COALESCE(created_by,0),created_at
			FROM diagnosis_feedback WHERE target_type=? AND target_id=?
			ORDER BY created_at DESC, id DESC LIMIT ?`, target.Type, target.ID, nextLimit)
	} else {
		rows, err = service.db.QueryContext(ctx, `
			SELECT id,target_type,target_id,value,COALESCE(note,''),COALESCE(created_by,0),created_at
			FROM diagnosis_feedback WHERE target_type=? AND target_id=?
			AND (created_at < ? OR (created_at = ? AND id < ?))
			ORDER BY created_at DESC, id DESC LIMIT ?`,
			target.Type, target.ID, after.CreatedAt, after.CreatedAt, after.ID, nextLimit)
	}
	if err != nil {
		return Timeline{}, err
	}
	defer rows.Close()
	items := make([]Event, 0, limit)
	for rows.Next() {
		var event Event
		if err := rows.Scan(&event.ID, &event.TargetType, &event.TargetID, &event.Value, &event.Note, &event.CreatedBy, &event.CreatedAt); err != nil {
			return Timeline{}, err
		}
		items = append(items, event)
	}
	if err := rows.Err(); err != nil {
		return Timeline{}, err
	}
	var next *Cursor
	if len(items) > limit {
		items = items[:limit]
		last := items[limit-1]
		next = &Cursor{CreatedAt: last.CreatedAt, ID: parseID(last.ID)}
	}
	var latestValue string
	if len(items) > 0 && after == nil {
		// The first page's newest item is the last committed event for
		// this target (the keyset order equals commit order).
		latestValue = items[0].Value
	}
	return Timeline{LatestValue: latestValue, Items: items, Next: next}, nil
}

func parseID(value string) int64 {
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0
	}
	return id
}

func recordAudit(ctx context.Context, conn *sql.Conn, actorID int64, action, targetType string, targetID int64, timestamp string) error {
	result, err := conn.ExecContext(ctx, `INSERT INTO audit_events(actor_type,actor_id,action,outcome,domain_ref_type,domain_ref_id,created_at) VALUES('user',?,?, 'success',?,?,?)`,
		actorID, action, targetType, targetID, timestamp)
	if err != nil {
		return err
	}
	auditID, err := result.LastInsertId()
	if err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, `INSERT INTO audit_event_targets(audit_event_id,target_type,target_id) VALUES(?,?,?)`, auditID, targetType, targetID)
	if err != nil {
		return fmt.Errorf("write audit target: %w", err)
	}
	return nil
}

// TrimNote is a defensive bound used by the HTTP layer before Append
// (characters, matching the schema's length() semantics).
func TrimNote(note string) string {
	note = strings.TrimSpace(note)
	if count := utf8.RuneCountInString(note); count > noteLimit {
		return string([]rune(note)[:noteLimit])
	}
	return note
}
