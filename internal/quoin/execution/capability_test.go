package execution_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/Suknna/quoin/internal/quoin/execution"
)

type delegatedWriter struct {
	execution.Executor
	db *sql.DB
}

func (b delegatedWriter) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return b.db.ExecContext(ctx, query, args...)
}

func TestExecutorRejectsRawDatabaseCapabilities(t *testing.T) {
	for name, value := range map[string]any{
		"pool":             (*sql.DB)(nil),
		"connection":       (*sql.Conn)(nil),
		"transaction":      (*sql.Tx)(nil),
		"embedded wrapper": delegatedWriter{},
	} {
		t.Run(name, func(t *testing.T) {
			if _, ok := value.(execution.Executor); ok {
				t.Fatal("raw SQL handle must not satisfy the guarded business write capability")
			}
		})
	}
	if _, ok := any((*execution.Tx)(nil)).(execution.Executor); !ok {
		t.Fatal("runner transaction must provide the business write capability")
	}
}
