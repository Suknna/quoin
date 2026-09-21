package main

// `stele dead-letters` CLI 契约：只读列表不得对错误 dataDirectory 产生任何
// 副作用（不建目录、不建表/迁移、不 chmod），v1 老库可列出且不被迁移；终端
// 载荷输出中和控制字符，重定向的 --full-payload 才给逐字节原文；replay 的
// ids/凭据校验先于开库，任一 id 失败整体回滚。用例执行真实编译的 stele
// 二进制，数据全部落在临时目录。

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	"github.com/Suknna/quoin/internal/stele"
	_ "modernc.org/sqlite"
)

var (
	deadLetterBinaryOnce sync.Once
	deadLetterBinaryPath string
	deadLetterBinaryErr  error
)

// builtSteleBinary compiles the real stele executable once; every CLI case
// executes this binary exactly like the deployment helper does.
func builtSteleBinary(t *testing.T) string {
	t.Helper()
	deadLetterBinaryOnce.Do(func() {
		if _, err := exec.LookPath("go"); err != nil {
			deadLetterBinaryErr = err
			return
		}
		directory, err := os.MkdirTemp("", "stele-dead-letters-bin")
		if err != nil {
			deadLetterBinaryErr = err
			return
		}
		deadLetterBinaryPath = filepath.Join(directory, "stele")
		deadLetterBinaryErr = exec.Command("go", "build", "-o", deadLetterBinaryPath, ".").Run()
	})
	if deadLetterBinaryErr != nil {
		t.Fatalf("build stele binary: %v", deadLetterBinaryErr)
	}
	return deadLetterBinaryPath
}

// runSteleCLI executes an arbitrary `stele` invocation and returns the exit
// code with both streams verbatim.
func runSteleCLI(t *testing.T, binary string, arguments ...string) (int, string, string) {
	t.Helper()
	command := exec.Command(binary, arguments...)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		if exitError, ok := err.(*exec.ExitError); ok {
			return exitError.ExitCode(), stdout.String(), stderr.String()
		}
		t.Fatalf("run stele %v: %v", arguments, err)
	}
	return 0, stdout.String(), stderr.String()
}

func writeSteleCLIConfig(t *testing.T, dataDirectory string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "component.yaml")
	document := fmt.Sprintf(`component: stele
quoinRuntimeEndpoint: https://quoin-runtime:8443
quoinRuntimeCaFile: /etc/quoin/ca.pem
quoinRuntimeClientCertificateFile: /etc/quoin/stele.crt
quoinRuntimeClientPrivateKeyFile: /etc/quoin/stele.key
dataDirectory: %s
`, dataDirectory)
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// seedDeadLetter reproduces the gateway's own write path: enqueue an event and
// mark it REJECTED so it lands in dead_letters.
func seedDeadLetter(t *testing.T, dataDirectory, id string, payload []byte) {
	t.Helper()
	queue, err := stele.OpenQueue(dataDirectory)
	if err != nil {
		t.Fatalf("seed open: %v", err)
	}
	defer queue.Close()
	if err := queue.EnqueueEvents(context.Background(), []stele.QueuedEvent{{
		ID: id, SourceKind: "alertmanager", SourceID: 7, CredentialID: 9,
		CredentialSnapshotVersion: 3, EventType: "alerts.batch",
		ReceivedAt: time.Now().UTC(), Payload: payload,
	}}); err != nil {
		t.Fatalf("seed enqueue: %v", err)
	}
	if _, err := queue.MarkResult(context.Background(), id,
		runtimev1.EventDeliveryStatus_EVENT_DELIVERY_STATUS_REJECTED, time.Now().UTC()); err != nil {
		t.Fatalf("seed reject: %v", err)
	}
}

// 列表对不存在的 dataDirectory 必须零副作用：不创建目录、不建库，
// 报错指向 stele.db。
func TestDeadLettersListRefusesMissingDataDirectory(t *testing.T) {
	dataDirectory := filepath.Join(t.TempDir(), "absent")
	configPath := writeSteleCLIConfig(t, dataDirectory)
	code, stdout, stderr := runSteleCLI(t, builtSteleBinary(t), "dead-letters", "list", "--config", configPath)
	if code == 0 {
		t.Fatalf("list against a missing data directory must fail (stdout=%q)", stdout)
	}
	if !strings.Contains(stderr, "stele.db") {
		t.Fatalf("error must point at the state database, got %q", stderr)
	}
	if stdout != "" {
		t.Fatalf("unexpected stdout: %q", stdout)
	}
	if _, err := os.Stat(dataDirectory); !os.IsNotExist(err) {
		t.Fatalf("read-only list must not create the data directory, stat err = %v", err)
	}
}

// 终端（此处为管道模拟的默认渲染路径）载荷预览：控制字符中和、按字节
// 上限截断；库文件权限不被触碰。
func TestDeadLettersListSanitizesAndTruncatesPayload(t *testing.T) {
	root := t.TempDir()
	dataDirectory := filepath.Join(root, "state")
	payload := []byte("{\"msg\":\"line1\nline2\x1b[31mRED\x1b[0m\x7f\",\"tail\":\"" +
		strings.Repeat("x", 200) + "\"}")
	seedDeadLetter(t, dataDirectory, "evt-escape", payload)
	configPath := writeSteleCLIConfig(t, dataDirectory)
	code, stdout, stderr := runSteleCLI(t, builtSteleBinary(t), "dead-letters", "list", "--config", configPath)
	if code != 0 {
		t.Fatalf("list failed: code=%d stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "evt-escape") || !strings.Contains(stdout, "rejected") {
		t.Fatalf("row missing from output: %q", stdout)
	}
	// 换行/ESC/DEL 都中和为空格：既防终端转义注入，也防破坏表格式。
	if !strings.Contains(stdout, "line1 line2") {
		t.Fatalf("control characters must be neutralized to spaces: %q", stdout)
	}
	if strings.Contains(stdout, "\x1b") || strings.Contains(stdout, "\x7f") || strings.Contains(stdout, "\r") {
		t.Fatalf("raw control characters leaked to the output: %q", stdout)
	}
	for _, b := range []byte(stdout) {
		if (b < 0x20 && b != '\n' && b != '\t') || b == 0x7f {
			t.Fatalf("output carries control byte %q in %q", b, stdout)
		}
	}
	if !strings.Contains(stdout, "…") {
		t.Fatalf("long payload must be truncated with an ellipsis: %q", stdout)
	}
	if strings.Contains(stdout, strings.Repeat("x", 170)) {
		t.Fatalf("preview must stop at the byte cap: %q", stdout)
	}
	info, err := os.Stat(filepath.Join(dataDirectory, "stele.db"))
	if err != nil {
		t.Fatalf("stat database: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("list must not touch database permissions, mode = %o", info.Mode().Perm())
	}
}

// --full-payload 只有重定向到非终端时才逐字节输出原文（子进程 stdout 是
// 管道，正对应留档场景）。
func TestDeadLettersListFullPayloadExactWhenRedirected(t *testing.T) {
	root := t.TempDir()
	dataDirectory := filepath.Join(root, "state")
	payload := []byte("{\"msg\":\"line1\nline2\x1b[31mRED\x1b[0m\",\"tail\":\"" +
		strings.Repeat("x", 200) + "\"}")
	seedDeadLetter(t, dataDirectory, "evt-exact", payload)
	configPath := writeSteleCLIConfig(t, dataDirectory)
	code, stdout, stderr := runSteleCLI(t, builtSteleBinary(t),
		"dead-letters", "list", "--config", configPath, "--full-payload")
	if code != 0 {
		t.Fatalf("list failed: code=%d stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "line1\nline2\x1b[31mRED\x1b[0m") {
		t.Fatalf("redirected --full-payload must be byte-exact: %q", stdout)
	}
	if !strings.Contains(stdout, strings.Repeat("x", 200)) {
		t.Fatalf("redirected --full-payload must not truncate: %q", stdout)
	}
}

// v1 老库（无凭据列）可只读列出，且 user_version 保持 1：只读列表绝不迁移。
func TestDeadLettersListV1DatabaseWithoutMigration(t *testing.T) {
	root := t.TempDir()
	dataDirectory := filepath.Join(root, "state")
	if err := os.MkdirAll(dataDirectory, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	databasePath := filepath.Join(dataDirectory, "stele.db")
	db, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatalf("open v1 database: %v", err)
	}
	for _, statement := range []string{
		`CREATE TABLE dead_letters(
			id TEXT PRIMARY KEY, source_kind TEXT NOT NULL, source_id INTEGER NOT NULL,
			event_type TEXT NOT NULL, payload BLOB NOT NULL, reason TEXT NOT NULL,
			attempts INTEGER NOT NULL, first_received_at TEXT NOT NULL, dead_at TEXT NOT NULL)`,
		`INSERT INTO dead_letters VALUES('legacy-1','alertmanager',7,'alerts.batch','{}','rejected',2,
			'2026-09-20T00:00:00.000000000Z','2026-09-20T01:00:00.000000000Z')`,
		`PRAGMA user_version = 1`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("seed v1 (%q): %v", statement, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close v1 database: %v", err)
	}
	configPath := writeSteleCLIConfig(t, dataDirectory)
	code, stdout, stderr := runSteleCLI(t, builtSteleBinary(t), "dead-letters", "list", "--config", configPath)
	if code != 0 {
		t.Fatalf("list on a v1 database failed: code=%d stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "legacy-1") {
		t.Fatalf("v1 row missing from output: %q", stdout)
	}
	db, err = sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatalf("reopen v1 database: %v", err)
	}
	defer db.Close()
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 1 {
		t.Fatalf("read-only list must not migrate, user_version = %d err = %v", version, err)
	}
}

// replay 的参数校验先于任何数据库访问：缺 ids、负凭据、凭据与快照失配。
func TestDeadLettersReplayValidatesArguments(t *testing.T) {
	binary := builtSteleBinary(t)
	// 指向不存在目录的配置：校验必须在此之前失败，目录也绝不能被创建。
	configPath := writeSteleCLIConfig(t, filepath.Join(t.TempDir(), "absent"))
	cases := []struct {
		name      string
		arguments []string
		wantError string
	}{
		{"no ids", []string{"dead-letters", "replay", "--config", configPath}, "--ids is required"},
		{"empty ids", []string{"dead-letters", "replay", "--config", configPath, "--ids", ""}, "--ids is required"},
		{"blank ids", []string{"dead-letters", "replay", "--config", configPath, "--ids", " , ,"}, "--ids is required"},
		{"negative credential", []string{
			"dead-letters", "replay", "--config", configPath,
			"--ids", "evt-1", "--credential-id", "-3", "--snapshot-version", "1",
		}, "positive"},
		{"credential without snapshot", []string{
			"dead-letters", "replay", "--config", configPath,
			"--ids", "evt-1", "--credential-id", "5",
		}, "must be given together"},
		{"snapshot without credential", []string{
			"dead-letters", "replay", "--config", configPath,
			"--ids", "evt-1", "--snapshot-version", "2",
		}, "must be given together"},
		{"unknown action", []string{"dead-letters", "purge"}, "unknown dead-letters action"},
	}
	for _, testCase := range cases {
		code, _, stderr := runSteleCLI(t, binary, testCase.arguments...)
		if code == 0 {
			t.Fatalf("%s: expected failure, got exit 0", testCase.name)
		}
		if !strings.Contains(stderr, testCase.wantError) {
			t.Fatalf("%s: stderr %q must contain %q", testCase.name, stderr, testCase.wantError)
		}
	}
	if _, err := os.Stat(filepath.Dir(configPath)); err != nil {
		t.Fatalf("validation must precede any database access: %v", err)
	}
}

// replay 真实链路：ids 命中后死信清空、事件回到 outbox 立即到期；任一 id
// 失败整体回滚，stdout 不得谎报已重放数。
func TestDeadLettersReplayReenqueuesAndRollsBack(t *testing.T) {
	root := t.TempDir()
	dataDirectory := filepath.Join(root, "state")
	seedDeadLetter(t, dataDirectory, "evt-a", []byte(`{"a":1}`))
	seedDeadLetter(t, dataDirectory, "evt-b", []byte(`{"b":2}`))
	configPath := writeSteleCLIConfig(t, dataDirectory)

	// 命中一个不存在的 id：整体失败，两条死信都保留。
	code, stdout, stderr := runSteleCLI(t, builtSteleBinary(t),
		"dead-letters", "replay", "--config", configPath, "--ids", "evt-a,evt-missing")
	if code == 0 || !strings.Contains(stderr, "rolled back") {
		t.Fatalf("missing id must fail the whole replay: code=%d stderr=%q", code, stderr)
	}
	if strings.Contains(stdout, "replayed") {
		t.Fatalf("failed replay must not report a count: %q", stdout)
	}

	code, stdout, stderr = runSteleCLI(t, builtSteleBinary(t),
		"dead-letters", "replay", "--config", configPath, "--ids", "evt-a , evt-b")
	if code != 0 {
		t.Fatalf("replay failed: code=%d stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "replayed 2 dead letter(s)") {
		t.Fatalf("replay report = %q, want replayed 2", stdout)
	}
	queue, err := stele.OpenQueue(dataDirectory)
	if err != nil {
		t.Fatalf("verify open: %v", err)
	}
	defer queue.Close()
	if letters, _ := queue.ListDeadLetters(context.Background(), stele.DeadLetterFilter{}); len(letters) != 0 {
		t.Fatalf("dead letters after replay = %d, want 0", len(letters))
	}
	batch, err := queue.FetchDueBatch(context.Background(), 10, time.Now().UTC())
	if err != nil || len(batch) != 2 {
		t.Fatalf("replayed batch = %v err = %v, want 2 due events", batch, err)
	}
	for _, event := range batch {
		if event.Attempts != 0 || event.CredentialID != 9 {
			t.Fatalf("replayed event must reset attempts and keep the credential: %+v", event)
		}
	}
}

// 非本组件配置直接拒绝。
func TestDeadLettersRejectsForeignComponentConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "component.yaml")
	document := "component: quoin\ndataDirectory: /tmp/nowhere\n"
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	code, _, stderr := runSteleCLI(t, builtSteleBinary(t), "dead-letters", "list", "--config", path)
	if code == 0 || (!strings.Contains(stderr, "configuration component must be stele") && !strings.Contains(stderr, "validate deployment configuration")) {
		t.Fatalf("foreign component must be rejected: code=%d stderr=%q", code, stderr)
	}
}

func TestSanitizeControlChars(t *testing.T) {
	input := "a\x00b\x1b[31m\nc\td\x7fe\u009bf"
	if got := sanitizeControlChars(input); got != "a b [31m c d e f" {
		t.Fatalf("sanitize = %q, want %q", got, "a b [31m c d e f")
	}
	if got := sanitizeControlChars("plain JSON {}"); got != "plain JSON {}" {
		t.Fatalf("control-free input must pass through unchanged, got %q", got)
	}
}

func TestTruncateRunesStaysOnRuneBoundary(t *testing.T) {
	if got := truncateRunes("héllo", 2); got != "h" {
		t.Fatalf("truncate = %q, want %q (must back up to the rune boundary)", got, "h")
	}
	if got := truncateRunes("abc", 10); got != "abc" {
		t.Fatalf("short input must pass through, got %q", got)
	}
}

func TestPayloadForDisplayModes(t *testing.T) {
	payload := []byte("short\x1bpayload")
	if got := payloadForDisplay(payload, false, true); got != "short payload" {
		t.Fatalf("terminal preview = %q, want sanitized %q", got, "short payload")
	}
	if got := payloadForDisplay(payload, true, true); got != "short payload" {
		t.Fatalf("terminal full = %q, want sanitized %q", got, "short payload")
	}
	if got := payloadForDisplay(payload, true, false); got != string(payload) {
		t.Fatalf("redirected full = %q, want byte-exact %q", got, payload)
	}
	long := strings.Repeat("é", 200) // 400 字节，超预览上限
	if got := payloadForDisplay([]byte(long), false, true); !strings.HasSuffix(got, "…") || len(got) > payloadPreviewBytes+3 {
		t.Fatalf("long preview must truncate near the byte cap: %d bytes", len(got))
	}
}
