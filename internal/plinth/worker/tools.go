package worker

// Workspace tools (ARCH-WORKER-003): the fixed bash/read/write/grep tool
// set runs inside the sandbox against the attempt workspace with a fixed
// environment; long outputs spill to a workspace file so the supervisor can
// stream them into a tool_result Artifact without unbounded memory
// (ARCH-OUTPUT-001, RUNTIME-ARTIFACT-001).

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	gencontracts "github.com/Suknna/quoin/internal/gen/contracts"
	"gopkg.in/yaml.v3"
)

// WorkerAgentVersion is the frozen executor generation this worker binary
// carries (DATA-ATTEMPT-001); a unit test pins it equal to the Quoin-side
// attempt.AgentVersion.
const WorkerAgentVersion = "initial-analysis-v1"

// Spill thresholds (ARCH-OUTPUT-001, RUNTIME-ARTIFACT-001).
const (
	spillBytes = 50 * 1024
	spillLines = 2000
)

// ProviderToolsJSON renders the provider-facing tool schema for the
// initial-analysis tool set. The bytes and digest must match the Quoin-side
// attempt.CanonicalToolsJSON exactly (pinned by tools_test.go and enforced
// at BeginModelCall through the tool schema digest).
func ProviderToolsJSON() ([]byte, error) {
	tools := []map[string]any{
		toolSchema("bash", "在当前一次性工作区执行一条 bash 命令（/bin/bash --noprofile --norc -c）。无网络、无凭据，只可访问工作区与只读系统路径。", map[string]any{
			"command": map[string]any{"type": "string"},
		}, []string{"command"}),
		toolSchema("read", "读取工作区内一个文本文件的内容（相对路径）。", map[string]any{
			"path": map[string]any{"type": "string"},
		}, []string{"path"}),
		toolSchema("write", "把文本内容写入工作区内一个文件（相对路径）。", map[string]any{
			"path": map[string]any{"type": "string"}, "content": map[string]any{"type": "string"},
		}, []string{"path", "content"}),
		toolSchema("grep", "在工作区文件内按 RE2 正则搜索并返回匹配行。", map[string]any{
			"pattern": map[string]any{"type": "string"}, "path": map[string]any{"type": "string"},
		}, []string{"pattern", "path"}),
		toolSchema("artifact_read", "按范围读取一个 Artifact 的文本片段；返回有界片段与 size/hash/eof/truncated。", map[string]any{
			"artifactId": map[string]any{"type": "string"}, "offset": map[string]any{"type": "number"}, "limit": map[string]any{"type": "number"},
		}, []string{"artifactId"}),
		toolSchema("artifact_grep", "在 Artifact 文本内按 RE2 正则搜索；返回有界匹配片段与截断标记。", map[string]any{
			"artifactId": map[string]any{"type": "string"}, "pattern": map[string]any{"type": "string"},
		}, []string{"artifactId", "pattern"}),
		toolSchema("thanos_query", "对全局 Thanos 执行一次只读 PromQL 即时查询（instant query）；模型只提供 query 表达式，连接与凭据由 Quoin 确定性解析，结果作为不可变 Evidence 封存。", map[string]any{
			"query": map[string]any{"type": "string"},
		}, []string{"query"}),
	}
	wrapped := make([]any, 0, len(tools))
	for _, tool := range tools {
		wrapped = append(wrapped, map[string]any{"type": "function", "function": tool})
	}
	return json.Marshal(wrapped)
}

func toolSchema(name, description string, properties map[string]any, required []string) map[string]any {
	sort.Strings(required)
	return map[string]any{
		"name": name, "description": description,
		"parameters": map[string]any{
			"type": "object", "properties": properties, "required": required,
		},
	}
}

// ProviderToolsDigest is the SHA-256 of ProviderToolsJSON as hex text.
func ProviderToolsDigest() (string, error) {
	body, err := ProviderToolsJSON()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:]), nil
}

// ReadOnlyRuntimePaths mirrors the frozen plinth-worker-tools.yaml
// readonly_runtime_paths for the compiled architecture; the worker uses
// them for the Landlock ruleset (ARCH-WORKER-003/007).
func ReadOnlyRuntimePaths() ([]string, error) {
	var document struct {
		ReadOnly struct {
			Common []string `yaml:"common"`
			AMD64  []string `yaml:"linux/amd64"`
			ARM64  []string `yaml:"linux/arm64"`
		} `yaml:"readonly_runtime_paths"`
	}
	if err := yaml.Unmarshal(gencontracts.PlinthWorkerToolsYAML, &document); err != nil {
		return nil, fmt.Errorf("parse frozen plinth-worker-tools.yaml: %w", err)
	}
	paths := append([]string{}, document.ReadOnly.Common...)
	if strings.Contains(runtimeGOARCH(), "arm64") {
		paths = append(paths, document.ReadOnly.ARM64...)
	} else {
		paths = append(paths, document.ReadOnly.AMD64...)
	}
	return paths, nil
}

func runtimeGOARCH() string {
	return archToken
}

// ExecutionModeFor resolves the fixed execution mode of one tool name
// (mirrors the Quoin-side catalog; pinned equal by tools_test.go).
func ExecutionModeFor(name string) string {
	switch name {
	case "artifact_read", "artifact_grep", "thanos_query":
		return "TOOL_EXECUTION_MODE_SUPERVISOR_TYPED"
	default:
		return "TOOL_EXECUTION_MODE_WORKER_LOCAL"
	}
}

// LocalResult is the deterministic outcome of one workspace tool run.
type LocalResult struct {
	Success       bool
	Preview       string // bounded model-visible preview (head or tail)
	TotalBytes    int
	TotalLines    int
	WorkspacePath string // set when the full body spilled to a workspace file
	MediaType     string
	ErrorCode     string
	ErrorDetail   string
}

// ExecuteLocal runs one worker_local tool inside the sandbox
// (ARCH-WORKER-003). Path arguments are workspace-relative only.
func ExecuteLocal(ctx context.Context, toolName string, arguments map[string]any, workspaceDir string) LocalResult {
	switch toolName {
	case "bash":
		return executeBash(ctx, arguments, workspaceDir)
	case "read":
		return executeRead(arguments, workspaceDir)
	case "write":
		return executeWrite(arguments, workspaceDir)
	case "grep":
		return executeGrep(ctx, arguments, workspaceDir)
	default:
		return LocalResult{Success: false, ErrorCode: "unknown_tool", ErrorDetail: "workspace tool " + toolName + " is not part of the fixed catalog"}
	}
}

func executeBash(ctx context.Context, arguments map[string]any, workspaceDir string) LocalResult {
	command, ok := arguments["command"].(string)
	if !ok || strings.TrimSpace(command) == "" {
		return LocalResult{Success: false, ErrorCode: "invalid_arguments", ErrorDetail: "command 必须是非空字符串"}
	}
	if err := os.MkdirAll(filepath.Join(workspaceDir, "tmp"), 0o700); err != nil {
		return LocalResult{Success: false, ErrorCode: "workspace_error", ErrorDetail: err.Error()}
	}
	process := exec.CommandContext(ctx, "/bin/bash", "--noprofile", "--norc", "-c", command)
	process.Dir = workspaceDir
	// The frozen bash tool environment (plinth-worker-tools.yaml); the
	// worker's own environment is empty and never inherited.
	process.Env = []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8", "LC_ALL=C.UTF-8", "TMPDIR=" + filepath.Join(workspaceDir, "tmp")}
	// Combine stdout+stderr: the model sees the real command output.
	output, err := process.CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(output))
		return LocalResult{Success: false, ErrorCode: "bash_failed", ErrorDetail: truncateText(detail, 4096)}
	}
	return renderOutput(string(output), workspaceDir)
}

func executeRead(arguments map[string]any, workspaceDir string) LocalResult {
	path, ok := arguments["path"].(string)
	if !ok || path == "" {
		return LocalResult{Success: false, ErrorCode: "invalid_arguments", ErrorDetail: "path 必须是相对路径字符串"}
	}
	target, err := resolveWorkspace(workspaceDir, path)
	if err != nil {
		return LocalResult{Success: false, ErrorCode: "invalid_arguments", ErrorDetail: err.Error()}
	}
	body, err := os.ReadFile(target)
	if err != nil {
		return LocalResult{Success: false, ErrorCode: "read_failed", ErrorDetail: err.Error()}
	}
	return renderOutput(string(body), workspaceDir)
}

func executeWrite(arguments map[string]any, workspaceDir string) LocalResult {
	path, ok := arguments["path"].(string)
	if !ok || path == "" {
		return LocalResult{Success: false, ErrorCode: "invalid_arguments", ErrorDetail: "path 必须是相对路径字符串"}
	}
	content, ok := arguments["content"].(string)
	if !ok {
		return LocalResult{Success: false, ErrorCode: "invalid_arguments", ErrorDetail: "content 必须是字符串"}
	}
	target, err := resolveWorkspace(workspaceDir, path)
	if err != nil {
		return LocalResult{Success: false, ErrorCode: "invalid_arguments", ErrorDetail: err.Error()}
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return LocalResult{Success: false, ErrorCode: "write_failed", ErrorDetail: err.Error()}
	}
	if err := os.WriteFile(target, []byte(content), 0o600); err != nil {
		return LocalResult{Success: false, ErrorCode: "write_failed", ErrorDetail: err.Error()}
	}
	return renderOutput(fmt.Sprintf("已写入 %d 字节到 %s", len(content), path), workspaceDir)
}

func executeGrep(ctx context.Context, arguments map[string]any, workspaceDir string) LocalResult {
	pattern, ok := arguments["pattern"].(string)
	if !ok || pattern == "" {
		return LocalResult{Success: false, ErrorCode: "invalid_arguments", ErrorDetail: "pattern 必须是非空字符串"}
	}
	path, ok := arguments["path"].(string)
	if !ok || path == "" {
		return LocalResult{Success: false, ErrorCode: "invalid_arguments", ErrorDetail: "path 必须是相对路径字符串"}
	}
	target, err := resolveWorkspace(workspaceDir, path)
	if err != nil {
		return LocalResult{Success: false, ErrorCode: "invalid_arguments", ErrorDetail: err.Error()}
	}
	process := exec.CommandContext(ctx, "/usr/bin/grep", "-nE", pattern, target)
	process.Dir = workspaceDir
	process.Env = []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8", "LC_ALL=C.UTF-8"}
	var output bytes.Buffer
	process.Stdout = &output
	process.Stderr = &output
	if err := process.Run(); err != nil {
		if exit, ok := err.(*exec.ExitError); ok && exit.ExitCode() == 1 {
			// grep exit 1 = no matches: a legitimate empty result.
			return renderOutput("(无匹配)\n", workspaceDir)
		}
		return LocalResult{Success: false, ErrorCode: "grep_failed", ErrorDetail: truncateText(output.String(), 4096)}
	}
	return renderOutput(output.String(), workspaceDir)
}

// resolveWorkspace confines every path argument to the workspace tree
// (no absolute paths, no .. escapes, no symlink escapes).
func resolveWorkspace(workspaceDir, path string) (string, error) {
	if filepath.IsAbs(path) {
		return "", fmt.Errorf("只允许工作区内的相对路径")
	}
	clean := filepath.Clean(path)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("路径不得越出工作区")
	}
	target := filepath.Join(workspaceDir, clean)
	// Symlink escape guard: resolve the deepest existing ancestor.
	ancestor := target
	for {
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			break
		}
		ancestor = parent
		if _, err := os.Lstat(ancestor); err == nil {
			break
		}
	}
	resolved, err := filepath.EvalSymlinks(ancestor)
	if err != nil {
		resolved = ancestor
	}
	if !strings.HasPrefix(resolved, filepath.Clean(workspaceDir)) {
		return "", fmt.Errorf("路径不得越出工作区")
	}
	return target, nil
}

// renderOutput applies the frozen spill policy: small outputs travel
// inline; long outputs spill to a workspace file the supervisor uploads
// (RUNTIME-ARTIFACT-001).
func renderOutput(body string, workspaceDir string) LocalResult {
	lines := strings.Split(body, "\n")
	totalBytes := len(body)
	totalLines := len(lines)
	if totalBytes > spillBytes || totalLines > spillLines {
		spillPath := filepath.Join(workspaceDir, "tool-results", fmt.Sprintf("result-%d.txt", os.Getpid()))
		if err := os.MkdirAll(filepath.Dir(spillPath), 0o700); err != nil {
			return LocalResult{Success: false, ErrorCode: "spill_failed", ErrorDetail: err.Error()}
		}
		if err := os.WriteFile(spillPath, []byte(body), 0o600); err != nil {
			return LocalResult{Success: false, ErrorCode: "spill_failed", ErrorDetail: err.Error()}
		}
		// Logs/commands prefer the tail (ARCH-OUTPUT-003).
		preview := tailPreview(body)
		return LocalResult{Success: true, Preview: preview, TotalBytes: totalBytes, TotalLines: totalLines, WorkspacePath: spillPath, MediaType: "text/plain"}
	}
	return LocalResult{Success: true, Preview: body, TotalBytes: totalBytes, TotalLines: totalLines, MediaType: "text/plain"}
}

// tailPreview keeps a bounded tail of the body as the model-visible
// preview; the preview is derived content, never the authority.
func tailPreview(body string) string {
	const previewBytes = 16 * 1024
	if len(body) <= previewBytes {
		return body
	}
	return "…（完整输出已存入 Artifact）\n" + body[len(body)-previewBytes:]
}

func truncateText(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "…"
}

// ResultJSON is the canonical tool result preview the supervisor seals
// through CompleteToolCall (schema kind <tool>_result_v1).
func ResultJSON(result LocalResult) []byte {
	body := map[string]any{"success": result.Success}
	if result.Success {
		body["output"] = result.Preview
		body["totalBytes"] = result.TotalBytes
		body["totalLines"] = result.TotalLines
	} else {
		body["errorCode"] = result.ErrorCode
		body["errorDetail"] = result.ErrorDetail
	}
	encoded, _ := json.Marshal(body)
	return encoded
}
