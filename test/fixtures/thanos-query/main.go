// Package main is the deterministic Prometheus-compatible query fixture for
// T11 (test/fixtures/thanos-query). It is a black-box target for the
// supervisor-typed thanos_query tool: real HTTP semantics, deterministic
// bodies and a hit counter so acceptance runs can prove that no query ever
// reaches the target before Quoin authorizes the tool call. It never
// asserts anything about the caller — it only answers.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

var hits struct {
	mu      sync.Mutex
	queries map[string]int
}

func main() {
	address := flag.String("address", "127.0.0.1:18444", "listen address")
	flag.Parse()
	hits.queries = map[string]int{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/query", func(writer http.ResponseWriter, request *http.Request) {
		query := request.URL.Query().Get("query")
		hits.mu.Lock()
		hits.queries[query]++
		hits.mu.Unlock()
		log.Printf("query hit: %q", query)
		if strings.HasPrefix(query, `up{business_system="`) {
			serveSmallVector(writer)
			return
		}
		if strings.HasPrefix(query, `slow{business_system="`) {
			// Integration acceptance uses this cooperative delay to prove that a
			// Runtime cancellation reaches a live typed PromQL worker.
			select {
			case <-time.After(5 * time.Second):
				serveSmallVector(writer)
			case <-request.Context().Done():
				log.Printf("slow query cancelled: %q", query)
			}
			return
		}
		switch query {
		case "big":
			serveBigMatrix(writer)
		case "up":
			serveSmallVector(writer)
		case "vector(1)":
			// The frozen Thanos connection probe contract requires exactly one
			// vector sample with value "1" (internal/plinth/connections).
			writeJSON(writer, map[string]any{"status": "success", "data": map[string]any{
				"resultType": "vector",
				"result":     []any{map[string]any{"metric": map[string]any{"__name": "vector", "job": "fixture"}, "value": []any{float64(time.Now().Unix()), "1"}}},
			}})
		case "error":
			writeJSON(writer, map[string]any{"status": "error", "errorType": "bad_data", "error": "deterministic fixture query failure"})
		case "notjson":
			fmt.Fprint(writer, "<html>not a prometheus response</html>")
		default:
			writeJSON(writer, map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": []any{}}})
		}
	})
	mux.HandleFunc("GET /hits", func(writer http.ResponseWriter, request *http.Request) {
		hits.mu.Lock()
		defer hits.mu.Unlock()
		writeJSON(writer, map[string]any{"queries": hits.queries})
	})
	// The listening line is only truthful after a successful bind: a stale
	// fixture holding the port must not masquerade as a live target.
	listener, err := net.Listen("tcp", *address)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("fixture thanos query listening on %s", *address)
	log.Fatal(http.Serve(listener, mux))
}

func writeJSON(writer http.ResponseWriter, value any) {
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(value)
}

// serveSmallVector answers a deterministic single-sample vector (the small
// inline path: the whole body stays inside the tool result payload).
func serveSmallVector(writer http.ResponseWriter) {
	writeJSON(writer, map[string]any{
		"status": "success",
		"data": map[string]any{
			"resultType": "vector",
			"result": []any{
				map[string]any{"metric": map[string]any{"__name__": "up", "job": "fixture"}, "value": []any{float64(time.Now().Unix()), "1"}},
			},
		},
	})
}

// serveBigMatrix answers a deterministic matrix large enough to cross the
// 50 KiB / 2000 line spill thresholds (the acceptance path proves the
// truncated preview plus the complete committed Artifact). Every line is
// deterministic so the content hash is reproducible.
func serveBigMatrix(writer http.ResponseWriter) {
	writer.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprint(writer, `{"status":"success","data":{"resultType":"matrix","result":[`)
	// 2000 series x 1 sample ≈ 96 KiB and ≈ 2002 lines: crosses both
	// frozen spill thresholds (50 KiB / 2000 lines).
	const series = 2000
	for index := 0; index < series; index++ {
		if index > 0 {
			_, _ = fmt.Fprint(writer, ",")
		}
		_, _ = fmt.Fprintf(writer,
			"\n{\"metric\":{\"__name__\":\"fixture_series\",\"index\":\"%d\"},\"values\":[[%d,\"%d\"],[%d,\"%d\"]]}",
			index, 1750000000+index, index, 1750000100+index, index+1)
	}
	_, _ = fmt.Fprint(writer, "\n]}}\n")
}
