package deploymentacceptance

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Suknna/quoin/internal/contract"
	"github.com/Suknna/quoin/internal/quoin/app"
	"github.com/Suknna/quoin/internal/quoin/artifact"
	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/bootstrap"
)

type ticket38Stack struct {
	server     *httptest.Server
	cookie     string
	database   *bootstrap.Database
	binding    *contract.DeploymentBinding
	rebindFunc func(*contract.DeploymentBinding)
}

// setConfigDigest re-freezes the deployment config digest before any
// invocation starts (the stack is constructed first, the fixture digest
// known afterwards).
func (stack *ticket38Stack) setConfigDigest(t *testing.T, digest string) {
	t.Helper()
	stack.binding.DeploymentConfigDigest = digest
	stack.rebindFunc(stack.binding)
}

func newTicket38Stack(t *testing.T) *ticket38Stack {
	t.Helper()
	ctx := t.Context()
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
	authService, err := auth.NewService(database.SQL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authService.CreateFirstAdmin(ctx, "admin", "Quoin Admin", adminPassword); err != nil {
		t.Fatal(err)
	}
	application := app.NewAPIServer(authService, database.SQL, config.RootKeyFile)
	binding := &contract.DeploymentBinding{
		ReleaseVersion:          "v1.2.3",
		ReleaseSubjectDigest:    "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		DeploymentConfigDigest:  "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Backend:                 "compose",
		Architecture:            "linux/amd64",
		BrowserChromiumRevision: "1200.0.6099.109",
	}
	application.SetDeploymentVerificationBinding(binding, config.PublicOrigin)
	store, err := artifact.NewStore(database.SQL, filepath.Join(config.DataDirectory, "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	application.WireVerificationArtifacts(store)
	handler, err := app.NewHandler(application, config.PublicOrigin)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	stack := &ticket38Stack{server: server, database: database, binding: binding,
		rebindFunc: func(updated *contract.DeploymentBinding) {
			application.SetDeploymentVerificationBinding(updated, config.PublicOrigin)
		}}
	stack.cookie = stack.login(t, adminPassword, replacementPassword)
	t.Cleanup(func() { database.Close() })
	return stack
}

func (stack *ticket38Stack) close() {}

func (stack *ticket38Stack) login(t *testing.T, current, replacement string) string {
	t.Helper()
	cookie := stack.cookieOf(t)
	if _, status := stack.do(t, http.MethodPut, `/api/v1/auth/password`, stack.headers(map[string]string{"Content-Type": "application/json"}), fmt.Sprintf(`{"currentPassword":%q,"newPassword":%q}`, current, replacement), cookie); status != http.StatusNoContent {
		t.Fatalf("password change %d", status)
	}
	return cookie
}

func (stack *ticket38Stack) cookieOf(t *testing.T) string {
	t.Helper()
	loginRequest, err := http.NewRequest(http.MethodPost, stack.server.URL+"/api/v1/auth/login", strings.NewReader(fmt.Sprintf(`{"username":"admin","password":%q}`, adminPassword)))
	if err != nil {
		t.Fatal(err)
	}
	loginRequest.Header.Set("Content-Type", "application/json")
	loginRequest.Header.Set("Origin", "https://quoin.example.test")
	response, err := stack.server.Client().Do(loginRequest)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	cookie := response.Header.Get("Set-Cookie")
	if !strings.HasPrefix(cookie, "__Host-quoin-session=") {
		t.Fatalf("no session cookie: %q", cookie)
	}
	return strings.SplitN(strings.Split(cookie, ";")[0], "=", 2)[1]
}

func (stack *ticket38Stack) headers(extra map[string]string) map[string]string {
	headers := map[string]string{"Content-Type": "application/json", "Origin": "https://quoin.example.test"}
	for key, value := range extra {
		headers[key] = value
	}
	return headers
}

func (stack *ticket38Stack) do(t *testing.T, method, path string, headers map[string]string, body, cookie string) (string, int) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request, err := http.NewRequest(method, stack.server.URL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	if cookie != "" {
		request.Header.Set("Cookie", "__Host-quoin-session="+cookie)
	}
	response, err := stack.server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(response.Body)
	return string(raw), response.StatusCode
}

func (stack *ticket38Stack) start(t *testing.T, commandID string) string {
	t.Helper()
	body, status := stack.do(t, http.MethodPost, `/api/v1/deployment-verifications`, stack.headers(nil), fmt.Sprintf(`{"clientCommandId":%q}`, commandID), stack.cookie)
	if status != http.StatusAccepted {
		t.Fatalf("start %d: %s", status, body)
	}
	var detail struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(body), &detail); err != nil {
		t.Fatal(err)
	}
	return detail.ID
}

func (stack *ticket38Stack) itemCount(t *testing.T, invocationID string) int {
	t.Helper()
	body, status := stack.do(t, http.MethodGet, `/api/v1/deployment-verifications/`+invocationID, stack.headers(nil), "", stack.cookie)
	if status != http.StatusOK {
		t.Fatalf("detail %d", status)
	}
	var detail struct {
		ItemCount int `json:"itemCount"`
	}
	if err := json.Unmarshal([]byte(body), &detail); err != nil {
		t.Fatal(err)
	}
	return detail.ItemCount
}

func (stack *ticket38Stack) helperRequest(t *testing.T, invocationID string) ([]byte, string) {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, stack.server.URL+"/api/v1/deployment-verifications/"+invocationID+"/helper-request", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Cookie", "__Host-quoin-session="+stack.cookie)
	request.Header.Set("Origin", "https://quoin.example.test")
	response, err := stack.server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("helper request %d: %s", response.StatusCode, body)
	}
	return body, response.Header.Get("X-Quoin-Request-Digest")
}

func (stack *ticket38Stack) importReport(t *testing.T, invocationID string, report []byte) int {
	t.Helper()
	_, status := stack.do(t, http.MethodPost, `/api/v1/deployment-verifications/`+invocationID+`/helper-reports`, stack.headers(map[string]string{"Content-Type": "application/yaml"}), string(report), stack.cookie)
	return status
}

func (stack *ticket38Stack) importReportRaw(t *testing.T, invocationID string, report []byte) int {
	t.Helper()
	return stack.importReport(t, invocationID, report)
}

// finalize cancels the remaining ui items (not_run) and returns the receipt
// overall outcome.
func (stack *ticket38Stack) finalize(t *testing.T, invocationID string) string {
	t.Helper()
	body, status := stack.do(t, http.MethodPost, `/api/v1/deployment-verifications/`+invocationID+`/cancel`, stack.headers(nil), `{"clientCommandId":"ticket38-close-01"}`, stack.cookie)
	if status != http.StatusOK {
		stack.diagnose(t, invocationID)
		t.Fatalf("finalize %d: %s", status, body)
	}
	var detail struct {
		Receipt struct {
			OverallOutcome string `json:"overallOutcome"`
		} `json:"receipt"`
	}
	if err := json.Unmarshal([]byte(body), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Receipt.OverallOutcome == "" {
		t.Fatalf("cancel did not finalize: %s", body)
	}
	return detail.Receipt.OverallOutcome
}

// diagnose prints the receipt-closure trigger arms for the invocation.
func (stack *ticket38Stack) diagnose(t *testing.T, invocationID string) {
	t.Helper()
	rows, err := stack.database.SQL.Query(`SELECT r.id,i.object_kind,r.outcome,r.category,r.observed_at,r.committed_at FROM verification_item_results r JOIN verification_invocation_items i ON i.id=r.item_id WHERE i.invocation_id=? ORDER BY r.id`, invocationID)
	if err != nil {
		t.Logf("diagnose results: %v", err)
		return
	}
	for rows.Next() {
		var id int64
		var kind, outcome, category, observed, committed string
		if err := rows.Scan(&id, &kind, &outcome, &category, &observed, &committed); err != nil {
			t.Logf("diagnose scan: %v", err)
		} else {
			t.Logf("result id=%d kind=%s outcome=%s category=%s observed=%s committed=%s", id, kind, outcome, category, observed, committed)
		}
	}
	rows.Close()
	var started, deadline, latest string
	if err := stack.database.SQL.QueryRow(`SELECT started_at,deadline_at,(SELECT MAX(observed_at) FROM verification_item_results r JOIN verification_invocation_items i ON i.id=r.item_id WHERE i.invocation_id=?) FROM verification_invocation_manifests WHERE id=?`, invocationID, invocationID).Scan(&started, &deadline, &latest); err != nil {
		t.Logf("diagnose manifest: %v", err)
	} else {
		t.Logf("manifest started=%s deadline=%s latestObserved=%s", started, deadline, latest)
	}
	var imports, observations, conflicts, drifts, artifacts int
	_ = stack.database.SQL.QueryRow(`SELECT (SELECT COUNT(*) FROM verification_helper_imports WHERE invocation_id=?),(SELECT COUNT(*) FROM verification_typed_observations),(SELECT COUNT(*) FROM verification_result_conflicts),(SELECT COUNT(*) FROM verification_subject_drifts WHERE invocation_id=?),(SELECT COUNT(*) FROM artifacts WHERE owner_type='verification_invocation' AND owner_id=?)`, invocationID, invocationID, invocationID).Scan(&imports, &observations, &conflicts, &drifts, &artifacts)
	t.Logf("imports=%d observations=%d conflicts=%d drifts=%d artifacts=%d", imports, observations, conflicts, drifts, artifacts)
}
