package config

// Embedded Journey Catalog static validation (CFG-JOURNEY-003,
// DATA-CONFIG-008): Quoin validates browser-check references offline against
// its own build-time embedded catalog. Until the full generator arrives with
// the Lintel browser stage, the catalog is the minimal empty-journeys
// document both components embed; the JCS bytes and digest therefore stay
// owned by the single lintel runtime definition and are referenced here
// rather than duplicated. Every journey_id is consequently rejected today —
// the structurally honest answer for a catalog with no journeys.

import (
	"encoding/json"

	lintelruntime "github.com/Suknna/quoin/internal/lintel/runtime"
)

// JourneyCatalog returns the embedded catalog document (parsed), its version
// and the raw-bytes digest (DATA-CONFIG-008).
func JourneyCatalog() (document map[string]any, version, digest string, err error) {
	digest = lintelruntime.EmptyCatalogDigest()
	version = lintelruntime.EmptyCatalogVersion
	if parseErr := json.Unmarshal([]byte(lintelruntime.EmptyCatalogJCS), &document); parseErr != nil {
		return nil, "", "", parseErr
	}
	return document, version, digest, nil
}

// ValidateJourneyReference statically validates one browser check against the
// embedded catalog: the stable journey_id must exist and the (normalized)
// params must satisfy that entry's closed params_schema.
func ValidateJourneyReference(journeyID string, params map[string]any, path string) []FieldError {
	document, _, _, err := JourneyCatalog()
	if err != nil {
		return []FieldError{{Path: path, Reason: "嵌入 Journey Catalog 无法加载: " + err.Error()}}
	}
	journeys, _ := document["journeys"].(map[string]any)
	entry, exists := journeys[journeyID]
	if !exists {
		return []FieldError{{
			Path:        path + ".journey_id",
			Reason:      "journey_id 不在嵌入 Journey Catalog 中: " + journeyID,
			Remediation: "使用 Catalog 中存在的稳定 Journey ID；浏览器能力接入后可在管理页查看 Catalog",
		}}
	}
	detail, _ := entry.(map[string]any)
	rawSchema, hasSchema := detail["params_schema"]
	if !hasSchema {
		// Catalog entries always declare a closed params_schema; a missing
		// schema is a catalog build fault surfaced deterministically.
		return []FieldError{{Path: path + ".journey_params", Reason: "Journey 缺少封闭参数 Schema（catalog 构建错误）"}}
	}
	if params == nil {
		params = map[string]any{}
	}
	// Compile the entry's schema through the same draft 2020-12 machinery.
	raw, err := json.Marshal(rawSchema)
	if err != nil {
		return []FieldError{{Path: path + ".journey_params", Reason: "Journey 参数 Schema 无法编码"}}
	}
	compiled, err := compileInlineSchema("journey:"+journeyID, raw)
	if err != nil {
		return []FieldError{{Path: path + ".journey_params", Reason: "Journey 参数 Schema 无法编译: " + firstLine(err.Error())}}
	}
	if err := compiled.Validate(params); err != nil {
		return []FieldError{{
			Path:        path + ".journey_params",
			Reason:      "journey_params 不满足该 Journey 的封闭参数 Schema: " + firstLine(err.Error()),
			Remediation: "参照 Journey 参数 Schema 调整参数",
		}}
	}
	return nil
}
