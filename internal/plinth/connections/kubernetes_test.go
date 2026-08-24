package connections

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestKubernetesGetRejectsResponseOverFourMiB(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(strings.Repeat("x", (4<<20)+1)))
	}))
	defer server.Close()

	client := &kubernetesClient{http: server.Client(), server: server.URL}
	if _, err := kubernetesGet(context.Background(), client, "/api"); err == nil || !strings.Contains(err.Error(), "超过读取上限") {
		t.Fatalf("kubernetesGet error=%v, want read ceiling rejection", err)
	}
}
