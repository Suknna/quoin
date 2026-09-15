package execution

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// Tx is the business-facing handle on the runner-owned transaction. It
// deliberately exposes only the database/sql query and exec surface — the
// same signatures as *sql.Conn, so existing service helpers adopt by
// accepting an interface. Commit, rollback and the underlying connection
// stay private to the runner: business code cannot end, nest or escape the
// transaction that also carries its command ledger and audit rows.
//
// Runtime guards close the known bypasses:
//   - every statement is tokenized (comments and quoted literals honored)
//     and must be exactly one statement whose first keyword is one of
//     SELECT, WITH, INSERT, UPDATE, DELETE or REPLACE — transaction control,
//     PRAGMA, ATTACH/DETACH, DDL and VACUUM are rejected, so audit-table
//     protection and single-database ownership cannot be disabled from
//     business code;
//   - the handle mutex is held from the expiry check through the initiation
//     of the database call, and the runner expires the handle BEFORE the
//     final commit, so a write initiated after the transaction ended can
//     never join or follow it. Business code must not retain or use the Tx
//     in goroutines it spawns (there is no legitimate async use; the
//     lifetime guard makes any such misuse fail closed);
//   - a rejected or expired call returns a row/result whose Scan or error
//     always fails via an already-canceled context — database/sql
//     short-circuits before any connection is taken, so rejection never
//     touches data or the pooled connection.
//
// *sql.Rows returned by QueryContext stream on the runner-owned connection;
// consume and Close them inside the callback. database/sql keeps the
// connection bound to open rows, so no other operation can steal it while
// they live.
type Tx struct {
	mu         sync.Mutex
	conn       *sql.Conn
	ended      bool
	afterAudit []func(context.Context, *Tx, int64) error
}

// Executor names the concrete runner-owned write capability. An interface
// could be embedded by a wrapper that redirects writes to an unguarded pool.
type Executor = *Tx

func newTx(conn *sql.Conn) *Tx {
	return &Tx{conn: conn}
}

// AfterAudit registers an atomic projection that needs the actual audit event ID.
// Projection failure rolls back the business mutation and its audit together.
func (tx *Tx) AfterAudit(project func(context.Context, *Tx, int64) error) error {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if tx.ended {
		return errTxHandleExpired
	}
	if project == nil {
		return errors.New("execution: nil audit projection")
	}
	tx.afterAudit = append(tx.afterAudit, project)
	return nil
}

func (tx *Tx) projectAudit(ctx context.Context, eventID int64) error {
	tx.mu.Lock()
	projections := tx.afterAudit
	tx.afterAudit = nil
	tx.mu.Unlock()
	for _, project := range projections {
		if err := project(ctx, tx, eventID); err != nil {
			return err
		}
	}
	return nil
}

// finish expires the handle. It is idempotent and safe to call before the
// commit itself so that the earliest possible moment closes the write window.
func (tx *Tx) finish() {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	tx.ended = true
}

// Active reports whether the runner still owns the underlying transaction.
func (tx *Tx) Active() bool {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	return !tx.ended
}

var errTxHandleExpired = errors.New("execution: transaction handle expired after the runner ended its transaction")

// expiredRow returns a *sql.Row whose Scan always fails with the canceled
// context: database/sql checks ctx.Done() before acquiring any connection,
// so the rejection is guaranteed not to touch the database or the pooled
// connection behind the already-ended transaction.
func (tx *Tx) expiredRow() *sql.Row {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return tx.conn.QueryRowContext(ctx, "")
}

// ExecContext runs one guarded write statement inside the runner transaction.
// The handle lock is held from the expiry check through the initiation of the
// database call, closing the finish/initiation race.
func (tx *Tx) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if tx.ended {
		return nil, errTxHandleExpired
	}
	if err := guardStatement(query); err != nil {
		return nil, err
	}
	return tx.conn.ExecContext(ctx, query, args...)
}

// QueryContext runs one guarded query inside the runner transaction. The
// returned rows stream on the runner-owned connection: consume and close them
// inside the callback.
func (tx *Tx) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if tx.ended {
		return nil, errTxHandleExpired
	}
	if err := guardStatement(query); err != nil {
		return nil, err
	}
	return tx.conn.QueryContext(ctx, query, args...)
}

// QueryRowContext runs one guarded single-row query inside the runner
// transaction. A rejected or expired call returns a row whose Scan always
// fails without executing anything.
func (tx *Tx) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if tx.ended {
		return tx.expiredRow()
	}
	if err := guardStatement(query); err != nil {
		return tx.expiredRow()
	}
	return tx.conn.QueryRowContext(ctx, query, args...)
}

// allowedBusinessStatements is the closed whitelist of statement keywords
// business code may run inside the runner transaction. Everything else —
// transaction control, PRAGMA, ATTACH/DETACH, DDL, VACUUM, EXPLAIN — is
// rejected: none of it belongs to business state changes, and several forms
// could weaken audit protection or escape the single database.
var allowedBusinessStatements = []string{"SELECT", "WITH", "INSERT", "UPDATE", "DELETE", "REPLACE"}

// guardStatement allows exactly one statement whose first keyword is on the
// whitelist. Tokenizing strips comments and honors quoted literals, so a
// keyword cannot be smuggled in through a comment and a semicolon cannot
// split out of a string literal. (The reverse conservative direction also
// holds: a keyword split by a comment is two tokens for SQLite and remains
// rejected here.)
func guardStatement(query string) error {
	statements, err := sqlStatements(query)
	if err != nil {
		return fmt.Errorf("execution: statement guard: %w", err)
	}
	if len(statements) > 1 {
		return errors.New("execution: multiple SQL statements are not allowed; run exactly one statement per call")
	}
	if len(statements) == 0 {
		return nil
	}
	first := firstSQLWord(statements[0])
	for _, keyword := range allowedBusinessStatements {
		if first == keyword {
			return nil
		}
	}
	return errors.New("execution: only SELECT, WITH, INSERT, UPDATE, DELETE or REPLACE statements may run inside the runner transaction")
}

// firstSQLWord returns the first keyword-ish token of a statement in upper
// case, or "" when the statement starts with a non-word token.
func firstSQLWord(statement string) string {
	trimmed := strings.TrimLeft(statement, " \t\r\n(")
	start := 0
	for start < len(trimmed) {
		c := trimmed[start]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_' {
			break
		}
		start++
	}
	if start == len(trimmed) {
		return ""
	}
	end := start
	for end < len(trimmed) {
		c := trimmed[end]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' {
			end++
			continue
		}
		break
	}
	return strings.ToUpper(trimmed[start:end])
}

// sqlStatements splits raw SQL into top-level statements, honoring string
// literals (with doubled-quote escapes), quoted identifiers and both comment
// forms. A trailing semicolon produces no extra statement; anything
// unterminated is rejected rather than guessed.
func sqlStatements(query string) ([]string, error) {
	var statements []string
	var current strings.Builder
	i := 0
	for i < len(query) {
		c := query[i]
		switch {
		case c == '\'' || c == '"' || c == '`':
			start := i
			i++
			closed := false
			for i < len(query) {
				index := strings.IndexByte(query[i:], c)
				if index < 0 {
					return nil, fmt.Errorf("unterminated %c literal", c)
				}
				i += index + 1
				if i < len(query) && query[i] == c {
					i++ // escaped (doubled) quote
					continue
				}
				closed = true
				break
			}
			if !closed {
				return nil, fmt.Errorf("unterminated %c literal", c)
			}
			current.WriteString(query[start:i])
		case c == '-' && i+1 < len(query) && query[i+1] == '-':
			for i < len(query) && query[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < len(query) && query[i+1] == '*':
			end := strings.Index(query[i+2:], "*/")
			if end < 0 {
				return nil, errors.New("unterminated comment")
			}
			i += end + 4
		case c == ';':
			if trimmed := strings.TrimSpace(current.String()); trimmed != "" {
				statements = append(statements, trimmed)
			}
			current.Reset()
			i++
		default:
			current.WriteByte(c)
			i++
		}
	}
	if trimmed := strings.TrimSpace(current.String()); trimmed != "" {
		statements = append(statements, trimmed)
	}
	return statements, nil
}
