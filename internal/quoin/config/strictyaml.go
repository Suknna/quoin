package config

// Strict single-document YAML parsing (CFG-YAML-002): the input is decoded as
// a yaml.Node tree and checked node-by-node before conversion to the
// canonical JSON-shaped value that the frozen JSON Schemas validate. The
// lexical rejections are complete here: multi-document input, duplicate
// mapping keys, anchors/aliases/merge keys, custom tags, non-string field
// names, trailing content and the byte/node/depth limits. Unknown fields are
// NOT rejected here — that belongs to the closed schema objects only
// (CFG-SCOPE-002).

import (
	"bytes"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// Default document limits (CFG-YAML-002, HTTP-FILE-002): 10 MiB input,
// 100k AST nodes, 128 nesting levels.
const (
	DefaultMaxDocumentBytes = 10 << 20
	DefaultMaxNodes         = 100_000
	DefaultMaxDepth         = 128
)

// Limits are the deployment-tunable parse boundaries; zero fields fall back
// to the frozen defaults.
type Limits struct {
	MaxDocumentBytes int64
	MaxNodes         int
	MaxDepth         int
}

func (limits Limits) withDefaults() Limits {
	if limits.MaxDocumentBytes <= 0 {
		limits.MaxDocumentBytes = DefaultMaxDocumentBytes
	}
	if limits.MaxNodes <= 0 {
		limits.MaxNodes = DefaultMaxNodes
	}
	if limits.MaxDepth <= 0 {
		limits.MaxDepth = DefaultMaxDepth
	}
	return limits
}

// scalarTagAllowed lists the YAML core-schema tags a document may use; merge
// keys are handled (and rejected) at the mapping-key position and custom
// local tags (!foo) never resolve to this set.
var scalarTagAllowed = map[string]bool{
	"!!str": true, "!!int": true, "!!float": true, "!!bool": true,
	"!!null": true, "!!timestamp": true, "!!binary": true,
}

type strictParser struct {
	limits Limits
	nodes  int
	fail   []FieldError
	root   string
}

// ParseStrictYAML validates the strict lexical rules and returns the
// canonical JSON-shaped value (map[string]any / []any / string / bool /
// int64 / float64 / nil). The second return is nil on success; on failure the
// value is nil and the errors list is non-empty.
func ParseStrictYAML(body []byte, limits Limits, rootPath string) (any, []FieldError) {
	limits = limits.withDefaults()
	parser := &strictParser{limits: limits, root: rootPath}
	if int64(len(body)) > limits.MaxDocumentBytes {
		parser.fail = append(parser.fail, FieldError{
			Path:        rootPath,
			Reason:      fmt.Sprintf("文档超过大小上限（%d 字节，上限 %d）", len(body), limits.MaxDocumentBytes),
			Remediation: "拆分或精简 YAML 后重新上传",
		})
		return nil, parser.fail
	}
	if !utf8.Valid(body) {
		parser.fail = append(parser.fail, FieldError{Path: rootPath, Reason: "文档不是有效的 UTF-8 文本", Remediation: "以 UTF-8 编码保存后重新上传"})
		return nil, parser.fail
	}
	decoder := yaml.NewDecoder(bytes.NewReader(body))
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil {
		if err == io.EOF {
			parser.fail = append(parser.fail, FieldError{Path: rootPath, Reason: "文档为空", Remediation: "上传一份非空的 YAML 文档"})
		} else {
			parser.fail = append(parser.fail, FieldError{Path: rootPath, Reason: "YAML 解析失败: " + firstLine(err.Error()), Remediation: "修正语法错误后重新上传"})
		}
		return nil, parser.fail
	}
	// Single document only: a second successful decode (or trailing garbage
	// that fails to decode) is rejected.
	var trailing yaml.Node
	switch err := decoder.Decode(&trailing); err {
	case nil:
		parser.fail = append(parser.fail, FieldError{Path: rootPath, Reason: "存在第二个 YAML 文档", Remediation: "只上传单个文档（删除多余的 --- 分隔）"})
		return nil, parser.fail
	case io.EOF:
		// clean end of input
	default:
		parser.fail = append(parser.fail, FieldError{Path: rootPath, Reason: "文档结尾存在无法解析的多余内容", Remediation: "删除文档之后的尾随内容"})
		return nil, parser.fail
	}
	value := parser.convert(&document, rootPath, 1)
	if len(parser.fail) > 0 {
		return nil, parser.fail
	}
	return value, nil
}

func (parser *strictParser) convert(node *yaml.Node, path string, depth int) any {
	parser.nodes++
	if parser.nodes > parser.limits.MaxNodes {
		parser.failOnce(FieldError{Path: parser.root, Reason: fmt.Sprintf("文档 AST 节点数超过上限（%d）", parser.limits.MaxNodes), Remediation: "精简文档结构后重新上传"})
		return nil
	}
	if depth > parser.limits.MaxDepth {
		parser.failOnce(FieldError{Path: path, Reason: fmt.Sprintf("嵌套深度超过上限（%d 层）", parser.limits.MaxDepth), Remediation: "减少嵌套层级后重新上传"})
		return nil
	}
	if node.Anchor != "" {
		parser.fail = append(parser.fail, FieldError{Path: path, Reason: "YAML 锚点（&name）不被允许", Remediation: "把内容展开为普通结构，删除锚点定义"})
		return nil
	}
	switch node.Kind {
	case yaml.DocumentNode:
		if len(node.Content) != 1 {
			parser.fail = append(parser.fail, FieldError{Path: path, Reason: "文档结构不完整", Remediation: "上传一份以映射为根的单个 YAML 文档"})
			return nil
		}
		return parser.convert(node.Content[0], path, depth)
	case yaml.AliasNode:
		parser.fail = append(parser.fail, FieldError{Path: path, Reason: "YAML 别名（*name）不被允许", Remediation: "把被引用的内容直接写入使用位置"})
		return nil
	case yaml.MappingNode:
		return parser.convertMapping(node, path, depth)
	case yaml.SequenceNode:
		items := make([]any, 0, len(node.Content))
		for index, child := range node.Content {
			items = append(items, parser.convert(child, fmt.Sprintf("%s[%d]", path, index), depth+1))
		}
		return items
	case yaml.ScalarNode:
		return parser.convertScalar(node, path)
	default:
		parser.fail = append(parser.fail, FieldError{Path: path, Reason: "不支持的 YAML 节点类型", Remediation: "只使用映射、序列和标量"})
		return nil
	}
}

func (parser *strictParser) convertMapping(node *yaml.Node, path string, depth int) any {
	result := make(map[string]any, len(node.Content))
	for index := 0; index+1 < len(node.Content); index += 2 {
		keyNode := node.Content[index]
		valueNode := node.Content[index+1]
		if keyNode.Kind == yaml.AliasNode {
			parser.fail = append(parser.fail, FieldError{Path: path, Reason: "字段名不允许使用别名", Remediation: "字段名必须是普通字符串"})
			return nil
		}
		if keyNode.Tag == "!!merge" {
			parser.fail = append(parser.fail, FieldError{Path: path, Reason: "YAML merge key（<<）不被允许", Remediation: "把被合并的字段直接写入当前映射"})
			return nil
		}
		if keyNode.Kind != yaml.ScalarNode || keyNode.Tag != "!!str" {
			parser.fail = append(parser.fail, FieldError{
				Path:        path,
				Reason:      "字段名必须是字符串（当前是 " + nonStringKind(keyNode) + "）",
				Remediation: "为字段名加引号，例如 \"1\":",
			})
			return nil
		}
		if _, duplicate := result[keyNode.Value]; duplicate {
			parser.fail = append(parser.fail, FieldError{
				Path:        joinPath(path, keyNode.Value),
				Reason:      fmt.Sprintf("字段 %q 在同一层重复声明", keyNode.Value),
				Remediation: "合并重复字段，只保留一份声明",
			})
			return nil
		}
		result[keyNode.Value] = parser.convert(valueNode, joinPath(path, keyNode.Value), depth+1)
	}
	return result
}

func (parser *strictParser) convertScalar(node *yaml.Node, path string) any {
	switch node.Tag {
	case "!!str", "!!timestamp", "!!binary":
		return node.Value
	case "!!null":
		return nil
	case "!!bool":
		switch node.Value {
		case "true", "True", "TRUE":
			return true
		case "false", "False", "FALSE":
			return false
		}
		parser.fail = append(parser.fail, FieldError{Path: path, Reason: "无法识别的布尔值 " + node.Value, Remediation: "使用 true 或 false"})
		return nil
	case "!!int":
		parsed, err := strconv.ParseInt(strings.ReplaceAll(node.Value, "_", ""), 0, 64)
		if err != nil {
			parser.fail = append(parser.fail, FieldError{Path: path, Reason: "整数超出 64 位范围", Remediation: "使用 64 位范围内的整数"})
			return nil
		}
		return parsed
	case "!!float":
		parsed, err := strconv.ParseFloat(strings.ReplaceAll(node.Value, "_", ""), 64)
		if err != nil {
			parser.fail = append(parser.fail, FieldError{Path: path, Reason: "无法解析数值 " + node.Value, Remediation: "使用十进制数值"})
			return nil
		}
		return parsed
	default:
		parser.fail = append(parser.fail, FieldError{
			Path:        path,
			Reason:      "自定义 YAML tag（" + node.Tag + "）不被允许",
			Remediation: "删除 tag 标记，使用普通 YAML 值",
		})
		return nil
	}
}

func (parser *strictParser) failOnce(item FieldError) {
	if len(parser.fail) == 0 {
		parser.fail = append(parser.fail, item)
	}
}

func nonStringKind(node *yaml.Node) string {
	switch node.Tag {
	case "!!int":
		return "数字"
	case "!!bool":
		return "布尔值"
	case "!!null":
		return "空值"
	}
	return "非字符串"
}

func joinPath(parent, field string) string {
	if parent == "" {
		return field
	}
	return parent + "." + field
}

func firstLine(text string) string {
	if index := strings.IndexByte(text, '\n'); index >= 0 {
		text = text[:index]
	}
	return strings.TrimSpace(text)
}
