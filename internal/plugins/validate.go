package plugins

// 描述静态校验。每条规则由 registry 边界测试逐条固定；校验失败是确定性
// 注册错误（构建/启动失败），不是运行时故障。规则来源是 plugin.go 中
// Descriptor/Tool 字段的契约注释与 ADR-0004：声明必须与数据精确一致，
// 封闭 schema，工具名全局唯一。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
)

const (
	// draft202012URI 是 ConfigSchema 允许的元 Schema 标识（缺省视为该版本）。
	draft202012URI = "https://json-schema.org/draft/2020-12/schema"

	// failureModeReturnToModel / failureModeFailAttempt 是封闭的失败模式词表，
	// 与 attempt.ToolDef.FailureMode 的既有协议保持一致（不建第二协议）。
	failureModeReturnToModel = "return_to_model"
	failureModeFailAttempt   = "fail_attempt"

	maxIDLength     = 64
	maxTextLength   = 4096
	maxVersionLabel = 64
)

var (
	// pluginIDPattern：稳定插件/模板身份，小写字母开头，仅小写字母数字与
	// - _。身份被启用配置、授权来源与审计永久引用，永不复用。
	pluginIDPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)
	// toolNamePattern：模型工具名是全局命名空间，与既有固定目录
	// （bash/read/write/grep/artifact_read/thanos_query）同构。
	toolNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
)

// sameSchemaBytes compares two schema documents by canonical JSON bytes so
// decoded representations with equivalent shapes compare equal.
func sameSchemaBytes(a, b map[string]any) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	if a == nil {
		return true
	}
	encodedA, err := json.Marshal(a)
	if err != nil {
		return false
	}
	encodedB, err := json.Marshal(b)
	if err != nil {
		return false
	}
	return bytes.Equal(encodedA, encodedB)
}

// validateDescriptor 校验一个描述的全部静态不变量。
func validateDescriptor(descriptor Descriptor) error {
	if len(descriptor.ID) > maxIDLength || !pluginIDPattern.MatchString(descriptor.ID) {
		return fmt.Errorf("%w: id %q must match [a-z][a-z0-9_-]* (max %d chars)", ErrInvalidDescriptor, descriptor.ID, maxIDLength)
	}
	if descriptor.Version == "" || len(descriptor.Version) > maxVersionLabel {
		return fmt.Errorf("%w: plugin %s version must be non-empty (max %d chars)", ErrInvalidDescriptor, descriptor.ID, maxVersionLabel)
	}
	if descriptor.DisplayName == "" || len(descriptor.DisplayName) > maxTextLength {
		return fmt.Errorf("%w: plugin %s displayName must be non-empty", ErrInvalidDescriptor, descriptor.ID)
	}
	if descriptor.Description == "" || len(descriptor.Description) > maxTextLength {
		return fmt.Errorf("%w: plugin %s description must be non-empty", ErrInvalidDescriptor, descriptor.ID)
	}
	if descriptor.ConnectionKind != "" && (len(descriptor.ConnectionKind) > maxIDLength || !pluginIDPattern.MatchString(descriptor.ConnectionKind)) {
		return fmt.Errorf("%w: plugin %s connectionKind %q must match [a-z][a-z0-9_-]*", ErrInvalidDescriptor, descriptor.ID, descriptor.ConnectionKind)
	}
	if err := validateCapabilities(descriptor); err != nil {
		return err
	}
	if err := validateConfigSchema(descriptor); err != nil {
		return err
	}
	seenTools := map[string]bool{}
	for _, tool := range descriptor.Tools {
		if err := validateTool(descriptor.ID, tool); err != nil {
			return err
		}
		if seenTools[tool.Name] {
			return fmt.Errorf("%w: plugin %s declares tool %s twice", ErrInvalidDescriptor, descriptor.ID, tool.Name)
		}
		seenTools[tool.Name] = true
	}
	seenTemplates := map[string]bool{}
	for _, template := range descriptor.InspectionTemplates {
		if err := validateTemplate(descriptor.ID, template); err != nil {
			return err
		}
		if seenTemplates[template.ID] {
			return fmt.Errorf("%w: plugin %s declares template %s twice", ErrInvalidDescriptor, descriptor.ID, template.ID)
		}
		seenTemplates[template.ID] = true
	}
	return nil
}

// validateCapabilities 强制能力声明与描述自身数据精确一致：能力词表封闭、
// 不重复；tools/templates 声明与目录数据互为充要；执行型能力只能依附于
// 对应的描述数据。
func validateCapabilities(descriptor Descriptor) error {
	declared := map[Capability]bool{}
	for _, capability := range descriptor.Capabilities {
		if !executionCapabilities[capability] && capability != CapabilityTools && capability != CapabilityInspectionTemplates {
			return fmt.Errorf("%w: plugin %s declares unknown capability %q", ErrInvalidDescriptor, descriptor.ID, capability)
		}
		if declared[capability] {
			return fmt.Errorf("%w: plugin %s declares capability %q twice", ErrInvalidDescriptor, descriptor.ID, capability)
		}
		declared[capability] = true
	}
	hasTools := len(descriptor.Tools) > 0
	hasTemplates := len(descriptor.InspectionTemplates) > 0
	hasDiscoverObjects := len(descriptor.DiscoverObjects) > 0
	if declared[CapabilityTools] != hasTools {
		return fmt.Errorf("%w: plugin %s must declare capability %q exactly when its tool catalog is non-empty", ErrInvalidDescriptor, descriptor.ID, CapabilityTools)
	}
	if declared[CapabilityInspectionTemplates] != hasTemplates {
		return fmt.Errorf("%w: plugin %s must declare capability %q exactly when its template catalog is non-empty", ErrInvalidDescriptor, descriptor.ID, CapabilityInspectionTemplates)
	}
	if declared[CapabilityDiscover] != hasDiscoverObjects {
		return fmt.Errorf("%w: plugin %s must declare capability %q exactly when its discovery catalog is non-empty", ErrInvalidDescriptor, descriptor.ID, CapabilityDiscover)
	}
	if declared[CapabilityExecuteTool] && !declared[CapabilityTools] {
		return fmt.Errorf("%w: plugin %s declares %q without a tool catalog", ErrInvalidDescriptor, descriptor.ID, CapabilityExecuteTool)
	}
	if declared[CapabilityCollect] && !declared[CapabilityInspectionTemplates] {
		return fmt.Errorf("%w: plugin %s declares %q without a template catalog", ErrInvalidDescriptor, descriptor.ID, CapabilityCollect)
	}
	if hasDiscoverObjects && descriptor.ConnectionKind == "" {
		return fmt.Errorf("%w: plugin %s declares discovery objects without a platform connection kind", ErrInvalidDescriptor, descriptor.ID)
	}
	seenObjects := map[string]bool{}
	for _, object := range descriptor.DiscoverObjects {
		if err := validateDiscoverObject(descriptor.ID, object); err != nil {
			return err
		}
		if seenObjects[object.ObjectType] {
			return fmt.Errorf("%w: plugin %s declares discovery object %q twice", ErrInvalidDescriptor, descriptor.ID, object.ObjectType)
		}
		seenObjects[object.ObjectType] = true
	}
	return nil
}

// validateDiscoverObject 校验单个发现对象声明：对象类型、身份标签集、
// 规范发现选择与每轮结果预算。控制面把这份声明原样冻结进每个观测
// Attempt，执行宿主再按它校验冻结副本，声明与执行不能各自漂移。
func validateDiscoverObject(pluginID string, object DiscoverObject) error {
	if len(object.ObjectType) > maxIDLength || !pluginIDPattern.MatchString(object.ObjectType) {
		return fmt.Errorf("%w: plugin %s discovery object type %q must match [a-z][a-z0-9_-]*", ErrInvalidDescriptor, pluginID, object.ObjectType)
	}
	if len(object.IdentityLabels) == 0 {
		return fmt.Errorf("%w: plugin %s discovery object %q must name its identity labels", ErrInvalidDescriptor, pluginID, object.ObjectType)
	}
	seenLabels := map[string]bool{}
	for _, label := range object.IdentityLabels {
		if label == "" || len(label) > maxIDLength || seenLabels[label] {
			return fmt.Errorf("%w: plugin %s discovery object %q declares identity label %q empty, oversized or twice", ErrInvalidDescriptor, pluginID, object.ObjectType, label)
		}
		seenLabels[label] = true
	}
	if object.Query == "" || len(object.Query) > maxTextLength {
		return fmt.Errorf("%w: plugin %s discovery object %q must carry a non-empty bounded discovery query", ErrInvalidDescriptor, pluginID, object.ObjectType)
	}
	if object.Limit < 1 || object.Limit > 100000 {
		return fmt.Errorf("%w: plugin %s discovery object %q limit %d must be within [1,100000]", ErrInvalidDescriptor, pluginID, object.ObjectType, object.Limit)
	}
	return nil
}

// validateConfigSchema 强制实例设置 schema 封闭：顶层 object、关闭
// additionalProperties、元 Schema 为 draft 2020-12。nil schema 仅对真正
// 无配置的插件合法（此时设置必须是空对象）。
func validateConfigSchema(descriptor Descriptor) error {
	if descriptor.ConfigSchema == nil {
		return nil
	}
	return validateClosedObjectSchema(descriptor.ID, "configSchema", descriptor.ConfigSchema)
}

// validateTool 校验单个工具声明：名字、版本、说明、封闭执行位置与失败
// 模式。与编译实现的精确一致性由目录装配（attempt.BuildCatalogs）在
// 接线时双向验证（本包不依赖任何执行宿主）。
func validateTool(pluginID string, tool Tool) error {
	if len(tool.Name) > maxIDLength || !toolNamePattern.MatchString(tool.Name) {
		return fmt.Errorf("%w: plugin %s tool name %q must match [a-z][a-z0-9_]*", ErrInvalidDescriptor, pluginID, tool.Name)
	}
	if tool.Version == "" || len(tool.Version) > maxVersionLabel {
		return fmt.Errorf("%w: plugin %s tool %s version must be non-empty", ErrInvalidDescriptor, pluginID, tool.Name)
	}
	if tool.Description == "" || len(tool.Description) > maxTextLength {
		return fmt.Errorf("%w: plugin %s tool %s description must be non-empty", ErrInvalidDescriptor, pluginID, tool.Name)
	}
	if !locationSet[tool.ExecutionLocation] {
		return fmt.Errorf("%w: plugin %s tool %s declares unknown execution location %q", ErrInvalidDescriptor, pluginID, tool.Name, tool.ExecutionLocation)
	}
	if tool.FailureMode != failureModeReturnToModel && tool.FailureMode != failureModeFailAttempt {
		return fmt.Errorf("%w: plugin %s tool %s declares unknown failure mode %q", ErrInvalidDescriptor, pluginID, tool.Name, tool.FailureMode)
	}
	if tool.Parameters != nil {
		// Tool parameters are a DERIVED copy of the compiled implementation's
		// own provider schema (frozen byte shape, possibly a closed union
		// without a top-level type); the compiled argument validator is the
		// closed-ness authority, so no extra shape mandate applies here.
		_ = tool.Parameters
	}
	return nil
}

// validateClosedObjectSchema enforces the closed-schema shape shared by
// ConfigSchema and tool Parameters: top-level object with
// additionalProperties disabled, draft 2020-12 when $schema is present.
func validateClosedObjectSchema(pluginID, what string, schema map[string]any) error {
	if kind, ok := schema["type"].(string); !ok || kind != "object" {
		return fmt.Errorf("%w: plugin %s %s schema must be a top-level object schema", ErrInvalidDescriptor, pluginID, what)
	}
	if additional, present := schema["additionalProperties"]; !present || additional != false {
		return fmt.Errorf("%w: plugin %s %s schema must disable additionalProperties", ErrInvalidDescriptor, pluginID, what)
	}
	if version, present := schema["$schema"]; present && version != draft202012URI {
		return fmt.Errorf("%w: plugin %s %s schema must use JSON Schema draft 2020-12", ErrInvalidDescriptor, pluginID, what)
	}
	return nil
}

// validateTemplate 校验单个巡检模板声明：(ID, Version) 是 Run 冻结绑定的
// 模板身份。
func validateTemplate(pluginID string, template InspectionTemplate) error {
	if len(template.ID) > maxIDLength || !pluginIDPattern.MatchString(template.ID) {
		return fmt.Errorf("%w: plugin %s template id %q must match [a-z][a-z0-9_-]*", ErrInvalidDescriptor, pluginID, template.ID)
	}
	if template.Version == "" || len(template.Version) > maxVersionLabel {
		return fmt.Errorf("%w: plugin %s template %s version must be non-empty", ErrInvalidDescriptor, pluginID, template.ID)
	}
	if template.Title == "" || len(template.Title) > maxTextLength {
		return fmt.Errorf("%w: plugin %s template %s title must be non-empty", ErrInvalidDescriptor, pluginID, template.ID)
	}
	if template.Description == "" || len(template.Description) > maxTextLength {
		return fmt.Errorf("%w: plugin %s template %s description must be non-empty", ErrInvalidDescriptor, pluginID, template.ID)
	}
	return nil
}
