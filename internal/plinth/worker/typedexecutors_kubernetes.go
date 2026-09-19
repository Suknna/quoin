package worker

// kubernetes_read typed executor (ADR-0004): one frozen tool call fans out
// over every Kubernetes connection grant frozen in the authorization
// transaction. Each mapping executes independently — one bound cluster
// failing must not hide another's observation — and the sealed payload shape
// is the contract kubernetes.EvidenceFor projects (partial failures only
// produce evidence when artifact-backed).

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	plinthconnections "github.com/Suknna/quoin/internal/plinth/connections"
	"github.com/Suknna/quoin/internal/quoin/tools/kubernetes"
)

// retiredExecutors are the retired implementations (受控退役) this host
// keeps serving through the assembled dispatch table, strictly for attempts
// whose frozen catalog predates the retirement. They can never enter a new
// catalog (the retired plugin can never be enabled), so the binding is
// history-serving only and derives from the same assembly as every live
// binding.
func retiredExecutors() map[string]TypedExecutor {
	return map[string]TypedExecutor{
		kubernetes.ReadToolName: executeKubernetesReadTyped,
	}
}

func executeKubernetesReadTyped(execution *TypedToolContext) error {
	ctx, runner := execution.BaseCtx, execution.Runner
	attemptID, toolCallID, args := execution.AttemptID, execution.ToolCallID, execution.Args

	operation, _ := args["operation"].(string)
	namespace, _ := args["namespace"].(string)
	name, _ := args["name"].(string)
	container, _ := args["container"].(string)
	if strings.TrimSpace(operation) == "" {
		return execution.Fail("invalid_arguments", "operation 必须是非空字符串")
	}

	meta, ok := runner.tools[toolCallID]
	if !ok || len(meta.grants) == 0 {
		return execution.Fail("grant_missing", "该工具调用没有冻结的 Kubernetes 连接 grant")
	}

	observedAt := time.Now().UTC().Format(time.RFC3339Nano)
	results := make([]json.RawMessage, 0, len(meta.grants))
	failures := 0
	var lastError string
	for _, grant := range meta.grants {
		item := executeKubernetesMapping(ctx, runner, attemptID, toolCallID, grant, operation, namespace, name, container)
		if !item.success {
			failures++
			lastError = item.errorDetail
		}
		results = append(results, item.raw)
	}
	payload := map[string]any{
		"success": failures == 0, "operation": operation, "observedAt": observedAt,
		"results": results,
	}
	artifactID := int64(0)
	if failures > 0 {
		payload["errorCode"] = "partial_failure"
		payload["errorDetail"] = lastError
		// Only artifact-backed partial failures produce evidence; stream the
		// raw multi-mapping body into the tool_result artifact store so the
		// projection stays well-formed.
		body, err := json.Marshal(payload)
		if err != nil {
			return execution.Fail("invalid_result", err.Error())
		}
		// Spill into THIS attempt's workspace directory (the thanos
		// convention, ARCH-WORKER-003) before the artifact upload.
		workspaceDir := filepath.Join(runner.Config.WorkspaceRoot, fmt.Sprintf("attempt-%d", attemptID))
		workspaceFile := filepath.Join(workspaceDir, fmt.Sprintf("kubernetes-read-%d.json", toolCallID))
		if writeErr := os.MkdirAll(workspaceDir, 0o700); writeErr != nil {
			return execution.Fail("artifact_write_failed", writeErr.Error())
		}
		if writeErr := os.WriteFile(workspaceFile, body, 0o600); writeErr != nil {
			return execution.Fail("artifact_write_failed", writeErr.Error())
		}
		artifact, uploadErr := runner.uploadWorkspaceFileAs(ctx, attemptID, toolCallID, workspaceFile, "application/json")
		if uploadErr != nil {
			return execution.Fail("artifact_write_failed", uploadErr.Error())
		}
		artifactID = artifact
	}
	return execution.Succeed(execution.DefaultResultSchemaKind(), payload, artifactID)
}

// kubernetesMappingResult is one mapping's sealed outcome.
type kubernetesMappingResult struct {
	success     bool
	raw         json.RawMessage
	errorDetail string
}

// executeKubernetesMapping runs one frozen grant independently.
func executeKubernetesMapping(ctx context.Context, runner *Runner, attemptID, toolCallID int64, grant *runtimev1.ConnectionGrant, operation, namespace, name, container string) kubernetesMappingResult {
	grantCtx, grantCancel := context.WithTimeout(ctx, 15*time.Second)
	grantPayload, err := runner.Client.FetchCredentialGrant(grantCtx, &runtimev1.FetchCredentialGrantRequest{
		GrantId: grant.GetGrantId(), AttemptId: attemptID, BootId: runner.Binding.BootID, ConnectionEpoch: runner.Binding.Epoch,
	})
	grantCancel()
	if err != nil {
		return kubernetesMappingResult{success: false, raw: mappingFailure("grant_missing", "获取 Kubernetes 凭据 grant 失败: "+err.Error()), errorDetail: err.Error()}
	}
	if grantPayload.GetKubernetes() == nil {
		return kubernetesMappingResult{success: false, raw: mappingFailure("grant_missing", "grant 回复未携带 Kubernetes 凭据"), errorDetail: "grant 回复未携带 Kubernetes 凭据"}
	}
	var config plinthconnections.KubernetesConfig
	if err := json.Unmarshal(grantPayload.GetRevisionConfigJson(), &config); err != nil {
		return kubernetesMappingResult{success: false, raw: mappingFailure("invalid_connection_config", "Kubernetes 连接配置无法解析: "+err.Error()), errorDetail: err.Error()}
	}
	secret := plinthconnections.KubernetesSecret{Kubeconfig: grantPayload.GetKubernetes().GetKubeconfig()}
	var output strings.Builder
	execErr := plinthconnections.RunKubernetesRead(ctx, config, secret, plinthconnections.KubernetesReadRequest{
		Operation: operation, Namespace: namespace, Name: name, Container: container,
	}, &output)
	if execErr != nil {
		detail := execErr.Error()
		return kubernetesMappingResult{success: false, raw: mappingFailure("kubernetes_execution_failed", detail), errorDetail: detail}
	}
	raw, err := json.Marshal(map[string]any{"success": true, "output": strings.TrimRight(output.String(), "\n")})
	if err != nil {
		return kubernetesMappingResult{success: false, raw: mappingFailure("invalid_result", "结果序列化失败"), errorDetail: err.Error()}
	}
	return kubernetesMappingResult{success: true, raw: raw}
}

func mappingFailure(code, detail string) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{"success": false, "errorCode": code, "errorDetail": detail})
	return raw
}
