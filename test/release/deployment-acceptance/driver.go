// Package deploymentacceptance drives the concrete-site Deployment
// Acceptance leg of the T42 release closure: a real compose install from
// the published Release manifest through the real quoin-deploy binary,
// then the real HTTP exchange (start → helper request export → helper
// report import → finalization receipt) against the running product. The
// receipt never writes back to the release (VERIFY-LAYER-004); callers
// prove that by digesting the release before and after.
package deploymentacceptance

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Suknna/quoin/internal/verification/suites"
)

// InstallRequest drives one real compose install of the published
// release. The admin bootstrap answers travel over the helper's attached
// stdin exactly as the operator path prescribes.
type InstallRequest struct {
	HelperBinary  string
	ConfigPath    string
	ManifestPath  string
	WorkRoot      string
	Project       string
	AdminPassword string
	QuoinPort     int
	StelePort     int
	Stdout        io.Writer
	Stderr        io.Writer
}

// Install runs the staged compose install and waits for the public
// listener; it returns the install report path.
func Install(request InstallRequest) (string, error) {
	report := filepath.Join(request.WorkRoot, request.Project, "install-report.json")
	install := exec.Command(request.HelperBinary, "compose", "install",
		"--config", request.ConfigPath, "--release-manifest", request.ManifestPath, "--report", report)
	install.Env = ComposeEnv(request)
	install.Dir = workDirOf(request.HelperBinary)
	install.Stdout, install.Stderr = request.Stdout, request.Stderr
	if request.AdminPassword != "" {
		install.Stdin = strings.NewReader(strings.Join([]string{"admin", "T42 Site Admin", request.AdminPassword, request.AdminPassword}, "\n") + "\n")
	}
	if err := install.Run(); err != nil {
		return report, fmt.Errorf("compose install: %w", err)
	}
	base := BaseURL(request.QuoinPort)
	deadline := time.Now().Add(300 * time.Second)
	client := &http.Client{Timeout: 5 * time.Second}
	for time.Now().Before(deadline) {
		probe, err := http.NewRequest(http.MethodGet, base+"/api/v1/auth/session", nil)
		if err == nil {
			probe.Header.Set("Origin", "https://quoin.example.com")
			if response, err := client.Do(probe); err == nil {
				response.Body.Close()
				return report, nil
			}
		}
		time.Sleep(2 * time.Second)
	}
	return report, fmt.Errorf("public listener never answered on %s", base)
}

// ComposeEnv is the isolated per-invocation environment of the install.
func ComposeEnv(request InstallRequest) []string {
	return append(os.Environ(),
		"XDG_STATE_HOME="+filepath.Join(request.WorkRoot, request.Project, "state"),
		"QUOIN_COMPOSE_PROJECT="+request.Project,
		"QUOIN_DEPLOY_SCRIPTED=1",
		"DOCKER_CLI_HINTS=false",
	)
}

// BaseURL is the loopback public origin of the installed product.
func BaseURL(quoinPort int) string {
	host := "127.0.0.1"
	if fromEnv := os.Getenv("QUOIN_LOOPBACK_HOST"); fromEnv != "" {
		host = fromEnv
	}
	return "http://" + host + ":" + strconv.Itoa(quoinPort)
}

// AcceptanceRequest drives the Deployment Acceptance HTTP exchange of one
// installed site.
type AcceptanceRequest struct {
	QuoinPort       int
	AdminPassword   string
	HelperBinary    string
	ConfigPath      string
	WorkRoot        string
	Project         string
	ClientCommandID string
	Stdout, Stderr  io.Writer
}

// Receipt is the closed invocation record the leg asserts on.
type Receipt struct {
	InvocationID         string
	ReleaseSubjectDigest string
	DeadlineAt           string
	StartedAt            string
	OverallOutcome       string
	FinalizedAt          string
	HelperExitCode       int
}

// Run drives start → helper request export → real helper verify → report
// import → cancellation-closure finalization, and returns the receipt.
// The helper runs against the real installed deployment; the overall
// outcome is whatever the product deterministically computes.
func Run(request AcceptanceRequest) (*Receipt, error) {
	stack := &suites.Stack{
		Project:       "acceptance",
		WorkRoot:      request.WorkRoot,
		ConfigPath:    request.ConfigPath,
		AdminPassword: request.AdminPassword,
		QuoinPort:     request.QuoinPort,
	}
	session, err := stack.Login("admin", request.AdminPassword)
	if err != nil {
		return nil, fmt.Errorf("admin login: %w", err)
	}
	startBody, status, err := session.Post("/api/v1/deployment-verifications", fmt.Sprintf(`{"clientCommandId":%q}`, request.ClientCommandID))
	if err != nil {
		return nil, err
	}
	if status != http.StatusAccepted {
		return nil, fmt.Errorf("start %d: %s", status, startBody)
	}
	var started struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(startBody), &started); err != nil {
		return nil, err
	}
	detailBody, status, err := session.Get("/api/v1/deployment-verifications/" + started.ID)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("detail %d: %s", status, detailBody)
	}
	var detail struct {
		ReleaseSubjectDigest string `json:"releaseSubjectDigest"`
		DeadlineAt           string `json:"deadlineAt"`
		StartedAt            string `json:"startedAt"`
	}
	if err := json.Unmarshal([]byte(detailBody), &detail); err != nil {
		return nil, err
	}

	requestPath := filepath.Join(request.WorkRoot, "deployment-verification-request.yaml")
	if err := sessionFetch(session, "/api/v1/deployment-verifications/"+started.ID+"/helper-request", requestPath); err != nil {
		return nil, fmt.Errorf("helper request export: %w", err)
	}

	reportPath := filepath.Join(request.WorkRoot, "deployment-verification-report.yaml")
	helper := exec.Command(request.HelperBinary, "compose", "verify",
		"--helper-request", requestPath, "--config", request.ConfigPath, "--report", reportPath)
	helper.Dir = workDirOf(request.HelperBinary)
	// The helper's read-only verifier must target the installed site's
	// project and generated compose file; without this environment it
	// falls back to the default state directory and probes an unrelated
	// network, failing livez on DNS.
	helper.Env = append(os.Environ(),
		"XDG_STATE_HOME="+filepath.Join(request.WorkRoot, request.Project, "state"),
		"QUOIN_COMPOSE_PROJECT="+request.Project,
		"QUOIN_DEPLOY_SCRIPTED=1",
		"DOCKER_CLI_HINTS=false",
	)
	var helperOutput bytes.Buffer
	helper.Stdout, helper.Stderr = &helperOutput, &helperOutput
	helperErr := helper.Run()
	helperCode := 0
	if helperErr != nil {
		if exit, ok := helperErr.(*exec.ExitError); ok {
			helperCode = exit.ExitCode()
		} else {
			return nil, helperErr
		}
	}
	reportBody, err := os.ReadFile(reportPath)
	if err != nil {
		return nil, fmt.Errorf("helper report missing: %w (output: %s)", err, helperOutput.String())
	}
	if status, err := sessionUpload(session, "/api/v1/deployment-verifications/"+started.ID+"/helper-reports", "application/yaml", reportBody); err != nil || (status != http.StatusOK && status != http.StatusCreated) {
		return nil, fmt.Errorf("helper report import %d: %v", status, err)
	}

	closeBody, status, err := session.Post("/api/v1/deployment-verifications/"+started.ID+"/cancel", `{"clientCommandId":"t42-site-close-001"}`)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("closure %d: %s", status, closeBody)
	}
	var closed struct {
		Receipt struct {
			OverallOutcome string `json:"overallOutcome"`
			FinalizedAt    string `json:"finalizedAt"`
		} `json:"receipt"`
	}
	if err := json.Unmarshal([]byte(closeBody), &closed); err != nil {
		return nil, err
	}
	if closed.Receipt.OverallOutcome == "" {
		return nil, fmt.Errorf("closure produced no receipt: %s", closeBody)
	}
	return &Receipt{
		InvocationID:         started.ID,
		ReleaseSubjectDigest: detail.ReleaseSubjectDigest,
		DeadlineAt:           detail.DeadlineAt,
		StartedAt:            detail.StartedAt,
		OverallOutcome:       closed.Receipt.OverallOutcome,
		FinalizedAt:          closed.Receipt.FinalizedAt,
		HelperExitCode:       helperCode,
	}, nil
}

func workDirOf(path string) string {
	directory := filepath.Dir(path)
	for {
		if _, err := os.Stat(filepath.Join(directory, "go.mod")); err == nil {
			return directory
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return directory
		}
		directory = parent
	}
}

// sessionFetch downloads one authenticated endpoint to a file.
func sessionFetch(session *suites.Session, path, destination string) error {
	request, err := http.NewRequest(http.MethodGet, session.Base+path, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Origin", session.Origin)
	if session.Cookie != nil {
		request.AddCookie(session.Cookie)
	}
	response, err := session.Client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		return fmt.Errorf("GET %s: %d: %s", path, response.StatusCode, body)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return err
	}
	return os.WriteFile(destination, body, 0o600)
}

// sessionUpload posts raw bytes with an explicit content type.
func sessionUpload(session *suites.Session, path, contentType string, body []byte) (int, error) {
	request, err := http.NewRequest(http.MethodPost, session.Base+path, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	request.Header.Set("Origin", session.Origin)
	request.Header.Set("Content-Type", contentType)
	if session.Cookie != nil {
		request.AddCookie(session.Cookie)
	}
	response, err := session.Client.Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	payload, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if response.StatusCode >= 400 {
		return response.StatusCode, fmt.Errorf("%s", payload)
	}
	return response.StatusCode, nil
}
