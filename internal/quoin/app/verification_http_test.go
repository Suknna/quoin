package app_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/contract"
	"github.com/Suknna/quoin/internal/quoin/app"
	"github.com/Suknna/quoin/internal/quoin/artifact"
	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/bootstrap"
)

// TestDeploymentVerificationHTTPEndpoints drives the frozen Deployment
// Acceptance HTTP family over the real Huma + SQLite surface: start/list/
// detail/cancel, the YAML helper request download, report import rejection
// and typed observation submission.
func TestDeploymentVerificationHTTPEndpoints(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	secrets := filepath.Join(root, "secrets")
	config := contract.QuoinConfig{
		Component: "quoin", PublicOrigin: "https://quoin.example.test",
		DataDirectory:             filepath.Join(root, "data"),
		BackupDirectory:           filepath.Join(root, "backup"),
		RootKeyFile:               filepath.Join(secrets, "root-key"),
		RuntimeTLSCertificateFile: filepath.Join(secrets, "runtime-tls.crt"),
		RuntimeTLSPrivateKeyFile:  filepath.Join(secrets, "runtime-tls.key"),
		SteleServiceTokenFile:     filepath.Join(secrets, "stele-service-token"),
	}
	if _, err := bootstrap.BootstrapSecrets(config); err != nil {
		t.Fatal(err)
	}
	database, err := bootstrap.OpenDatabase(ctx, config.DataDirectory, config.RootKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	authService, err := auth.NewService(database.SQL)
	if err != nil {
		t.Fatal(err)
	}
	const temporary = "Correct horse battery staple 2026!"
	if _, err := authService.CreateFirstAdmin(ctx, "admin", "Quoin Admin", temporary); err != nil {
		t.Fatal(err)
	}
	application := app.NewAPIServer(authService, database.SQL, config.RootKeyFile)
	application.SetDeploymentVerificationBinding(&contract.DeploymentBinding{
		ReleaseVersion:          "v1.2.3",
		ReleaseSubjectDigest:    "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		DeploymentConfigDigest:  "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Backend:                 "compose",
		Architecture:            "linux/amd64",
		BrowserChromiumRevision: "1200.0.6099.109",
	}, config.PublicOrigin)
	artifactStore, err := artifact.NewStore(database.SQL, filepath.Join(config.DataDirectory, "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	application.WireVerificationArtifacts(artifactStore)
	handler, err := app.NewHandler(application, config.PublicOrigin)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()

	origin := map[string]string{"Origin": config.PublicOrigin, "Content-Type": "application/json"}
	login := mustPost(t, server, origin, `/api/v1/auth/login`, `{"username":"admin","password":"`+temporary+`"}`, http.StatusOK)
	cookie := strings.SplitN(strings.Split(login.headers.Get("Set-Cookie"), ";")[0], "=", 2)[1]

	// First-login sessions are gated behind the mandatory password change.
	mustDo(t, server, http.MethodPut, map[string]string{"Origin": config.PublicOrigin, "Content-Type": "application/json", "Cookie": "__Host-quoin-session=" + cookie},
		`/api/v1/auth/password`, `{"currentPassword":"`+temporary+`","newPassword":"Replacement staple 2026! xk"}`, http.StatusNoContent)

	authenticated := map[string]string{"Origin": config.PublicOrigin, "Content-Type": "application/json", "Cookie": "__Host-quoin-session=" + cookie}

	// Unauthenticated reads are rejected.
	mustPost(t, server, origin, `/api/v1/deployment-verifications`, `{"clientCommandId":"unauth-start-1"}`, http.StatusUnauthorized)

	// Start freezes the manifest through the real route surface.
	started := mustPost(t, server, authenticated, `/api/v1/deployment-verifications`, `{"clientCommandId":"http-start-0001"}`, http.StatusAccepted)
	var detail struct {
		ID         string `json:"id"`
		ItemCount  int    `json:"itemCount"`
		Items      []any  `json:"items"`
		StartedAt  string `json:"startedAt"`
		DeadlineAt string `json:"deadlineAt"`
	}
	if err := json.Unmarshal([]byte(started.body), &detail); err != nil {
		t.Fatalf("start response: %v\n%s", err, started.body)
	}
	if detail.ItemCount != 18 || len(detail.Items) != 18 {
		t.Fatalf("item count = %d items = %d\n%s", detail.ItemCount, len(detail.Items), started.body)
	}
	if startedAt, err := time.Parse(time.RFC3339Nano, detail.StartedAt); err != nil {
		t.Fatal(err)
	} else if deadlineAt, err := time.Parse(time.RFC3339Nano, detail.DeadlineAt); err != nil {
		t.Fatal(err)
	} else if deadlineAt.Sub(startedAt) != 8*time.Hour {
		t.Fatalf("deadline window = %s", deadlineAt.Sub(startedAt))
	}

	// Replay returns the same invocation.
	replayed := mustPost(t, server, authenticated, `/api/v1/deployment-verifications`, `{"clientCommandId":"http-start-0001"}`, http.StatusAccepted)
	var replayDetail struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(replayed.body), &replayDetail); err != nil {
		t.Fatal(err)
	}
	if replayDetail.ID != detail.ID {
		t.Fatalf("replay id %s != %s", replayDetail.ID, detail.ID)
	}

	// List and detail read the frozen state.
	listed := mustDo(t, server, http.MethodGet, authenticated, `/api/v1/deployment-verifications`, "", http.StatusOK)
	if !strings.Contains(listed.body, `"itemCount":18`) {
		t.Fatalf("list body missing item count:\n%s", listed.body)
	}
	fetched := mustDo(t, server, http.MethodGet, authenticated, `/api/v1/deployment-verifications/`+detail.ID, "", http.StatusOK)
	if !strings.Contains(fetched.body, `"progress"`) {
		t.Fatalf("detail body missing progress:\n%s", fetched.body)
	}
	mustDo(t, server, http.MethodGet, authenticated, `/api/v1/deployment-verifications/999`, "", http.StatusNotFound)

	// The helper request download is a deterministic YAML attachment.
	request := mustDoFull(t, server, http.MethodGet, map[string]string{"Origin": config.PublicOrigin, "Cookie": "__Host-quoin-session=" + cookie}, `/api/v1/deployment-verifications/`+detail.ID+`/helper-request`, "", http.StatusOK)
	if !strings.Contains(request.body, "documentType: helper_request") || !strings.Contains(request.headers.Get("Content-Type"), "application/yaml") {
		t.Fatalf("helper request shape: %s %s", request.headers.Get("Content-Type"), request.body[:120])
	}
	digest := request.headers.Get("X-Quoin-Request-Digest")
	if len(digest) != 64 {
		t.Fatalf("helper request digest header = %q", digest)
	}
	again := mustDoFull(t, server, http.MethodGet, map[string]string{"Origin": config.PublicOrigin, "Cookie": "__Host-quoin-session=" + cookie}, `/api/v1/deployment-verifications/`+detail.ID+`/helper-request`, "", http.StatusOK)
	if again.headers.Get("X-Quoin-Request-Digest") != digest {
		t.Fatal("helper request digest must be stable across reads")
	}

	// A schema-invalid helper report is rejected without side effects.
	mustDoFull(t, server, http.MethodPost, map[string]string{"Origin": config.PublicOrigin, "Content-Type": "application/yaml", "Cookie": "__Host-quoin-session=" + cookie},
		`/api/v1/deployment-verifications/`+detail.ID+`/helper-reports`, "not: a: report", http.StatusBadRequest)

	// Typed observation for the first ui cell through the real route.
	var firstUI struct {
		ID          string `json:"id"`
		InputDigest string `json:"inputDigest"`
	}
	items := struct {
		Items []struct {
			ID          string `json:"id"`
			ObjectKind  string `json:"objectKind"`
			InputDigest string `json:"inputDigest"`
		} `json:"items"`
	}{}
	if err := json.Unmarshal([]byte(fetched.body), &items); err != nil {
		t.Fatal(err)
	}
	for _, item := range items.Items {
		if item.ObjectKind != "ui_observation" {
			continue
		}
		firstUI.ID, firstUI.InputDigest = item.ID, item.InputDigest
		break
	}
	if firstUI.ID == "" {
		t.Fatal("no ui_observation item found")
	}
	observation := mustPost(t, server, authenticated, `/api/v1/deployment-verifications/`+detail.ID+`/observations`,
		`{"clientCommandId":"obs-00000001","itemId":"`+firstUI.ID+`","inputDigest":"`+firstUI.InputDigest+`","visualResult":"passed","motionResult":"passed","focusOcclusionResult":"passed"}`, http.StatusCreated)
	if !strings.Contains(observation.body, `"id"`) {
		t.Fatalf("observation response: %s", observation.body)
	}

	// Cancel finalizes from the observed facts.
	cancelled := mustPost(t, server, authenticated, `/api/v1/deployment-verifications/`+detail.ID+`/cancel`,
		`{"clientCommandId":"cancel-000001"}`, http.StatusOK)
	if !strings.Contains(cancelled.body, `"receipt"`) {
		t.Fatalf("cancel must finalize:\n%s", cancelled.body)
	}

}

// mustDoFull mirrors mustDo but also returns response headers.
func mustDoFull(t *testing.T, server *httptest.Server, method string, headers map[string]string, path, body string, want int) httpResult {
	t.Helper()
	result := mustDo(t, server, method, headers, path, body, want)
	if method == http.MethodGet {
		// mustDo drops headers; re-fetch once for header assertions.
		request, err := http.NewRequest(method, server.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		for key, value := range headers {
			request.Header.Set(key, value)
		}
		return perform(t, server.Client(), request, want)
	}
	return result
}
