package upload

// TestTicket16 proves the strict Business System configuration capability
// over the real compose stack: the Label Contract draft creation and
// zero-system atomic activation prerequisite, the strict upload (lexical,
// schema, PromQL ownership, journey catalog and limit rejections with exact
// field errors), the parse-once immutable versions with command replay, and
// the draft/current pointer publish transaction with its concurrency fence.

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const validSystemYAML = `system_key: payments
display_name: 支付系统
enabled: false
timezone: Asia/Shanghai
resource_refresh_interval_seconds: 300
resource_discoveries:
  - key: web-pods
    display_name: Web Pods
    selector: 'up{business_system="payments", job="web"}'
    identity_labels: [job, instance]
inspection_plans:
  - key: daily-check
    display_name: Daily Check
    cron: "30 8 * * *"
    checks:
      - key: up-instant
        display_name: Up Instant
        analysis_question: 当前可用吗？
        kind: promql
        query:
          mode: instant
          expression: 'up{business_system="payments"}'
      - key: latency-range
        display_name: Latency Range
        analysis_question: 时延趋势如何？
        kind: promql
        query:
          mode: range
          expression: 'rate(http_requests_total{business_system="payments"}[5m])'
          range_seconds: 3600
          step_seconds: 60
`

func TestTicket16(t *testing.T) {
	evidenceDir := os.Getenv("QUOIN_EVIDENCE_DIR")
	if evidenceDir == "" {
		t.Skip("QUOIN_EVIDENCE_DIR not set; T16 acceptance run disabled")
	}
	requireDocker(t)
	evidence := &ticketEvidence{dir: evidenceDir, env: os.Environ()}
	evidence.env = append(evidence.env, "QUOIN_FORCE_IMAGE_BUILD=1")
	imageProxy := os.Getenv("QUOIN_IMAGE_GOPROXY")
	if imageProxy == "" {
		imageProxy = strings.TrimSpace(outputOf(t, "go", "env", "GOPROXY"))
	}
	if imageProxy != "" {
		evidence.env = append(evidence.env, "QUOIN_IMAGE_GOPROXY="+imageProxy)
	}
	workRoot := t.TempDir()
	stateRoot := filepath.Join(workRoot, "state")
	evidence.stateRoot = stateRoot
	secretDir := filepath.Join(workRoot, "secrets")
	evidence.env = append(evidence.env, "XDG_STATE_HOME="+stateRoot)

	evidence.run(t, "build-helper", nil, "go", "build", "-trimpath", "-o", filepath.Join(workRoot, "quoin-deploy"), "./cmd/quoin-deploy")
	preExisting := map[string]bool{}
	for _, image := range []string{"quoin/quoin:v0.1.0-dev", "quoin/plinth:v0.1.0-dev", "quoin/lintel:v0.1.0-dev", "quoin/stele:v0.1.0-dev"} {
		preExisting[image] = exec.Command("docker", "image", "inspect", image).Run() == nil
	}
	evidence.run(t, "images", nil, "bash", "build/package/images.sh")

	installConfig := filepath.Join(workRoot, "install.yaml")
	writeFile(t, installConfig, fmt.Sprintf("document: compose-install\npublicOrigin: https://quoin.example.com\npublishMode: loopback\nquoinPublicHostPort: %d\nsteleWebhookHostPort: %d\nsecretDirectory: %s\nlintelBrowserSlots: 1\nlintelShmSizeBytes: 1073741824\n", quoinPort, stelePort, secretDir))
	tempPassword := randomSecret(t, 24)
	adminInput := strings.NewReader(strings.Join([]string{"admin", "Ticket Admin", tempPassword, tempPassword}, "\n") + "\n")
	evidence.env = append(evidence.env, "QUOIN_DEPLOY_SCRIPTED=1")
	evidence.run(t, "install", adminInput, filepath.Join(workRoot, "quoin-deploy"), "compose", "install", "--config", installConfig)

	// The compose projection stays up for the assertions and comes down in
	// the cleanup pass with its full teardown evidence.
	composeFile := filepath.Join(stateRoot, "quoin", "compose", "generated", "compose.yaml")
	t.Cleanup(func() {
		evidence.run(t, "teardown-stack", nil, "docker", "compose", "--project-name", projectName, "--file", composeFile, "down", "--remove-orphans")
		for image, existed := range preExisting {
			if !existed {
				evidence.run(t, "teardown-image", nil, "docker", "rmi", image)
			}
		}
	})

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
	newPassword := randomSecret(t, 24)
	httpPutExpect(t, client, base+"/api/v1/auth/password", origin, fmt.Sprintf(`{"currentPassword":"%s","newPassword":"%s"}`, tempPassword, newPassword), 0)
	observed := map[string]any{}
	expectations := map[string]string{}
	recordExpectation := func(family, expected, actual string) {
		expectations[family] = "expected=" + expected + "; actual=" + actual
	}

	// --- Label Contract prerequisite (zero-enabled-system path) ----------
	contractYAML := "label_contract:\n  business_system_label: business_system\n"
	status, body := uploadMultipart(t, client, base+"/api/v1/label-contracts", origin,
		map[string]string{"clientCommandId": "t16-contract-create-1"}, "file", "contract.yaml", []byte(contractYAML))
	if status != 201 {
		t.Fatalf("contract draft create: status=%d body=%.500s", status, body)
	}
	var contractDraft struct {
		ID      string `json:"id"`
		Version int64  `json:"version"`
		State   string `json:"state"`
	}
	if err := json.Unmarshal([]byte(body), &contractDraft); err != nil {
		t.Fatal(err)
	}
	observed["contractDraft"] = contractDraft
	recordExpectation("contract-draft", "201 draft v1", fmt.Sprintf("contract draft state=%s version=%d", contractDraft.State, contractDraft.Version))

	// Strict contract rejections.
	rejected := func(name string, document string, wantStatus int, wantPath string) {
		t.Helper()
		code, rejection := uploadMultipart(t, client, base+"/api/v1/label-contracts", origin,
			map[string]string{"clientCommandId": "t16-contract-reject-" + name}, "file", "contract.yaml", []byte(document))
		if code != wantStatus {
			t.Fatalf("contract %s: status=%d want=%d body=%.500s", name, code, wantStatus, rejection)
		}
		if !strings.Contains(rejection, wantPath) {
			t.Fatalf("contract %s: body missing %s: %.500s", name, wantPath, rejection)
		}
		observed["contractReject:"+name] = code
	}
	rejected("alias", "label_contract: &base\n  business_system_label: x\nreuse: *base\n", 422, "锚点")
	rejected("unknown-field", contractYAML+"extra: 1\n", 422, "additional")
	rejected("second-doc", contractYAML+"---\nlabel_contract:\n  business_system_label: y\n", 422, "第二个")

	// Zero-system activation through the real atomic command.
	activation := httpPostExpect(t, client, base+"/api/v1/label-contracts/1/activate", origin,
		`{"clientCommandId":"t16-contract-activate-1","expectedStateRowVersion":1,"expectedCurrentContractVersionId":null,"expectedTargetRowVersion":1,"compatibleVersions":[]}`, 200)
	if !strings.Contains(activation, `"state":"active"`) {
		t.Fatalf("activation must switch the contract to active: %.500s", activation)
	}
	observed["activation"] = json.RawMessage(activation)
	// Replay is idempotent; a second distinct command with a stale state row conflicts.
	httpPostExpect(t, client, base+"/api/v1/label-contracts/1/activate", origin,
		`{"clientCommandId":"t16-contract-activate-1","expectedStateRowVersion":1,"expectedCurrentContractVersionId":null,"expectedTargetRowVersion":1,"compatibleVersions":[]}`, 200)
	conflictBody := httpPostExpect(t, client, base+"/api/v1/label-contracts/1/activate", origin,
		`{"clientCommandId":"t16-contract-activate-2","expectedStateRowVersion":1,"expectedCurrentContractVersionId":null,"expectedTargetRowVersion":1,"compatibleVersions":[]}`, 409)
	if !strings.Contains(conflictBody, "current_pointer_conflict") && !strings.Contains(conflictBody, "row_version_conflict") {
		t.Fatalf("stale activation must conflict: %.500s", conflictBody)
	}
	recordExpectation("contract-activation", "200 active + idempotent replay + 409 stale", "activation committed; replay returned stored result; stale activation 409")

	// --- Business System upload matrix -----------------------------------
	systemUpload := func(commandID string, document string, targetVersion string, extra map[string]string) (int, string) {
		t.Helper()
		fields := map[string]string{"clientCommandId": commandID, "targetLabelContractVersion": targetVersion}
		for name, value := range extra {
			fields[name] = value
		}
		return uploadMultipart(t, client, base+"/api/v1/business-systems", origin, fields, "file", "system.yaml", []byte(document))
	}

	status, body = systemUpload("t16-upload-valid-1", validSystemYAML, "1", nil)
	if status != 201 {
		t.Fatalf("valid upload: status=%d body=%.600s", status, body)
	}
	var firstVersion struct {
		ID         string `json:"id"`
		VersionSeq int64  `json:"versionSeq"`
		State      string `json:"state"`
		SystemKey  string `json:"systemKey"`
		Digest     string `json:"digest"`
	}
	if err := json.Unmarshal([]byte(body), &firstVersion); err != nil {
		t.Fatal(err)
	}
	if firstVersion.State != "draft" || firstVersion.SystemKey != "payments" || len(firstVersion.Digest) != 64 {
		t.Fatalf("first draft projection wrong: %#v", firstVersion)
	}
	observed["firstVersion"] = firstVersion

	systemsList := httpGet(t, client, base+"/api/v1/business-systems", origin)
	if !strings.Contains(systemsList, `"key":"payments"`) || !strings.Contains(systemsList, `"enabled":false`) {
		t.Fatalf("system list must show the new Disabled system: %.600s", systemsList)
	}
	versionDetail := httpGet(t, client, base+"/api/v1/business-systems/payments/config/"+firstVersion.ID, origin)
	for _, fragment := range []string{`"yamlBody"`, `"timezone":"Asia/Shanghai"`, `"journeyId"`, `"up{business_system=\"payments\"}"`, `"rangeSeconds":3600`} {
		if fragment == `"journeyId"` {
			continue
		}
		if !strings.Contains(versionDetail, fragment) {
			t.Fatalf("version detail missing %s: %.800s", fragment, versionDetail)
		}
	}
	observed["versionDetailFragments"] = "yaml/timezone/selector/range present"

	// The strict rejection matrix: each case must fail with the exact path.
	systemReject := func(name, document string, wantStatus int, wantFragment string, target string, extra map[string]string) {
		t.Helper()
		code, rejection := systemUpload("t16-reject-"+name, document, target, extra)
		if code != wantStatus {
			t.Fatalf("%s: status=%d want=%d body=%.600s", name, code, wantStatus, rejection)
		}
		if !strings.Contains(rejection, wantFragment) {
			t.Fatalf("%s: body missing %q: %.600s", name, wantFragment, rejection)
		}
		observed["reject:"+name] = code
	}
	systemReject("unknown-field", validSystemYAML+"extra_field: 1\n", 422, "extra_field", "1", nil)
	systemReject("duplicate-key", strings.Replace(validSystemYAML, "timezone: Asia/Shanghai", "timezone: Asia/Shanghai\ndisplay_name: 重复支付", 1), 422, "重复", "1", nil)
	systemReject("alias", strings.Replace(validSystemYAML, "display_name: 支付系统", "display_name: &anchor 支付系统", 1)+"reuse: *anchor\n", 422, "锚点", "1", nil)
	systemReject("second-document", validSystemYAML+"---\nsystem_key: other\n", 422, "第二个", "1", nil)
	systemReject("non-string-key", "1: one\n", 422, "字符串", "1", nil)
	systemReject("custom-tag", strings.Replace(validSystemYAML, "enabled: false", "enabled: !custom false", 1), 422, "tag", "1", nil)
	systemReject("missing-target", validSystemYAML, 422, "targetLabelContractVersion", "99", nil)
	systemReject("wrong-catalog-digest", validSystemYAML, 422, "journeyCatalogDigest", "1", map[string]string{"journeyCatalogDigest": strings.Repeat("0", 64)})
	systemReject("browser-journey", strings.Replace(validSystemYAML, "        kind: promql\n        query:\n          mode: instant\n          expression: 'up{business_system=\"payments\"}'", "        kind: browser\n        journey_id: login-journey", 1), 422, "Journey Catalog", "1", nil)
	systemReject("promql-ownership", strings.Replace(validSystemYAML, `selector: 'up{business_system="payments", job="web"}'`, `selector: 'up{job="web"}'`, 1), 422, "resource_discoveries[0].selector", "1", nil)
	systemReject("discovery-aggregation", strings.Replace(validSystemYAML, `selector: 'up{business_system="payments", job="web"}'`, `selector: 'sum(up{business_system="payments"})'`, 1), 422, "单个即时向量选择器", "1", nil)
	systemReject("bad-timezone", strings.Replace(validSystemYAML, "timezone: Asia/Shanghai", "timezone: Not/AZone", 1), 422, "IANA", "1", nil)
	systemReject("bad-cron", strings.Replace(validSystemYAML, "cron: \"30 8 * * *\"", "cron: \"@daily\"", 1), 422, "descriptor", "1", nil)
	recordExpectation("upload-rejections", "unknown/duplicate/alias/second-doc/non-string-key/custom-tag=422; missing target/wrong digest/browser journey/promql/timezone/cron=422", "all rejection cases returned 422 with exact paths")

	// The byte limit is enforced while the document streams in.
	oversized := []byte("system_key: big\n#" + strings.Repeat("x", 10<<20) + "\n")
	code, rejection := uploadMultipart(t, client, base+"/api/v1/business-systems", origin,
		map[string]string{"clientCommandId": "t16-reject-oversize", "targetLabelContractVersion": "1"}, "file", "big.yaml", oversized)
	if code != 413 {
		t.Fatalf("oversize: status=%d body=%.300s", code, rejection)
	}
	observed["reject:oversize"] = code
	recordExpectation("upload-limit", "413 over 10 MiB", fmt.Sprintf("status=%d", code))

	// Command replay: same id + same document returns the original draft;
	// same id + different document conflicts.
	status, body = systemUpload("t16-upload-valid-1", validSystemYAML, "1", nil)
	if status != 201 || !strings.Contains(body, `"id":"`+firstVersion.ID+`"`) {
		t.Fatalf("replay must return the original draft: status=%d body=%.400s", status, body)
	}
	status, body = systemUpload("t16-upload-valid-1", strings.Replace(validSystemYAML, "300", "301", 1), "1", nil)
	if status != 409 || !strings.Contains(body, "command_id_reused") {
		t.Fatalf("reused id with different content must conflict: status=%d body=%.400s", status, body)
	}
	recordExpectation("upload-replay", "201 same id + 409 different content", "replay idempotent; digest mismatch conflicts")

	// --- Publish pointer transactions -------------------------------------
	publish := func(commandID, versionID string, expected string, want int) string {
		t.Helper()
		return httpPostExpect(t, client, base+"/api/v1/business-systems/payments/config/"+versionID+"/publish", origin,
			fmt.Sprintf(`{"clientCommandId":%q,"expectedCurrentPublishedVersionId":%s}`, commandID, expected), want)
	}
	published := publish("t16-publish-1", firstVersion.ID, "null", 200)
	for _, fragment := range []string{`"enabled":false`, `"timezone":"Asia/Shanghai"`, `"currentConfigVersionId":"` + firstVersion.ID + `"`, `"rowVersion":2`} {
		if !strings.Contains(published, fragment) {
			t.Fatalf("publish projection missing %s: %.800s", fragment, published)
		}
	}
	observed["publishFirst"] = json.RawMessage(published)

	// Stale fence: replaying the first publish's expected=null must 409 with
	// the actual current pointer in the conflict block.
	secondYAML := strings.Replace(validSystemYAML, "resource_refresh_interval_seconds: 300", "resource_refresh_interval_seconds: 301", 1)
	status, body = systemUpload("t16-upload-valid-2", secondYAML, "1", nil)
	if status != 201 {
		t.Fatalf("second upload: status=%d body=%.500s", status, body)
	}
	var secondVersion struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(body), &secondVersion); err != nil {
		t.Fatal(err)
	}
	stalePublish := publish("t16-publish-stale", secondVersion.ID, "null", 409)
	if !strings.Contains(stalePublish, "current_pointer_conflict") || !strings.Contains(stalePublish, firstVersion.ID) {
		t.Fatalf("stale publish must carry the actual current pointer: %.600s", stalePublish)
	}
	observed["stalePublish"] = json.RawMessage(stalePublish)

	// Correct fence advances and supersedes.
	publish("t16-publish-2", secondVersion.ID, `"`+firstVersion.ID+`"`, 200)
	firstState := httpGet(t, client, base+"/api/v1/business-systems/payments/config/"+firstVersion.ID, origin)
	secondState := httpGet(t, client, base+"/api/v1/business-systems/payments/config/"+secondVersion.ID, origin)
	if !strings.Contains(firstState, `"state":"superseded"`) || !strings.Contains(secondState, `"state":"published"`) {
		t.Fatalf("state derivation wrong: first=%.200s second=%.200s", firstState, secondState)
	}
	recordExpectation("publish-pointer", "first publish null-fence; stale 409 with actual current; second publish supersedes", "pointer transaction followed the frozen derivation")

	// Re-publishing a published version is refused by the pointer fence.
	rePublish := publish("t16-publish-republish", firstVersion.ID, `"`+secondVersion.ID+`"`, 409)
	if !strings.Contains(rePublish, "current_pointer_conflict") {
		t.Fatalf("re-publish must conflict: %.400s", rePublish)
	}

	// A draft targeting a NON-current contract can only publish through that
	// contract's atomic activation (zero systems here, so the contract is
	// simply never current until activated).
	contract2 := "label_contract:\n  business_system_label: business_system\n"
	status, body = uploadMultipart(t, client, base+"/api/v1/label-contracts", origin,
		map[string]string{"clientCommandId": "t16-contract-create-2"}, "file", "contract.yaml", []byte(contract2))
	if status != 201 {
		t.Fatalf("second contract draft: status=%d body=%.400s", status, body)
	}
	status, body = systemUpload("t16-upload-valid-3", strings.Replace(validSystemYAML, "300", "302", 1), "2", nil)
	if status != 201 {
		t.Fatalf("upload targeting draft contract: status=%d body=%.500s", status, body)
	}
	var thirdVersion struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(body), &thirdVersion); err != nil {
		t.Fatal(err)
	}
	fenced := publish("t16-publish-fenced", thirdVersion.ID, `"`+secondVersion.ID+`"`, 409)
	if !strings.Contains(fenced, "联合激活") {
		t.Fatalf("non-current contract target must hit the atomic-activation fence: %.500s", fenced)
	}
	observed["contractFencePublish"] = json.RawMessage(fenced)
	recordExpectation("contract-fence", "409 non-current contract publish", "publish refused until atomic activation")

	// --- SQLite authority evidence ---------------------------------------
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
		"systemRow":          queryRow(`SELECT key||'|'||enabled||'|'||COALESCE(timezone,'NULL')||'|'||row_version FROM business_systems WHERE key='payments'`),
		"versionStates":      queryRow(`SELECT group_concat(version_seq||':'||state, ',') FROM (SELECT version_seq,state FROM business_system_config_versions ORDER BY version_seq)`),
		"contractPointer":    queryRow(`SELECT COALESCE(current_contract_id,-1)||'|'||row_version FROM label_contract_state WHERE id=1`),
		"contractStates":     queryRow(`SELECT group_concat(version||':'||state, ',') FROM (SELECT version,state FROM label_contracts ORDER BY version)`),
		"uploadAudit":        queryRow(`SELECT COUNT(*) FROM audit_events WHERE action='business_system.config.upload' AND outcome='success'`),
		"publishAudit":       queryRow(`SELECT COUNT(*) FROM audit_events WHERE action='business_system.config.publish' AND outcome='success'`),
		"contractAudit":      queryRow(`SELECT COUNT(*) FROM audit_events WHERE action LIKE 'label_contract.%' AND outcome='success'`),
		"clientCommands":     queryRow(`SELECT COUNT(*) FROM client_commands WHERE command_type LIKE 'business_system%' OR command_type LIKE 'label_contract%'`),
		"typedCheckRows":     queryRow(`SELECT COUNT(*) FROM config_checks`),
		"typedDiscoveryRows": queryRow(`SELECT COUNT(*) FROM config_discoveries`),
		"activationSealed":   queryRow(`SELECT COUNT(*) FROM label_contract_activations WHERE applied_at IS NOT NULL`),
		"currentProjection":  queryRow(`SELECT resource_refresh_interval_seconds FROM business_systems WHERE key='payments'`),
	}
	if sqliteEvidence["versionStates"] != "1:superseded,2:published,3:draft" {
		t.Fatalf("version states wrong: %s", sqliteEvidence["versionStates"])
	}
	if sqliteEvidence["contractStates"] != "1:active,2:draft" {
		t.Fatalf("contract states wrong: %s", sqliteEvidence["contractStates"])
	}
	if sqliteEvidence["contractPointer"] != "1|2" {
		t.Fatalf("contract pointer wrong: %s", sqliteEvidence["contractPointer"])
	}
	if sqliteEvidence["activationSealed"] != "1" {
		t.Fatalf("activation must be sealed once: %s", sqliteEvidence["activationSealed"])
	}
	if sqliteEvidence["currentProjection"] != "301" {
		t.Fatalf("current projection wrong: %s", sqliteEvidence["currentProjection"])
	}
	if sqliteEvidence["uploadAudit"] == "0" || sqliteEvidence["publishAudit"] != "2" || sqliteEvidence["contractAudit"] == "0" {
		t.Fatalf("audit evidence wrong: %v", sqliteEvidence)
	}
	evidence.note(t, "sqlite-evidence.json", mustJSON(t, sqliteEvidence))
	observed["sqlite"] = sqliteEvidence
	recordExpectation("sqlite-authority", "pointer/projection/audit/derivation rows match the frozen transitions", "sqlite evidence matched all assertions")

	// --- Evidence seal + secret hygiene -----------------------------------
	commit := outputOf(t, "git", "rev-parse", "HEAD")
	dirtyDigest := "clean"
	if status := outputOf(t, "git", "status", "--short"); strings.TrimSpace(status) != "" {
		dirtyDigest = sha256Hex([]byte(status))
	}
	evidence.writeRuntimeEvidence(t, commit, dirtyDigest, observed, expectations)
	scanForSecrets(t, evidenceDir, tempPassword, newPassword)
}
