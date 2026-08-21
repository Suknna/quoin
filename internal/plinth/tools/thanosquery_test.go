package tools

// Thanos query executor tests (ARCH-OUTPUT-001/003/005): the bounded
// streaming accumulator, the spill decision, the artifact commit on
// overflow, and the structured failure shapes (return_to_model semantics —
// nothing is retried inside the supervisor).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	plinthconnections "github.com/Suknna/quoin/internal/plinth/connections"
	"github.com/Suknna/quoin/internal/quoin/tools/thanos"
)

func serveFixture(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/query", func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Query().Get("query") {
		case "big":
			var builder strings.Builder
			builder.WriteString(`{"status":"success","data":{"resultType":"matrix","result":[`)
			// 2000 series x one value ≈ 96 KiB and ≈ 2002 lines: crosses
			// both frozen spill thresholds (50 KiB / 2000 lines).
			for index := 0; index < 2000; index++ {
				if index > 0 {
					builder.WriteString(",")
				}
				fmt.Fprintf(&builder, "\n{\"metric\":{\"index\":\"%d\"},\"values\":[[%d,\"%d\"]]}", index, index, index)
			}
			builder.WriteString("\n]}}\n")
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(builder.String()))
		case "up":
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{"__name__":"up"},"value":[1,"1"]}]}}`))
		case "error":
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{"status":"error","errorType":"bad_data","error":"fixture failure"}`))
		case "notjson":
			_, _ = writer.Write([]byte(`<html>broken</html>`))
		default:
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
		}
	})
	return httptest.NewServer(mux)
}

func testParams(t *testing.T, server *httptest.Server, query string) ThanosQueryParams {
	t.Helper()
	return ThanosQueryParams{
		Config:       plinthconnections.ThanosConfig{BaseURL: server.URL},
		Secret:       plinthconnections.ThanosSecret{},
		Query:        query,
		WorkspaceDir: t.TempDir(),
		AttemptID:    7, ToolCallID: 11,
	}
}

func TestThanosQuerySmallInline(t *testing.T) {
	server := serveFixture(t)
	defer server.Close()
	payload, artifactID, err := ExecuteThanosQuery(context.Background(), testParams(t, server, "up"))
	if err != nil {
		t.Fatal(err)
	}
	if artifactID != 0 {
		t.Fatalf("small result must not spill, artifact=%d", artifactID)
	}
	result, err := thanos.ParseResult(payload)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Success || result.Truncated || result.ResultType != "vector" || result.SampleCount != 1 {
		t.Fatalf("result=%+v", result)
	}
	if result.Artifact != nil {
		t.Fatalf("artifact=%+v", result.Artifact)
	}
	// The inline output carries the complete response body.
	if !strings.Contains(result.Output, `"status":"success"`) {
		t.Fatalf("output=%s", result.Output)
	}
}

func TestThanosQueryLargeSpillAndArtifactCommit(t *testing.T) {
	server := serveFixture(t)
	defer server.Close()
	var uploadedBody []byte
	params := testParams(t, server, "big")
	params.Upload = func(ctx context.Context, attemptID, toolCallID int64, path string) (int64, error) {
		// The executor owns the transient spill file and removes it right
		// after the commit returns; capture the bytes here.
		body, err := os.ReadFile(path)
		if err != nil {
			return 0, err
		}
		uploadedBody = body
		return 42, nil
	}
	payload, artifactID, err := ExecuteThanosQuery(context.Background(), params)
	if err != nil {
		t.Fatal(err)
	}
	if artifactID != 42 {
		t.Fatalf("artifact=%d want 42", artifactID)
	}
	result, err := thanos.ParseResult(payload)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Success || !result.Truncated || result.Artifact == nil {
		t.Fatalf("result=%+v", result)
	}
	if result.Artifact.ID != "42" || result.Artifact.TotalLines == 0 || result.Artifact.SizeBytes < spillBytes {
		t.Fatalf("artifact=%+v", result.Artifact)
	}
	// The uploaded file carried the complete raw body with a verifiable
	// hash matching the payload locator.
	hash := sha256.Sum256(uploadedBody)
	if hex.EncodeToString(hash[:]) != result.Artifact.SHA256 {
		t.Fatal("artifact locator hash differs from the uploaded bytes")
	}
	// The bounded preview marks the truncation and never contains the
	// full body.
	if !strings.Contains(result.Output, "…（完整输出已存入 Artifact）") {
		t.Fatalf("preview lacks the spill marker: %.80s", result.Output)
	}
	if len(result.Output) > previewBytes+64 {
		t.Fatalf("preview too large: %d", len(result.Output))
	}
	// The transient spill file is gone after the commit (never a durable
	// second authority).
	if _, err := os.Stat(filepath.Join(params.WorkspaceDir, "tool-results", "thanos-11.json")); !os.IsNotExist(err) {
		t.Fatalf("spill file survived: %v", err)
	}
}

func TestThanosQuerySpillUploadFailureFailsTheTool(t *testing.T) {
	server := serveFixture(t)
	defer server.Close()
	params := testParams(t, server, "big")
	params.Upload = func(ctx context.Context, attemptID, toolCallID int64, path string) (int64, error) {
		return 0, fmt.Errorf("upload interrupted")
	}
	payload, artifactID, err := ExecuteThanosQuery(context.Background(), params)
	if err != nil {
		t.Fatal(err)
	}
	if artifactID != 0 {
		t.Fatalf("failed upload must not yield an artifact, got %d", artifactID)
	}
	result, err := thanos.ParseResult(payload)
	if err != nil {
		t.Fatal(err)
	}
	// ARCH-OUTPUT-005: the upload failure fails the tool call with the
	// structured error; a bare preview never pretends to be the result.
	if result.Success || result.ErrorCode != "artifact_commit_failed" {
		t.Fatalf("result=%+v", result)
	}
}

func TestThanosQueryStructuredFailures(t *testing.T) {
	server := serveFixture(t)
	defer server.Close()
	cases := []struct {
		query     string
		errorCode string
	}{
		{"error", "thanos_query_error"},
		{"notjson", "thanos_invalid_response"},
	}
	for _, testCase := range cases {
		payload, _, err := ExecuteThanosQuery(context.Background(), testParams(t, server, testCase.query))
		if err != nil {
			t.Fatal(err)
		}
		result, err := thanos.ParseResult(payload)
		if err != nil {
			t.Fatal(err)
		}
		if result.Success || result.ErrorCode != testCase.errorCode || result.ErrorDetail == "" {
			t.Fatalf("query=%s result=%+v", testCase.query, result)
		}
	}
}

func TestThanosQueryHTTPErrorAndTimeout(t *testing.T) {
	// An HTTP 500 target is a structured return_to_model failure.
	broken := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Error(writer, "boom", http.StatusInternalServerError)
	}))
	defer broken.Close()
	params := testParams(t, broken, "up")
	payload, _, err := ExecuteThanosQuery(context.Background(), params)
	if err != nil {
		t.Fatal(err)
	}
	result, err := thanos.ParseResult(payload)
	if err != nil {
		t.Fatal(err)
	}
	if result.Success || result.ErrorCode != "thanos_http_error" {
		t.Fatalf("result=%+v", result)
	}
	// A hung target hits the per-call deadline (bounded external calls,
	// ARCH-MODE-003); the handler waits on the request context so the
	// client deadline tears it down.
	hung := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		<-request.Context().Done()
	}))
	defer hung.Close()
	hungParams := testParams(t, hung, "up")
	hungParams.Timeout = 300 * time.Millisecond
	started := time.Now()
	payload, _, err = ExecuteThanosQuery(context.Background(), hungParams)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("query deadline not enforced: %v", elapsed)
	}
	result, err = thanos.ParseResult(payload)
	if err != nil {
		t.Fatal(err)
	}
	if result.Success || result.ErrorCode != "thanos_timeout" {
		t.Fatalf("result=%+v", result)
	}
}
