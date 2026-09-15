package execution

import (
	"context"
	"database/sql"
	"errors"
)

// Snapshot holds one consistent read view without exposing a raw connection.
type Snapshot struct {
	tx *sql.Tx
}

func (r Reader) BeginSnapshot(ctx context.Context) (*Snapshot, error) {
	if r.db == nil {
		return nil, errors.New("execution: no read-only reader is configured")
	}
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	return &Snapshot{tx: tx}, nil
}

func (s *Snapshot) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if err := guardReadStatement(query); err != nil {
		return nil, err
	}
	return s.tx.QueryContext(ctx, query, args...)
}

func (s *Snapshot) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	if err := guardReadStatement(query); err != nil {
		return failClosedRow()
	}
	return s.tx.QueryRowContext(ctx, query, args...)
}

func (s *Snapshot) Commit() error   { return s.tx.Commit() }
func (s *Snapshot) Rollback() error { return s.tx.Rollback() }
