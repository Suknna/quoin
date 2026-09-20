package connections

// Stele 网关的按需连接材料投递（ADR-0011）：出向工具执行从 Quoin 派发到
// Stele，Stele 通过 SteleRelay.AcquireConnectionCredential 按连接获取执行
// 材料——非秘密配置投影（endpoint/TLS/auth 模式）加上解密后的类型化秘密。
// Quoin 对外部平台凭据只写不读：本方法是唯一的"读缝"，解密仅为按需投递，
// 结果只经 mTLS 信道（CN=stele）离开进程，Quoin 自身不使用、不缓存。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Suknna/quoin/internal/quoin/execution"
)

// ErrAcquireDenied 报告连接材料投递的确定性拒绝（连接不存在、禁用或类型
// 不在出向执行词表内）。调用方据此返回 gRPC 拒绝而非 UNAVAILABLE。
var ErrAcquireDenied = errors.New("connection material acquire denied")

// MetricsConnectionPayload 是一次按需连接材料投递的产物（只在内存中存在，
// 绝不落库）。RevisionConfigJSON 是连接当前 revision 的非秘密类型化投影；
// Metrics 是解密后的凭据（prometheus/thanos 共用同一 HTTP 凭据形状）。
type MetricsConnectionPayload struct {
	ConnectionID         int64
	ConnectionRevisionID int64
	CredentialGeneration int64
	ConnectionType       string
	RevisionConfigJSON   json.RawMessage
	Metrics              *MetricsCredentialSecret
}

// AcquireMetricsConnection 按 connection_id 投递一次出向执行材料：读
// connections + current_revision + 最新 credential_generations，校验连接
// enabled 且 type ∈ {prometheus,thanos}，然后复用 FulfillGrant 的解密管线
// 在 runner 守卫事务内打开 envelope（敏感读的审计纪律与 grant reveal 一致，
// DATA-CONN-002）。model_provider 连接在此处被确定性拒绝——模型凭据只走
// Plinth grant（FetchCredentialGrant），绝不经网关投递。
func (service *Service) AcquireMetricsConnection(ctx context.Context, connectionID int64) (MetricsConnectionPayload, error) {
	// gRPC 流处理器上下文不带用户身份；这里以系统 task 主体建立执行范围，
	// 与其它 runtime 驱动的连接生命周期操作同型。
	correlation, err := execution.NewCorrelationID()
	if err != nil {
		return MetricsConnectionPayload{}, err
	}
	system := execution.Principal{Kind: execution.PrincipalSystem, ID: 0}
	scope, err := execution.ReplaceMetadata(ctx, execution.Metadata{
		CorrelationID: correlation,
		Actor:         system,
		Initiator:     system,
		Source:        execution.Source{Kind: execution.SourceTask},
	})
	if err != nil {
		return MetricsConnectionPayload{}, err
	}
	payload, err := execution.Execute(scope, service.commands.runner, service.commands.metricsAcquire,
		func(tx *execution.Tx) (MetricsConnectionPayload, error) {
			return service.acquireMetricsConnectionOn(ctx, tx, connectionID)
		},
		func(MetricsConnectionPayload) int64 { return connectionID })
	if err != nil {
		return MetricsConnectionPayload{}, domainError(err)
	}
	return payload, nil
}

// acquireMetricsConnectionOn 是投递的守卫事务内业务段：所有确定性校验先于
// 解密执行，被拒的投递不留任何审计之外的状态。
func (service *Service) acquireMetricsConnectionOn(ctx context.Context, tx *execution.Tx, connectionID int64) (MetricsConnectionPayload, error) {
	var connectionType string
	var enabled int
	var revisionID, generationID int64
	if err := tx.QueryRowContext(ctx, `
		SELECT c.type,c.enabled,COALESCE(c.current_revision_id,0),COALESCE(c.current_credential_generation_id,0)
		FROM connections c WHERE c.id=?`, connectionID).
		Scan(&connectionType, &enabled, &revisionID, &generationID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return MetricsConnectionPayload{}, fmt.Errorf("%w: connection %d not found", ErrAcquireDenied, connectionID)
		}
		return MetricsConnectionPayload{}, err
	}
	if connectionType != TypePrometheus && connectionType != TypeThanos {
		return MetricsConnectionPayload{}, fmt.Errorf("%w: connection type %q is not gateway-executable", ErrAcquireDenied, connectionType)
	}
	if enabled != 1 {
		return MetricsConnectionPayload{}, fmt.Errorf("%w: connection %d is disabled", ErrAcquireDenied, connectionID)
	}
	var revisionConfig sql.NullString
	if err := tx.QueryRowContext(ctx, `
		SELECT config_json FROM connection_revisions WHERE id=? AND connection_id=?`,
		revisionID, connectionID).Scan(&revisionConfig); err != nil {
		return MetricsConnectionPayload{}, err
	}
	payload := MetricsConnectionPayload{
		ConnectionID:         connectionID,
		ConnectionRevisionID: revisionID,
		CredentialGeneration: generationID,
		ConnectionType:       connectionType,
		RevisionConfigJSON:   json.RawMessage("null"),
	}
	if revisionConfig.Valid && revisionConfig.String != "" {
		payload.RevisionConfigJSON = json.RawMessage(revisionConfig.String)
	}
	// 解密复用 grant reveal 的唯一入口：root binding 校验与 envelope 打开
	// 都在 openGenerationOn 内，与本事务同一提交序。
	secret, err := service.openGenerationOn(ctx, tx, generationID)
	if err != nil {
		return MetricsConnectionPayload{}, err
	}
	switch {
	case secret.Prometheus != nil:
		payload.Metrics = &MetricsCredentialSecret{Username: secret.Prometheus.Username, Password: secret.Prometheus.Password, BearerToken: secret.Prometheus.BearerToken}
	case secret.Thanos != nil:
		payload.Metrics = &MetricsCredentialSecret{Username: secret.Thanos.Username, Password: secret.Thanos.Password, BearerToken: secret.Thanos.BearerToken}
	default:
		// metrics 连接允许无认证（空 carrier 也封存了独立 generation）。
		payload.Metrics = &MetricsCredentialSecret{}
	}
	return payload, nil
}
