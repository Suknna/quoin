package config

// Frozen JSON Schema validation (CFG-SCOPE-001/CFG-VALIDATION-001): the two
// embedded draft 2020-12 documents under internal/gen/contracts are the only
// structural authority for the parsed document shapes. Schema failures are
// converted to the frozen fieldErrors list with document paths derived from
// the instance locations.

import (
	"fmt"
	"strings"
	"sync"

	"github.com/Suknna/quoin/internal/gen/contracts"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

// Schema document names accepted by ValidateSchema.
const (
	SchemaBusinessSystemConfig = "business-system-config"
	SchemaLabelContract        = "label-contract"
)

type compiledSchema struct {
	schema  *jsonschema.Schema
	version string
}

var (
	schemaOnce  sync.Once
	schemaError error
	// compiledSchemas holds the compiled frozen schemas; populated once.
	compiledSchemas = map[string]*compiledSchema{}
)

func loadSchemas() {
	schemaOnce.Do(func() {
		for name, body := range map[string][]byte{
			SchemaBusinessSystemConfig: contracts.BusinessSystemConfigSchema,
			SchemaLabelContract:        contracts.LabelContractSchema,
		} {
			document, err := jsonschema.UnmarshalJSON(strings.NewReader(string(body)))
			if err != nil {
				schemaError = fmt.Errorf("compile %s schema: %w", name, err)
				return
			}
			compiler := jsonschema.NewCompiler()
			resourceURL := "https://github.com/Suknna/quoin/schemas/" + name + ".schema.json"
			if err := compiler.AddResource(resourceURL, document); err != nil {
				schemaError = fmt.Errorf("register %s schema: %w", name, err)
				return
			}
			compiled, err := compiler.Compile(resourceURL)
			if err != nil {
				schemaError = fmt.Errorf("compile %s schema: %w", name, err)
				return
			}
			compiledSchemas[name] = &compiledSchema{schema: compiled, version: resourceURL}
		}
	})
}

// compileInlineSchema compiles an embedded (catalog-declared) params schema
// from raw JSON bytes under the given resource URL.
func compileInlineSchema(resourceURL string, raw []byte) (*jsonschema.Schema, error) {
	document, err := jsonschema.UnmarshalJSON(strings.NewReader(string(raw)))
	if err != nil {
		return nil, err
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource(resourceURL, document); err != nil {
		return nil, err
	}
	return compiler.Compile(resourceURL)
}

// ValidateSchema checks the canonical parsed value against the named frozen
// schema and returns the mapped field errors (empty when valid).
func ValidateSchema(value any, schemaName string) []FieldError {
	loadSchemas()
	if schemaError != nil {
		// A broken embedded schema is a build fault, not user input; surface
		// it as a single non-path error so the 422 still carries the cause.
		return []FieldError{{Path: "", Reason: "内部配置 Schema 无法加载: " + schemaError.Error()}}
	}
	entry, ok := compiledSchemas[schemaName]
	if !ok {
		return []FieldError{{Path: "", Reason: "未知配置 Schema: " + schemaName}}
	}
	err := entry.schema.Validate(value)
	if err == nil {
		return nil
	}
	validation, ok := err.(*jsonschema.ValidationError)
	if !ok {
		return []FieldError{{Path: "", Reason: "结构校验失败: " + firstLine(err.Error())}}
	}
	// Walk the validation tree itself: the basic-output projection loses the
	// specific messages of failures that cross a $ref boundary (they collapse
	// to "validation failed"), while the tree leaves keep the exact kind.
	printer := message.NewPrinter(language.English)
	var fields []FieldError
	var walk func(item *jsonschema.ValidationError)
	walk = func(item *jsonschema.ValidationError) {
		if len(item.Causes) == 0 {
			fields = append(fields, FieldError{
				Path:   instancePath(item.InstanceLocation),
				Reason: item.ErrorKind.LocalizedString(printer),
			})
			return
		}
		for _, child := range item.Causes {
			walk(child)
		}
	}
	walk(validation)
	if len(fields) == 0 {
		fields = append(fields, FieldError{Path: "", Reason: validation.Error()})
	}
	return describe(fields)
}

func instancePath(tokens []string) string {
	var builder strings.Builder
	for index, token := range tokens {
		if index == 0 {
			builder.WriteString(token)
			continue
		}
		if isIndex(token) {
			builder.WriteString("[" + token + "]")
		} else {
			builder.WriteString("." + token)
		}
	}
	return builder.String()
}

func isIndex(token string) bool {
	if token == "" {
		return false
	}
	for _, char := range token {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

// describe maps the library's English messages to ordinary-language reasons
// plus remediation hints for the frequent closed-object failures; unknown
// messages pass through unchanged so no failure is ever swallowed.
func describe(fields []FieldError) []FieldError {
	for index := range fields {
		message := fields[index].Reason
		switch {
		case strings.Contains(message, "additionalProperties"):
			fields[index].Reason = "存在 Schema 未定义的字段: " + stripLocation(message)
			fields[index].Remediation = "删除该字段；字段清单以模板与 Schema 为准"
		case strings.Contains(message, "missing properties"):
			fields[index].Reason = "缺少必填字段: " + stripLocation(message)
			fields[index].Remediation = "参照模板补齐必填字段"
		case strings.Contains(message, "oneOf"):
			fields[index].Reason = "巡检项必须且只能匹配 promql 或 browser 的一种封闭形态"
			fields[index].Remediation = "检查 kind 与对应字段，删除另一种形态的字段"
		case strings.Contains(message, "expected integer"):
			fields[index].Reason = "字段必须是整数: " + stripLocation(message)
			fields[index].Remediation = "使用整数值"
		case strings.Contains(message, "expected string"):
			fields[index].Reason = "字段必须是字符串: " + stripLocation(message)
			fields[index].Remediation = "使用文本值（必要时加引号）"
		case strings.Contains(message, "expected boolean"):
			fields[index].Reason = "字段必须是布尔值: " + stripLocation(message)
			fields[index].Remediation = "使用 true 或 false"
		}
	}
	return fields
}

// stripLocation removes the leading "at /path: " decoration the library adds
// to messages; the path is already carried by FieldError.Path.
func stripLocation(message string) string {
	if index := strings.Index(message, ": "); index >= 0 && strings.HasPrefix(message, "at ") {
		return message[index+2:]
	}
	return message
}
