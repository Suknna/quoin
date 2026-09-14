package supervisor

// metrics 插件的共享 Call 构造与凭据解析（ADR-0004）：supervisor 内两个
// 执行适配器（source observation 的 Discoverer 与独立计划巡检的 Collector）
// 共用同一构造/解析实现，消除复制并统一失败语义。

import (
	"context"
	"encoding/json"
	"fmt"

	plinthconnections "github.com/Suknna/quoin/internal/plinth/connections"
	"github.com/Suknna/quoin/internal/plugins"
)

// metrics secret 槽位是插件的命名秘密槽；槽名即引用名（Call.SecretRefs 的
// value），解析器按同一命名空间分发。
const (
	metricSecretSlotUsername    = "username"
	metricSecretSlotPassword    = "password"
	metricSecretSlotBearerToken = "bearerToken"
)

// newMetricsCall 冻结一次插件调用的非秘密连接配置与 grant 解析出的凭据。
// 秘密只经 SecretResolver 边界进入适配器；解析失败必须 fail closed（返回
// 错误终止本次采集），绝不匿名降级。
func newMetricsCall(registry *plugins.Registry, pluginID string, settingsJSON json.RawMessage, username, password, bearerToken string) (*plugins.Call, error) {
	if len(settingsJSON) == 0 {
		return nil, fmt.Errorf("plugin %q call is missing its frozen connection settings", pluginID)
	}
	// Instance-settings validation against the plugin's declared
	// ConfigSchema (ADR-0004): unknown fields, missing required fields and
	// wrong types fail the call here — errors return to the caller's result,
	// never panic.
	if err := registry.ValidateConfig(pluginID, settingsJSON); err != nil {
		return nil, err
	}
	secrets := map[string][]byte{
		metricSecretSlotUsername:    []byte(username),
		metricSecretSlotPassword:    []byte(password),
		metricSecretSlotBearerToken: []byte(bearerToken),
	}
	return &plugins.Call{
		PluginID: pluginID,
		Settings: settingsJSON,
		SecretRefs: map[string]string{
			metricSecretSlotUsername:    metricSecretSlotUsername,
			metricSecretSlotPassword:    metricSecretSlotPassword,
			metricSecretSlotBearerToken: metricSecretSlotBearerToken,
		},
		Secrets: secretResolverFunc(func(ref string) ([]byte, error) {
			clear, ok := secrets[ref]
			if !ok {
				return nil, fmt.Errorf("unknown metric secret slot %q", ref)
			}
			return clear, nil
		}),
	}, nil
}

// metricsSecret 解析本次调用的指标凭据；任何缺失槽位都是硬错误，调用方必须
// 将采集标记为技术失败而不是以空凭据继续。
func metricsSecret(ctx context.Context, call *plugins.Call) (plinthconnections.MetricsSecret, error) {
	if call.Secrets == nil {
		return plinthconnections.MetricsSecret{}, fmt.Errorf("plugin %q call has no secret resolver", call.PluginID)
	}
	var secret plinthconnections.MetricsSecret
	slots := map[string]*string{
		metricSecretSlotUsername:    &secret.Username,
		metricSecretSlotPassword:    &secret.Password,
		metricSecretSlotBearerToken: &secret.BearerToken,
	}
	for slot, target := range slots {
		clear, err := call.Secrets.Resolve(ctx, slot)
		if err != nil {
			return plinthconnections.MetricsSecret{}, fmt.Errorf("metric secret slot %q unavailable: %w", slot, err)
		}
		*target = string(clear)
	}
	return secret, nil
}

// metricsSettings 解析调用的非秘密连接配置。
func metricsSettings(call *plugins.Call) (plinthconnections.MetricsConfig, error) {
	var config plinthconnections.MetricsConfig
	if len(call.Settings) == 0 || json.Unmarshal(call.Settings, &config) != nil {
		return plinthconnections.MetricsConfig{}, fmt.Errorf("plugin %q settings are not a metrics connection config", call.PluginID)
	}
	return config, nil
}
