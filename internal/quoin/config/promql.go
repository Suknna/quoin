package config

// PromQL semantic validation against the official Prometheus parser
// (CFG-PROMQL-001/002/003): every expression and discovery selector is parsed
// with the locked upstream AST — never regex or string rewriting — and every
// VectorSelector must carry the target Label Contract's business-system label
// as an exact `=` match on the current system key. Discovery selectors must
// additionally be a single instant vector selector without offset/@,
// aggregation, label_replace or subqueries.

import (
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"
)

// promQLParser is the frozen parser instance (all experimental features off,
// matching the deployment-locked upstream AST).
var promQLParser = parser.NewParser(parser.Options{})

// ValidateCheckExpression parses one check expression and enforces the
// ownership rule on every VectorSelector (CFG-PROMQL-002). offset/@ modifiers
// and subqueries are allowed here (CFG-PROMQL-003); only the discovery
// selector carries the instant-vector restriction.
func ValidateCheckExpression(expression, businessSystemLabel, systemKey, path string) []FieldError {
	expr, err := promQLParser.ParseExpr(expression)
	if err != nil {
		return []FieldError{{Path: path, Reason: "PromQL 解析失败: " + firstLine(err.Error()), Remediation: "修正表达式语法后重试"}}
	}
	return inspectSelectors(expr, businessSystemLabel, systemKey, path)
}

// ValidateDiscoverySelector enforces the single instant VectorSelector shape
// (CFG-PROMQL-003): the AST must be the selector itself, with no offset/@
// modifier, no aggregation, no label_replace/label_join and no subquery.
func ValidateDiscoverySelector(selector, businessSystemLabel, systemKey, path string) []FieldError {
	expr, err := promQLParser.ParseExpr(selector)
	if err != nil {
		return []FieldError{{Path: path, Reason: "PromQL 解析失败: " + firstLine(err.Error()), Remediation: "修正 selector 语法后重试"}}
	}
	vector, ok := expr.(*parser.VectorSelector)
	if !ok {
		return []FieldError{{
			Path:        path,
			Reason:      "discovery selector 必须是单个即时向量选择器（如 up{...}），不允许聚合、函数、子查询或括号表达式",
			Remediation: "只保留一个基本的向量选择器",
		}}
	}
	if vector.OriginalOffset != 0 || vector.OriginalOffsetExpr != nil {
		return []FieldError{{Path: path, Reason: "discovery selector 不允许 offset 修饰（禁止用历史数据伪装当前资源）", Remediation: "删除 offset 修饰符"}}
	}
	if vector.Timestamp != nil || vector.StartOrEnd != 0 {
		return []FieldError{{Path: path, Reason: "discovery selector 不允许 @ 修饰符", Remediation: "删除 @ 修饰符"}}
	}
	return ownershipErrors(vector, businessSystemLabel, systemKey, path)
}

// inspectSelectors walks the full AST and checks the ownership rule on every
// embedded VectorSelector.
func inspectSelectors(expr parser.Expr, businessSystemLabel, systemKey, path string) []FieldError {
	var failures []FieldError
	parser.Inspect(expr, func(node parser.Node, _ []parser.Node) error {
		if vector, ok := node.(*parser.VectorSelector); ok {
			if errs := ownershipErrors(vector, businessSystemLabel, systemKey, path); len(errs) > 0 {
				failures = append(failures, errs...)
			}
		}
		return nil
	})
	return failures
}

// ownershipErrors requires one exact MatchEqual on the contract label whose
// value equals the current system key; inequality, regex or negation are all
// rejected (CFG-PROMQL-002).
func ownershipErrors(vector *parser.VectorSelector, businessSystemLabel, systemKey, path string) []FieldError {
	for _, matcher := range vector.LabelMatchers {
		if matcher.Type == labels.MatchEqual && matcher.Name == businessSystemLabel {
			if matcher.Value == systemKey {
				return nil
			}
			return []FieldError{{
				Path:        path,
				Reason:      "业务系统归属匹配值必须等于当前 system key（期望 " + systemKey + "，实际 " + matcher.Value + "）",
				Remediation: "把 selector 中 " + businessSystemLabel + " 的值改为当前 system key",
			}}
		}
	}
	return []FieldError{{
		Path:        path,
		Reason:      "每个向量选择器必须携带 " + businessSystemLabel + "=\"" + systemKey + "\" 的精确匹配（目标 Label Contract 的归属约束）",
		Remediation: "在 selector 中加入 " + businessSystemLabel + ": " + systemKey + " 精确匹配",
	}}
}
