// Package tools owns the supervisor-side execution of the fixed
// supervisor_typed observation tools (T11). The supervisor holds the
// connection config and the decrypted secret (fetched through the frozen
// grant, ARCH-WORKER-002); the worker only ever sees the sealed bounded
// result preview and the Artifact locator. Long raw responses stream
// through a bounded accumulator into the attempt's one-shot workspace and
// then into the existing tool_result Artifact store (ARCH-OUTPUT-001):
// nothing is buffered whole in memory and no durable second index exists.
package tools

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	plinthconnections "github.com/Suknna/quoin/internal/plinth/connections"
	"github.com/Suknna/quoin/internal/quoin/tools/thanos"
)

// Spill thresholds (ARCH-OUTPUT-001, DATA-ARTIFACT-007): the first bound
// to be reached sends the complete raw bytes into a tool_result Artifact
// and keeps only a bounded preview in the model context.
const (
	spillBytes = 50 * 1024
	spillLines = 2000
)

// queryTimeout bounds one external Thanos call (ARCH-MODE-003: per-call
// deadlines, no product-level totals).
const queryTimeout = 30 * time.Second

// previewBytes bounds the head preview of a spilled response
// (ARCH-OUTPUT-003: query results keep the head).
const previewBytes = 16 * 1024

// ThanosQueryParams carries the supervisor-side inputs of one thanos_query
// execution. The config/secret never cross into the worker process.
type ThanosQueryParams struct {
	Config plinthconnections.ThanosConfig
	Secret plinthconnections.ThanosSecret
	Query  string
	// WorkspaceDir is the attempt's one-shot workspace on the supervisor
	// host: the raw response spills here before upload (the file is
	// removed after the artifact commits; it is never a durable locator).
	WorkspaceDir string
	AttemptID    int64
	ToolCallID   int64
	// Upload streams one workspace file into the existing tool_result
	// Artifact store and returns the committed artifact id (wired to
	// ArtifactService.Upload).
	Upload func(ctx context.Context, attemptID, toolCallID int64, path string) (int64, error)
	// Timeout bounds this one external call; the frozen release default
	// is queryTimeout (ARCH-MODE-003). Tests shrink it.
	Timeout time.Duration
}

// ExecuteThanosQuery runs one instant query against the frozen Thanos
// projection and returns the canonical thanos_query_result_v1 payload plus
// the committed artifact id when the raw response spilled. Failures are
// structured return_to_model results (the model may retry with a fixed
// query; nothing is retried inside the supervisor, ARCH-RECOVERY-005).
func ExecuteThanosQuery(ctx context.Context, params ThanosQueryParams) (payload []byte, artifactID int64, err error) {
	startedAt := time.Now().UTC()
	fail := func(code, detail string) ([]byte, int64, error) {
		body, marshalErr := json.Marshal(map[string]any{
			"success": false, "startedAt": startedAt.Format(time.RFC3339Nano),
			"finishedAt": time.Now().UTC().Format(time.RFC3339Nano),
			"errorCode":  code, "errorDetail": detail,
		})
		if marshalErr != nil {
			return nil, 0, marshalErr
		}
		return body, 0, nil
	}
	if params.WorkspaceDir == "" {
		return fail("supervisor_error", "supervisor 未提供本次 Attempt 工作区")
	}
	if err := os.MkdirAll(filepath.Join(params.WorkspaceDir, "tool-results"), 0o700); err != nil {
		return fail("supervisor_error", "无法创建工作区溢出目录")
	}
	spillPath := filepath.Join(params.WorkspaceDir, "tool-results", fmt.Sprintf("thanos-%d.json", params.ToolCallID))
	// The spill file is a transient accumulator inside the one-shot
	// attempt workspace, never a durable locator (ARCH-OUTPUT-001); the
	// deferred close+remove owns every exit path.
	cleanup := func() { _ = os.Remove(spillPath) }
	file, err := os.OpenFile(spillPath, os.O_CREATE|os.O_TRUNC|os.O_RDWR, 0o600)
	if err != nil {
		return fail("supervisor_error", "无法创建工作区溢出文件")
	}
	defer func() {
		file.Close()
		cleanup()
	}()
	timeout := queryTimeout
	if params.Timeout > 0 {
		timeout = params.Timeout
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	client, err := plinthconnections.NewHTTPClient(params.Config)
	if err != nil {
		return fail("invalid_connection_config", "Thanos 连接配置无法解析: "+err.Error())
	}
	target := strings.TrimSuffix(params.Config.BaseURL, "/") + "/api/v1/query?query=" + url.QueryEscape(params.Query)
	request, err := http.NewRequestWithContext(callCtx, http.MethodGet, target, nil)
	if err != nil {
		return fail("invalid_arguments", "查询 URL 构造失败: "+err.Error())
	}
	if params.Secret.Password != "" {
		username := params.Secret.Username
		if username == "" {
			username = params.Config.Username
		}
		request.SetBasicAuth(username, params.Secret.Password)
	}
	response, err := client.Do(request)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return fail("thanos_timeout", "Thanos 查询超时；请缩小查询范围后重试")
		}
		return fail("thanos_unavailable", "Thanos 查询请求失败: "+err.Error())
	}
	// The bounded streaming accumulator: the raw bytes go straight to the
	// spill file while bytes and lines are counted (ARCH-OUTPUT-001).
	totalBytes, totalLines, copyErr := streamAccumulate(file, response.Body)
	response.Body.Close()
	if copyErr != nil {
		return fail("thanos_unavailable", "读取 Thanos 响应失败: "+copyErr.Error())
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return fail("supervisor_error", "溢出文件无法回读")
	}
	if response.StatusCode != http.StatusOK {
		detail := boundedFileHead(file, 4096)
		return fail("thanos_http_error", fmt.Sprintf("Thanos 查询端点返回 HTTP %d", response.StatusCode)+suffixDetail(detail))
	}
	// Validate the response shape before declaring an observation
	// (unparseable bodies are structured failures, never fake evidence).
	status, resultType, sampleCount, parseErr := summarizeResponse(file)
	if parseErr != nil {
		return fail("thanos_invalid_response", "Thanos 响应不是合法查询结果: "+parseErr.Error())
	}
	if status != "success" {
		detail := boundedFileHead(file, 4096)
		return fail("thanos_query_error", "Thanos 查询失败"+suffixDetail(detail))
	}
	spilled := totalBytes > spillBytes || totalLines > spillLines
	var artifact *thanos.ArtifactRef
	var uploadedArtifactID int64
	if spilled {
		if params.Upload == nil {
			return fail("supervisor_error", "supervisor 未接入 Artifact 上传")
		}
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			return fail("supervisor_error", "溢出文件无法回读")
		}
		shaHex, err := fileSHA256(file)
		if err != nil {
			return fail("supervisor_error", "溢出文件摘要失败")
		}
		uploadedID, err := params.Upload(ctx, params.AttemptID, params.ToolCallID, spillPath)
		if err != nil {
			// ARCH-OUTPUT-005: upload failure fails the tool call; a bare
			// preview is never reported as the complete result.
			return fail("artifact_commit_failed", "长输出 Artifact 提交失败: "+err.Error())
		}
		uploadedArtifactID = uploadedID
		artifact = &thanos.ArtifactRef{
			ID:         fmt.Sprint(uploadedID),
			MediaType:  "application/json",
			SHA256:     shaHex,
			SizeBytes:  totalBytes,
			TotalLines: totalLines,
		}
	}
	// The bounded model-visible preview: the complete head for small
	// bodies, a marked head slice plus the Artifact locator for spills
	// (ARCH-OUTPUT-003).
	var output string
	if spilled {
		output = "…（完整输出已存入 Artifact）\n" + boundedFileHead(file, previewBytes)
	} else {
		output = boundedFileHead(file, spillBytes)
	}
	finishedAt := time.Now().UTC()
	payloadBody := map[string]any{
		"success": true, "status": status, "resultType": resultType, "sampleCount": sampleCount,
		"startedAt": startedAt.Format(time.RFC3339Nano), "finishedAt": finishedAt.Format(time.RFC3339Nano),
		"truncated": spilled, "totalBytes": totalBytes, "totalLines": totalLines, "output": output,
	}
	if artifact != nil {
		payloadBody["artifact"] = artifact
	}
	canonical, err := json.Marshal(payloadBody)
	if err != nil {
		return fail("supervisor_error", "结果序列化失败: "+err.Error())
	}
	return canonical, uploadedArtifactID, nil
}

// streamAccumulate copies the response body into the spill file while
// counting bytes and lines (bounded memory: the caller streams, nothing is
// held whole).
func streamAccumulate(file *os.File, body io.Reader) (totalBytes, totalLines int64, err error) {
	reader := bufio.NewReaderSize(body, 64*1024)
	writer := bufio.NewWriterSize(file, 64*1024)
	buffer := make([]byte, 64*1024)
	endsWithNewline := false
	for {
		n, readErr := reader.Read(buffer)
		if n > 0 {
			totalBytes += int64(n)
			totalLines += int64(strings.Count(string(buffer[:n]), "\n"))
			endsWithNewline = buffer[n-1] == '\n'
			if _, writeErr := writer.Write(buffer[:n]); writeErr != nil {
				return totalBytes, totalLines, writeErr
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return totalBytes, totalLines, readErr
		}
	}
	if err := writer.Flush(); err != nil {
		return totalBytes, totalLines, err
	}
	// A body of N lines carries N-1 newlines plus a final line without a
	// terminator; a trailing newline does not open another line.
	if totalBytes > 0 && !endsWithNewline {
		totalLines++
	}
	return totalBytes, totalLines, nil
}

// summarizeResponse extracts status/resultType/sampleCount without
// materializing the result array (a huge matrix is walked token-wise).
func summarizeResponse(file *os.File) (status, resultType string, sampleCount int, err error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", "", 0, err
	}
	decoder := json.NewDecoder(file)
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return "", "", 0, errors.New("顶层不是 JSON 对象")
	}
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return "", "", 0, err
		}
		name, _ := key.(string)
		switch name {
		case "status":
			if err := decoder.Decode(&status); err != nil {
				return "", "", 0, err
			}
		case "data":
			dataToken, err := decoder.Token()
			if err != nil || dataToken != json.Delim('{') {
				return "", "", 0, errors.New("data 不是 JSON 对象")
			}
			for decoder.More() {
				dataKey, err := decoder.Token()
				if err != nil {
					return "", "", 0, err
				}
				dataName, _ := dataKey.(string)
				switch dataName {
				case "resultType":
					if err := decoder.Decode(&resultType); err != nil {
						return "", "", 0, err
					}
				case "result":
					resultToken, err := decoder.Token()
					if err != nil || resultToken != json.Delim('[') {
						return "", "", 0, errors.New("result 不是 JSON 数组")
					}
					sampleCount, err = countArrayElements(decoder)
					if err != nil {
						return "", "", 0, err
					}
				default:
					if err := skipValue(decoder); err != nil {
						return "", "", 0, err
					}
				}
			}
			if _, err := decoder.Token(); err != nil { // close of data object
				return "", "", 0, err
			}
		default:
			if err := skipValue(decoder); err != nil {
				return "", "", 0, err
			}
		}
	}
	return status, resultType, sampleCount, nil
}

// countArrayElements walks one JSON array token-wise and counts its
// elements (bounded memory regardless of array size).
func countArrayElements(decoder *json.Decoder) (int, error) {
	count := 0
	for decoder.More() {
		if err := skipValue(decoder); err != nil {
			return count, err
		}
		count++
	}
	if _, err := decoder.Token(); err != nil { // closing ']'
		return count, err
	}
	return count, nil
}

// skipValue skips one complete JSON value (any kind).
func skipValue(decoder *json.Decoder) error {
	depth := 0
	for {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		switch token {
		case json.Delim('{'), json.Delim('['):
			depth++
		case json.Delim('}'), json.Delim(']'):
			depth--
			if depth == 0 {
				return nil
			}
		default:
			if depth == 0 {
				return nil
			}
		}
	}
}

// boundedFileHead returns the bounded head of the spill file (the file is
// already at the caller's chosen position; it is left positioned after the
// read).
func boundedFileHead(file *os.File, limit int64) string {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return ""
	}
	body, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return ""
	}
	if int64(len(body)) > limit {
		return string(body[:limit]) + "…"
	}
	return string(body)
}

func suffixDetail(detail string) string {
	if strings.TrimSpace(detail) == "" {
		return ""
	}
	return "：" + strings.TrimSpace(detail)
}

func fileSHA256(file *os.File) (string, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
