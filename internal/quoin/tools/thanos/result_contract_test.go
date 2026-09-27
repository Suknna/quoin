package thanos

import (
	"encoding/json"
	"testing"

	"github.com/Suknna/quoin/internal/contract"
)

func TestSharedQueryResultPreservesFrozenJSON(t *testing.T) {
	result := contract.PromQLQueryResult{
		Success: true, StartedAt: "2026-01-01T00:00:00Z", FinishedAt: "2026-01-01T00:00:01Z",
		Status: "success", ResultType: "vector", Truncated: false, TotalBytes: 2, TotalLines: 1, Output: "{}",
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"success":true,"startedAt":"2026-01-01T00:00:00Z","finishedAt":"2026-01-01T00:00:01Z","status":"success","resultType":"vector","truncated":false,"totalBytes":2,"totalLines":1,"output":"{}"}`
	if string(encoded) != want {
		t.Fatalf("frozen result changed: %s", encoded)
	}
	if _, err := ParseResult(encoded); err != nil {
		t.Fatalf("Quoin cannot consume produced result: %v", err)
	}
}
