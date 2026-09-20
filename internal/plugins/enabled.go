package plugins

// 部署启用解析（ADR-0004：部署 YAML 选择启用插件）。启用是部署级、
// 进程启动时冻结的事实：目录、冻结工具目录与授权都从同一解析结果派生，
// 不存在运行时开关。

import (
	"fmt"
	"sort"
)

// ResolveEnabled 把部署配置的 enabledPlugins 解析为排序后的启用插件 ID
// 集合：
//
//   - configured 为 nil（部署配置缺省该字段）时，返回全部 DefaultEnabled
//     的已注册插件——默认主线为 prometheus/thanos/alertmanager；
//   - configured 非 nil 时是显式白名单：每个 ID 必须是已注册插件，未知
//     ID 返回 ErrUnknownPlugin（启动失败，绝不静默忽略）；重复 ID 去重。
//
// 返回值按 ID 稳定排序，供目录、冻结目录与审计共用。
func (r *Registry) ResolveEnabled(configured []string) ([]string, error) {
	r.mu.Lock()
	r.ensureFrozen()
	enabled := map[string]bool{}
	if configured == nil {
		for id, plugin := range r.plugins {
			if plugin.DefaultEnabled {
				enabled[id] = true
			}
		}
	} else {
		for _, id := range configured {
			if _, exists := r.plugins[id]; !exists {
				r.mu.Unlock()
				return nil, fmt.Errorf("%w: %s", ErrUnknownPlugin, id)
			}
			enabled[id] = true
		}
	}
	r.mu.Unlock()
	result := make([]string, 0, len(enabled))
	for id := range enabled {
		result = append(result, id)
	}
	sort.Strings(result)
	return result, nil
}

// IsEnabled 报告一个插件 ID 是否在解析后的启用集合中。
func IsEnabled(enabled []string, id string) bool {
	for _, candidate := range enabled {
		if candidate == id {
			return true
		}
	}
	return false
}
