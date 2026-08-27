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
	"strings"

	gencontracts "github.com/Suknna/quoin/internal/gen/contracts"
	"github.com/Suknna/quoin/internal/quoin/attempt"
	"gopkg.in/yaml.v3"
)

// WorkerAgentVersion is the frozen executor generation this worker binary
// carries (DATA-ATTEMPT-001); a unit test pins it equal to the Quoin-side
// attempt.AgentVersion.
const WorkerAgentVersion = "initial-analysis-v1"

// WorkerInvestigationAgentVersion pins the investigation agent generation
// (mirrors investigation.AgentVersion).
const WorkerInvestigationAgentVersion = "investigation-v1"

// Spill thresholds (ARCH-OUTPUT-001, RUNTIME-ARTIFACT-001).
const (
	spillBytes = 50 * 1024
	spillLines = 2000
)

// ProviderToolsJSON delegates to Quoin's frozen catalog renderer. This keeps
// provider schema/digest pinning byte-identical while the worker still owns
// execution-mode dispatch.
func ProviderToolsJSON(agentVersions ...string) ([]byte, error) {
	agentVersion := WorkerAgentVersion
	if len(agentVersions) == 1 {
		agentVersion = agentVersions[0]
	}
	return attempt.CanonicalToolsJSON(agentVersion)
}

// ProviderToolsDigest is the SHA-256 of ProviderToolsJSON as hex text.
func ProviderToolsDigest(agentVersions ...string) (string, error) {
	body, err := ProviderToolsJSON(agentVersions...)
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
	case "artifact_read", "artifact_grep", "thanos_query", "kubernetes_read":
		return "TOOL_EXECUTION_MODE_SUPERVISOR_TYPED"
	case "quoin_browser":
		return "TOOL_EXECUTION_MODE_QUOIN_BROWSER"
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
