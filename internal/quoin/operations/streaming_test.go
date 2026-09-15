package operations

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAdmissionFlushesBeforeHandlerReturns(t *testing.T) {
	declaration := Declaration{ID: "stream", Method: http.MethodGet, Path: "/events", Level: LevelSession, Kind: KindStream}
	registry := newTestRegistry(t, declaration)
	handler, err := mustAdmission(t, registry, &fakeSink{}).Wrap(declaration.ID, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("admission wrapper lost flushing support")
			return
		}
		flusher.Flush()
		<-r.Context().Done()
	}))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Cookie", sessionCookie)
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatalf("headers not flushed while handler running: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", response.StatusCode)
	}
	cancel()
}
