import type {
	BusinessView,
	EnrichmentRule,
	PluginInspectionPlan,
	UserSummary,
} from "../../api/generated/types";

interface LabelContractSummary {
	id: string;
	version: number;
	state: "draft" | "active" | "retired";
	rowVersion: number;
	parserVersion: string;
	schemaVersion: string;
	createdAt: string;
	activatedAt?: string;
}

import type {
	ConnectionDetailView,
	ProbeAttemptView,
	ProbeResultView,
} from "../../features/settings/platform/connections/api";
import type {
	AdminUser,
	AuditEventInfo,
	SessionInfo,
} from "../../features/settings/platform/users/api";
import type {
	AlertOccurrenceSummary,
	AlertSourceDetail,
	IntakeIssue,
	ObservationSummary,
} from "../../features/alerts/api";
import type {
	AttemptSummary,
	InitialAnalysisDetail,
} from "../../features/analysis/api";
import type { EvidenceDetail } from "../../features/analysis/tool-details/api";
import type {
	InspectionReportDetail,
	InspectionRunDetail,
} from "../../features/inspection/api";
import type {
	InvestigationAttempt,
	InvestigationDetail,
	InvestigationMessage,
} from "../../features/investigation/api";
import type {
	CandidateDetail,
	ImportBatchDetail,
	KnowledgeDetail,
	KnowledgeVersionDetail,
} from "../../features/knowledge/api";

/** Public scenario vocabulary consumed by the future mock bootstrap and scenario panel. */
export type MockScenario =
	| "administrator"
	| "operator"
	| "unauthenticated"
	| "password-change"
	| "session-expired"
	| "unavailable"
	| "maintenance"
	| "platform-one"
	| "platform-boundary"
	| "metrics-one"
	| "metrics-boundary"
	| "empty"
	| "oidc"
	| "slow"
	| "conflict";

export const DEMO_CREDENTIALS = {
	admin: { username: "admin", password: "demo-admin-password" },
	operator: { username: "operator", password: "demo-operator-password" },
	// Fixed second-factor code the auth flow preview "delivers".
} as const;

/** Server-side flow state the HttpOnly cookie points at; one per browser preview. */

const now = "2026-09-09T09:30:00.000Z";
export const adminUser: UserSummary = {
	id: "user-admin",
	username: "admin",
	displayName: "演示管理员",
	role: "admin",
	enabled: true,
	initialized: true,
	passwordChangeRequired: false,
	authRevision: 1,
	rowVersion: 1,
	lastLoginAt: now,
};
export const operatorUser: UserSummary = {
	id: "user-operator",
	username: "operator",
	displayName: "演示操作员",
	role: "operator",
	enabled: true,
	initialized: true,
	passwordChangeRequired: false,
	authRevision: 1,
	rowVersion: 1,
	lastLoginAt: now,
};
const passwordChangeUser: UserSummary = {
	...operatorUser,
	passwordChangeRequired: true,
};

export interface MockState {
	scenario: MockScenario;
	currentUser: UserSummary | null;
	/** False only for a fresh deployment awaiting first-run admin initialization. */
	adminInitialized: boolean;
	users: AdminUser[];
	/** Masked receive targets per user; the mock never holds plaintext targets. */
	contacts: Record<
		string,
		{
			id: string;
			channel: "email" | "sms";
			maskedTarget: string;
			verified: boolean;
		}[]
	>;
	sessions: SessionInfo[];
	auditEvents: AuditEventInfo[];
	passwords: Record<string, string>;
	alertCredentials: Record<
		string,
		Array<{
			id: string;
			rowVersion: number;
			state: "Active" | "Retired";
			createdAt: string;
		}>
	>;
	feedback: Array<{
		id: string;
		targetType: string;
		targetId: string;
		value: string;
		note?: string;
		createdBy?: string;
		createdAt: string;
	}>;
	backupSettings: {
		enabled: boolean;
		scheduleCron: string | null;
		timezone: string;
		backupTarget: string;
		retentionCount: number;
		rowVersion: number;
	};
	artifactRetention: { generatedRetentionDays: number; rowVersion: number };
	backups: Array<{
		id: string;
		status: "Succeeded" | "Failed";
		stage: string;
		createdAt: string;
		completedAt?: string;
		errorDetail?: string;
	}>;
	alerts: AlertOccurrenceSummary[];
	observations: Record<string, ObservationSummary[]>;
	intakeIssues: IntakeIssue[];
	alertSources: AlertSourceDetail[];
	analyses: Record<string, InitialAnalysisDetail[]>;
	analysisAttempts: Record<string, AttemptSummary[]>;
	investigations: InvestigationDetail[];
	messages: Record<string, InvestigationMessage[]>;
	investigationAttempts: Record<string, InvestigationAttempt[]>;
	attachments: Map<string, InvestigationMessage["attachments"][number]>;
	connections: ConnectionDetailView[];
	probes: Record<string, ProbeAttemptView[]>;
	probeResults: Record<string, ProbeResultView[]>;
	labels: LabelContractSummary[];
	enrichmentRules: EnrichmentRule[];
	businessViews: BusinessView[];
	inspectionPlans: PluginInspectionPlan[];
	inspectionRuns: InspectionRunDetail[];
	reports: Record<string, InspectionReportDetail[]>;
	candidates: CandidateDetail[];
	knowledge: KnowledgeDetail[];
	versions: Record<string, KnowledgeVersionDetail[]>;
	imports: ImportBatchDetail[];
	evidence: Record<string, EvidenceDetail>;
	/** Domain-command idempotency cache keyed by (principal, clientCommandId). */
	commands: Map<
		string,
		{ commandType: string; digest: string; status: number; body: unknown }
	>;
	sequence: number;
}

function baseState(scenario: MockScenario): MockState {
	// Platform-fault frontend currently requires explicit source metadata on every unified alert fixture.
	// ADR-0012 归一化语义 fixture：severity/title/correlations/enrichment 均
	// 为首观测冻结投影。
	const webAlert: AlertOccurrenceSummary = {
		id: "alert-checkout-latency",
		state: "Firing",
		rowVersion: 3,
		severity: "critical",
		title: "CheckoutLatencyHigh",
		resource: "checkout-8080",
		firstSeenAt: "2026-09-09T08:10:00Z",
		lastStateChangeAt: "2026-09-09T09:20:00Z",
		source: "alertmanager",
		labels: {
			alertname: "CheckoutLatencyHigh",
			service: "checkout",
			severity: "critical",
		},
		annotations: { summary: "结算接口 P95 延迟超过阈值" },
		correlations: [{ viewKey: "checkout", displayName: "结算" }],
		enrichment: { fields: { team: "payments", tier: "gold" } },
	};
	const resolvedAlert: AlertOccurrenceSummary = {
		id: "alert-catalog-errors",
		state: "Resolved",
		rowVersion: 2,
		severity: "warning",
		title: "CatalogErrors",
		firstSeenAt: "2026-09-08T07:00:00Z",
		lastStateChangeAt: "2026-09-08T08:20:00Z",
		resolvedAt: "2026-09-08T08:20:00Z",
		source: "alertmanager",
		labels: {
			alertname: "CatalogErrors",
			service: "catalog",
			severity: "warning",
		},
		annotations: { summary: "目录错误率已恢复" },
		correlations: [{ viewKey: "catalog", displayName: "目录服务" }],
	};
	const analysis: InitialAnalysisDetail = {
		id: "analysis-1",
		state: "Succeeded",
		rowVersion: 2,
		createdAt: now,
		attemptCount: 1,
		output: {
			id: "analysis-output-1",
			modelId: "gpt-demo",
			content:
				"延迟主要来自 payment 依赖的上游等待。建议检查 payment 服务和连接池。",
			evidenceIds: ["evidence-latency"],
			createdAt: now,
		},
	};
	const firstMessage: InvestigationMessage = {
		id: "message-1",
		seq: 1,
		role: "user",
		status: "active",
		content: "请分析结算服务延迟告警。",
		attachments: [],
		attemptId: "attempt-1",
		evidenceIds: null,
		createdAt: now,
	};
	const assistantMessage: InvestigationMessage = {
		id: "message-2",
		seq: 2,
		role: "assistant",
		status: "active",
		content: "已关联告警和指标证据。payment 上游等待是最可能的原因。",
		parentMessageId: "message-1",
		attachments: [],
		attemptId: "attempt-1",
		evidenceIds: ["evidence-latency"],
		createdAt: now,
	};
	const investigation: InvestigationDetail = {
		id: "investigation-checkout",
		displayTitle: "结算延迟调查",
		createdAt: now,
		lastActivityAt: now,
		createdBy: adminUser.id,
		headMessageId: assistantMessage.id,
		activeAttemptId: undefined,
		messageCount: 2,
		attemptCount: 1,
		sources: [
			{
				id: "source-1",
				type: "occurrence",
				sourceId: webAlert.id,
				linkedAt: now,
			},
		],
	};
	const thanos: ConnectionDetailView & { id: string } = {
		id: "connection-thanos-primary",
		name: "thanos-primary",
		type: "thanos",
		enabled: true,
		revalidationRequired: false,
		currentRevisionId: "revision-thanos-1",
		currentCredentialGenerationId: "generation-thanos-1",
		rowVersion: 3,
		config: {
			type: "thanos",
			baseUrl: "https://thanos.demo.invalid",
			username: "readonly",
		},
		revisionCount: 1,
		generationCount: 1,
	};
	const model: ConnectionDetailView & { id: string } = {
		id: "connection-model-provider",
		name: "model-provider",
		type: "model_provider",
		enabled: true,
		revalidationRequired: false,
		currentRevisionId: "revision-model-1",
		currentCredentialGenerationId: "generation-model-1",
		rowVersion: 2,
		config: {
			type: "model_provider",
			baseUrl: "https://models.demo.invalid",
			chatModelId: "gpt-demo",
			embeddingModelId: "embedding-demo",
			contextBudgetTokens: 8192,
			maxOutputTokens: 1024,
		},
		revisionCount: 1,
		generationCount: 1,
	};
	const run: InspectionRunDetail = {
		id: "inspection-run-1",
		planKey: "checkout-health",
		state: "Completed",
		rowVersion: 2,
		triggerKind: "manual",
		evidenceAt: now,
		createdAt: now,
		checks: [
			{ checkKey: "latency", status: "ok", evidenceId: "evidence-latency" },
		],
		reportCount: 1,
		analysisActive: false,
	};
	// ADR-0012 富化规则 fixture：命中即叠加 outputs 的声明式配置。
	const checkoutTierRule: EnrichmentRule = {
		ruleKey: "payments-tier",
		displayName: "结算服务富化",
		description: "按 team/tier 标注结算域告警",
		enabled: true,
		labelConditions: { service: "checkout" },
		alertSourceKeys: [],
		outputs: { team: "payments", tier: "gold" },
		priority: 100,
		rowVersion: 1,
		createdAt: now,
		updatedAt: now,
	};
	// Optional business views (ADR 0004): scope-and-description only, never permissions.
	const checkoutView: BusinessView = {
		viewKey: "checkout",
		displayName: "结算",
		description: "结算业务范围说明",
		scope: {
			connectionName: "thanos-primary",
			labelConditions: { service: "checkout" },
		},
		rowVersion: 3,
		createdAt: now,
		updatedAt: now,
	};
	const catalogView: BusinessView = {
		viewKey: "catalog",
		displayName: "目录服务",
		description: "不限定来源接入的示例视图",
		scope: { labelConditions: { service: "catalog" } },
		rowVersion: 1,
		createdAt: now,
		updatedAt: now,
	};
	// Standalone plugin inspection plans select range via scope, independent of any business declaration.
	const latencyPlan: PluginInspectionPlan = {
		planKey: "checkout-latency-watch",
		displayName: "结算延迟观测",
		enabled: true,
		connectionName: "thanos-primary",
		pluginId: "thanos",
		templateId: "promql-check",
		templateVersion: "1",
		params: { query: "histogram_quantile(0.95, checkout_latency)" },
		scope: { kind: "businessView", businessViewKey: "checkout" },
		cron: "*/5 * * * *",
		timezone: "Asia/Shanghai",
		rowVersion: 2,
		createdAt: now,
		updatedAt: now,
	};
	// A seeded active run exercises cancel semantics on a plan-scoped (non-legacy) Run.
	const activeRun: InspectionRunDetail = {
		id: "inspection-run-2",
		planKey: latencyPlan.planKey,
		connectionName: latencyPlan.connectionName,
		state: "Running",
		rowVersion: 1,
		triggerKind: "schedule",
		scheduledFor: now,
		createdAt: now,
		checks: [],
		reportCount: 0,
		analysisActive: false,
	};
	const candidate: CandidateDetail = {
		id: "candidate-1",
		sourceType: "initial_analysis_output",
		sourceId: "analysis-output-1",
		state: "AwaitingConfirmation",
		rowVersion: 1,
		generation: 1,
		draftRevision: 1,
		draftTitle: "排查 payment 上游等待",
		draftBody: "当结算延迟升高时，检查 payment 服务等待与连接池。",
		draftScope: { service: "checkout" },
		originalSuggestion: {
			v: 1,
			source: {
				type: "initial_analysis_output",
				id: "analysis-output-1",
				createdAt: now,
			},
			title: "排查 payment 上游等待",
			body: "当结算延迟升高时，检查 payment 服务等待与连接池。",
		},
	};
	const knowledge: KnowledgeDetail = {
		id: "knowledge-1",
		title: "结算延迟排查",
		currentVersionId: "knowledge-version-1",
		currentVersionSeq: 1,
		eligible: true,
		rowVersion: 1,
		versionCount: 1,
	};
	const version: KnowledgeVersionDetail = {
		id: "knowledge-version-1",
		versionSeq: 1,
		title: knowledge.title,
		body: "先检查 payment 服务和连接池，再确认指标是否恢复。",
		scope: { service: "checkout" },
		conditions: { severity: "critical", window: "5m" },
		limitations: { note: "不适用于跨集群场景" },
		sourceCandidateId: candidate.id,
		createdAt: now,
		eligible: true,
		retrievalStateRowVersion: 1,
		embeddingState: "ready",
	};
	const importCandidate: CandidateDetail = {
		id: "candidate-import-1",
		sourceType: "source_material",
		sourceId: "import-1",
		state: "AwaitingConfirmation",
		rowVersion: 1,
		generation: 1,
		draftRevision: 1,
		draftTitle: "导入的结算故障排查步骤",
		draftBody: "检查 payment 上游、连接池和结算延迟指标。",
		targetKnowledgeId: knowledge.id,
		originalSuggestion: {
			v: 1,
			source: { type: "source_material", id: "import-1", createdAt: now },
			title: "导入的结算故障排查步骤",
			body: "检查 payment 上游、连接池和结算延迟指标。",
		},
	};
	const importBatch: ImportBatchDetail = {
		id: "import-1",
		state: "AwaitingConfirmation",
		rowVersion: 1,
		generation: 1,
		createdAt: now,
		candidates: [importCandidate],
	};
	const evidence: EvidenceDetail = {
		id: "evidence-latency",
		targetType: "thanos_query",
		targetId: webAlert.id,
		params: { query: "histogram_quantile(0.95, checkout_latency)" },
		observedAt: now,
		integrity: "complete",
		producer: { kind: "quoin_local" },
		connections: [{ key: thanos.name, type: "thanos" }],
		body: { kind: "inline_json", value: { p95Ms: 1420, thresholdMs: 800 } },
		createdAt: now,
	};
	const state: MockState = {
		scenario,
		currentUser:
			scenario === "operator"
				? operatorUser
				: scenario === "password-change"
					? passwordChangeUser
					: scenario === "unauthenticated" || scenario === "session-expired"
						? null
						: adminUser,
		adminInitialized: scenario !== "empty",
		users: [adminUser, operatorUser].map((user) => ({ ...user, authSource: "local" as const })),
		contacts: {
			[adminUser.id]: [
				{
					id: "contact-admin-email",
					channel: "email",
					maskedTarget: "a***@example.test",
					verified: true,
				},
			],
			[operatorUser.id]: [
				{
					id: "contact-operator-email",
					channel: "email",
					maskedTarget: "o***@example.test",
					verified: true,
				},
			],
		},
		passwords: {
			[adminUser.id]: DEMO_CREDENTIALS.admin.password,
			[operatorUser.id]: DEMO_CREDENTIALS.operator.password,
		},
		sessions: [
			{
				id: "session-current",
				clientLabel: "本地演示浏览器",
				createdAt: now,
				lastActiveAt: now,
				idleExpiresAt: "2026-09-09T17:30:00Z",
				absoluteExpiresAt: "2026-09-10T09:30:00Z",
				current: true,
			},
		],
		auditEvents: [
			{
				id: "audit-1",
				actorType: "user",
				actorId: adminUser.id,
				action: "mock_session_started",
				outcome: "success",
				createdAt: now,
			},
		],
		alerts: [webAlert, resolvedAlert],
		observations: {
			[webAlert.id]: [
				{
					id: "observation-1",
					observedState: "firing",
					startsAt: webAlert.firstSeenAt,
					receivedAt: now,
					committedAt: now,
					effect: "repeat_firing",
				},
			],
			[resolvedAlert.id]: [
				{
					id: "observation-2",
					observedState: "resolved",
					startsAt: resolvedAlert.firstSeenAt,
					endsAt: resolvedAlert.resolvedAt,
					receivedAt: resolvedAlert.resolvedAt!,
					committedAt: resolvedAlert.resolvedAt!,
					effect: "resolved",
				},
			],
		},
		intakeIssues: [
			{
				id: "intake-1",
				kind: "delivery_truncated",
				issueKey: "alertmanager/demo",
				detailJson: '{"source":"demo"}',
				firstSeenAt: now,
				lastSeenAt: now,
				occurrenceCount: 2,
				rowVersion: 1,
			},
		],
		alertSources: [
			{
				key: "demo-alertmanager",
				protocol: "alertmanager",
				enabled: true,
				rowVersion: 1,
				createdAt: now,
				credentialCount: 1,
			},
		],
		alertCredentials: {
			"demo-alertmanager": [
				{
					id: "alert-credential-1",
					rowVersion: 1,
					state: "Active",
					createdAt: now,
				},
			],
		},
		feedback: [
			{
				id: "feedback-1",
				targetType: "initial_analysis_output",
				targetId: "analysis-output-1",
				value: "adopted",
				createdBy: adminUser.id,
				createdAt: now,
			},
		],
		backupSettings: {
			enabled: true,
			scheduleCron: "0 2 * * *",
			timezone: "Asia/Shanghai",
			backupTarget: "local",
			retentionCount: 7,
			rowVersion: 3,
		},
		artifactRetention: { generatedRetentionDays: 14, rowVersion: 5 },
		backups: [
			{
				id: "backup-1",
				status: "Succeeded",
				stage: "Completed",
				createdAt: now,
				completedAt: now,
			},
		],
		analyses: { [webAlert.id]: [analysis] },
		analysisAttempts: {
			[analysis.id]: [
				{
					id: "analysis-attempt-1",
					type: "initial_analysis",
					state: "Succeeded",
					rowVersion: 1,
					startedAt: now,
					endedAt: now,
					createdAt: now,
				},
			],
		},
		investigations: [investigation],
		messages: { [investigation.id]: [firstMessage, assistantMessage] },
		investigationAttempts: {
			[investigation.id]: [
				{
					id: "attempt-1",
					type: "investigation",
					state: "Succeeded",
					rowVersion: 1,
					createdAt: now,
					startedAt: now,
					endedAt: now,
				},
			],
		},
		attachments: new Map(),
		connections: [thanos, model],
		probes: {},
		probeResults: {
			[thanos.name]: [
				{
					id: "probe-result-1",
					attemptId: "probe-attempt-1",
					connectionType: "thanos",
					outcome: "passed",
					actionSetId: "thanos",
					actionSetVersion: 1,
					resultDigest: "sha256:probe",
					startedAt: now,
					finishedAt: now,
					details: { endpoint: "reachable" },
				},
			],
		},
		labels: [
			{
				id: "label-1",
				version: 1,
				state: "active",
				rowVersion: 1,
				parserVersion: "1",
				schemaVersion: "1",
				createdAt: now,
				activatedAt: now,
			},
		],
		enrichmentRules: [checkoutTierRule],
		businessViews: [checkoutView, catalogView],
		inspectionPlans: [latencyPlan],
		inspectionRuns: [activeRun, run],
		reports: {
			[run.id]: [
				{
					id: "report-1",
					runId: run.id,
					version: 1,
					evidenceDigest: "sha256:evidence",
					evidenceIds: [evidence.id],
					modelId: "gpt-demo",
					content: "巡检完成，结算服务延迟需要关注。",
					createdAt: now,
				},
			],
		},
		candidates: [candidate, importCandidate],
		knowledge: [knowledge],
		versions: { [knowledge.id]: [version] },
		imports: [importBatch],
		evidence: { [evidence.id]: evidence },
		commands: new Map(),
		sequence: 10,
	};
	// Explicit preview fixtures exercise the zero/one/fifty and long-name list boundaries without hidden UI-only switches.
	if (scenario === "metrics-one" || scenario === "metrics-boundary") {
		const count = scenario === "metrics-one" ? 1 : 50;
		state.connections = Array.from({ length: count }, (_, index) => ({
			...thanos,
			id: `connection-metrics-${index + 1}`,
			name:
				index === count - 1 && count === 50
					? "thanos-metrics-with-an-intentionally-long-non-secret-display-name-for-editor-boundary-preview-2026-09-10"
					: `thanos-metrics-${String(index + 1).padStart(2, "0")}`,
			rowVersion: index + 1,
		}));
	}
	// Empty mode keeps a signed-in identity but removes every domain projection and its linked history.
	if (scenario === "empty") {
		state.adminInitialized = false;
		state.users = [];
		state.sessions = [];
		state.auditEvents = [];
		state.alerts = [];
		state.observations = {};
		state.intakeIssues = [];
		state.alertSources = [];
		state.alertCredentials = {};
		state.analyses = {};
		state.analysisAttempts = {};
		state.investigations = [];
		state.messages = {};
		state.investigationAttempts = {};
		state.attachments = new Map();
		state.connections = [];
		state.probes = {};
		state.probeResults = {};
		state.labels = [];
		state.enrichmentRules = [];
		state.businessViews = [];
		state.inspectionPlans = [];
		state.inspectionRuns = [];
		state.reports = {};
		state.candidates = [];
		state.knowledge = [];
		state.versions = {};
		state.imports = [];
		state.evidence = {};
		state.feedback = [];
		state.backups = [];
	}
	return state;
}

let state = baseState("administrator");
export function getMockState(): MockState {
	return state;
}
export function resetMockState(): void {
	state = baseState(state.scenario);
}
export function setMockScenario(scenario: MockScenario): void {
	state = baseState(scenario);
}
export function getMockScenario(): MockScenario {
	return state.scenario;
}
export function nextId(prefix: string): string {
	state.sequence += 1;
	return `${prefix}-${state.sequence}`;
}
