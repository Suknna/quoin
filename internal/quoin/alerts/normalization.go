// 告警语义归一化（ADR-0012 intake 流水线 Normalize 段）：在 Quoin 内直接调用
// 插件注册表（plugins.Default）按来源协议（alert_sources.protocol =
// EventSource kind）解析 AlertNormalizer，把交付的归一化 payload（webhook 原始
// 解析结果）投影为统一告警语义——severity/title/annotations/resource。每条
// payload alerts[i] 与交付条目一一对应（按 index 对位）。无 normalizer 或解析
// 失败时全部退化为冻结缺省（info/”/'{}'/”）并由交付事务记 normalizer_missing
// 接入问题；词表外的 severity（normalizer 本应保证，防御性）一律降为 info。
package alerts

import (
	"github.com/Suknna/quoin/internal/plugins"
)

// deliveryNormalization 是一次交付的归一化结果：normalized 与 payload 的
// alerts 数组按 index 一一对应；ok=false 表示协议无 normalizer 或归一化失败，
// 全部条目使用缺省语义。
type deliveryNormalization struct {
	normalized []plugins.NormalizedAlert
	ok         bool
}

// normalizeDelivery 按来源协议解析一次归一化器并投影整个 payload。body 是
// Stele 已验证并解析过的 webhook 原始字节，无需重新 marshal。
func normalizeDelivery(protocol string, body []byte) deliveryNormalization {
	registry := plugins.Default()
	normalizer, _, ok := registry.AlertNormalizer(protocol)
	if !ok || normalizer == nil {
		return deliveryNormalization{ok: false}
	}
	normalized, err := normalizer.NormalizeAlert(body)
	if err != nil || normalized == nil {
		return deliveryNormalization{ok: false}
	}
	return deliveryNormalization{normalized: normalized, ok: true}
}

// semanticsFor 返回 payload 第 index 条告警的冻结语义。越界（normalizer 输出
// 与 payload 条目数不一致的防御分支）与归一化缺失都退化为缺省值。
func (normalization deliveryNormalization) semanticsFor(index int) (severity, title, annotationsJSON, resource string) {
	if !normalization.ok || index < 0 || index >= len(normalization.normalized) {
		return string(plugins.SeverityInfo), "", "{}", ""
	}
	alert := normalization.normalized[index]
	mapped := alert.Severity
	// 防御性词表收敛：normalizer 契约保证词表内，越界值一律降为 info。
	if !plugins.ValidSeverity(mapped) {
		mapped = plugins.SeverityInfo
	}
	return string(mapped), alert.Title, string(plugins.AnnotationsJSON(alert.Annotations)), alert.Resource
}
