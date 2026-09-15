package execution

import (
	"context"
	"errors"
	"testing"
)

func TestAuditProjectionUsesActualSequenceAndRollsBack(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "actual-sequence", true: "projection-failure"}[fail], func(t *testing.T) {
			db := newTestDB(t)
			// AUTOINCREMENT can exceed MAX(id) after retained rows have expired.
			if _, err := db.Exec(`INSERT INTO sqlite_sequence(name,seq) VALUES('audit_events',100)`); err != nil {
				t.Fatal(err)
			}
			runner, op, _ := newTestRunner(t, db)
			ctx := metadataContext(t, "corr-projection")
			sentinel := errors.New("projection failed")
			var seen int64
			_, err := Execute(ctx, runner, op, func(tx *Tx) (itemResult, error) {
				result, err := tx.ExecContext(ctx, `INSERT INTO quoin_items(name) VALUES('before')`)
				if err != nil {
					return itemResult{}, err
				}
				id, err := result.LastInsertId()
				if err != nil {
					return itemResult{}, err
				}
				err = tx.AfterAudit(func(ctx context.Context, tx *Tx, eventID int64) error {
					seen = eventID
					var actual int64
					if err := tx.QueryRowContext(ctx, `SELECT id FROM audit_events WHERE correlation_id='corr-projection'`).Scan(&actual); err != nil {
						return err
					}
					if actual != eventID {
						return errors.New("projection received guessed event id")
					}
					if _, err := tx.ExecContext(ctx, `UPDATE quoin_items SET name='projected' WHERE id=?`, id); err != nil {
						return err
					}
					if fail {
						return sentinel
					}
					return nil
				})
				return itemResult{ID: id}, err
			}, func(item itemResult) int64 { return item.ID })
			if seen != 101 {
				t.Fatalf("actual audit id=%d, want101", seen)
			}
			if fail {
				if !errors.Is(err, sentinel) {
					t.Fatalf("projection error=%v", err)
				}
				if countRows(t, db, "audit_events") != 0 || countRows(t, db, "quoin_items") != 0 {
					t.Fatal("projection failure left durable state")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				var name string
				if err := db.QueryRow(`SELECT name FROM quoin_items`).Scan(&name); err != nil {
					t.Fatal(err)
				}
				if name != "projected" {
					t.Fatal(name)
				}
			}
		})
	}
}
