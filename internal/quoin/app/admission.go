package app

// admission.go owns Quoin's fixed access declarations and the production
// wiring of the admission guard (internal/quoin/operations). Declarations are
// keyed by exact OperationID with their exact route; construction validation
// fails on any drift and nothing is inferred from method or path. Main wires
// the guard into NewHandler/newMaintenanceHandler; adoption order matters:
// api.UseMiddleware before any huma.Register call.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"

	sharedops "github.com/Suknna/quoin/internal/ops"
	"github.com/Suknna/quoin/internal/quoin/audit"
	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/execution"
	"github.com/Suknna/quoin/internal/quoin/operations"
)

// declaration builds one huma operation declaration.
func declaration(id, method, path string, level operations.Level, kind operations.Kind, objectType string) operations.Declaration {
	return operations.Declaration{ID: id, Method: method, Path: path, Level: level, Kind: kind, ObjectType: objectType}
}

// raw marks a declaration for a raw mux wrapper registered through Wrap.
func raw(d operations.Declaration) operations.Declaration {
	d.Raw = true
	return d
}

// flowCorrelated marks an admin declaration that must continue the stored
// correlation of the authentication flow that started it.
func flowCorrelated(d operations.Declaration) operations.Declaration {
	d.FlowCorrelated = true
	return d
}

// accessDeclarationTable is the authoritative operation inventory. Object
// types feed audit_events.domain_ref_type. Sensitive reads stay declared even
// where handlers keep their own pre-release audit (reveal/download); the
// guard's fact is the access record, the handler's remains the release record.
func accessDeclarationTable() map[string]operations.Declaration {
	table := map[string]operations.Declaration{}
	add := func(declarations ...operations.Declaration) {
		for _, d := range declarations {
			if _, exists := table[d.ID]; exists {
				panic(fmt.Sprintf("operations: duplicate declaration %q", d.ID))
			}
			table[d.ID] = d
		}
	}

	// 认证与流程：the unified flow replaced the old single-step login route.
	add(
		declaration("startAuthentication", http.MethodPost, "/api/v1/auth/login", operations.LevelPublic, operations.KindCommand, "session"),
		declaration("readAuthenticationFlow", http.MethodGet, "/api/v1/auth/flow", operations.LevelFlow, operations.KindQuery, "auth_flow"),
		declaration("setInitializationPassword", http.MethodPut, "/api/v1/auth/flow/password", operations.LevelFlow, operations.KindCommand, "user"),
		declaration("registerInitializationContact", http.MethodPost, "/api/v1/auth/flow/contacts", operations.LevelFlow, operations.KindCommand, "user"),
		declaration("sendAuthenticationChallenge", http.MethodPost, "/api/v1/auth/flow/challenge", operations.LevelFlow, operations.KindCommand, "user"),
		declaration("verifyInitializationChallenge", http.MethodPost, "/api/v1/auth/flow/verify", operations.LevelFlow, operations.KindCommand, "user"),
		declaration("completeInitialization", http.MethodPost, "/api/v1/auth/flow/complete", operations.LevelFlow, operations.KindCommand, "user"),
		declaration("touchSessionActivity", http.MethodPost, "/api/v1/auth/activity", operations.LevelSession, operations.KindCommand, "session"),
		declaration("getCurrentUser", http.MethodGet, "/api/v1/auth/me", operations.LevelSession, operations.KindQuery, "user"),
		declaration("listOwnContacts", http.MethodGet, "/api/v1/auth/contacts", operations.LevelFull, operations.KindQuery, "user"),
		declaration("changeOwnPassword", http.MethodPut, "/api/v1/auth/password", operations.LevelSession, operations.KindCommand, "user"),
		declaration("logout", http.MethodPost, "/api/v1/auth/logout", operations.LevelSession, operations.KindCommand, "session"),
		declaration("listOwnSessions", http.MethodGet, "/api/v1/auth/sessions", operations.LevelFull, operations.KindQuery, "session"),
		declaration("revokeOwnSession", http.MethodPost, "/api/v1/auth/sessions/{sessionId}/revoke", operations.LevelFull, operations.KindCommand, "session"),
		// Delivery management accepts a full admin session or an
		// admin_initialize/recovery flow (deliveryActor validates the actor).
		// Contact change: the start command creates the contact_change flow
		// (its correlation persists server-side); the complete command is an
		// admin operation that must continue that stored correlation.
		declaration("startContactChange", http.MethodPost, "/api/v1/auth/contact-change", operations.LevelAdmin, operations.KindCommand, "user"),
		flowCorrelated(declaration("completeContactChange", http.MethodPost, "/api/v1/auth/contact-change/complete", operations.LevelAdmin, operations.KindCommand, "user")),
		declaration("readAuthDelivery", http.MethodGet, "/api/v1/auth/flow/delivery", operations.LevelFlowOrAdmin, operations.KindQuery, "auth_delivery"),
		declaration("configureAuthDelivery", http.MethodPut, "/api/v1/auth/flow/delivery", operations.LevelFlowOrAdmin, operations.KindCommand, "auth_delivery"),
	)

	// 用户与审计（Admin）。
	add(
		declaration("listUsers", http.MethodGet, "/api/v1/admin/users", operations.LevelAdmin, operations.KindQuery, "user"),
		declaration("createUser", http.MethodPost, "/api/v1/admin/users", operations.LevelAdmin, operations.KindCommand, "user"),
		declaration("updateUser", http.MethodPatch, "/api/v1/admin/users/{userId}", operations.LevelAdmin, operations.KindCommand, "user"),
		declaration("resetUserPassword", http.MethodPost, "/api/v1/admin/users/{userId}/reset-password", operations.LevelAdmin, operations.KindCommand, "user"),
		declaration("revokeUserSessions", http.MethodPost, "/api/v1/admin/users/{userId}/revoke-sessions", operations.LevelAdmin, operations.KindCommand, "user"),
		declaration("setUserContacts", http.MethodPut, "/api/v1/admin/users/{userId}/contacts", operations.LevelAdmin, operations.KindCommand, "user"),
		declaration("listAuditEvents", http.MethodGet, "/api/v1/audit-events", operations.LevelAdmin, operations.KindQuery, "audit_event"),
		declaration("getAuditSettings", http.MethodGet, "/api/v1/admin/audit-settings", operations.LevelAdmin, operations.KindQuery, "audit_settings"),
		declaration("updateAuditSettings", http.MethodPatch, "/api/v1/admin/audit-settings", operations.LevelAdmin, operations.KindCommand, "audit_settings"),
		declaration("previewAuditRetention", http.MethodPost, "/api/v1/admin/audit-settings/preview", operations.LevelAdmin, operations.KindCommand, "audit_settings"),
	)

	// 系统（状态读取按现有边界，其余 Admin）。
	add(
		declaration("getMaintenanceState", http.MethodGet, "/api/v1/maintenance", operations.LevelSession, operations.KindQuery, "maintenance"),
		declaration("exitMaintenance", http.MethodPost, "/api/v1/maintenance/exit", operations.LevelAdmin, operations.KindCommand, "maintenance"),
		declaration("getRuntimeStatus", http.MethodGet, "/api/v1/runtime", operations.LevelAdmin, operations.KindQuery, "runtime"),
		declaration("getAdminAbout", http.MethodGet, "/api/v1/admin/about", operations.LevelAdmin, operations.KindQuery, "platform"),
		declaration("prepareUpgrade", http.MethodPost, "/api/v1/maintenance/upgrade/prepare", operations.LevelAdmin, operations.KindCommand, "maintenance"),
	)

	// 告警与接入。
	add(
		declaration("listAlerts", http.MethodGet, "/api/v1/alerts", operations.LevelSession, operations.KindQuery, "alert"),
		declaration("getAlertOccurrence", http.MethodGet, "/api/v1/alerts/{occurrenceId}", operations.LevelSession, operations.KindQuery, "alert"),
		declaration("listAlertObservations", http.MethodGet, "/api/v1/alerts/{occurrenceId}/observations", operations.LevelSession, operations.KindQuery, "alert"),
		declaration("listAlertIntakeIssues", http.MethodGet, "/api/v1/alert-intake-issues", operations.LevelAdmin, operations.KindQuery, "alert"),
		declaration("acknowledgeIntakeIssue", http.MethodPost, "/api/v1/alert-intake-issues/{issueId}/acknowledge", operations.LevelAdmin, operations.KindCommand, "alert"),
		declaration("listAlertSources", http.MethodGet, "/api/v1/alert-sources", operations.LevelAdmin, operations.KindQuery, "alert_source"),
		declaration("getAlertSource", http.MethodGet, "/api/v1/alert-sources/{sourceKey}", operations.LevelAdmin, operations.KindQuery, "alert_source"),
		declaration("getAlertmanagerReceiverConfig", http.MethodGet, "/api/v1/alert-sources/receiver-config", operations.LevelAdmin, operations.KindQuery, "alert_source"),
		// cancelStandaloneBrowserOperation survives only on the maintenance
		// Upgrade drain allowlist; the retired normal surface never re-registers it.
		declaration("cancelStandaloneBrowserOperation", http.MethodPost, "/api/v1/browser-identities/{identityKey}/operations/{operationId}/cancel", operations.LevelAdmin, operations.KindCommand, "browser_operation"),
		declaration("createAlertSource", http.MethodPost, "/api/v1/alert-sources", operations.LevelAdmin, operations.KindCommand, "alert_source"),
		declaration("listAlertSourceCredentials", http.MethodGet, "/api/v1/alert-sources/{sourceKey}/credentials", operations.LevelAdmin, operations.KindQuery, "alert_source"),
		declaration("rotateAlertSourceCredential", http.MethodPost, "/api/v1/alert-sources/{sourceKey}/rotate", operations.LevelAdmin, operations.KindCommand, "alert_source"),
		declaration("retireAlertSourceCredential", http.MethodPost, "/api/v1/alert-sources/{sourceKey}/credentials/{credentialId}/retire", operations.LevelAdmin, operations.KindCommand, "alert_source"),
		declaration("disableAlertSource", http.MethodPost, "/api/v1/alert-sources/{sourceKey}/disable", operations.LevelAdmin, operations.KindCommand, "alert_source"),
		declaration("revealAlertSourceCredential", http.MethodPost, "/api/v1/alert-sources/credentials/reveal", operations.LevelAdmin, operations.KindSensitiveRead, "alert_source"),
	)

	// 连接与模型（Admin）。
	add(
		declaration("listConnections", http.MethodGet, "/api/v1/connections", operations.LevelAdmin, operations.KindQuery, "connection"),
		declaration("createConnection", http.MethodPost, "/api/v1/connections", operations.LevelAdmin, operations.KindCommand, "connection"),
		declaration("getConnection", http.MethodGet, "/api/v1/connections/{connectionName}", operations.LevelAdmin, operations.KindQuery, "connection"),
		declaration("probeConnection", http.MethodPost, "/api/v1/connections/{connectionName}/probe", operations.LevelAdmin, operations.KindCommand, "connection"),
		declaration("enableConnection", http.MethodPost, "/api/v1/connections/{connectionName}/enable", operations.LevelAdmin, operations.KindCommand, "connection"),
		declaration("disableConnection", http.MethodPost, "/api/v1/connections/{connectionName}/disable", operations.LevelAdmin, operations.KindCommand, "connection"),
		declaration("discoverProviderModels", http.MethodPost, "/api/v1/model-providers/discover", operations.LevelAdmin, operations.KindCommand, "model_provider"),
		declaration("rotateConnectionCredential", http.MethodPost, "/api/v1/connections/{connectionName}/rotate", operations.LevelAdmin, operations.KindCommand, "connection"),
		declaration("getConnectionProbeAttempt", http.MethodGet, "/api/v1/connections/{connectionName}/probe-attempts/{attemptId}", operations.LevelAdmin, operations.KindQuery, "attempt"),
		declaration("cancelConnectionProbeAttempt", http.MethodPost, "/api/v1/connections/{connectionName}/probe-attempts/{attemptId}/cancel", operations.LevelAdmin, operations.KindCommand, "attempt"),
		declaration("listConnectionProbeResults", http.MethodGet, "/api/v1/connections/{connectionName}/probe-results", operations.LevelAdmin, operations.KindQuery, "attempt"),
		declaration("listConnectionRevisions", http.MethodGet, "/api/v1/connections/{connectionName}/revisions", operations.LevelAdmin, operations.KindQuery, "connection"),
		declaration("listCredentialGenerations", http.MethodGet, "/api/v1/connections/{connectionName}/generations", operations.LevelAdmin, operations.KindQuery, "connection"),
	)

	// 观测与插件（Admin）。
	add(
		declaration("listSourceObservedResources", http.MethodGet, "/api/v1/integrations/{connectionName}/resources", operations.LevelAdmin, operations.KindQuery, "connection"),
		declaration("getSourceObservedResource", http.MethodGet, "/api/v1/integrations/{connectionName}/resources/{resourceId}", operations.LevelAdmin, operations.KindQuery, "connection"),
		declaration("listSourceObservationRuns", http.MethodGet, "/api/v1/integrations/{connectionName}/observation-runs", operations.LevelAdmin, operations.KindQuery, "observation_run"),
		declaration("getSourceObservationRun", http.MethodGet, "/api/v1/integrations/{connectionName}/observation-runs/{observationRunId}", operations.LevelAdmin, operations.KindQuery, "observation_run"),
		declaration("refreshIntegrationResources", http.MethodPost, "/api/v1/integrations/{connectionName}/resources:refresh", operations.LevelAdmin, operations.KindCommand, "connection"),
		declaration("listIntegrationPlugins", http.MethodGet, "/api/v1/integrations/plugins", operations.LevelAdmin, operations.KindQuery, "plugin"),
	)

	// 业务上下文（User）。
	add(declaration("listBusinessContext", http.MethodGet, "/api/v1/business-context", operations.LevelFull, operations.KindQuery, "business_context"))

	// 分析（User）。
	add(
		declaration("createInitialAnalysis", http.MethodPost, "/api/v1/alerts/{occurrenceId}/analyses", operations.LevelFull, operations.KindCommand, "analysis"),
		declaration("listInitialAnalyses", http.MethodGet, "/api/v1/alerts/{occurrenceId}/analyses", operations.LevelFull, operations.KindQuery, "analysis"),
		declaration("getInitialAnalysis", http.MethodGet, "/api/v1/alerts/{occurrenceId}/analyses/{analysisId}", operations.LevelFull, operations.KindQuery, "analysis"),
		declaration("listInitialAnalysisAttempts", http.MethodGet, "/api/v1/alerts/{occurrenceId}/analyses/{analysisId}/attempts", operations.LevelFull, operations.KindQuery, "attempt"),
		declaration("retryInitialAnalysis", http.MethodPost, "/api/v1/alerts/{occurrenceId}/analyses/{analysisId}/retry", operations.LevelFull, operations.KindCommand, "analysis"),
		declaration("cancelInitialAnalysis", http.MethodPost, "/api/v1/alerts/{occurrenceId}/analyses/{analysisId}/cancel", operations.LevelFull, operations.KindCommand, "analysis"),
		declaration("getTaskSnapshot", http.MethodGet, "/api/v1/tasks/snapshot", operations.LevelFull, operations.KindQuery, "attempt"),
	)

	// 证据与敏感内容。
	add(
		declaration("getEvidence", http.MethodGet, "/api/v1/evidence/{evidenceId}", operations.LevelFull, operations.KindQuery, "evidence"),
		declaration("getArtifactMetadata", http.MethodGet, "/api/v1/artifacts/{artifactId}", operations.LevelFull, operations.KindQuery, "artifact"),
		declaration("getSourceMaterial", http.MethodGet, "/api/v1/source-materials/{materialId}", operations.LevelFull, operations.KindQuery, "source_material"),
		declaration("downloadSourceMaterialContent", http.MethodGet, "/api/v1/source-materials/{materialId}/content", operations.LevelFull, operations.KindSensitiveRead, "source_material"),
	)

	// 备份（Admin）。
	add(
		declaration("listBackups", http.MethodGet, "/api/v1/backups", operations.LevelAdmin, operations.KindQuery, "backup"),
		declaration("triggerBackup", http.MethodPost, "/api/v1/backups", operations.LevelAdmin, operations.KindCommand, "backup"),
		declaration("getBackup", http.MethodGet, "/api/v1/backups/{backupId}", operations.LevelAdmin, operations.KindQuery, "backup"),
		declaration("getBackupSettings", http.MethodGet, "/api/v1/backups/settings", operations.LevelAdmin, operations.KindQuery, "backup"),
		declaration("updateBackupSettings", http.MethodPut, "/api/v1/backups/settings", operations.LevelAdmin, operations.KindCommand, "backup"),
		declaration("getArtifactRetentionSettings", http.MethodGet, "/api/v1/artifacts/retention-settings", operations.LevelAdmin, operations.KindQuery, "artifact"),
		declaration("updateArtifactRetentionSettings", http.MethodPut, "/api/v1/artifacts/retention-settings", operations.LevelAdmin, operations.KindCommand, "artifact"),
	)

	// 调查（User）。
	add(
		declaration("listInvestigations", http.MethodGet, "/api/v1/investigations", operations.LevelFull, operations.KindQuery, "investigation"),
		declaration("createInvestigation", http.MethodPost, "/api/v1/investigations", operations.LevelFull, operations.KindCommand, "investigation"),
		declaration("getInvestigation", http.MethodGet, "/api/v1/investigations/{investigationId}", operations.LevelFull, operations.KindQuery, "investigation"),
		declaration("listInvestigationMessages", http.MethodGet, "/api/v1/investigations/{investigationId}/messages", operations.LevelFull, operations.KindQuery, "message"),
		declaration("sendInvestigationMessage", http.MethodPost, "/api/v1/investigations/{investigationId}/messages", operations.LevelFull, operations.KindCommand, "message"),
		declaration("streamInvestigationMessage", http.MethodPost, "/api/v1/investigations/{investigationId}/messages/{messageId}/stream", operations.LevelFull, operations.KindStream, "message"),
		declaration("listInvestigationAttempts", http.MethodGet, "/api/v1/investigations/{investigationId}/attempts", operations.LevelFull, operations.KindQuery, "attempt"),
		declaration("listAttemptToolCalls", http.MethodGet, "/api/v1/investigations/{investigationId}/attempts/{attemptId}/tool-calls", operations.LevelFull, operations.KindQuery, "attempt"),
		declaration("undoInvestigationMessage", http.MethodPost, "/api/v1/investigations/{investigationId}/undo", operations.LevelFull, operations.KindCommand, "message"),
		declaration("retryInvestigationAttempt", http.MethodPost, "/api/v1/investigations/{investigationId}/attempts/{attemptId}/retry", operations.LevelFull, operations.KindCommand, "attempt"),
		declaration("cancelInvestigationAttempt", http.MethodPost, "/api/v1/investigations/{investigationId}/attempts/{attemptId}/cancel", operations.LevelFull, operations.KindCommand, "attempt"),
		declaration("getInvestigationAttachment", http.MethodGet, "/api/v1/investigation-attachments/{attachmentId}", operations.LevelFull, operations.KindQuery, "attachment"),
	)

	// 历史业务系统（Admin，只读解释）。
	add(
		declaration("listBusinessSystems", http.MethodGet, "/api/v1/business-systems", operations.LevelAdmin, operations.KindQuery, "business_system"),
		declaration("getBusinessSystem", http.MethodGet, "/api/v1/business-systems/{systemKey}", operations.LevelAdmin, operations.KindQuery, "business_system"),
		declaration("listBusinessSystemKubernetesConnections", http.MethodGet, "/api/v1/business-systems/{systemKey}/kubernetes-connections", operations.LevelAdmin, operations.KindQuery, "business_system"),
		declaration("listBusinessSystemConfigs", http.MethodGet, "/api/v1/business-systems/{systemKey}/config", operations.LevelAdmin, operations.KindQuery, "business_system"),
		declaration("getBusinessSystemConfig", http.MethodGet, "/api/v1/business-systems/{systemKey}/config/{versionId}", operations.LevelAdmin, operations.KindQuery, "business_system"),
		declaration("listConfigVerificationRuns", http.MethodGet, "/api/v1/business-systems/{systemKey}/config/{versionId}/verifications", operations.LevelAdmin, operations.KindQuery, "config_verification_run"),
		declaration("getConfigVerificationRun", http.MethodGet, "/api/v1/business-systems/{systemKey}/config/{versionId}/verifications/{verificationRunId}", operations.LevelAdmin, operations.KindQuery, "config_verification_run"),
		declaration("getResourceRefreshRun", http.MethodGet, "/api/v1/business-systems/{systemKey}/resource-refresh-runs/{resourceRefreshRunId}", operations.LevelAdmin, operations.KindQuery, "resource_refresh_run"),
		declaration("listObservedResources", http.MethodGet, "/api/v1/business-systems/{systemKey}/resources", operations.LevelAdmin, operations.KindQuery, "observed_resource"),
		declaration("getObservedResource", http.MethodGet, "/api/v1/business-systems/{systemKey}/resources/{resourceId}", operations.LevelAdmin, operations.KindQuery, "observed_resource"),
	)

	// 巡检（Admin）。
	add(
		declaration("listInspectionRuns", http.MethodGet, "/api/v1/inspections/runs", operations.LevelAdmin, operations.KindQuery, "inspection_run"),
		declaration("createInspectionRun", http.MethodPost, "/api/v1/inspections/runs", operations.LevelAdmin, operations.KindCommand, "inspection_run"),
		declaration("getInspectionRun", http.MethodGet, "/api/v1/inspections/runs/{runId}", operations.LevelAdmin, operations.KindQuery, "inspection_run"),
		declaration("cancelInspectionRun", http.MethodPost, "/api/v1/inspections/runs/{runId}/cancel", operations.LevelAdmin, operations.KindCommand, "inspection_run"),
		declaration("listInspectionReports", http.MethodGet, "/api/v1/inspections/runs/{runId}/reports", operations.LevelAdmin, operations.KindQuery, "inspection_run"),
		declaration("getInspectionReport", http.MethodGet, "/api/v1/inspections/runs/{runId}/reports/{reportVersion}", operations.LevelAdmin, operations.KindQuery, "inspection_run"),
		declaration("retryInspectionAnalysis", http.MethodPost, "/api/v1/inspections/runs/{runId}/analyze", operations.LevelAdmin, operations.KindCommand, "inspection_run"),
		declaration("rerunInspection", http.MethodPost, "/api/v1/inspections/runs/{runId}/rerun", operations.LevelAdmin, operations.KindCommand, "inspection_run"),
		declaration("listPluginInspectionPlans", http.MethodGet, "/api/v1/inspections/plans", operations.LevelAdmin, operations.KindQuery, "inspection_plan"),
		declaration("createPluginInspectionPlan", http.MethodPost, "/api/v1/inspections/plans", operations.LevelAdmin, operations.KindCommand, "inspection_plan"),
		declaration("getPluginInspectionPlan", http.MethodGet, "/api/v1/inspections/plans/{planKey}", operations.LevelAdmin, operations.KindQuery, "inspection_plan"),
		declaration("updatePluginInspectionPlan", http.MethodPut, "/api/v1/inspections/plans/{planKey}", operations.LevelAdmin, operations.KindCommand, "inspection_plan"),
	)

	// 业务视图（Admin）。
	add(
		declaration("listBusinessViews", http.MethodGet, "/api/v1/business-views", operations.LevelAdmin, operations.KindQuery, "business_view"),
		declaration("createBusinessView", http.MethodPost, "/api/v1/business-views", operations.LevelAdmin, operations.KindCommand, "business_view"),
		declaration("getBusinessView", http.MethodGet, "/api/v1/business-views/{viewKey}", operations.LevelAdmin, operations.KindQuery, "business_view"),
		declaration("updateBusinessView", http.MethodPut, "/api/v1/business-views/{viewKey}", operations.LevelAdmin, operations.KindCommand, "business_view"),
	)

	// 知识与反馈（User）。
	add(
		declaration("appendDiagnosisFeedback", http.MethodPost, "/api/v1/knowledge/feedback", operations.LevelFull, operations.KindCommand, "feedback"),
		declaration("listDiagnosisFeedback", http.MethodGet, "/api/v1/knowledge/feedback", operations.LevelFull, operations.KindQuery, "feedback"),
		declaration("createAnalysisKnowledgeCandidate", http.MethodPost, "/api/v1/alerts/{occurrenceId}/analyses/{analysisId}/knowledge-candidates", operations.LevelFull, operations.KindCommand, "knowledge_candidate"),
		declaration("createInvestigationKnowledgeCandidate", http.MethodPost, "/api/v1/investigations/{investigationId}/knowledge-candidates", operations.LevelFull, operations.KindCommand, "knowledge_candidate"),
		declaration("createReportKnowledgeCandidate", http.MethodPost, "/api/v1/inspections/runs/{runId}/reports/{reportVersion}/knowledge-candidates", operations.LevelFull, operations.KindCommand, "knowledge_candidate"),
		declaration("listKnowledgeCandidates", http.MethodGet, "/api/v1/knowledge/candidates", operations.LevelFull, operations.KindQuery, "knowledge_candidate"),
		declaration("getKnowledgeCandidate", http.MethodGet, "/api/v1/knowledge/candidates/{candidateId}", operations.LevelFull, operations.KindQuery, "knowledge_candidate"),
		declaration("editKnowledgeCandidateDraft", http.MethodPatch, "/api/v1/knowledge/candidates/{candidateId}", operations.LevelFull, operations.KindCommand, "knowledge_candidate"),
		declaration("confirmKnowledgeCandidate", http.MethodPost, "/api/v1/knowledge/candidates/{candidateId}/confirm", operations.LevelFull, operations.KindCommand, "knowledge_candidate"),
		declaration("excludeKnowledgeCandidate", http.MethodPost, "/api/v1/knowledge/candidates/{candidateId}/exclude", operations.LevelFull, operations.KindCommand, "knowledge_candidate"),
		declaration("searchKnowledge", http.MethodGet, "/api/v1/knowledge", operations.LevelFull, operations.KindQuery, "knowledge"),
		declaration("getKnowledge", http.MethodGet, "/api/v1/knowledge/items/{knowledgeId}", operations.LevelFull, operations.KindQuery, "knowledge"),
		declaration("listKnowledgeVersions", http.MethodGet, "/api/v1/knowledge/items/{knowledgeId}/versions", operations.LevelFull, operations.KindQuery, "knowledge"),
		declaration("getKnowledgeVersion", http.MethodGet, "/api/v1/knowledge/items/{knowledgeId}/versions/{versionId}", operations.LevelFull, operations.KindQuery, "knowledge"),
		declaration("createKnowledgeRevisionCandidate", http.MethodPost, "/api/v1/knowledge/items/{knowledgeId}/versions", operations.LevelFull, operations.KindCommand, "knowledge_candidate"),
		declaration("stopKnowledgeReuse", http.MethodPost, "/api/v1/knowledge/items/{knowledgeId}/versions/{versionId}/stop-reuse", operations.LevelFull, operations.KindCommand, "knowledge"),
		declaration("importKnowledgeBatch", http.MethodPost, "/api/v1/knowledge/import-batches", operations.LevelFull, operations.KindCommand, "import_batch"),
		declaration("listKnowledgeImportBatches", http.MethodGet, "/api/v1/knowledge/import-batches", operations.LevelFull, operations.KindQuery, "import_batch"),
		declaration("getKnowledgeImportBatch", http.MethodGet, "/api/v1/knowledge/import-batches/{batchId}", operations.LevelFull, operations.KindQuery, "import_batch"),
		declaration("confirmKnowledgeBatch", http.MethodPost, "/api/v1/knowledge/import-batches/{batchId}/confirm", operations.LevelFull, operations.KindCommand, "import_batch"),
		declaration("cancelKnowledgeImportBatch", http.MethodPost, "/api/v1/knowledge/import-batches/{batchId}/cancel", operations.LevelFull, operations.KindCommand, "import_batch"),
	)

	// 原生 mux 路由（正常面）：SSE、下载与上传经 Wrap 包装，不缓冲响应。
	add(
		raw(declaration("streamAlertEvents", http.MethodGet, "/api/v1/alerts/events", operations.LevelSession, operations.KindStream, "alert")),
		raw(declaration("streamTaskEvents", http.MethodGet, "/api/v1/tasks/events", operations.LevelSession, operations.KindStream, "attempt")),
		raw(declaration("downloadArtifactContent", http.MethodGet, "/api/v1/artifacts/{artifactId}/content", operations.LevelSession, operations.KindSensitiveRead, "artifact")),
		raw(declaration("downloadBackup", http.MethodGet, "/api/v1/backups/{backupId}/download", operations.LevelAdmin, operations.KindSensitiveRead, "backup")),
		raw(declaration("uploadInvestigationAttachment", http.MethodPost, "/api/v1/investigation-attachments", operations.LevelFull, operations.KindCommand, "attachment")),
	)
	return table
}

// buildNormalAccessRegistry slices the table for the normal surface: every
// declared operation except the maintenance-only exit command. Raw wrappers
// are included (Admission.Wrap requires them); planned entries ride along.
// maintenanceOnly lists declarations registered exclusively on maintenance
// surfaces; the normal surface never serves them.
var maintenanceOnly = map[string]bool{
	"exitMaintenance":                  true,
	"cancelStandaloneBrowserOperation": true,
}

func buildNormalAccessRegistry() (*operations.AccessRegistry, error) {
	table := accessDeclarationTable()
	declarations := make([]operations.Declaration, 0, len(table))
	for _, d := range table {
		if maintenanceOnly[d.ID] {
			continue
		}
		declarations = append(declarations, d)
	}
	return operations.NewAccessRegistry(declarations...)
}

// NormalAccessRegistry returns the normal surface's validated declaration set.
var NormalAccessRegistry = sync.OnceValues(buildNormalAccessRegistry)

// maintenanceAllowlists are the fixed per-reason maintenance allowlists plus
// the shared base (authentication flows, delivery, audit, maintenance state)
// and the any-method catch-all raw wrapper.
var maintenanceBase = []string{
	"startAuthentication", "readAuthenticationFlow",
	"setInitializationPassword", "registerInitializationContact", "sendAuthenticationChallenge",
	"verifyInitializationChallenge", "completeInitialization", "touchSessionActivity",
	"readAuthDelivery", "configureAuthDelivery",
	"getCurrentUser", "changeOwnPassword", "logout",
	"listAuditEvents", "getAuditSettings", "updateAuditSettings", "previewAuditRetention",
	"getMaintenanceState", "exitMaintenance",
}

var maintenanceReasonAllowlists = map[string][]string{
	"RootKeyRebind": {
		"listConnections", "getConnection", "rotateConnectionCredential", "disableConnection",
		"getConnectionProbeAttempt", "listConnectionProbeResults", "listConnectionRevisions", "listCredentialGenerations",
	},
	"Restore": {
		"getRuntimeStatus",
		"listUsers", "createUser", "updateUser", "resetUserPassword", "revokeUserSessions",
		"listConnections", "getConnection", "rotateConnectionCredential", "disableConnection",
		"getConnectionProbeAttempt", "listConnectionProbeResults", "listConnectionRevisions", "listCredentialGenerations",
		"listAlertSources", "getAlertSource", "listAlertSourceCredentials", "rotateAlertSourceCredential",
		"retireAlertSourceCredential", "disableAlertSource", "revealAlertSourceCredential",
	},
	"Upgrade": {
		"prepareUpgrade",
		"cancelInitialAnalysis", "cancelStandaloneBrowserOperation", "cancelConnectionProbeAttempt",
		"cancelInspectionRun", "cancelInvestigationAttempt", "cancelKnowledgeImportBatch",
	},
}

func buildMaintenanceAccessRegistry(reason string) (*operations.AccessRegistry, error) {
	reasonIDs, ok := maintenanceReasonAllowlists[reason]
	if !ok {
		return nil, fmt.Errorf("operations: unknown maintenance reason %q", reason)
	}
	table := accessDeclarationTable()
	declarations := make([]operations.Declaration, 0, len(maintenanceBase)+len(reasonIDs)+1)
	for _, id := range append(append([]string(nil), maintenanceBase...), reasonIDs...) {
		d, exists := table[id]
		if !exists {
			return nil, fmt.Errorf("operations: maintenance allowlist entry %q has no declaration", id)
		}
		declarations = append(declarations, d)
	}
	declarations = append(declarations, raw(declaration("maintenanceUnavailable", "", "/api/", operations.LevelSession, operations.KindSystem, "")))
	return operations.NewAccessRegistry(declarations...)
}

var maintenanceAccessRegistries = map[string]func() (*operations.AccessRegistry, error){}

func init() {
	for reason := range maintenanceReasonAllowlists {
		allowed := reason
		maintenanceAccessRegistries[reason] = sync.OnceValues(func() (*operations.AccessRegistry, error) {
			return buildMaintenanceAccessRegistry(allowed)
		})
	}
}

// MaintenanceAccessRegistry returns the maintenance surface's declaration set
// for one maintenance reason.
func MaintenanceAccessRegistry(reason string) (*operations.AccessRegistry, error) {
	build, ok := maintenanceAccessRegistries[reason]
	if !ok {
		return nil, fmt.Errorf("operations: unknown maintenance reason %q", reason)
	}
	return build()
}

// accessSessionResolver projects auth sessions onto guard subjects. The
// initialized flag is the real users.initialized state — never a permissive
// fallback. Clean rejections map to the guard's sentinel; infrastructure
// failures stay verbatim so admission answers 503 instead of 401.
func (application *apiServer) accessSessionResolver() operations.SessionResolver {
	return func(ctx context.Context, credential string) (operations.Subject, error) {
		session, err := application.auth.Authenticate(ctx, credential)
		if err != nil {
			if errors.Is(err, auth.ErrUnauthenticated) {
				return operations.Subject{}, fmt.Errorf("operations: session rejected: %w", operations.ErrUnauthenticated)
			}
			return operations.Subject{}, err
		}
		return operations.Subject{
			UserID:                 session.User.ID,
			Role:                   session.User.Role,
			Initialized:            session.User.Initialized,
			PasswordChangeRequired: session.User.PasswordChangeRequired,
			SessionID:              session.ID,
			AuthRevision:           session.User.AuthRevision,
		}, nil
	}
}

// accessFlowResolver is the auth.ReadFlow seam: it returns the flow's stored
// correlation so every step of one authentication flow shares it.
func (application *apiServer) accessFlowResolver() operations.FlowResolver {
	return func(ctx context.Context, credential string) (operations.FlowIdentity, error) {
		flow, err := application.auth.ReadFlow(ctx, credential)
		if err != nil {
			if errors.Is(err, auth.ErrUnauthenticated) || errors.Is(err, auth.ErrFlowInvalid) || errors.Is(err, auth.ErrFlowExpired) {
				return operations.FlowIdentity{}, fmt.Errorf("operations: flow rejected: %w", operations.ErrUnauthenticated)
			}
			return operations.FlowIdentity{}, err
		}
		return operations.FlowIdentity{Type: string(flow.Type), CorrelationID: flow.CorrelationID, UserID: flow.User.ID}, nil
	}
}

// accessAuditSink bridges guard facts onto the shared audit writer with
// phase=access. Facts always carry a claimed user identity — anonymous
// traffic never reaches the sink. The write is a single whitelisted INSERT
// (no targets, no business claim) on the request context's metadata.
type accessAuditSink struct {
	writer *audit.Writer
	db     audit.DB
}

func (sink accessAuditSink) RecordAccess(ctx context.Context, fact operations.AccessFact) error {
	if fact.ActorUserID <= 0 {
		return fmt.Errorf("access fact %q has no claimed identity", fact.OperationID)
	}
	outcome := audit.OutcomeSuccess
	switch fact.Outcome {
	case operations.OutcomeDenied:
		outcome = audit.OutcomeRejected
	case operations.OutcomeRequestFailure:
		outcome = audit.OutcomeFailure
	}
	// The guard's context carries the root execution metadata; the initiator
	// equals the acting user for direct HTTP access.
	initiatorType, initiatorID := "user", fact.ActorUserID
	if meta, ok := execution.FromContext(ctx); ok && meta.Initiator.Kind == execution.PrincipalUser && meta.Initiator.ID > 0 {
		initiatorType, initiatorID = string(meta.Initiator.Kind), meta.Initiator.ID
	}
	_, err := sink.writer.Write(ctx, sink.db, audit.Record{
		ActorType:     "user",
		ActorID:       fact.ActorUserID,
		Action:        fact.OperationID,
		Outcome:       outcome,
		Phase:         audit.PhaseAccess,
		DomainRefType: fact.ObjectType,
		CorrelationID: fact.CorrelationID,
		RequestID:     fact.RequestID,
		InitiatorType: initiatorType,
		InitiatorID:   initiatorID,
	})
	return err
}

// NewAccessAdmission builds the production admission guard for one surface.
func NewAccessAdmission(application *apiServer, registry *operations.AccessRegistry, flows operations.FlowResolver, sink operations.Sink) (*operations.Admission, error) {
	if sink == nil {
		sink = accessAuditSink{writer: audit.NewWriter(), db: application.db}
	}
	if flows == nil {
		flows = application.accessFlowResolver()
	}
	return operations.NewAdmission(operations.AdmissionDeps{
		Registry: registry,
		Sessions: application.accessSessionResolver(),
		Flows:    flows,
		Sink:     sink,
		OnSinkError: func(fact operations.AccessFact, err error) {
			sharedops.LogEvent("quoin", "error", "access_audit.write_failed", fact.OperationID+": "+err.Error())
		},
	})
}

// newBootstrapGate closes the whole managed surface while the deployment has
// no initialized administrator: only the minimal authentication-flow routes
// (declared LevelPublic/LevelFlow) pass through, so no normal managed
// endpoint — not even an initialized operator session in a crafted legacy
// database — is reachable before deployment initialization completes. The
// probe re-reads deployment state on every request (pure read, no writes);
// probe failures fail closed with the generic envelope and never leak
// database detail. Main wraps the NewHandler result with this gate.
func newBootstrapGate(application *apiServer, registry *operations.AccessRegistry, next http.Handler) (http.Handler, error) {
	allow := map[string]bool{}
	for _, d := range registry.Declarations() {
		if d.Raw {
			continue
		}
		// LevelFlowOrAdmin covers the delivery settings the initializing
		// administrator must configure before initialization completes; the
		// admission guard still demands the flow credential.
		if d.Level == operations.LevelPublic || d.Level == operations.LevelFlow || d.Level == operations.LevelFlowOrAdmin {
			allow[d.Method+" "+d.Path] = true
		}
	}
	if len(allow) == 0 {
		return nil, errors.New("operations: bootstrap gate found no declared authentication-flow routes")
	}
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		initialized, err := application.auth.IsDeploymentInitialized(request.Context())
		if err != nil {
			sharedops.LogEvent("quoin", "error", "bootstrap_gate.probe_failed", err.Error())
			writeBackupProblem(writer, http.StatusServiceUnavailable, "unavailable", "暂时无法判断系统初始化状态，请稍后重试。", true)
			return
		}
		if initialized || allow[request.Method+" "+request.URL.Path] {
			next.ServeHTTP(writer, request)
			return
		}
		if request.Method == http.MethodGet && request.URL.Path == "/api/v1/auth/me" {
			writeBackupProblem(writer, http.StatusUnauthorized, "initialization_required", "系统尚未完成初始化。", false)
			return
		}
		writeBackupProblem(writer, http.StatusServiceUnavailable, "initialization_required", "系统尚未完成初始化。", true)
	}), nil
}
