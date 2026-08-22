package businesssystem

// Read projections for the Business System surface: list/detail of systems
// (current-version projections) and the immutable configuration version
// history with the parse-once typed structures (DATA-CONFIG-003).

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
)

// DiscoveryView is DiscoverySummary.
type DiscoveryView struct {
	DiscoveryKey   string   `json:"discoveryKey"`
	DisplayName    string   `json:"displayName"`
	Selector       string   `json:"selector"`
	IdentityLabels []string `json:"identityLabels"`
}

// CheckView is the closed promql|browser check discrimination.
type CheckView struct {
	CheckKey         string         `json:"checkKey"`
	DisplayName      string         `json:"displayName"`
	AnalysisQuestion string         `json:"analysisQuestion"`
	Kind             string         `json:"kind"`
	QueryMode        string         `json:"queryMode,omitempty"`
	Expression       string         `json:"expression,omitempty"`
	RangeSeconds     *int64         `json:"rangeSeconds,omitempty"`
	StepSeconds      *int64         `json:"stepSeconds,omitempty"`
	JourneyID        string         `json:"journeyId,omitempty"`
	JourneyParams    map[string]any `json:"journeyParams,omitempty"`
}

// PlanView is PlanSummary with its checks.
type PlanView struct {
	PlanKey     string      `json:"planKey"`
	DisplayName string      `json:"displayName"`
	Cron        *string     `json:"cron,omitempty"`
	Checks      []CheckView `json:"checks"`
}

// SystemDetail is BusinessSystemDetail (browser identity arrives with the
// Lintel stage and projects the frozen `none` state until then).
type SystemDetail struct {
	Key                            string          `json:"key"`
	DisplayName                    string          `json:"displayName"`
	Enabled                        bool            `json:"enabled"`
	RowVersion                     int64           `json:"rowVersion"`
	CurrentConfigVersionID         *string         `json:"currentConfigVersionId"`
	Timezone                       *string         `json:"timezone"`
	ResourceRefreshIntervalSeconds *int64          `json:"resourceRefreshIntervalSeconds"`
	BrowserIdentityState           string          `json:"browserIdentityState"`
	ConfigVersionCount             int64           `json:"configVersionCount"`
	Discoveries                    []DiscoveryView `json:"discoveries"`
	Plans                          []PlanView      `json:"plans"`
}

// VersionDetail is ConfigVersionSummary + ConfigVersionDetail.
type VersionDetail struct {
	ID                             string          `json:"id"`
	VersionSeq                     int64           `json:"versionSeq"`
	State                          string          `json:"state"`
	CreatedAt                      string          `json:"createdAt"`
	PublishedAt                    *string         `json:"publishedAt,omitempty"`
	Digest                         string          `json:"digest"`
	ParserVersion                  string          `json:"parserVersion"`
	SchemaVersion                  string          `json:"schemaVersion"`
	SystemKey                      string          `json:"systemKey"`
	DisplayName                    string          `json:"displayName"`
	Enabled                        bool            `json:"enabled"`
	LabelContractVersionID         string          `json:"labelContractVersionId"`
	JourneyCatalogDigest           string          `json:"journeyCatalogDigest"`
	JourneyCatalogVersion          string          `json:"journeyCatalogVersion"`
	YAMLBody                       string          `json:"yamlBody"`
	Timezone                       string          `json:"timezone"`
	ResourceRefreshIntervalSeconds int64           `json:"resourceRefreshIntervalSeconds"`
	Discoveries                    []DiscoveryView `json:"discoveries"`
	Plans                          []PlanView      `json:"plans"`
}

// VersionSummary is ConfigVersionSummary for the history list.
type VersionSummary struct {
	ID                     string  `json:"id"`
	VersionSeq             int64   `json:"versionSeq"`
	State                  string  `json:"state"`
	CreatedAt              string  `json:"createdAt"`
	PublishedAt            *string `json:"publishedAt,omitempty"`
	Digest                 string  `json:"digest"`
	ParserVersion          string  `json:"parserVersion"`
	SchemaVersion          string  `json:"schemaVersion"`
	SystemKey              string  `json:"systemKey"`
	DisplayName            string  `json:"displayName"`
	Enabled                bool    `json:"enabled"`
	LabelContractVersionID string  `json:"labelContractVersionId"`
	JourneyCatalogDigest   string  `json:"journeyCatalogDigest"`
	JourneyCatalogVersion  string  `json:"journeyCatalogVersion"`
}

// ListSystems returns the business system summaries newest-created-first
// with the enabled filter and display-name contains search (cursor on id).
func (service *Service) ListSystems(ctx context.Context, enabled *bool, query string, cursor string, limit int) ([]SystemDetail, bool, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	conditions := []string{"1=1"}
	args := []any{}
	if enabled != nil {
		conditions = append(conditions, "enabled=?")
		args = append(args, boolToInt(*enabled))
	}
	if query != "" {
		conditions = append(conditions, "LOWER(display_name) LIKE '%' || LOWER(?) || '%'")
		args = append(args, query)
	}
	if cursor != "" {
		if last, err := strconv.ParseInt(cursor, 10, 64); err == nil {
			conditions = append(conditions, "id<?")
			args = append(args, last)
		}
	}
	rows, err := service.db.QueryContext(ctx, `
		SELECT id,key,display_name,enabled,row_version,current_config_version_id,timezone,resource_refresh_interval_seconds
		FROM business_systems WHERE `+joinAnd(conditions)+` ORDER BY id DESC LIMIT ?`,
		append(args, limit+1)...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	ids := []int64{}
	systems := []SystemDetail{}
	for rows.Next() {
		var (
			id          int64
			current     sql.NullInt64
			timezone    sql.NullString
			refresh     sql.NullInt64
			detail      SystemDetail
			enabledFlag int64
		)
		if err := rows.Scan(&id, &detail.Key, &detail.DisplayName, &enabledFlag, &detail.RowVersion, &current, &timezone, &refresh); err != nil {
			return nil, false, err
		}
		detail.Enabled = enabledFlag == 1
		detail.BrowserIdentityState = "none"
		if current.Valid {
			value := strconv.FormatInt(current.Int64, 10)
			detail.CurrentConfigVersionID = &value
		}
		if timezone.Valid {
			value := timezone.String
			detail.Timezone = &value
		}
		if refresh.Valid {
			value := refresh.Int64
			detail.ResourceRefreshIntervalSeconds = &value
		}
		ids = append(ids, id)
		systems = append(systems, detail)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	more := false
	if len(systems) > limit {
		systems = systems[:limit]
		ids = ids[:limit]
		more = true
	}
	for index, id := range ids {
		count, err := service.countVersions(ctx, id)
		if err != nil {
			return nil, false, err
		}
		systems[index].ConfigVersionCount = count
	}
	return systems, more, nil
}

// GetSystem returns one system detail with its current version projections.
func (service *Service) GetSystem(ctx context.Context, key string) (SystemDetail, error) {
	var id int64
	if err := service.db.QueryRowContext(ctx, `SELECT id FROM business_systems WHERE key=?`, key).Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return SystemDetail{}, ErrNotFound
		}
		return SystemDetail{}, err
	}
	return service.systemDetailOn(ctx, nil, id)
}

// ListVersions returns the immutable version history newest-first.
func (service *Service) ListVersions(ctx context.Context, systemKey string, cursor string, limit int) ([]VersionSummary, bool, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	conditions := []string{"bs.key=?"}
	args := []any{systemKey}
	if cursor != "" {
		if last, err := strconv.ParseInt(cursor, 10, 64); err == nil {
			conditions = append(conditions, "v.id<?")
			args = append(args, last)
		}
	}
	rows, err := service.db.QueryContext(ctx, `
		SELECT v.id,v.version_seq,v.state,v.created_at,v.published_at,v.digest,v.parser_version,v.schema_version,
			v.system_key,v.display_name,v.enabled,v.label_contract_version_id,v.journey_catalog_digest,v.journey_catalog_version
		FROM business_system_config_versions v JOIN business_systems bs ON bs.id=v.business_system_id
		WHERE `+joinAnd(conditions)+` ORDER BY v.id DESC LIMIT ?`,
		append(args, limit+1)...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	items := []VersionSummary{}
	for rows.Next() {
		summary, scanErr := scanVersionSummary(rows)
		if scanErr != nil {
			return nil, false, scanErr
		}
		items = append(items, summary)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	more := false
	if len(items) > limit {
		items = items[:limit]
		more = true
	}
	return items, more, nil
}

// GetVersion returns the full version detail with projections and YAML body.
func (service *Service) GetVersion(ctx context.Context, systemKey string, versionID int64) (VersionDetail, error) {
	var systemID int64
	if err := service.db.QueryRowContext(ctx, `SELECT id FROM business_systems WHERE key=?`, systemKey).Scan(&systemID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return VersionDetail{}, ErrNotFound
		}
		return VersionDetail{}, err
	}
	detail, err := service.versionDetailOn(ctx, nil, systemID, versionID)
	if errors.Is(err, sql.ErrNoRows) {
		return VersionDetail{}, ErrNotFound
	}
	return detail, err
}

func (service *Service) countVersions(ctx context.Context, systemID int64) (int64, error) {
	var count int64
	if err := service.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM business_system_config_versions WHERE business_system_id=?`, systemID).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

func (service *Service) systemDetailOn(ctx context.Context, conn *sql.Conn, systemID int64) (SystemDetail, error) {
	query := `
		SELECT id,key,display_name,enabled,row_version,current_config_version_id,timezone,resource_refresh_interval_seconds
		FROM business_systems WHERE id=?`
	var (
		id          int64
		current     sql.NullInt64
		timezone    sql.NullString
		refresh     sql.NullInt64
		enabledFlag int64
		detail      SystemDetail
	)
	var err error
	if conn != nil {
		err = conn.QueryRowContext(ctx, query, systemID).Scan(&id, &detail.Key, &detail.DisplayName, &enabledFlag, &detail.RowVersion, &current, &timezone, &refresh)
	} else {
		err = service.db.QueryRowContext(ctx, query, systemID).Scan(&id, &detail.Key, &detail.DisplayName, &enabledFlag, &detail.RowVersion, &current, &timezone, &refresh)
	}
	if err != nil {
		return SystemDetail{}, err
	}
	detail.Enabled = enabledFlag == 1
	detail.BrowserIdentityState = "none"
	if current.Valid {
		value := strconv.FormatInt(current.Int64, 10)
		detail.CurrentConfigVersionID = &value
	}
	if timezone.Valid {
		value := timezone.String
		detail.Timezone = &value
	}
	if refresh.Valid {
		value := refresh.Int64
		detail.ResourceRefreshIntervalSeconds = &value
	}
	var count int64
	if conn != nil {
		if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM business_system_config_versions WHERE business_system_id=?`, systemID).Scan(&count); err != nil {
			return SystemDetail{}, err
		}
	} else if err := service.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM business_system_config_versions WHERE business_system_id=?`, systemID).Scan(&count); err != nil {
		return SystemDetail{}, err
	}
	detail.ConfigVersionCount = count
	if !current.Valid {
		detail.Discoveries = []DiscoveryView{}
		detail.Plans = []PlanView{}
		return detail, nil
	}
	discoveries, plans, err := service.projectionsOn(ctx, conn, current.Int64)
	if err != nil {
		return SystemDetail{}, err
	}
	detail.Discoveries = discoveries
	detail.Plans = plans
	return detail, nil
}

func (service *Service) versionDetailOn(ctx context.Context, conn *sql.Conn, systemID, versionID int64) (VersionDetail, error) {
	query := `
		SELECT id,version_seq,state,created_at,published_at,digest,parser_version,schema_version,
			system_key,display_name,enabled,label_contract_version_id,journey_catalog_digest,journey_catalog_version,
			yaml_body,timezone,resource_refresh_interval_seconds
		FROM business_system_config_versions WHERE id=? AND business_system_id=?`
	var (
		detail      VersionDetail
		id          int64
		contractID  int64
		publishedAt sql.NullString
		enabledFlag int64
	)
	var err error
	if conn != nil {
		err = conn.QueryRowContext(ctx, query, versionID, systemID).Scan(&id, &detail.VersionSeq, &detail.State, &detail.CreatedAt, &publishedAt, &detail.Digest, &detail.ParserVersion, &detail.SchemaVersion, &detail.SystemKey, &detail.DisplayName, &enabledFlag, &contractID, &detail.JourneyCatalogDigest, &detail.JourneyCatalogVersion, &detail.YAMLBody, &detail.Timezone, &detail.ResourceRefreshIntervalSeconds)
	} else {
		err = service.db.QueryRowContext(ctx, query, versionID, systemID).Scan(&id, &detail.VersionSeq, &detail.State, &detail.CreatedAt, &publishedAt, &detail.Digest, &detail.ParserVersion, &detail.SchemaVersion, &detail.SystemKey, &detail.DisplayName, &enabledFlag, &contractID, &detail.JourneyCatalogDigest, &detail.JourneyCatalogVersion, &detail.YAMLBody, &detail.Timezone, &detail.ResourceRefreshIntervalSeconds)
	}
	if err != nil {
		return VersionDetail{}, err
	}
	detail.ID = strconv.FormatInt(id, 10)
	detail.Enabled = enabledFlag == 1
	detail.LabelContractVersionID = strconv.FormatInt(contractID, 10)
	if publishedAt.Valid {
		value := publishedAt.String
		detail.PublishedAt = &value
	}
	discoveries, plans, err := service.projectionsOn(ctx, conn, versionID)
	if err != nil {
		return VersionDetail{}, err
	}
	detail.Discoveries = discoveries
	detail.Plans = plans
	return detail, nil
}

func (service *Service) projectionsOn(ctx context.Context, conn *sql.Conn, versionID int64) ([]DiscoveryView, []PlanView, error) {
	var (
		discoveryRows *sql.Rows
		planRows      *sql.Rows
		err           error
	)
	if conn != nil {
		discoveryRows, err = conn.QueryContext(ctx, `SELECT discovery_key,display_name,selector,identity_labels_json FROM config_discoveries WHERE config_version_id=? ORDER BY id`, versionID)
	} else {
		discoveryRows, err = service.db.QueryContext(ctx, `SELECT discovery_key,display_name,selector,identity_labels_json FROM config_discoveries WHERE config_version_id=? ORDER BY id`, versionID)
	}
	if err != nil {
		return nil, nil, err
	}
	discoveries := []DiscoveryView{}
	for discoveryRows.Next() {
		var view DiscoveryView
		var labelsJSON string
		if err := discoveryRows.Scan(&view.DiscoveryKey, &view.DisplayName, &view.Selector, &labelsJSON); err != nil {
			discoveryRows.Close()
			return nil, nil, err
		}
		view.IdentityLabels = []string{}
		_ = decodeStored(labelsJSON, &view.IdentityLabels)
		discoveries = append(discoveries, view)
	}
	discoveryRows.Close()
	if err := discoveryRows.Err(); err != nil {
		return nil, nil, err
	}
	if conn != nil {
		planRows, err = conn.QueryContext(ctx, `SELECT id,plan_key,display_name,cron FROM config_plans WHERE config_version_id=? ORDER BY id`, versionID)
	} else {
		planRows, err = service.db.QueryContext(ctx, `SELECT id,plan_key,display_name,cron FROM config_plans WHERE config_version_id=? ORDER BY id`, versionID)
	}
	if err != nil {
		return nil, nil, err
	}
	plans := []PlanView{}
	planIDs := []int64{}
	for planRows.Next() {
		var (
			id     int64
			view   PlanView
			cronSv sql.NullString
		)
		if err := planRows.Scan(&id, &view.PlanKey, &view.DisplayName, &cronSv); err != nil {
			planRows.Close()
			return nil, nil, err
		}
		if cronSv.Valid {
			value := cronSv.String
			view.Cron = &value
		}
		view.Checks = []CheckView{}
		plans = append(plans, view)
		planIDs = append(planIDs, id)
	}
	planRows.Close()
	if err := planRows.Err(); err != nil {
		return nil, nil, err
	}
	for index, planID := range planIDs {
		var checkRows *sql.Rows
		if conn != nil {
			checkRows, err = conn.QueryContext(ctx, `SELECT check_key,display_name,analysis_question,kind,query_mode,expression,range_seconds,step_seconds,journey_id,journey_params_json FROM config_checks WHERE plan_id=? ORDER BY id`, planID)
		} else {
			checkRows, err = service.db.QueryContext(ctx, `SELECT check_key,display_name,analysis_question,kind,query_mode,expression,range_seconds,step_seconds,journey_id,journey_params_json FROM config_checks WHERE plan_id=? ORDER BY id`, planID)
		}
		if err != nil {
			return nil, nil, err
		}
		for checkRows.Next() {
			var (
				view          CheckView
				queryMode     sql.NullString
				expression    sql.NullString
				rangeSeconds  sql.NullInt64
				stepSeconds   sql.NullInt64
				journeyID     sql.NullString
				journeyParams sql.NullString
			)
			if err := checkRows.Scan(&view.CheckKey, &view.DisplayName, &view.AnalysisQuestion, &view.Kind, &queryMode, &expression, &rangeSeconds, &stepSeconds, &journeyID, &journeyParams); err != nil {
				checkRows.Close()
				return nil, nil, err
			}
			if queryMode.Valid {
				view.QueryMode = queryMode.String
			}
			if expression.Valid {
				view.Expression = expression.String
			}
			if rangeSeconds.Valid {
				value := rangeSeconds.Int64
				view.RangeSeconds = &value
			}
			if stepSeconds.Valid {
				value := stepSeconds.Int64
				view.StepSeconds = &value
			}
			if journeyID.Valid {
				view.JourneyID = journeyID.String
			}
			if journeyParams.Valid {
				view.JourneyParams = map[string]any{}
				_ = decodeStored(journeyParams.String, &view.JourneyParams)
			}
			plans[index].Checks = append(plans[index].Checks, view)
		}
		checkRows.Close()
		if err := checkRows.Err(); err != nil {
			return nil, nil, err
		}
	}
	return discoveries, plans, nil
}

func scanVersionSummary(rows *sql.Rows) (VersionSummary, error) {
	var (
		summary     VersionSummary
		id          int64
		contractID  int64
		publishedAt sql.NullString
		enabledFlag int64
	)
	if err := rows.Scan(&id, &summary.VersionSeq, &summary.State, &summary.CreatedAt, &publishedAt, &summary.Digest, &summary.ParserVersion, &summary.SchemaVersion, &summary.SystemKey, &summary.DisplayName, &enabledFlag, &contractID, &summary.JourneyCatalogDigest, &summary.JourneyCatalogVersion); err != nil {
		return VersionSummary{}, err
	}
	summary.ID = strconv.FormatInt(id, 10)
	summary.Enabled = enabledFlag == 1
	summary.LabelContractVersionID = strconv.FormatInt(contractID, 10)
	if publishedAt.Valid {
		value := publishedAt.String
		summary.PublishedAt = &value
	}
	return summary, nil
}

func joinAnd(conditions []string) string {
	result := conditions[0]
	for _, condition := range conditions[1:] {
		result += " AND " + condition
	}
	return result
}
