package analysis

// Task change projection (DATA-SSE-004..009): the task_change_log is the
// bounded derived change stream for every observable task object; the
// snapshot lists only the active set (HTTP-PAGE-007). The log is
// trigger-derived from the frozen schema and stays discardable/replayable.

import (
	"context"
	"strconv"
)

// TaskChange is one derived task change event.
type TaskChange struct {
	Seq        int64
	ObjectType string
	ObjectID   int64
	ChangeType string
	RowVersion int64
}

// TaskRef is one active task object in the snapshot (TaskObjectRef).
type TaskRef struct {
	ObjectType string `json:"objectType"`
	ObjectID   string `json:"objectId"`
	RowVersion int64  `json:"rowVersion"`
}

// TaskSnapshot is the bounded active-task projection.
type TaskSnapshot struct {
	SnapshotSeq int64     `json:"snapshotSeq"`
	Items       []TaskRef `json:"items"`
	NextCursor  string    `json:"nextCursor,omitempty"`
}

// Watermarks derives the replay watermarks directly from the change log
// itself (DATA-SSE-009): high_water = MAX(id), oldest_available = MIN(id).
func (service *Service) TaskWatermarks(ctx context.Context) (highWater, oldest int64, err error) {
	err = service.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0), COALESCE(MIN(id),0) FROM task_change_log`).Scan(&highWater, &oldest)
	return highWater, oldest, err
}

// TaskCursorExpired applies the last-seen predicate (DATA-SSE-009): with
// high_water = 0 only cursor = 0 is current; otherwise a cursor below the
// oldest available row is expired.
func TaskCursorExpired(cursor, highWater, oldest int64) bool {
	if highWater == 0 {
		return cursor != 0
	}
	return cursor < highWater && cursor < oldest-1
}

// TaskChangesAfter replays the bounded change window after the cursor.
func (service *Service) TaskChangesAfter(ctx context.Context, cursor int64, limit int) ([]TaskChange, error) {
	rows, err := service.db.QueryContext(ctx, `
		SELECT id,object_type,object_id,change_type,row_version
		FROM task_change_log WHERE id>? ORDER BY id LIMIT ?`, cursor, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	changes := []TaskChange{}
	for rows.Next() {
		var change TaskChange
		if err := rows.Scan(&change.Seq, &change.ObjectType, &change.ObjectID, &change.ChangeType, &change.RowVersion); err != nil {
			return nil, err
		}
		changes = append(changes, change)
	}
	return changes, rows.Err()
}

// ActiveTaskSnapshot returns the bounded active-task set (HTTP-PAGE-007):
// non-terminal initial_analyses, execution_attempts and tool_calls.
func (service *Service) ActiveTaskSnapshot(ctx context.Context) ([]TaskRef, error) {
	items := []TaskRef{}
	rows, err := service.db.QueryContext(ctx, `
		SELECT 'initial_analysis', id, row_version FROM initial_analyses
		WHERE state IN ('Queued','Running')
		UNION ALL SELECT 'execution_attempt', id, row_version FROM execution_attempts
		WHERE state IN ('Queued','Assigned','Running','Cancelling') AND attempt_type IN ('initial_analysis','investigation','inspection_analysis','inspection_collection','knowledge_extraction','embedding','connection_probe')
		UNION ALL SELECT 'tool_call', id, row_version FROM tool_calls
		WHERE status IN ('pending','running')
		ORDER BY 2 DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var objectType string
		var id, version int64
		if err := rows.Scan(&objectType, &id, &version); err != nil {
			return nil, err
		}
		items = append(items, TaskRef{ObjectType: objectType, ObjectID: strconv.FormatInt(id, 10), RowVersion: version})
	}
	if items == nil {
		items = []TaskRef{}
	}
	return items, rows.Err()
}
