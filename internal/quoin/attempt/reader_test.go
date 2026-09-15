package attempt

import (
	"context"
	"testing"
)

func TestAttemptReaderRequiresReadonlyComposition(t *testing.T) {
	db := newTestDB(t)
	t.Cleanup(func() { _ = db.Close() })
	attemptID, _ := seedAttempt(t, db)
	unwired := NewService(db)
	if _, err := unwired.Get(context.Background(), attemptID); err == nil {
		t.Fatal("unwired attempt reads must fail closed")
	}
	if err := unwired.SetReader(db); err == nil {
		t.Fatal("attempt must reject its write pool as reader")
	}
	service := newTestService(t, db)
	if _, err := service.Get(context.Background(), attemptID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Reader().QueryContext(context.Background(), `UPDATE execution_attempts SET row_version=row_version+1 WHERE id=? RETURNING id`, attemptID); err == nil {
		t.Fatal("attempt reader must reject writes through QueryContext")
	}
}
