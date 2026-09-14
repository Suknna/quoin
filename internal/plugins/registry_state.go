package plugins

// registryState 集中持有目录内部状态与全部写路径校验，便于在单个文件内
// 审查不变量；公开方法见 registry.go。

import (
	"fmt"
	"sync"
)

type registryState struct {
	mu sync.RWMutex
	// descriptors 是权威描述目录（纯数据，注册后视为不可变）。
	descriptors map[string]Descriptor
	// toolOwners 记录模型工具名的规范归属（首个注册者）。同一契约工具
	// 可被多个提供者插件声明（如 PromQL 查询之于 prometheus/thanos），
	// 前提是声明逐字段相同；授权按实际来源连接解析。
	toolOwners map[string]string
	// toolDeclarations 保存每个工具名的规范声明，用于同契约校验。
	toolDeclarations map[string]Tool
	// bundles 是本进程真实绑定的执行实现，按插件 ID 唯一（一个进程一个
	// 执行位置）。
	bundles map[string]ExecutionBundle
}

// rlock/wlock 成对地获取内部状态并返回它，调用方只需 defer 对应的 Unlock；
// 这样每个公开方法读用 rlock/RUnlock、写用 wlock/Lock，杜绝漏锁。
func (s *registryState) rlock() *registryState { s.mu.RLock(); return s }

func (s *registryState) wlock() *registryState { s.mu.Lock(); return s }

func (s *registryState) registerDescriptor(descriptor Descriptor) error {
	if err := validateDescriptor(descriptor); err != nil {
		return err
	}
	if s.descriptors == nil {
		s.descriptors = map[string]Descriptor{}
	}
	if _, exists := s.descriptors[descriptor.ID]; exists {
		return fmt.Errorf("%w: %s", ErrDuplicateDescriptor, descriptor.ID)
	}
	for _, tool := range descriptor.Tools {
		if canonical, exists := s.toolDeclarations[tool.Name]; exists {
			if !sameToolContract(canonical, tool) {
				owner := s.toolOwners[tool.Name]
				return fmt.Errorf("%w: %s already declared by plugin %s with a different contract", ErrDuplicateToolName, tool.Name, owner)
			}
			continue
		}
		if s.toolDeclarations == nil {
			s.toolDeclarations = map[string]Tool{}
		}
		s.toolDeclarations[tool.Name] = tool
	}
	s.descriptors[descriptor.ID] = descriptor
	if s.toolOwners == nil {
		s.toolOwners = map[string]string{}
	}
	for _, tool := range descriptor.Tools {
		if _, exists := s.toolOwners[tool.Name]; !exists {
			s.toolOwners[tool.Name] = descriptor.ID
		}
	}
	return nil
}

// sameToolContract reports whether two declarations are the SAME tool
// contract: every field identical, parameters compared by canonical JSON
// bytes (round-trip safe).
func sameToolContract(a, b Tool) bool {
	if a.Name != b.Name || a.Version != b.Version || a.ExecutionLocation != b.ExecutionLocation ||
		a.FailureMode != b.FailureMode || a.Description != b.Description {
		return false
	}
	return sameSchemaBytes(a.Parameters, b.Parameters)
}

// registerBundle 绑定一个真实执行实现。规则（ADR-0004：声明不能伪装不
// 存在的实现，实现也不能脱离声明）：
//   - 描述必须已注册；
//   - 位置词表封闭，且每个插件只绑定一次；
//   - Capabilities 必须与下方非 nil 接口精确相等，且是描述声明能力的子集；
//   - 绑定 ToolExecutor 时，插件声明的每个工具必须由本位置执行。
func (s *registryState) registerBundle(bundle ExecutionBundle) error {
	descriptor, exists := s.descriptors[bundle.PluginID]
	if !exists {
		return fmt.Errorf("%w: no descriptor registered for plugin %s", ErrUndeclaredBundle, bundle.PluginID)
	}
	if !locationSet[bundle.Location] {
		return fmt.Errorf("%w: unknown execution location %q", ErrInvalidBundle, bundle.Location)
	}
	if s.bundles == nil {
		s.bundles = map[string]ExecutionBundle{}
	}
	if _, exists := s.bundles[bundle.PluginID]; exists {
		return fmt.Errorf("%w: plugin %s already has an execution bundle in this process", ErrDuplicateBundle, bundle.PluginID)
	}
	declared := map[Capability]bool{}
	for _, capability := range descriptor.Capabilities {
		declared[capability] = true
	}
	listed := map[Capability]bool{}
	for _, capability := range bundle.Capabilities {
		if !executionCapabilities[capability] {
			return fmt.Errorf("%w: capability %q is descriptive and cannot be bound to an execution", ErrInvalidBundle, capability)
		}
		if !backedFor(bundle, capability) {
			return fmt.Errorf("%w: capability %q is listed without its implementation", ErrInvalidBundle, capability)
		}
		if !declared[capability] {
			return fmt.Errorf("%w: plugin %s does not declare capability %q", ErrUndeclaredBundle, bundle.PluginID, capability)
		}
		listed[capability] = true
	}
	// 反向精确：非 nil 接口必须被列出，否则绑定宣称了未声明的能力集合。
	for capability, backed := range map[Capability]bool{
		CapabilityProbe:       bundle.Prober != nil,
		CapabilityDiscover:    bundle.Discoverer != nil,
		CapabilityExecuteTool: bundle.ToolExecutor != nil,
		CapabilityCollect:     bundle.Collector != nil,
	} {
		if backed && !listed[capability] {
			return fmt.Errorf("%w: %s is implemented but not listed in capabilities", ErrInvalidBundle, capability)
		}
	}
	if bundle.ToolExecutor != nil {
		for _, tool := range descriptor.Tools {
			if tool.ExecutionLocation != bundle.Location {
				return fmt.Errorf("%w: tool %s declares execution location %q, bundle executes at %q", ErrInvalidBundle, tool.Name, tool.ExecutionLocation, bundle.Location)
			}
		}
	}
	s.bundles[bundle.PluginID] = bundle
	return nil
}

// backedFor 报告一个执行型能力是否有对应的非 nil 实现。
func backedFor(bundle ExecutionBundle, capability Capability) bool {
	switch capability {
	case CapabilityProbe:
		return bundle.Prober != nil
	case CapabilityDiscover:
		return bundle.Discoverer != nil
	case CapabilityExecuteTool:
		return bundle.ToolExecutor != nil
	case CapabilityCollect:
		return bundle.Collector != nil
	}
	return false
}

// 注册边界拒绝的确定性错误。调用方（接线层）应把它们映射为构建/启动失败，
// 而不是运行时重试。
var (
	// ErrDuplicateDescriptor: 同一插件 ID 被注册两次。
	ErrDuplicateDescriptor = fmt.Errorf("duplicate plugin descriptor")
	// ErrDuplicateToolName: 同一模型工具名出现在多个插件（工具名是全局命名空间）。
	ErrDuplicateToolName = fmt.Errorf("duplicate tool name")
	// ErrInvalidDescriptor: 描述未通过静态校验（ID/schema/能力声明与数据不一致等）。
	ErrInvalidDescriptor = fmt.Errorf("invalid plugin descriptor")
	// ErrUnknownPlugin: 启用配置引用了未注册的插件 ID。
	ErrUnknownPlugin = fmt.Errorf("unknown plugin id")
	// ErrUndeclaredBundle: 执行绑定没有对应的已注册描述，或绑定了描述未
	// 声明的能力。
	ErrUndeclaredBundle = fmt.Errorf("execution bundle without matching declaration")
	// ErrInvalidBundle: 执行绑定自身不一致（未知位置、能力集合与非 nil
	// 接口不精确相等、工具执行位置不匹配等）。
	ErrInvalidBundle = fmt.Errorf("invalid execution bundle")
	// ErrDuplicateBundle: 同一插件在本进程被绑定第二次。
	ErrDuplicateBundle = fmt.Errorf("duplicate execution bundle")
	// ErrCatalogFrozen: 进程目录已冻结，禁止二次冻结。
	ErrCatalogFrozen = fmt.Errorf("plugin catalog already frozen")
)
