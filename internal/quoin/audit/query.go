// Package audit implements the audit module (docs/audit-design.md §6-7):
// persistence of events, targets and cleanup batches, the read model with
// list/detail queries, and retention settings, preview and bounded cleanup.
package audit

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Actor value domains mirror the audit_events CHECK constraints.
const (
	ActorUser    = "user"
	ActorService = "service"
	ActorSystem  = "system"
)

// Query outcome aliases for the audit_events outcome domain; uniquely named
// next to the writer-owned outcome constants so both sides share one vocabulary.
const (
	QueryOutcomeSuccess  = OutcomeSuccess
	QueryOutcomeFailure  = OutcomeFailure
	QueryOutcomeRejected = OutcomeRejected
	QueryOutcomeUnknown  = OutcomeUnknown
)

// TargetView is one audit_event_targets row attached to an event.
type TargetView struct {
	Type    string
	ID      int64
	Version int64 // 0 when the row has no version
}

// EventView is the read model of one audit event plus its targets, distinct
// from the writer's Record input. CorrelationID is empty only for
// pre-consolidation history ("历史无关联"); it is never guessed from actor or
// time.
type EventView struct {
	ID              int64
	ActorType       string
	ActorID         int64
	Action          string
	CorrelationID   string
	RequestID       string
	Phase           string
	InitiatorType   string
	InitiatorID     int64
	ClientCommandID string
	Outcome         string
	DomainRefType   string
	DomainRefID     int64
	CreatedAt       string // UTC, as recorded by the writer
	Targets         []TargetView
}

// Filter carries optional list criteria; zero-value fields are ignored so the
// server never interprets blank strings as criteria.
type Filter struct {
	CorrelationID string
	ActorType     string
	ActorID       int64
	Action        string
	Outcome       string
	DomainRefType string
	DomainRefID   int64
	// Since is inclusive, Until exclusive; RFC3339. Comparisons canonicalize
	// both sides to fixed-precision UTC so variable RFC3339Nano fractions in
	// stored rows cannot invert lexicographic order.
	Since string
	Until string
}

// Page is one keyset page; NextCursor is empty once exhausted.
type Page struct {
	Events     []EventView
	NextCursor string
}

// Reader is the read seam shared by *sql.DB, *sql.Conn and transaction
// wrappers; queries always run through the read-only capability.
type Reader interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

const (
	DefaultPageSize = 50
	MaxPageSize     = 200
)

// ErrInvalidFilter rejects unknown enum values rather than silently returning
// an empty page that would look like "nothing was audited".
var ErrInvalidFilter = errors.New("audit: invalid query filter")

// ErrInvalidCursor reports an opaque page cursor that failed to decode.
var ErrInvalidCursor = errors.New("audit: malformed page cursor")

// QueryEvents lists events newest-first (record order, keyset by id so pages
// stay stable under inserts) with targets attached to the returned page only.
func QueryEvents(ctx context.Context, r Reader, filter Filter, cursor string, limit int) (Page, error) {
	if err := validateFilter(filter); err != nil {
		return Page{}, err
	}
	if limit <= 0 {
		limit = DefaultPageSize
	}
	if limit > MaxPageSize {
		limit = MaxPageSize
	}
	where := "WHERE 1=1"
	var args []any
	if filter.CorrelationID != "" {
		where += " AND correlation_id = ?"
		args = append(args, filter.CorrelationID)
	}
	if filter.ActorType != "" {
		where += " AND actor_type = ?"
		args = append(args, filter.ActorType)
	}
	if filter.ActorID > 0 {
		where += " AND actor_id = ?"
		args = append(args, filter.ActorID)
	}
	if filter.Action != "" {
		where += " AND action = ?"
		args = append(args, filter.Action)
	}
	if filter.Outcome != "" {
		where += " AND outcome = ?"
		args = append(args, filter.Outcome)
	}
	if filter.DomainRefType != "" {
		where += " AND domain_ref_type = ?"
		args = append(args, filter.DomainRefType)
	}
	if filter.DomainRefID > 0 {
		where += " AND domain_ref_id = ?"
		args = append(args, filter.DomainRefID)
	}
	since, until, err := canonicalRange(filter.Since, filter.Until)
	if err != nil {
		return Page{}, err
	}
	if since != "" {
		where += " AND julianday(created_at) >= julianday(?)"
		args = append(args, since)
	}
	if until != "" {
		where += " AND julianday(created_at) < julianday(?)"
		args = append(args, until)
	}
	afterID, err := decodeCursor(cursor)
	if err != nil {
		return Page{}, err
	}
	if afterID > 0 {
		where += " AND id < ?"
		args = append(args, afterID)
	}
	args = append(args, limit)

	rows, err := r.QueryContext(ctx, `SELECT id, actor_type, actor_id, action,
			COALESCE(correlation_id,''), COALESCE(request_id,''), phase,
			COALESCE(initiator_type,''), COALESCE(initiator_id,0),
			COALESCE(client_command_id,''), outcome,
			COALESCE(domain_ref_type,''), COALESCE(domain_ref_id,0), created_at
		FROM audit_events `+where+" ORDER BY id DESC LIMIT ?", args...)
	if err != nil {
		return Page{}, fmt.Errorf("query audit events: %w", err)
	}
	defer rows.Close()
	var events []EventView
	for rows.Next() {
		var event EventView
		if err := rows.Scan(&event.ID, &event.ActorType, &event.ActorID, &event.Action,
			&event.CorrelationID, &event.RequestID, &event.Phase,
			&event.InitiatorType, &event.InitiatorID,
			&event.ClientCommandID, &event.Outcome,
			&event.DomainRefType, &event.DomainRefID, &event.CreatedAt); err != nil {
			return Page{}, fmt.Errorf("scan audit event: %w", err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return Page{}, fmt.Errorf("iterate audit events: %w", err)
	}
	if err := attachTargets(ctx, r, events); err != nil {
		return Page{}, err
	}
	page := Page{Events: events}
	if len(events) == limit {
		// A full page may have a successor; the cursor is the last id.
		page.NextCursor = encodeCursor(events[len(events)-1].ID)
	}
	return page, nil
}

// GetEvent returns one event with its targets; sql.ErrNoRows passes through.
func GetEvent(ctx context.Context, r Reader, id int64) (EventView, error) {
	var event EventView
	err := r.QueryRowContext(ctx, `SELECT id, actor_type, actor_id, action,
			COALESCE(correlation_id,''), COALESCE(request_id,''), phase,
			COALESCE(initiator_type,''), COALESCE(initiator_id,0),
			COALESCE(client_command_id,''), outcome,
			COALESCE(domain_ref_type,''), COALESCE(domain_ref_id,0), created_at
		FROM audit_events WHERE id = ?`, id).
		Scan(&event.ID, &event.ActorType, &event.ActorID, &event.Action,
			&event.CorrelationID, &event.RequestID, &event.Phase,
			&event.InitiatorType, &event.InitiatorID,
			&event.ClientCommandID, &event.Outcome,
			&event.DomainRefType, &event.DomainRefID, &event.CreatedAt)
	if err != nil {
		return EventView{}, err
	}
	events := []EventView{event}
	if err := attachTargets(ctx, r, events); err != nil {
		return EventView{}, err
	}
	return events[0], nil
}

func attachTargets(ctx context.Context, r Reader, events []EventView) error {
	if len(events) == 0 {
		return nil
	}
	byID := make(map[int64]*EventView, len(events))
	placeholders := ""
	args := make([]any, 0, len(events))
	for i := range events {
		byID[events[i].ID] = &events[i]
		if i > 0 {
			placeholders += ","
		}
		placeholders += "?"
		args = append(args, events[i].ID)
	}
	rows, err := r.QueryContext(ctx, `SELECT audit_event_id, target_type, target_id, COALESCE(target_version,0)
		FROM audit_event_targets WHERE audit_event_id IN (`+placeholders+") ORDER BY id", args...)
	if err != nil {
		return fmt.Errorf("query audit event targets: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var eventID int64
		var target TargetView
		if err := rows.Scan(&eventID, &target.Type, &target.ID, &target.Version); err != nil {
			return fmt.Errorf("scan audit event target: %w", err)
		}
		if event := byID[eventID]; event != nil {
			event.Targets = append(event.Targets, target)
		}
	}
	return rows.Err()
}

// validateFilter accepts exactly the audit_events value domains so a typo
// cannot look like "nothing was audited".
func validateFilter(filter Filter) error {
	switch filter.ActorType {
	case "", ActorUser, ActorService, ActorSystem:
	default:
		return fmt.Errorf("%w: actor_type %q", ErrInvalidFilter, filter.ActorType)
	}
	if filter.Outcome != "" && !outcomes[filter.Outcome] {
		return fmt.Errorf("%w: outcome %q", ErrInvalidFilter, filter.Outcome)
	}
	return nil
}

// canonicalRange validates RFC3339 bounds and re-emits them in the fixed
// nine-digit-fraction UTC form. Comparisons run through julianday() (the same
// basis as the retention triggers) because lexicographic order of stored
// RFC3339Nano values is unreliable when trailing zero fractions were trimmed.
func canonicalRange(since, until string) (string, string, error) {
	parse := func(value, name string) (string, error) {
		if value == "" {
			return "", nil
		}
		parsed, err := time.Parse(time.RFC3339Nano, value)
		if err != nil {
			return "", fmt.Errorf("%w: %s %q", ErrInvalidFilter, name, value)
		}
		return CanonicalTimestamp(parsed), nil
	}
	sinceCanonical, err := parse(since, "since")
	if err != nil {
		return "", "", err
	}
	untilCanonical, err := parse(until, "until")
	if err != nil {
		return "", "", err
	}
	return sinceCanonical, untilCanonical, nil
}

// CanonicalTimestamp renders a UTC timestamp with a constant nine-digit
// fraction so generated cutoffs parse identically everywhere and never rely
// on RFC3339Nano's trailing-zero trimming.
func CanonicalTimestamp(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000000000Z")
}

type cursorWire struct {
	ID int64 `json:"i"`
}

func encodeCursor(id int64) string {
	body, _ := json.Marshal(cursorWire{ID: id})
	return base64.RawURLEncoding.EncodeToString(body)
}

func decodeCursor(cursor string) (int64, error) {
	if cursor == "" {
		return 0, nil
	}
	body, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, fmt.Errorf("%w", ErrInvalidCursor)
	}
	var wire cursorWire
	if err := json.Unmarshal(body, &wire); err != nil || wire.ID <= 0 {
		return 0, fmt.Errorf("%w", ErrInvalidCursor)
	}
	return wire.ID, nil
}
