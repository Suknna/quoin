package alerts

import (
	"context"
)

// ChangeEvent is the bounded derived change record the SSE surface replays
// (DATA-SSE-001/003): Occurrence ID, change type and row version only —
// never object bodies.
type ChangeEvent struct {
	Seq          int64
	OccurrenceID int64
	ChangeType   string
	RowVersion   int64
}

// Watermarks derives the replay watermarks directly from the change log
// itself (DATA-SSE-009): high_water = MAX(id), oldest_available = MIN(id).
// No second watermark table exists.
func (service *Service) Watermarks(ctx context.Context) (highWater int64, oldestAvailable int64, err error) {
	err = service.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0), COALESCE(MIN(id),0) FROM alert_change_log`).Scan(&highWater, &oldestAvailable)
	return highWater, oldestAvailable, err
}

// ChangesAfter returns up to limit change events with id greater than after,
// in ascending id order (bounded replay window).
func (service *Service) ChangesAfter(ctx context.Context, after int64, limit int) ([]ChangeEvent, error) {
	rows, err := service.db.QueryContext(ctx, `SELECT id, occurrence_id, change_type, row_version FROM alert_change_log WHERE id > ? ORDER BY id ASC LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := []ChangeEvent{}
	for rows.Next() {
		var event ChangeEvent
		if err := rows.Scan(&event.Seq, &event.OccurrenceID, &event.ChangeType, &event.RowVersion); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

// CursorExpired evaluates the frozen last-seen predicate (DATA-SSE-002/009):
// high_water = 0 → only cursor 0 is current; otherwise the cursor is expired
// when it is below high_water AND below oldest_available - 1. The comparison
// is written as subtraction-free inequalities to avoid overflow.
func CursorExpired(cursor, highWater, oldestAvailable int64) bool {
	if highWater == 0 {
		return cursor != 0
	}
	return cursor < highWater && cursor+1 < oldestAvailable
}
