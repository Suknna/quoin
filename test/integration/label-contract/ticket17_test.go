package labelcontract

// TestTicket17 proves the T17 capability over the real compose stack:
// attribution baseline before any contract, the readiness projection with
// blockers, the atomic activation race and failure rollback, the enabled
// publish projection, and alert attribution + live filter behavior for
// known/unknown/missing business_system values through real Stele relays.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The zero-check enabled draft: verification runs to Passed inside the
// create command (the PromQL executor lands with T18).
const enabledZeroCheckYAML = `system_key: payments
display_name: 支付系统
enabled: true
timezone: Asia/Shanghai
resource_refresh_interval_seconds: 300
resource_discoveries: []
inspection_plans: []
`

const contractYAML = "label_contract:\n  business_system_label: business_system\n"

const enabledCheckYAML = `system_key: payments
display_name: 支付系统
enabled: true
timezone: Asia/Shanghai
resource_refresh_interval_seconds: 302
resource_discoveries: []
inspection_plans:
  - key: queued-check
    display_name: Queued Check
    cron: "0 * * * *"
    checks:
      - key: up-instant
        display_name: Up Instant
        analysis_question: 当前可用吗？
        kind: promql
        query:
          mode: instant
          expression: 'up{business_system="payments"}'
`

func TestTicket17(t *testing.T) {
	evidenceDir := os.Getenv("QUOIN_EVIDENCE_DIR")
	if evidenceDir == "" {
		t.Skip("QUOIN_EVIDENCE_DIR not set; T17 acceptance run disabled")
	}
	if err := os.MkdirAll(evidenceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	requireDocker(t)
	evidence := &ticketEvidence{dir: evidenceDir, env: os.Environ(), imageIDs: map[string]string{}}
	evidence.env = append(evidence.env, "QUOIN_FORCE_IMAGE_BUILD=1")
	imageProxy := os.Getenv("QUOIN_IMAGE_GOPROXY")
	if imageProxy == "" {
		imageProxy = strings.TrimSpace(outputOf(t, "go", "env", "GOPROXY"))
	}
	if imageProxy != "" {
		evidence.env = append(evidence.env, "QUOIN_IMAGE_GOPROXY="+imageProxy)
	}
	workRoot := t.TempDir()
	// quoin-deploy deliberately owns the stable production project name `quoin`.
	// Its compose topology is still tested here, but a test-local Docker shim
	// maps that name to a process-unique project so concurrent/previous ticket
	// stacks cannot share the internal network or be affected by teardown.
	fixtureRunID := randomFixtureRunID(t)
	projectName := "quoin-t17-" + fixtureRunID
	imageNamespace := projectName + "-image"
	fixtureLabel := "com.quoin.fixture=" + fixtureRunID
	imageOverride := filepath.Join(workRoot, "fixture-images.compose.yaml")
	writeFile(t, imageOverride, fmt.Sprintf(`services:
  secret-bootstrap: { image: %s/quoin:v0.1.0-dev, labels: { com.quoin.fixture: %q } }
  admin-bootstrap: { image: %s/quoin:v0.1.0-dev, labels: { com.quoin.fixture: %q } }
  quoin: { image: %s/quoin:v0.1.0-dev, labels: { com.quoin.fixture: %q } }
  plinth: { image: %s/plinth:v0.1.0-dev, labels: { com.quoin.fixture: %q } }
  lintel: { image: %s/lintel:v0.1.0-dev, labels: { com.quoin.fixture: %q } }
  stele: { image: %s/stele:v0.1.0-dev, labels: { com.quoin.fixture: %q } }
`, imageNamespace, fixtureRunID, imageNamespace, fixtureRunID, imageNamespace, fixtureRunID, imageNamespace, fixtureRunID, imageNamespace, fixtureRunID, imageNamespace, fixtureRunID))
	shimDirectory := dockerProjectShim(t, filepath.Join(workRoot, "docker-shim"), defaultProjectName, projectName, imageNamespace, imageOverride, fixtureLabel)
	evidence.dockerPath = filepath.Join(shimDirectory, "docker")
	evidence.env = replaceEnvironment(evidence.env, "PATH", shimDirectory+string(os.PathListSeparator)+os.Getenv("PATH"))
	stateRoot := filepath.Join(workRoot, "state")
	evidence.stateRoot = stateRoot
	secretDir := filepath.Join(workRoot, "secrets")
	evidence.env = replaceEnvironment(evidence.env, "XDG_STATE_HOME", stateRoot)

	evidence.run(t, "build-helper", nil, "go", "build", "-trimpath", "-o", filepath.Join(workRoot, "quoin-deploy"), "./cmd/quoin-deploy")
	// The relay path drives deliveries through the same SteleRelay gRPC
	// interface Stele uses (Runtime gRPC is never host-published, OPS-NET-004).
	relayLinux := filepath.Join(workRoot, "relayclient-linux")
	evidence.runWithEnv(t, "build-relayclient-linux", nil, []string{"GOOS=linux", "CGO_ENABLED=0"}, "go", "build", "-trimpath", "-o", relayLinux, "./cmd/relayclient")
	images := []string{imageNamespace + "/quoin:v0.1.0-dev", imageNamespace + "/plinth:v0.1.0-dev", imageNamespace + "/lintel:v0.1.0-dev", imageNamespace + "/stele:v0.1.0-dev"}
	for _, image := range images {
		if err := exec.Command("docker", "image", "inspect", image).Run(); err == nil {
			t.Fatalf("refusing to replace pre-existing ticket fixture image %s", image)
		}
	}
	composeFile := filepath.Join(stateRoot, "quoin", "compose", "generated", "compose.yaml")
	internalNetwork := projectName + "_internal"
	sharedInternalNetwork := defaultProjectName + "_internal"
	networkBeforeRaw, networkBeforeCode := evidence.allowFailure(t, "network-before", nil, "docker", "network", "inspect", "--format", "{{range $id, $container := .Containers}}{{$id}}={{$container.Name}}{{println}}{{end}}", internalNetwork)
	networkBefore, err := parseDockerNetworkSnapshot(networkBeforeCode, networkBeforeRaw)
	if err != nil {
		t.Fatal(err)
	}
	if networkBefore.Exists {
		t.Fatalf("refusing to reuse pre-existing ticket fixture network %s", internalNetwork)
	}
	sharedNetworkBeforeRaw, sharedNetworkBeforeCode := evidence.allowFailure(t, "shared-network-before", nil, "docker", "network", "inspect", "--format", "{{range $id, $container := .Containers}}{{$id}}={{$container.Name}}{{println}}{{end}}", sharedInternalNetwork)
	sharedNetworkBefore, err := parseDockerNetworkSnapshot(sharedNetworkBeforeCode, sharedNetworkBeforeRaw)
	if err != nil {
		t.Fatal(err)
	}
	var tempPassword, newPassword, revealedBearer string
	runtimeEvidence := map[string]any{
		"startedAt": time.Now().UTC().Format(time.RFC3339),
		"outcome":   "incomplete",
		"components": map[string]any{
			"deployHelper":       "cmd/quoin-deploy (go build -trimpath, host binary)",
			"composeProject":     projectName,
			"fixtureRunID":       fixtureRunID,
			"relayClient":        "cmd/relayclient (linux static, GOOS=linux CGO_ENABLED=0) over SteleRelay gRPC inside " + internalNetwork,
			"projectIsolation":   "test-local Docker shim rewrites only quoin-deploy's stable production compose project name and local image tags",
			"imageNamespace":     imageNamespace,
			"sharedNetworkGuard": sharedInternalNetwork,
		},
		"redactions": "admin passwords and the alert bearer are not written to evidence",
	}

	// Register before building images or installing: every owned resource is
	// cleaned even if setup aborts halfway. The failure-tolerant commands retain
	// raw teardown evidence and only fail the test after all cleanup was tried.
	t.Cleanup(func() {
		cleanup := map[string]any{"timestamp": time.Now().UTC().Format(time.RFC3339), "steps": []map[string]any{}}
		steps := cleanup["steps"].([]map[string]any)
		cleanupFailed := false
		fixtureOwnsContainer := func(id string) bool {
			label, code := evidence.allowFailure(t, "teardown-container-label-"+id, nil, "docker", "inspect", "--format", "{{ index .Config.Labels \"com.quoin.fixture\" }}", id)
			return code == 0 && strings.TrimSpace(label) == fixtureRunID
		}
		if _, err := os.Stat(composeFile); err == nil {
			containerIDs, code := evidence.allowFailure(t, "teardown-project-containers", nil, "docker", "ps", "-aq", "--filter", "label=com.docker.compose.project="+projectName)
			owned := code == 0
			for _, id := range strings.Fields(containerIDs) {
				owned = owned && fixtureOwnsContainer(id)
			}
			if !owned {
				steps = append(steps, map[string]any{"name": "compose down --remove-orphans", "status": "ownership-mismatch"})
				cleanupFailed = true
			} else {
				_, code := evidence.allowFailure(t, "teardown-stack", nil, "docker", "compose", "--project-name", projectName, "--file", composeFile, "down", "--remove-orphans")
				steps = append(steps, map[string]any{"name": "compose down --remove-orphans", "exitCode": code, "ownership": "verified"})
				cleanupFailed = cleanupFailed || code != 0
			}
			out, code := evidence.allowFailure(t, "teardown-projection", nil, "docker", "compose", "--project-name", projectName, "--file", composeFile, "ps", "--all", "--format", "json")
			projection := strings.TrimSpace(out)
			steps = append(steps, map[string]any{"name": "compose ps --all", "exitCode": code, "output": projection})
			// Docker Compose v2 prints an empty stream (rather than `[]`) when a
			// project has no remaining containers; both forms prove the same empty
			// owned projection.
			cleanupFailed = cleanupFailed || code != 0 || (projection != "" && projection != "[]")
		} else {
			steps = append(steps, map[string]any{"name": "compose down --remove-orphans", "status": "not-created"})
		}
		networkAfterRaw, networkAfterCode := evidence.allowFailure(t, "teardown-network", nil, "docker", "network", "inspect", "--format", "{{range $id, $container := .Containers}}{{$id}}={{$container.Name}}{{println}}{{end}}", internalNetwork)
		networkAfter, networkErr := parseDockerNetworkSnapshot(networkAfterCode, networkAfterRaw)
		networkStep := map[string]any{"name": "owned internal network cleanup", "network": internalNetwork, "before": networkBefore, "after": networkAfter}
		if networkErr != nil {
			cleanupFailed = true
			networkStep["status"] = "inspect-failed"
			networkStep["error"] = networkErr.Error()
		} else if networkBefore.Exists {
			networkStep["expected"] = "pre-existing attachments preserved exactly; no test-owned attachment remains"
			if !sameStrings(networkBefore.Attachments, networkAfter.Attachments) {
				cleanupFailed = true
				networkStep["status"] = "mismatch"
			} else {
				networkStep["status"] = "preserved"
			}
		} else {
			networkStep["expected"] = "network removed after all test-owned containers stopped"
			if networkAfter.Exists {
				cleanupFailed = true
				networkStep["status"] = "leaked"
			} else {
				networkStep["status"] = "removed"
			}
		}
		steps = append(steps, networkStep)
		sharedNetworkAfterRaw, sharedNetworkAfterCode := evidence.allowFailure(t, "teardown-shared-network", nil, "docker", "network", "inspect", "--format", "{{range $id, $container := .Containers}}{{$id}}={{$container.Name}}{{println}}{{end}}", sharedInternalNetwork)
		sharedNetworkAfter, sharedNetworkErr := parseDockerNetworkSnapshot(sharedNetworkAfterCode, sharedNetworkAfterRaw)
		sharedNetworkStep := map[string]any{"name": "shared network preservation", "network": sharedInternalNetwork, "before": sharedNetworkBefore, "after": sharedNetworkAfter, "expected": "unrelated pre-existing network and its attachments are unchanged"}
		if sharedNetworkErr != nil {
			cleanupFailed = true
			sharedNetworkStep["status"] = "inspect-failed"
			sharedNetworkStep["error"] = sharedNetworkErr.Error()
		} else if sharedNetworkBefore.Exists != sharedNetworkAfter.Exists || !sameStrings(sharedNetworkBefore.Attachments, sharedNetworkAfter.Attachments) {
			cleanupFailed = true
			sharedNetworkStep["status"] = "mismatch"
		} else {
			sharedNetworkStep["status"] = "preserved"
		}
		steps = append(steps, sharedNetworkStep)
		for _, image := range images {
			name := "teardown-image-" + strings.NewReplacer("/", "_", ":", "_").Replace(image)
			label, inspectCode := evidence.allowFailure(t, name+"-label", nil, "docker", "image", "inspect", "--format", "{{ index .Config.Labels \"com.quoin.fixture\" }}", image)
			if inspectCode != 0 || strings.TrimSpace(label) != fixtureRunID {
				steps = append(steps, map[string]any{"name": "docker rmi " + image, "status": "ownership-mismatch"})
				cleanupFailed = true
				continue
			}
			_, code := evidence.allowFailure(t, name, nil, "docker", "rmi", image)
			steps = append(steps, map[string]any{"name": "docker rmi " + image, "exitCode": code, "ownership": "verified"})
			cleanupFailed = cleanupFailed || code != 0
		}
		cleanup["steps"] = steps
		cleanup["safety"] = "temporary XDG_STATE_HOME, compose data, secrets and install config are removed with the test temp root; relayclient containers run with --rm"
		secretScanErr := scanForSecrets(evidenceDir, tempPassword, newPassword, revealedBearer)
		if secretScanErr != nil {
			cleanup["secretScan"] = secretScanErr.Error()
		} else {
			cleanup["secretScan"] = "passed"
		}
		evidence.note(t, "cleanup.json", mustJSON(t, cleanup))
		if cleanupFailed {
			t.Error("T17 cleanup failed; inspect cleanup.json and teardown logs")
		}
		if secretScanErr != nil {
			t.Error(secretScanErr)
		}
		runtimeEvidence["commands"] = evidence.commands
		runtimeEvidence["artifacts"] = evidence.artifacts
		runtimeEvidence["outcome"] = map[bool]string{true: "failed", false: "passed"}[t.Failed()]
		if err := os.WriteFile(filepath.Join(evidenceDir, "runtime-evidence.json"), []byte(mustJSON(t, runtimeEvidence)), 0o644); err != nil {
			t.Error(err)
		}
	})

	evidence.run(t, "images", nil, "bash", "build/package/images.sh")

	installConfig := filepath.Join(workRoot, "install.yaml")
	writeFile(t, installConfig, fmt.Sprintf("document: compose-install\npublicOrigin: https://quoin.example.com\npublishMode: loopback\nquoinPublicHostPort: %d\nsteleWebhookHostPort: %d\nsecretDirectory: %s\nlintelBrowserSlots: 1\nlintelShmSizeBytes: 1073741824\n", quoinPort, stelePort, secretDir))
	tempPassword = randomSecret(t, 24)
	adminInput := strings.NewReader(strings.Join([]string{"admin", "Ticket Admin", tempPassword, tempPassword}, "\n") + "\n")
	evidence.env = append(evidence.env, "QUOIN_DEPLOY_SCRIPTED=1")
	evidence.run(t, "install", adminInput, filepath.Join(workRoot, "quoin-deploy"), "compose", "install", "--config", installConfig)

	for _, image := range images {
		if digest, err := exec.Command("docker", "image", "inspect", image, "--format", "{{.Id}}").Output(); err == nil {
			evidence.imageIDs[image] = strings.TrimSpace(string(digest))
		}
	}

	base := fmt.Sprintf("http://127.0.0.1:%d", quoinPort)
	origin := "https://quoin.example.com"
	client := cookieClient(t)
	var login string
	if !awaitCondition(60*time.Second, func() bool {
		login = httpPostExpect(t, client, base+"/api/v1/auth/login", origin, fmt.Sprintf(`{"username":"admin","password":"%s"}`, tempPassword), 0)
		return strings.Contains(login, `"passwordChangeRequired":true`)
	}) {
		t.Fatalf("first login must require password change: %.300s", login)
	}
	newPassword = randomSecret(t, 24)
	httpPutExpect(t, client, base+"/api/v1/auth/password", origin, fmt.Sprintf(`{"currentPassword":"%s","newPassword":"%s"}`, tempPassword, newPassword), 0)

	observed := map[string]any{}
	expectations := map[string]string{}
	recordExpectation := func(family, expected, actual string) {
		expectations[family] = "expected=" + expected + "; actual=" + actual
	}

	// --- Alert source + relay machinery (real Stele credentials path) ------
	metaJSON := httpPostExpect(t, client, base+"/api/v1/alert-sources", origin, `{"key":"t17-am","protocol":"alertmanager","clientCommandId":"t17-cmd-0001"}`, 201)
	var metaObj struct {
		SourceKey    string `json:"sourceKey"`
		CredentialID string `json:"credentialId"`
		RevealHandle string `json:"revealHandle"`
	}
	if err := json.Unmarshal([]byte(metaJSON), &metaObj); err != nil {
		t.Fatal(err)
	}
	revealJSON := httpPostExpect(t, client, base+"/api/v1/alert-sources/credentials/reveal", origin, fmt.Sprintf(`{"revealHandle":%q}`, metaObj.RevealHandle), 0)
	var revealObj struct {
		BearerToken string `json:"bearerToken"`
	}
	if err := json.Unmarshal([]byte(revealJSON), &revealObj); err != nil {
		t.Fatal(err)
	}
	revealedBearer = revealObj.BearerToken
	sourcesList := httpGet(t, client, base+"/api/v1/alert-sources", origin)
	var sourcesObj struct {
		Items []struct {
			Key string `json:"key"`
			ID  string `json:"id"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(sourcesList), &sourcesObj); err != nil {
		t.Fatal(err)
	}
	sourceID := ""
	for _, item := range sourcesObj.Items {
		if item.Key == "t17-am" {
			sourceID = item.ID
		}
	}
	if sourceID == "" {
		t.Fatalf("source id not found: %.400s", sourcesList)
	}
	relay := func(name, relayID string, body []byte) string {
		bodyPath := filepath.Join(workRoot, name+".json")
		if err := os.WriteFile(bodyPath, body, 0o600); err != nil {
			t.Fatal(err)
		}
		return evidence.run(t, name, nil, "docker", "run", "--rm",
			"--network", internalNetwork, "--user", "0:0", "--entrypoint", "/relayclient",
			"-v", relayLinux+":/relayclient:ro",
			"-v", filepath.Join(secretDir, "runtime-ca.pem")+":/ca.pem:ro",
			"-v", filepath.Join(secretDir, "stele-service-token")+":/token:ro",
			"-v", bodyPath+":/body.json:ro",
			"quoin/quoin:v0.1.0-dev",
			"-endpoint", "quoin:8443", "-ca", "/ca.pem", "-token", "/token",
			"-relay-id", relayID, "-source", sourceID,
			"-credential", metaObj.CredentialID, "-snapshot", "1", "-body", "/body.json")
	}

	// Live SSE consumer must attach before any delivery; otherwise replay from
	// after=0 could make an absent live frame look like a live delivery.
	cookieValue := sessionCookieOf(t, client, base)
	recorder := &streamRecorder{}
	streamCtx := t.Context()
	streamReady := make(chan int, 1)
	go consumeSSE(streamCtx, base, cookieValue, "0", recorder, streamReady)
	select {
	case status := <-streamReady:
		if status != 200 {
			t.Fatalf("SSE consumer must attach with 200, got %d", status)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("SSE consumer did not attach within 15 seconds")
	}

	// --- 1) Pre-contract attribution baseline ------------------------------
	preLabels := map[string]string{"alertname": "T17Pre", "severity": "warning", "business_system": "payments"}
	out := relay("t17-pre", "t17-r-pre", webhookBodyJSON(t, "firing", preLabels, "2026-09-07T09:00:00Z"))
	if !strings.Contains(out, "DELIVERY_STATUS_ACCEPTED") {
		t.Fatalf("pre-contract relay: %.300s", out)
	}
	preSnapshot := httpGet(t, client, base+"/api/v1/alerts?state=Firing", origin)
	if !strings.Contains(preSnapshot, "T17Pre") || strings.Contains(preSnapshot, `"businessSystemKey":"payments"`) {
		t.Fatalf("pre-contract occurrence must exist unattributed: %.500s", preSnapshot)
	}
	observed["preContractOccurrence"] = "T17Pre firing, businessSystemKey absent"
	recordExpectation("pre-contract-attribution", "未归属 (no active contract)", "snapshot row carries no businessSystemKey")

	// --- 2) Real enabled-system path: v1 activation → v1 publish → v2 ------
	// An initial YAML upload only creates a Disabled aggregate. To make the
	// v2 readiness projection meaningful, first publish an enabled v1 under the
	// active contract, then upload the v2 draft targeting the new contract.
	status, body := uploadMultipart(t, client, base+"/api/v1/label-contracts", origin,
		map[string]string{"clientCommandId": "t17-contract-1"}, "file", "contract.yaml", []byte(contractYAML))
	if status != 201 {
		t.Fatalf("v1 contract upload: status=%d body=%.500s", status, body)
	}
	httpPostExpect(t, client, base+"/api/v1/label-contracts/1/activate", origin,
		`{"clientCommandId":"t17-activate-0","expectedStateRowVersion":1,"expectedCurrentContractVersionId":null,"expectedTargetRowVersion":1,"compatibleVersions":[]}`, 200)

	status, body = uploadMultipart(t, client, base+"/api/v1/business-systems", origin,
		map[string]string{"clientCommandId": "t17-upload-v1", "targetLabelContractVersion": "1"}, "file", "system.yaml", []byte(enabledZeroCheckYAML))
	if status != 201 {
		t.Fatalf("v1 enabled upload: status=%d body=%.500s", status, body)
	}
	var v1Version struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(body), &v1Version); err != nil {
		t.Fatal(err)
	}
	publishedV1 := httpPostExpect(t, client, base+"/api/v1/business-systems/payments/config/"+v1Version.ID+"/publish", origin,
		`{"clientCommandId":"t17-publish-v1","expectedCurrentPublishedVersionId":null}`, 200)
	if !strings.Contains(publishedV1, `"enabled":true`) || !strings.Contains(publishedV1, `"rowVersion":2`) {
		t.Fatalf("v1 publish must enable the aggregate at row version 2: %.500s", publishedV1)
	}

	status, body = uploadMultipart(t, client, base+"/api/v1/label-contracts", origin,
		map[string]string{"clientCommandId": "t17-contract-2"}, "file", "contract.yaml", []byte(contractYAML))
	if status != 201 {
		t.Fatalf("v2 contract upload: status=%d body=%.500s", status, body)
	}
	v2YAML := strings.Replace(enabledZeroCheckYAML, "resource_refresh_interval_seconds: 300", "resource_refresh_interval_seconds: 301", 1)
	status, body = uploadMultipart(t, client, base+"/api/v1/business-systems", origin,
		map[string]string{"clientCommandId": "t17-upload-v2", "targetLabelContractVersion": "2"}, "file", "system.yaml", []byte(v2YAML))
	if status != 201 {
		t.Fatalf("v2 enabled upload: status=%d body=%.500s", status, body)
	}
	var versionObj struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(body), &versionObj); err != nil {
		t.Fatal(err)
	}

	// The HTTP command shape is closed: missing/wrong purpose and unknown
	// members must not create an executable run.
	for name, payload := range map[string]string{
		"missing-purpose": `{"clientCommandId":"t17-missing-purpose"}`,
		"wrong-purpose":   `{"clientCommandId":"t17-invalid-purpose","purpose":"deployment_acceptance"}`,
		"unknown-member":  `{"clientCommandId":"t17-invalid-member","purpose":"prepublish","unexpected":true}`,
	} {
		code, response, headers := postResponse(client, base+"/api/v1/business-systems/payments/config/"+versionObj.ID+"/verifications", origin, payload)
		if code != http.StatusUnprocessableEntity {
			t.Fatalf("verification command %s must return 422, got %d: %.500s", name, code, response)
		}
		if contentType := headers.Get("Content-Type"); !strings.HasPrefix(contentType, "application/problem+json") {
			t.Fatalf("verification command %s content type=%q, want application/problem+json", name, contentType)
		}
		var rejection struct {
			Code        string `json:"code"`
			Message     string `json:"message"`
			Retryable   *bool  `json:"retryable"`
			FieldErrors []struct {
				Path string `json:"path"`
			} `json:"fieldErrors"`
		}
		if err := json.Unmarshal([]byte(response), &rejection); err != nil {
			t.Fatal(err)
		}
		if rejection.Code != "validation_failed" || rejection.Message == "" || rejection.Retryable == nil || len(rejection.FieldErrors) == 0 || strings.Contains(response, `"value"`) {
			t.Fatalf("verification command %s must use frozen redacted ErrorModel: %.500s", name, response)
		}
		observed["verificationReject_"+name] = fmt.Sprintf("status=%d code=%s", code, rejection.Code)
	}

	// --- 3) Readiness before verification: blocker set ----------------------
	readinessBlocked := httpGet(t, client, base+"/api/v1/label-contracts/2/readiness", origin)
	for _, fragment := range []string{`"businessSystemKey":"payments"`, `"blockers":["verification_run_missing"]`, `"activationCandidates":[]`, `"targetContractVersion":2`} {
		if !strings.Contains(readinessBlocked, fragment) {
			t.Fatalf("blocked readiness missing %s: %.600s", fragment, readinessBlocked)
		}
	}
	observed["readinessBlocked"] = json.RawMessage(readinessBlocked)

	// --- 4) Contract-shaped validation and failure rollback -----------------
	// Invalid command grammar must be rejected before it can enter the command
	// ledger; an empty nullable locator must not silently mean null.
	code, malformedBody := postStatus(client, base+"/api/v1/label-contracts/2/activate", origin,
		`{"clientCommandId":"bad id","expectedStateRowVersion":2,"expectedCurrentContractVersionId":"","expectedTargetRowVersion":1,"compatibleVersions":[]}`)
	if code != 422 {
		t.Fatalf("malformed activation command must 422, got %d: %.500s", code, malformedBody)
	}
	observed["activationValidation"] = json.RawMessage(malformedBody)

	code, conflictBody := postStatus(client, base+"/api/v1/label-contracts/2/activate", origin,
		`{"clientCommandId":"t17-activate-bad","expectedStateRowVersion":2,"expectedCurrentContractVersionId":"1","expectedTargetRowVersion":1,"compatibleVersions":[{"businessSystemKey":"payments","configVersionId":"`+versionObj.ID+`","verificationRunId":"999999","expectedCurrentConfigVersionId":"`+v1Version.ID+`","expectedBusinessSystemRowVersion":2}]}`)
	if code != 409 {
		t.Fatalf("bogus evidence activation must 409, got %d: %.500s", code, conflictBody)
	}
	observed["activationRollbackConflict"] = json.RawMessage(conflictBody)
	readinessAfterRollback := httpGet(t, client, base+"/api/v1/label-contracts/2/readiness", origin)
	for _, fragment := range []string{`"blockers":["verification_run_missing"]`, `"activationCandidates":[]`, `"targetContractVersion":2`} {
		if !strings.Contains(readinessAfterRollback, fragment) {
			t.Fatalf("failed activation changed readiness; missing %s: %.600s", fragment, readinessAfterRollback)
		}
	}
	observed["activationRollbackReadback"] = json.RawMessage(readinessAfterRollback)

	// --- 5) Verification run: 202 + deterministic Passed -------------------
	runBody := httpPostExpect(t, client, base+"/api/v1/business-systems/payments/config/"+versionObj.ID+"/verifications", origin,
		`{"clientCommandId":"t17-verify-1","purpose":"prepublish"}`, 202)
	for _, fragment := range []string{`"purpose":"prepublish"`, `"state":"Passed"`, `"rowVersion":3`, `"checkResults":[]`} {
		if !strings.Contains(runBody, fragment) {
			t.Fatalf("verification run missing %s: %.500s", fragment, runBody)
		}
	}
	var runObj struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(runBody), &runObj); err != nil {
		t.Fatal(err)
	}
	observed["verificationRun"] = json.RawMessage(runBody)
	runHistory := httpGet(t, client, base+"/api/v1/business-systems/payments/config/"+versionObj.ID+"/verifications", origin)
	if !strings.Contains(runHistory, `"state":"Passed"`) {
		t.Fatalf("run history missing Passed: %.400s", runHistory)
	}

	// Readiness now exposes the exact candidate pair.
	readinessReady := httpGet(t, client, base+"/api/v1/label-contracts/2/readiness", origin)
	if !strings.Contains(readinessReady, `"blockers":[]`) || !strings.Contains(readinessReady, `"configVersionId":"`+versionObj.ID+`"`) || !strings.Contains(readinessReady, `"passedVerificationRunId":"`+runObj.ID+`"`) {
		t.Fatalf("ready readiness missing candidate pair: %.600s", readinessReady)
	}
	observed["readinessReady"] = json.RawMessage(readinessReady)
	recordExpectation("readiness", "blocked verification_run_missing -> candidate pair after Passed run", "blockers listed before the run; exact (configVersionId, passedVerificationRunId) pair after")

	// --- 6) Activation race: two concurrent commands, one wins -------------
	activationPayload := `{"clientCommandId":"%s","expectedStateRowVersion":2,"expectedCurrentContractVersionId":"1","expectedTargetRowVersion":1,"compatibleVersions":[{"businessSystemKey":"payments","configVersionId":"` + versionObj.ID + `","verificationRunId":"` + runObj.ID + `","expectedCurrentConfigVersionId":"` + v1Version.ID + `","expectedBusinessSystemRowVersion":2}]}`
	var raceWG sync.WaitGroup
	startRace := make(chan struct{})
	results := make([]int, 2)
	bodies := make([]string, 2)
	raceWG.Add(2)
	for index, commandID := range []string{"t17-activate-race-a", "t17-activate-race-b"} {
		go func(index int, commandID string) {
			defer raceWG.Done()
			<-startRace
			results[index], bodies[index] = postStatus(client, base+"/api/v1/label-contracts/2/activate", origin, fmt.Sprintf(activationPayload, commandID))
		}(index, commandID)
	}
	close(startRace)
	raceWG.Wait()
	wins := 0
	for index, code := range results {
		if code == 200 {
			wins++
		} else if code != 409 {
			t.Fatalf("racer %d must be 200 or 409, got %d: %.400s", index, code, bodies[index])
		}
	}
	if wins != 1 {
		t.Fatalf("exactly one activation racer must win, got %d (%d/%d: %.300s %.300s)", wins, results[0], results[1], bodies[0], bodies[1])
	}
	observed["raceResults"] = results
	recordExpectation("activation-race", "exactly one 200 + one 409", fmt.Sprintf("codes %v; exactly one activation row", results))
	// Replay of the winning command id returns the stored result.
	winner := 0
	if results[1] == 200 {
		winner = 1
	}
	replay := httpPostExpect(t, client, base+"/api/v1/label-contracts/2/activate", origin, fmt.Sprintf(activationPayload, []string{"t17-activate-race-a", "t17-activate-race-b"}[winner]), 200)
	if !strings.Contains(replay, `"state":"active"`) {
		t.Fatalf("replay must return the stored activation: %.300s", replay)
	}

	// --- 7) Post-activation attribution over real deliveries ----------------
	known := relay("t17-known", "t17-r-known", webhookBodyJSON(t, "firing", map[string]string{"alertname": "T17Known", "severity": "critical", "business_system": "payments"}, "2026-09-07T09:05:00Z"))
	unknownValue := relay("t17-unknown", "t17-r-unknown", webhookBodyJSON(t, "firing", map[string]string{"alertname": "T17Unknown", "severity": "warning", "business_system": "ghost-system"}, "2026-09-07T09:06:00Z"))
	missingLabel := relay("t17-missing", "t17-r-missing", webhookBodyJSON(t, "firing", map[string]string{"alertname": "T17Missing", "severity": "info"}, "2026-09-07T09:07:00Z"))
	for name, out := range map[string]string{"known": known, "unknown": unknownValue, "missing": missingLabel} {
		if !strings.Contains(out, "DELIVERY_STATUS_ACCEPTED") {
			t.Fatalf("relay %s: %.300s", name, out)
		}
	}
	// Write-once attribution: the pre-contract occurrence repeats under the
	// active contract but keeps its historical 未归属.
	if out := relay("t17-pre-repeat", "t17-r-pre-repeat", webhookBodyJSON(t, "firing", preLabels, "2026-09-07T09:00:00Z")); !strings.Contains(out, "DELIVERY_STATUS_ACCEPTED") {
		t.Fatalf("pre-repeat relay: %.300s", out)
	}

	if !awaitCondition(30*time.Second, func() bool {
		snapshot := httpGet(t, client, base+"/api/v1/alerts?state=Firing", origin)
		return strings.Contains(snapshot, "T17Known") && strings.Contains(snapshot, "T17Unknown") && strings.Contains(snapshot, "T17Missing")
	}) {
		t.Fatal("post-activation occurrences did not reach the alert snapshot within 30 seconds")
	}

	filtered := httpGet(t, client, base+"/api/v1/alerts?state=Firing&businessSystemKey=payments", origin)
	var filteredSnapshot struct {
		Items []struct {
			Labels         map[string]string `json:"labels"`
			BusinessSystem *string           `json:"businessSystemKey"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(filtered), &filteredSnapshot); err != nil {
		t.Fatal(err)
	}
	if len(filteredSnapshot.Items) != 1 || filteredSnapshot.Items[0].Labels["alertname"] != "T17Known" {
		t.Fatalf("payments filter must contain exactly T17Known, got %+v", filteredSnapshot.Items)
	}
	if filteredSnapshot.Items[0].BusinessSystem == nil || *filteredSnapshot.Items[0].BusinessSystem != "payments" {
		t.Fatalf("filtered row must carry businessSystemKey=payments: %+v", filteredSnapshot.Items[0])
	}
	unfiltered := httpGet(t, client, base+"/api/v1/alerts?state=Firing", origin)
	for _, alertname := range []string{"T17Known", "T17Unknown", "T17Missing", "T17Pre"} {
		if !strings.Contains(unfiltered, alertname) {
			t.Fatalf("unfiltered snapshot missing %s: %.500s", alertname, unfiltered)
		}
	}
	observed["filteredSnapshot"] = filteredSnapshot.Items
	recordExpectation("attribution-projection", "T17Known→payments; T17Unknown/T17Missing→未归属; T17Pre repeats stay 未归属", fmt.Sprintf("filter=exactly T17Known; unfiltered carries all four (%d items)", len(filteredSnapshot.Items)+3))

	// Live behavior: every post-activation delivery produced a created frame.
	awaitCondition(10*time.Second, func() bool {
		return len(recorder.changeFrames()) >= 4
	})
	changes := recorder.changeFrames()
	eventIDs := []string{}
	for _, frame := range changes {
		var payload struct {
			OccurrenceID string `json:"occurrenceId"`
			Type         string `json:"type"`
		}
		if err := json.Unmarshal([]byte(frame.Data), &payload); err == nil {
			eventIDs = append(eventIDs, frame.ID+":"+payload.Type+":"+payload.OccurrenceID)
		}
	}
	observed["sseFrames"] = eventIDs
	evidence.note(t, "sse-live-frames.json", mustJSON(t, changes))
	recordExpectation("live-filter-list", "created events for pre + 3 attributed deliveries; snapshot+filter reconcile", fmt.Sprintf("%d change frames observed live", len(changes)))

	// --- 8) Queued verification cancellation over the real HTTP route -------
	status, body = uploadMultipart(t, client, base+"/api/v1/business-systems", origin,
		map[string]string{"clientCommandId": "t17-upload-v3", "targetLabelContractVersion": "2"}, "file", "system.yaml", []byte(enabledCheckYAML))
	if status != 201 {
		t.Fatalf("v3 checked upload: status=%d body=%.500s", status, body)
	}
	var v3Version struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(body), &v3Version); err != nil {
		t.Fatal(err)
	}
	code, malformedBody = postStatus(client, base+"/api/v1/business-systems/payments/config/"+v3Version.ID+"/verifications", origin,
		`{"clientCommandId":"t17-verify-bad","purpose":"other"}`)
	if code != 422 {
		t.Fatalf("non-prepublish verification purpose must 422, got %d: %.500s", code, malformedBody)
	}
	queuedBody := httpPostExpect(t, client, base+"/api/v1/business-systems/payments/config/"+v3Version.ID+"/verifications", origin,
		`{"clientCommandId":"t17-verify-queued","purpose":"prepublish"}`, 202)
	var queuedRun struct {
		ID         string `json:"id"`
		RowVersion int64  `json:"rowVersion"`
		State      string `json:"state"`
	}
	if err := json.Unmarshal([]byte(queuedBody), &queuedRun); err != nil {
		t.Fatal(err)
	}
	if queuedRun.State != "Queued" || queuedRun.RowVersion != 1 {
		t.Fatalf("checked config must wait for the later executor: %s", queuedBody)
	}
	code, malformedBody = postStatus(client, base+"/api/v1/business-systems/payments/config/"+v3Version.ID+"/verifications/"+queuedRun.ID+"/cancel", origin,
		`{"clientCommandId":"bad id","expectedRowVersion":1}`)
	if code != 422 {
		t.Fatalf("malformed cancellation command must 422, got %d: %.500s", code, malformedBody)
	}
	cancelledBody := httpPostExpect(t, client, base+"/api/v1/business-systems/payments/config/"+v3Version.ID+"/verifications/"+queuedRun.ID+"/cancel", origin,
		`{"clientCommandId":"t17-cancel-queued","expectedRowVersion":1}`, 200)
	if !strings.Contains(cancelledBody, `"state":"Cancelled"`) || !strings.Contains(cancelledBody, `"rowVersion":2`) {
		t.Fatalf("queued verification cancellation response wrong: %.500s", cancelledBody)
	}
	observed["verificationCancellation"] = json.RawMessage(cancelledBody)
	recordExpectation("verification-cancel", "checked Run Queued→Cancelled through its HTTP command", "run id "+queuedRun.ID+" returned Cancelled rv2")

	// --- 9) SQLite authority evidence ---------------------------------------
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Fatalf("host sqlite3 is required for the evidence path: %v", err)
	}
	dbPath := filepath.Join(stateRoot, "quoin", "compose", "data", "quoin.db")
	queryRow := func(query string) string {
		t.Helper()
		out, err := exec.Command("sqlite3", dbPath, query).CombinedOutput()
		if err != nil {
			t.Fatalf("sqlite query %q: %v\n%s", query, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	sqliteEvidence := map[string]string{
		"contractPointer":     queryRow(`SELECT current_contract_id||'|'||row_version FROM label_contract_state WHERE id=1`),
		"contractStates":      queryRow(`SELECT group_concat(version||':'||state, ',') FROM (SELECT version,state FROM label_contracts ORDER BY version)`),
		"activationRows":      queryRow(`SELECT COUNT(*)||'|'||COALESCE(SUM(applied_at IS NOT NULL),0) FROM label_contract_activations WHERE contract_id=2`),
		"systemProjection":    queryRow(`SELECT key||'|'||enabled||'|'||(current_config_version_id IS NOT NULL)||'|'||row_version FROM business_systems WHERE key='payments'`),
		"versionStates":       queryRow(`SELECT group_concat(version_seq||':'||state, ',') FROM (SELECT version_seq,state FROM business_system_config_versions ORDER BY version_seq)`),
		"verificationStates":  queryRow(`SELECT group_concat(id||':'||state||':'||row_version, ',') FROM (SELECT id,state,row_version FROM config_verification_runs ORDER BY id)`),
		"verificationTaskLog": queryRow(`SELECT COUNT(*) FROM task_change_log WHERE object_type='config_verification_run'`),
		"attributionMap":      queryRow(`SELECT COALESCE(group_concat(alertname||'='||COALESCE(bs.key,'未归属'), ','),'') FROM (SELECT labels_canonical, json_extract(labels_canonical,'$.alertname') AS alertname, business_system_id FROM alert_occurrences ORDER BY id) o LEFT JOIN business_systems bs ON bs.id=o.business_system_id`),
		"preRepeatWriteOnce":  queryRow(`SELECT COALESCE(business_system_id,-1) FROM alert_occurrences WHERE json_extract(labels_canonical,'$.alertname')='T17Pre'`),
		"activationAudit":     queryRow(`SELECT COUNT(*) FROM audit_events WHERE action='label_contract.activate' AND outcome='success'`),
		"verifyAudit":         queryRow(`SELECT COUNT(*) FROM audit_events WHERE action LIKE 'config_verification.%' AND outcome='success'`),
		"clientCommands":      queryRow(`SELECT COUNT(*) FROM client_commands WHERE command_type LIKE 'label_contract%' OR command_type LIKE 'config_verification%' OR command_type='business_system.config.upload'`),
	}
	if sqliteEvidence["contractPointer"] != "2|3" {
		t.Fatalf("contract pointer wrong: %s", sqliteEvidence["contractPointer"])
	}
	if sqliteEvidence["contractStates"] != "1:retired,2:active" {
		t.Fatalf("contract states wrong: %s", sqliteEvidence["contractStates"])
	}
	if sqliteEvidence["activationRows"] != "1|1" {
		// Exactly one winning INSERT for contract 2 (the loser aborted whole,
		// the replay returned the stored result without a new row), sealed once.
		t.Fatalf("activation rows wrong: %s", sqliteEvidence["activationRows"])
	}
	if sqliteEvidence["systemProjection"] != "payments|1|1|3" {
		t.Fatalf("system projection wrong: %s", sqliteEvidence["systemProjection"])
	}
	if sqliteEvidence["versionStates"] != "1:superseded,2:published,3:draft" {
		t.Fatalf("version states wrong: %s", sqliteEvidence["versionStates"])
	}
	if !strings.Contains(sqliteEvidence["verificationStates"], "1:Passed:3") || !strings.Contains(sqliteEvidence["verificationStates"], "2:Cancelled:2") {
		t.Fatalf("verification states wrong: %s", sqliteEvidence["verificationStates"])
	}
	if sqliteEvidence["verificationTaskLog"] == "0" {
		t.Fatalf("verification task log rows missing")
	}
	if sqliteEvidence["preRepeatWriteOnce"] != "-1" {
		t.Fatalf("T17Pre must keep NULL attribution after repeat: %s", sqliteEvidence["preRepeatWriteOnce"])
	}
	for _, required := range []string{"T17Pre=未归属", "T17Known=payments", "T17Unknown=未归属", "T17Missing=未归属"} {
		if !strings.Contains(sqliteEvidence["attributionMap"], required) {
			t.Fatalf("attribution map missing %s: %s", required, sqliteEvidence["attributionMap"])
		}
	}
	observed["sqlite"] = sqliteEvidence
	evidence.note(t, "sqlite-evidence.json", mustJSON(t, sqliteEvidence))
	recordExpectation("sqlite-authority", "pointer 2|3; retired/active; payments v1 superseded→v2 published with v3 draft; Passed and Cancelled Runs; attribution map exact", "all sqlite rows matched the frozen transitions")

	// --- Evidence seal --------------------------------------------------------
	commit := outputOf(t, "git", "rev-parse", "HEAD")
	statusOut := outputOf(t, "git", "status", "--short")
	dirtyDigest := "clean"
	if strings.TrimSpace(statusOut) != "" {
		dirtyDigest = sha256Hex([]byte(statusOut))
	}
	runtimeEvidence["gitCommit"] = commit
	runtimeEvidence["dirtyStateDigest"] = dirtyDigest
	runtimeEvidence["components"].(map[string]any)["imageIds"] = evidence.imageIDs
	runtimeEvidence["observed"] = observed
	runtimeEvidence["expectedVersusActual"] = expectations
}
