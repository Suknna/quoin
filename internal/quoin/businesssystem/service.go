// Package businesssystem owns the Business System aggregate (T16): the
// parse-once upload that creates a Disabled system with its first immutable
// draft in one transaction, the append-only immutable configuration versions
// with their typed projections, and the single-UPDATE publish command that
// moves business_systems.current_config_version_id under the frozen
// concurrency fence (DATA-CONFIG-001/003/004, HTTP-CONFIG-001/002).
package businesssystem

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/config"
)

// ErrNotFound reports a missing business system or version.
var ErrNotFound = errors.New("business system or config version not found")

// ErrCommandReused reports a client command id replayed with a different
// request digest (HTTP-COMMAND-003).
var ErrCommandReused = errors.New("client command id reused with a different request")

// ConflictError carries the frozen publish conflict codes.
type ConflictError struct {
	Code           string // row_version_conflict | current_pointer_conflict
	Detail         string
	SystemKey      string
	ObjectID       int64
	CurrentVersion *int64 // actual current published version id (nil = none)
}

func (err *ConflictError) Error() string { return err.Detail }

// UploadInput carries the multipart command fields; YAMLBody is the raw
// document bytes.
type UploadInput struct {
	YAMLBody                   []byte
	TargetLabelContractVersion int64
	JourneyCatalogDigest       string // optional; must equal the embedded digest when set
}

// Service owns the Business System SQLite transactions.
type Service struct {
	db  *sql.DB
	now func() time.Time
}

// NewService builds the service on the shared database pool.
func NewService(db *sql.DB) *Service {
	return &Service{db: db, now: time.Now}
}

func (service *Service) nowText() string { return service.now().UTC().Format(time.RFC3339Nano) }

// Upload parses the strict YAML once, validates it against the explicitly
// targeted Label Contract version and the embedded Journey Catalog, then
// persists the immutable draft and its typed projections in one transaction.
// A first upload of an unknown stable key creates the Disabled Business
// System in the same transaction (HTTP-CONFIG-001, MUST NOT 404).
func (service *Service) Upload(ctx context.Context, principalID int64, clientCommandID string, input UploadInput, limits config.Limits) (VersionDetail, error) {
	catalogDocument, catalogVersion, catalogDigest, err := config.JourneyCatalog()
	if err != nil {
		return VersionDetail{}, err
	}
	if input.JourneyCatalogDigest != "" && input.JourneyCatalogDigest != catalogDigest {
		return VersionDetail{}, &config.ValidationError{Errors: []config.FieldError{{
			Path:        "journeyCatalogDigest",
			Reason:      "提供的 Journey Catalog digest 与 Quoin 嵌入版本不一致",
			Remediation: "删除该字段以记录当前嵌入 digest，或刷新页面后使用当前 catalog digest",
		}}}
	}
	// Resolve the explicit target contract version before validation: the
	// PromQL ownership rule needs the contract's business_system_label
	// (CFG-CONTRACT-003 — never the silently-current contract).
	var (
		contractID    int64
		contractState string
		contractJSON  string
	)
	err = service.db.QueryRowContext(ctx, `SELECT id,state,contract_json FROM label_contracts WHERE version=?`, input.TargetLabelContractVersion).Scan(&contractID, &contractState, &contractJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return VersionDetail{}, &config.ValidationError{Errors: []config.FieldError{{
			Path:        "targetLabelContractVersion",
			Reason:      fmt.Sprintf("目标 Label Contract 版本不存在: %d", input.TargetLabelContractVersion),
			Remediation: "选择一个已上传的契约版本",
		}}}
	}
	if err != nil {
		return VersionDetail{}, err
	}
	if contractState == "retired" {
		return VersionDetail{}, &config.ValidationError{Errors: []config.FieldError{{
			Path:        "targetLabelContractVersion",
			Reason:      fmt.Sprintf("目标 Label Contract 版本已 retired，不能作为上传目标: %d", input.TargetLabelContractVersion),
			Remediation: "选择一个 draft 或 active 契约版本",
		}}}
	}
	businessSystemLabel, err := labelOfContract(contractJSON)
	if err != nil {
		return VersionDetail{}, err
	}
	value, fieldErrors := config.ParseStrictYAML(input.YAMLBody, limits, "document")
	if len(fieldErrors) != 0 {
		return VersionDetail{}, &config.ValidationError{Errors: fieldErrors}
	}
	if fields := config.ValidateSchema(value, config.SchemaBusinessSystemConfig); len(fields) != 0 {
		return VersionDetail{}, &config.ValidationError{Errors: fields}
	}
	document, err := config.ExtractBusinessSystem(value)
	if err != nil {
		return VersionDetail{}, &config.ValidationError{Errors: []config.FieldError{{Path: "document", Reason: err.Error()}}}
	}
	if fields := config.SemanticChecks(document, businessSystemLabel); len(fields) != 0 {
		return VersionDetail{}, &config.ValidationError{Errors: fields}
	}
	_ = catalogDocument
	digest := document.Digest()
	commandDigest := auth.DigestCommand("business_system.config.upload", map[string]any{
		"digest": digest, "labelContractVersion": input.TargetLabelContractVersion, "catalogDigest": catalogDigest,
	})
	if record, found, lookupErr := auth.LookupCommand(ctx, service.db, principalID, clientCommandID); lookupErr == nil && found {
		if record.RequestDigest != commandDigest {
			return VersionDetail{}, ErrCommandReused
		}
		var replayed VersionDetail
		if err := decodeStored(record.ResultPayload, &replayed); err == nil && replayed.ID != "" {
			return replayed, nil
		}
	}
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return VersionDetail{}, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return VersionDetail{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	// Re-verify the target contract inside the serialized transaction; a
	// concurrent activation that retires it must fail this upload.
	var currentState string
	if err := conn.QueryRowContext(ctx, `SELECT state FROM label_contracts WHERE id=?`, contractID).Scan(&currentState); err != nil {
		return VersionDetail{}, err
	}
	if currentState != contractState {
		return VersionDetail{}, &config.ValidationError{Errors: []config.FieldError{{
			Path:        "targetLabelContractVersion",
			Reason:      "目标 Label Contract 状态刚发生变化（" + currentState + "），请刷新后重试",
			Remediation: "重新选择目标契约版本",
		}}}
	}
	now := service.nowText()
	var systemID int64
	if err := conn.QueryRowContext(ctx, `SELECT id FROM business_systems WHERE key=?`, document.SystemKey).Scan(&systemID); errors.Is(err, sql.ErrNoRows) {
		insert, insertErr := conn.ExecContext(ctx, `
			INSERT INTO business_systems(key,display_name,enabled,row_version,created_at) VALUES(?,?,0,1,?)`,
			document.SystemKey, document.DisplayName, now)
		if insertErr != nil {
			return VersionDetail{}, insertErr
		}
		systemID, _ = insert.LastInsertId()
	} else if err != nil {
		return VersionDetail{}, err
	}
	var versionSeq int64
	if err := conn.QueryRowContext(ctx, `SELECT COALESCE(MAX(version_seq),0)+1 FROM business_system_config_versions WHERE business_system_id=?`, systemID).Scan(&versionSeq); err != nil {
		return VersionDetail{}, err
	}
	versionInsert, err := conn.ExecContext(ctx, `
		INSERT INTO business_system_config_versions(
			business_system_id,version_seq,state,yaml_body,parser_version,schema_version,
			label_contract_version_id,journey_catalog_digest,journey_catalog_version,digest,
			created_by,created_at,system_key,display_name,enabled,timezone,resource_refresh_interval_seconds)
		VALUES(?,?,'draft',?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		systemID, versionSeq, string(input.YAMLBody), config.ParserVersion, config.SchemaVersion(config.SchemaBusinessSystemConfig),
		contractID, catalogDigest, catalogVersion, digest, principalID, now,
		document.SystemKey, document.DisplayName, boolToInt(document.Enabled), document.Timezone, document.ResourceRefreshIntervalSeconds)
	if err != nil {
		return VersionDetail{}, err
	}
	versionID, err := versionInsert.LastInsertId()
	if err != nil {
		return VersionDetail{}, err
	}
	if err := insertProjections(ctx, conn, versionID, document); err != nil {
		return VersionDetail{}, err
	}
	if err := service.audit(ctx, conn, principalID, "business_system.config.upload", systemID, now); err != nil {
		return VersionDetail{}, err
	}
	detail, err := service.versionDetailOn(ctx, conn, systemID, versionID)
	if err != nil {
		return VersionDetail{}, err
	}
	if err := auth.RecordCommand(ctx, conn, principalID, clientCommandID, "business_system.config.upload", commandDigest, "committed", "business_system_config_version", versionID, encode(detail)); err != nil {
		return VersionDetail{}, err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return VersionDetail{}, err
	}
	committed = true
	return detail, nil
}

// insertProjections persists the typed discovery/plan/check columns
// (DATA-CONFIG-003: runtime never re-parses the YAML).
func insertProjections(ctx context.Context, conn *sql.Conn, versionID int64, document config.BusinessSystemDocument) error {
	for _, discovery := range document.Discoveries {
		labels, err := json.Marshal(discovery.IdentityLabels)
		if err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `
			INSERT INTO config_discoveries(config_version_id,discovery_key,display_name,selector,identity_labels_json) VALUES(?,?,?,?,?)`,
			versionID, discovery.Key, discovery.DisplayName, discovery.Selector, string(labels)); err != nil {
			return err
		}
	}
	for _, plan := range document.Plans {
		planInsert, err := conn.ExecContext(ctx, `
			INSERT INTO config_plans(config_version_id,plan_key,display_name,cron) VALUES(?,?,?,?)`,
			versionID, plan.Key, plan.DisplayName, nullableString(plan.Cron))
		if err != nil {
			return err
		}
		planID, err := planInsert.LastInsertId()
		if err != nil {
			return err
		}
		for _, check := range plan.Checks {
			var queryMode, expression, journeyID, journeyParams any
			var rangeSeconds, stepSeconds any
			if check.Kind == "promql" {
				queryMode, expression = check.QueryMode, check.Expression
				if check.QueryMode == "range" {
					rangeSeconds, stepSeconds = check.RangeSeconds, check.StepSeconds
				}
			} else {
				journeyID = check.JourneyID
				params, err := json.Marshal(check.JourneyParams)
				if err != nil {
					return err
				}
				journeyParams = string(params)
			}
			if _, err := conn.ExecContext(ctx, `
				INSERT INTO config_checks(plan_id,check_key,display_name,analysis_question,kind,query_mode,expression,range_seconds,step_seconds,journey_id,journey_params_json)
				VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
				planID, check.Key, check.DisplayName, check.AnalysisQuestion, check.Kind, queryMode, expression, rangeSeconds, stepSeconds, journeyID, journeyParams); err != nil {
				return err
			}
		}
	}
	return nil
}

// Publish moves the current pointer in one UPDATE carrying the target
// version's root projection; the WHERE clause compares
// expectedCurrentPublishedVersionId (nil = first publish) and the frozen
// triggers derive the published/superseded states and enforce the Label
// Contract fence (DATA-CONFIG-001, HTTP-CONFIG-002).
func (service *Service) Publish(ctx context.Context, principalID int64, clientCommandID, systemKey string, versionID int64, expectedCurrent *int64) (SystemDetail, error) {
	commandDigest := auth.DigestCommand("business_system.config.publish", map[string]any{
		"systemKey": systemKey, "versionId": versionID, "expectedCurrent": idString(expectedCurrent),
	})
	if record, found, lookupErr := auth.LookupCommand(ctx, service.db, principalID, clientCommandID); lookupErr == nil && found {
		if record.RequestDigest != commandDigest {
			return SystemDetail{}, ErrCommandReused
		}
		var replayed SystemDetail
		if err := decodeStored(record.ResultPayload, &replayed); err == nil && replayed.Key != "" {
			return replayed, nil
		}
	}
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return SystemDetail{}, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return SystemDetail{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	var systemID int64
	if err := conn.QueryRowContext(ctx, `SELECT id FROM business_systems WHERE key=?`, systemKey).Scan(&systemID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return SystemDetail{}, ErrNotFound
		}
		return SystemDetail{}, err
	}
	// The target must be an unpublished version of this system; published
	// versions can never be re-published (trg_business_systems_config_owner_update).
	var projection struct {
		DisplayName string
		Enabled     int64
		Timezone    string
		Refresh     int64
		PublishedAt sql.NullString
	}
	err = conn.QueryRowContext(ctx, `
		SELECT display_name,enabled,timezone,resource_refresh_interval_seconds,published_at
		FROM business_system_config_versions WHERE id=? AND business_system_id=?`, versionID, systemID).Scan(
		&projection.DisplayName, &projection.Enabled, &projection.Timezone, &projection.Refresh, &projection.PublishedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return SystemDetail{}, ErrNotFound
	}
	if err != nil {
		return SystemDetail{}, err
	}
	if projection.PublishedAt.Valid {
		var current sql.NullInt64
		_ = conn.QueryRowContext(ctx, `SELECT current_config_version_id FROM business_systems WHERE id=?`, systemID).Scan(&current)
		return SystemDetail{}, &ConflictError{
			Code: "current_pointer_conflict", Detail: "该配置版本已被发布过，发布指针只能移动到未发布版本",
			SystemKey: systemKey, ObjectID: versionID, CurrentVersion: nullInt64Ptr(current),
		}
	}
	update, err := conn.ExecContext(ctx, `
		UPDATE business_systems SET
			current_config_version_id=?,
			display_name=?, enabled=?, timezone=?, resource_refresh_interval_seconds=?,
			row_version=row_version+1
		WHERE id=? AND current_config_version_id IS ?`,
		versionID, projection.DisplayName, projection.Enabled, projection.Timezone, projection.Refresh, systemID, expectedCurrent)
	if err != nil {
		return SystemDetail{}, mapPublishAbort(err, systemKey, versionID)
	}
	affected, err := update.RowsAffected()
	if err != nil {
		return SystemDetail{}, err
	}
	if affected == 0 {
		var current sql.NullInt64
		_ = conn.QueryRowContext(ctx, `SELECT current_config_version_id FROM business_systems WHERE id=?`, systemID).Scan(&current)
		return SystemDetail{}, &ConflictError{
			Code: "current_pointer_conflict", Detail: "当前已发布版本与 expectedCurrentPublishedVersionId 不匹配，请刷新后重试",
			SystemKey: systemKey, ObjectID: versionID, CurrentVersion: nullInt64Ptr(current),
		}
	}
	if err := service.audit(ctx, conn, principalID, "business_system.config.publish", systemID, service.nowText()); err != nil {
		return SystemDetail{}, err
	}
	detail, err := service.systemDetailOn(ctx, conn, systemID)
	if err != nil {
		return SystemDetail{}, err
	}
	if err := auth.RecordCommand(ctx, conn, principalID, clientCommandID, "business_system.config.publish", commandDigest, "committed", "business_system", systemID, encode(detail)); err != nil {
		return SystemDetail{}, err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return SystemDetail{}, err
	}
	committed = true
	return detail, nil
}

func (service *Service) audit(ctx context.Context, conn *sql.Conn, principalID int64, action string, systemID int64, timestamp string) error {
	_, err := conn.ExecContext(ctx, `INSERT INTO audit_events(actor_type,actor_id,action,outcome,domain_ref_type,domain_ref_id,created_at) VALUES('user',?,?,'success','business_system',?,?)`,
		principalID, action, systemID, timestamp)
	return err
}

// mapPublishAbort converts the frozen publish triggers' RAISE messages into
// typed conflict errors; unknown aborts propagate unchanged.
func mapPublishAbort(err error, systemKey string, versionID int64) error {
	message := err.Error()
	switch {
	case strings.Contains(message, "can only be published by that contract atomic activation"):
		return &ConflictError{
			Code:      "current_pointer_conflict",
			Detail:    "该配置版本以非当前 Label Contract 为目标，只能随该契约的原子联合激活切换",
			SystemKey: systemKey, ObjectID: versionID,
		}
	case strings.Contains(message, "current config pointer can only move to an unpublished version"):
		return &ConflictError{Code: "current_pointer_conflict", Detail: "发布指针只能移动到同系统的未发布版本", SystemKey: systemKey, ObjectID: versionID}
	case strings.Contains(message, "root projection must equal"):
		return &ConflictError{Code: "current_pointer_conflict", Detail: "根投影与目标版本不一致", SystemKey: systemKey, ObjectID: versionID}
	default:
		return err
	}
}

// labelOfContract extracts business_system_label from a stored contract
// projection.
func labelOfContract(contractJSON string) (string, error) {
	var projection struct {
		LabelContract struct {
			BusinessSystemLabel string `json:"business_system_label"`
		} `json:"label_contract"`
	}
	if err := json.Unmarshal([]byte(contractJSON), &projection); err != nil {
		return "", fmt.Errorf("contract projection decode: %w", err)
	}
	if projection.LabelContract.BusinessSystemLabel == "" {
		return "", errors.New("contract projection missing business_system_label")
	}
	return projection.LabelContract.BusinessSystemLabel, nil
}

func nullableString(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

func boolToInt(value bool) int64 {
	if value {
		return 1
	}
	return 0
}

func nullInt64Ptr(value sql.NullInt64) *int64 {
	if !value.Valid {
		return nil
	}
	return &value.Int64
}

func idString(value *int64) string {
	if value == nil {
		return "null"
	}
	return strconv.FormatInt(*value, 10)
}

func encode(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "{}"
	}
	return string(encoded)
}

func decodeStored(payload string, target any) error {
	if payload == "" {
		return errors.New("empty stored result")
	}
	return json.Unmarshal([]byte(payload), target)
}
