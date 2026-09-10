import type { UserSummary } from '../../api/generated/types'
import type { AlertOccurrenceSummary, AlertSourceDetail, IntakeIssue, ObservationSummary } from '../../features/alerts/api'
import type { InitialAnalysisDetail, AttemptSummary } from '../../features/analysis/api'
import type { EvidenceDetail } from '../../features/analysis/tool-details/api'
import type { AdminUser, AuditEventInfo, SessionInfo } from '../../features/admin/users/api'
import type { ConnectionDetailView, ProbeAttemptView, ProbeResultView } from '../../features/admin/connections/api'
import type { BusinessSystemDetail, ConfigVersionDetail, LabelContractSummary, ResourceRefreshRunDetail, VerificationRunDetail } from '../../features/admin/business-systems/api'
import type { InspectionReportDetail, InspectionRunDetail } from '../../features/inspection/api'
import type { CandidateDetail, ImportBatchDetail, KnowledgeDetail, KnowledgeVersionDetail } from '../../features/knowledge/api'
import type { InvestigationAttempt, InvestigationDetail, InvestigationMessage } from '../../features/investigation/api'

/** Public scenario vocabulary consumed by the future mock bootstrap and scenario panel. */
export type MockScenario = 'administrator' | 'operator' | 'unauthenticated' | 'password-change' | 'session-expired' | 'unavailable' | 'maintenance' | 'platform-one' | 'platform-boundary' | 'metrics-one' | 'metrics-boundary' | 'business-boundary' | 'empty' | 'slow' | 'conflict'

export const DEMO_CREDENTIALS = {
  admin: { username: 'admin', password: 'demo-admin-password' },
  operator: { username: 'operator', password: 'demo-operator-password' },
} as const

const now = '2026-09-09T09:30:00.000Z'
export const adminUser: UserSummary = { id: 'user-admin', username: 'admin', displayName: '演示管理员', role: 'admin', enabled: true, passwordChangeRequired: false, authRevision: 1, rowVersion: 1, lastLoginAt: now }
export const operatorUser: UserSummary = { id: 'user-operator', username: 'operator', displayName: '演示操作员', role: 'operator', enabled: true, passwordChangeRequired: false, authRevision: 1, rowVersion: 1, lastLoginAt: now }
const passwordChangeUser: UserSummary = { ...operatorUser, passwordChangeRequired: true }

export interface MockState {
  scenario: MockScenario
  currentUser: UserSummary | null
  users: AdminUser[]
  sessions: SessionInfo[]
  auditEvents: AuditEventInfo[]
  passwords: Record<string, string>
  alertCredentials: Record<string, Array<{ id: string; rowVersion: number; state: 'Active' | 'Retired'; createdAt: string }>>
  mappings: Record<string, Array<{ id: string; connectionId: string; connectionName: string; state: 'Active' | 'Retired'; rowVersion: number; createdBy: string; createdAt: string; retiredBy: string | null; retiredAt?: string }>>
  feedback: Array<{ id: string; targetType: string; targetId: string; value: string; note?: string; createdBy?: string; createdAt: string }>
  backupSettings: { enabled: boolean; scheduleCron: string | null; timezone: string; backupTarget: string; retentionCount: number; rowVersion: number }
  artifactRetention: { generatedRetentionDays: number; rowVersion: number }
  backups: Array<{ id: string; status: 'Succeeded' | 'Failed'; stage: string; createdAt: string; completedAt?: string; errorDetail?: string }>
  alerts: AlertOccurrenceSummary[]
  observations: Record<string, ObservationSummary[]>
  intakeIssues: IntakeIssue[]
  alertSources: AlertSourceDetail[]
  analyses: Record<string, InitialAnalysisDetail[]>
  analysisAttempts: Record<string, AttemptSummary[]>
  investigations: InvestigationDetail[]
  messages: Record<string, InvestigationMessage[]>
  investigationAttempts: Record<string, InvestigationAttempt[]>
  attachments: Map<string, InvestigationMessage['attachments'][number]>
  connections: ConnectionDetailView[]
  probes: Record<string, ProbeAttemptView[]>
  probeResults: Record<string, ProbeResultView[]>
  systems: BusinessSystemDetail[]
  configVersions: Record<string, ConfigVersionDetail[]>
  refreshRuns: Record<string, ResourceRefreshRunDetail[]>
  verifications: Record<string, VerificationRunDetail[]>
  labels: LabelContractSummary[]
  inspectionRuns: InspectionRunDetail[]
  reports: Record<string, InspectionReportDetail[]>
  candidates: CandidateDetail[]
  knowledge: KnowledgeDetail[]
  versions: Record<string, KnowledgeVersionDetail[]>
  imports: ImportBatchDetail[]
  evidence: Record<string, EvidenceDetail>
  sequence: number
}

function baseState(scenario: MockScenario): MockState {
  // Platform-fault frontend currently requires explicit source metadata on every unified alert fixture.
  const webAlert: AlertOccurrenceSummary = { id: 'alert-checkout-latency', state: 'Firing', rowVersion: 3, businessSystemKey: 'checkout', firstSeenAt: '2026-09-09T08:10:00Z', lastStateChangeAt: '2026-09-09T09:20:00Z', source: 'alertmanager', labels: { alertname: 'CheckoutLatencyHigh', service: 'checkout', severity: 'critical' }, annotations: { summary: '结算接口 P95 延迟超过阈值' } }
  const resolvedAlert: AlertOccurrenceSummary = { id: 'alert-catalog-errors', state: 'Resolved', rowVersion: 2, businessSystemKey: 'catalog', firstSeenAt: '2026-09-08T07:00:00Z', lastStateChangeAt: '2026-09-08T08:20:00Z', resolvedAt: '2026-09-08T08:20:00Z', source: 'alertmanager', labels: { alertname: 'CatalogErrors', service: 'catalog', severity: 'warning' }, annotations: { summary: '目录错误率已恢复' } }
  const analysis: InitialAnalysisDetail = { id: 'analysis-1', state: 'Succeeded', rowVersion: 2, createdAt: now, attemptCount: 1, output: { id: 'analysis-output-1', modelId: 'gpt-demo', content: '延迟主要来自 payment 依赖的上游等待。建议检查 payment 服务和连接池。', evidenceIds: ['evidence-latency'], createdAt: now } }
  const firstMessage: InvestigationMessage = { id: 'message-1', seq: 1, role: 'user', status: 'active', content: '请分析结算服务延迟告警。', attachments: [], evidenceIds: null, createdAt: now }
  const assistantMessage: InvestigationMessage = { id: 'message-2', seq: 2, role: 'assistant', status: 'active', content: '已关联告警和指标证据。payment 上游等待是最可能的原因。', parentMessageId: 'message-1', attachments: [], attemptId: 'attempt-1', evidenceIds: ['evidence-latency'], createdAt: now }
  const investigation: InvestigationDetail = { id: 'investigation-checkout', displayTitle: '结算延迟调查', createdAt: now, lastActivityAt: now, createdBy: adminUser.id, headMessageId: assistantMessage.id, activeAttemptId: undefined, messageCount: 2, attemptCount: 1, sources: [{ id: 'source-1', type: 'occurrence', sourceId: webAlert.id, linkedAt: now }] }
  const thanos: ConnectionDetailView & { id: string } = { id: 'connection-thanos-primary', name: 'thanos-primary', type: 'thanos', enabled: true, revalidationRequired: false, currentRevisionId: 'revision-thanos-1', currentCredentialGenerationId: 'generation-thanos-1', rowVersion: 3, config: { type: 'thanos', baseUrl: 'https://thanos.demo.invalid', username: 'readonly' }, revisionCount: 1, generationCount: 1 }
  const kubernetes: ConnectionDetailView & { id: string } = { id: 'connection-kubernetes-prod', name: 'kubernetes-prod', type: 'kubernetes', enabled: true, revalidationRequired: false, currentRevisionId: 'revision-k8s-1', currentCredentialGenerationId: 'generation-k8s-1', rowVersion: 2, config: { type: 'kubernetes', contextName: 'production', defaultNamespace: 'checkout' }, revisionCount: 1, generationCount: 1 }
  const model: ConnectionDetailView & { id: string } = { id: 'connection-model-provider', name: 'model-provider', type: 'model_provider', enabled: true, revalidationRequired: false, currentRevisionId: 'revision-model-1', currentCredentialGenerationId: 'generation-model-1', rowVersion: 2, config: { type: 'model_provider', baseUrl: 'https://models.demo.invalid', chatModelId: 'gpt-demo', embeddingModelId: 'embedding-demo', contextBudgetTokens: 8192, maxOutputTokens: 1024 }, revisionCount: 1, generationCount: 1 }
  const checkout: BusinessSystemDetail = { key: 'checkout', displayName: '结算系统', enabled: true, rowVersion: 3, currentConfigVersionId: 'config-checkout-1', timezone: 'Asia/Shanghai', resourceRefreshIntervalSeconds: 300, browserIdentityState: 'Ready', configVersionCount: 1, discoveries: [{ discoveryKey: 'checkout-pods', displayName: '结算工作负载', selector: 'app=checkout', identityLabels: ['namespace', 'pod'] }], plans: [{ planKey: 'checkout-health', displayName: '结算健康检查', cron: '*/5 * * * *', checks: [{ checkKey: 'latency', displayName: '请求延迟', analysisQuestion: '延迟是否异常？', kind: 'promql', queryMode: 'range', expression: 'histogram_quantile(0.95, checkout_latency)', rangeSeconds: 300, stepSeconds: 30 }] }] }
  const config: ConfigVersionDetail = { id: 'config-checkout-1', versionSeq: 1, state: 'published', createdAt: now, publishedAt: now, digest: 'sha256:checkout-config', parserVersion: '1', schemaVersion: '1', systemKey: 'checkout', displayName: checkout.displayName, enabled: true, labelContractVersionId: 'label-1', journeyCatalogDigest: 'sha256:journeys', journeyCatalogVersion: '1', yamlBody: 'key: checkout\ndisplayName: 结算系统\n', timezone: 'Asia/Shanghai', resourceRefreshIntervalSeconds: 300, discoveries: checkout.discoveries, plans: checkout.plans }
  const run: InspectionRunDetail = { id: 'inspection-run-1', businessSystemKey: 'checkout', planKey: 'checkout-health', state: 'Completed', rowVersion: 2, triggerKind: 'manual', evidenceAt: now, createdAt: now, checks: [{ checkKey: 'latency', status: 'ok', evidenceId: 'evidence-latency' }], reportCount: 1, analysisActive: false }
  const candidate: CandidateDetail = { id: 'candidate-1', sourceType: 'initial_analysis_output', sourceId: 'analysis-output-1', state: 'AwaitingConfirmation', rowVersion: 1, generation: 1, draftRevision: 1, draftTitle: '排查 payment 上游等待', draftBody: '当结算延迟升高时，检查 payment 服务等待与连接池。', draftScope: { service: 'checkout' }, originalSuggestion: { v: 1, source: { type: 'initial_analysis_output', id: 'analysis-output-1', createdAt: now }, title: '排查 payment 上游等待', body: '当结算延迟升高时，检查 payment 服务等待与连接池。' } }
  const knowledge: KnowledgeDetail = { id: 'knowledge-1', title: '结算延迟排查', currentVersionId: 'knowledge-version-1', currentVersionSeq: 1, eligible: true, rowVersion: 1, versionCount: 1 }
  const version: KnowledgeVersionDetail = { id: 'knowledge-version-1', versionSeq: 1, title: knowledge.title, body: '先检查 payment 服务和连接池，再确认指标是否恢复。', sourceCandidateId: candidate.id, createdAt: now, eligible: true, retrievalStateRowVersion: 1, embeddingState: 'ready' }
  const importCandidate: CandidateDetail = { id: 'candidate-import-1', sourceType: 'source_material', sourceId: 'import-1', state: 'AwaitingConfirmation', rowVersion: 1, generation: 1, draftRevision: 1, draftTitle: '导入的结算故障排查步骤', draftBody: '检查 payment 上游、连接池和结算延迟指标。', targetKnowledgeId: knowledge.id, originalSuggestion: { v: 1, source: { type: 'source_material', id: 'import-1', createdAt: now }, title: '导入的结算故障排查步骤', body: '检查 payment 上游、连接池和结算延迟指标。' } }
  const importBatch: ImportBatchDetail = { id: 'import-1', state: 'AwaitingConfirmation', rowVersion: 1, generation: 1, createdAt: now, candidates: [importCandidate] }
  const evidence: EvidenceDetail = { id: 'evidence-latency', targetType: 'thanos_query', targetId: webAlert.id, params: { query: 'histogram_quantile(0.95, checkout_latency)' }, observedAt: now, integrity: 'complete', producer: { kind: 'quoin_local' }, connections: [{ key: thanos.name, type: 'thanos' }], body: { kind: 'inline_json', value: { p95Ms: 1420, thresholdMs: 800 } }, createdAt: now }
  const state: MockState = {
    scenario, currentUser: scenario === 'operator' ? operatorUser : scenario === 'password-change' ? passwordChangeUser : scenario === 'unauthenticated' || scenario === 'session-expired' ? null : adminUser,
    users: [adminUser, operatorUser].map(user => ({ ...user })), passwords: { [adminUser.id]: DEMO_CREDENTIALS.admin.password, [operatorUser.id]: DEMO_CREDENTIALS.operator.password }, sessions: [{ id: 'session-current', clientLabel: '本地演示浏览器', createdAt: now, lastActiveAt: now, idleExpiresAt: '2026-09-09T17:30:00Z', absoluteExpiresAt: '2026-09-10T09:30:00Z', current: true }], auditEvents: [{ id: 'audit-1', actorType: 'user', actorId: adminUser.id, action: 'mock_session_started', outcome: 'success', createdAt: now }],
    alerts: [webAlert, resolvedAlert], observations: { [webAlert.id]: [{ id: 'observation-1', observedState: 'firing', startsAt: webAlert.firstSeenAt, receivedAt: now, committedAt: now, effect: 'repeat_firing' }], [resolvedAlert.id]: [{ id: 'observation-2', observedState: 'resolved', startsAt: resolvedAlert.firstSeenAt, endsAt: resolvedAlert.resolvedAt, receivedAt: resolvedAlert.resolvedAt!, committedAt: resolvedAlert.resolvedAt!, effect: 'resolved' }] }, intakeIssues: [{ id: 'intake-1', kind: 'delivery_truncated', issueKey: 'alertmanager/demo', detailJson: '{"source":"demo"}', firstSeenAt: now, lastSeenAt: now, occurrenceCount: 2, rowVersion: 1 }], alertSources: [{ key: 'demo-alertmanager', protocol: 'alertmanager', enabled: true, rowVersion: 1, createdAt: now, credentialCount: 1 }], alertCredentials: { 'demo-alertmanager': [{ id: 'alert-credential-1', rowVersion: 1, state: 'Active', createdAt: now }] }, mappings: { checkout: [{ id: 'mapping-k8s-1', connectionId: kubernetes.id, connectionName: kubernetes.name, state: 'Active', rowVersion: 1, createdBy: adminUser.id, createdAt: now, retiredBy: null }] }, feedback: [{ id: 'feedback-1', targetType: 'initial_analysis_output', targetId: 'analysis-output-1', value: 'adopted', createdBy: adminUser.id, createdAt: now }], backupSettings: { enabled: true, scheduleCron: '0 2 * * *', timezone: 'Asia/Shanghai', backupTarget: 'local', retentionCount: 7, rowVersion: 3 }, artifactRetention: { generatedRetentionDays: 14, rowVersion: 5 }, backups: [{ id: 'backup-1', status: 'Succeeded', stage: 'Completed', createdAt: now, completedAt: now }], analyses: { [webAlert.id]: [analysis] }, analysisAttempts: { [analysis.id]: [{ id: 'analysis-attempt-1', type: 'initial_analysis', state: 'Succeeded', rowVersion: 1, startedAt: now, endedAt: now, createdAt: now }] },
    investigations: [investigation], messages: { [investigation.id]: [firstMessage, assistantMessage] }, investigationAttempts: { [investigation.id]: [{ id: 'attempt-1', type: 'investigation', state: 'Succeeded', rowVersion: 1, createdAt: now, startedAt: now, endedAt: now }] }, attachments: new Map(), connections: [thanos, kubernetes, model], probes: {}, probeResults: { [thanos.name]: [{ id: 'probe-result-1', attemptId: 'probe-attempt-1', connectionType: 'thanos', outcome: 'passed', actionSetId: 'thanos', actionSetVersion: 1, resultDigest: 'sha256:probe', startedAt: now, finishedAt: now, details: { endpoint: 'reachable' } }] }, systems: [checkout], configVersions: { checkout: [config] }, refreshRuns: { checkout: [] }, verifications: { 'checkout/config-checkout-1': [{ id: 'verification-1', purpose: 'prepublish', configVersionId: config.id, labelContractVersionId: 'label-1', state: 'Passed', rowVersion: 1, evidenceAt: now, createdAt: now, checkResults: [{ planKey: 'checkout-health', checkKey: 'latency', status: 'ok', evidenceId: evidence.id }] }] }, labels: [{ id: 'label-1', version: 1, state: 'active', rowVersion: 1, parserVersion: '1', schemaVersion: '1', createdAt: now, activatedAt: now }], inspectionRuns: [run], reports: { [run.id]: [{ runId: run.id, version: 1, evidenceDigest: 'sha256:evidence', evidenceIds: [evidence.id], modelId: 'gpt-demo', content: '巡检完成，结算服务延迟需要关注。', createdAt: now }] }, candidates: [candidate, importCandidate], knowledge: [knowledge], versions: { [knowledge.id]: [version] }, imports: [importBatch], evidence: { [evidence.id]: evidence }, sequence: 10,
  }
  // Explicit preview fixtures exercise the zero/one/fifty and long-name list boundaries without hidden UI-only switches.
  if (scenario === 'metrics-one' || scenario === 'metrics-boundary') {
    const count = scenario === 'metrics-one' ? 1 : 50
    state.connections = Array.from({ length: count }, (_, index) => ({ ...thanos, id: `connection-metrics-${index + 1}`, name: index === count - 1 && count === 50 ? 'thanos-metrics-with-an-intentionally-long-non-secret-display-name-for-editor-boundary-preview-2026-09-10' : `thanos-metrics-${String(index + 1).padStart(2, '0')}`, rowVersion: index + 1 }))
  }
  if (scenario === 'business-boundary') {
    state.systems = Array.from({ length: 50 }, (_, index) => ({ ...checkout, key: `business-system-${String(index + 1).padStart(2, '0')}`, displayName: index === 49 ? '业务系统名称非常长用于验证左侧列表项目截断、悬停和箭头布局的边界预览二零二六零九一零' : `业务系统 ${String(index + 1).padStart(2, '0')}` }))
  }
  // Empty mode keeps a signed-in identity but removes every domain projection and its linked history.
  if (scenario === 'empty') { state.users = []; state.sessions = []; state.auditEvents = []; state.alerts = []; state.observations = {}; state.intakeIssues = []; state.alertSources = []; state.alertCredentials = {}; state.analyses = {}; state.analysisAttempts = {}; state.investigations = []; state.messages = {}; state.investigationAttempts = {}; state.attachments = new Map(); state.connections = []; state.probes = {}; state.probeResults = {}; state.systems = []; state.configVersions = {}; state.refreshRuns = {}; state.verifications = {}; state.labels = []; state.inspectionRuns = []; state.reports = {}; state.candidates = []; state.knowledge = []; state.versions = {}; state.imports = []; state.evidence = {}; state.mappings = {}; state.feedback = []; state.backups = [] }
  return state
}

let state = baseState('administrator')
export function getMockState(): MockState { return state }
export function resetMockState(): void { state = baseState(state.scenario) }
export function setMockScenario(scenario: MockScenario): void { state = baseState(scenario) }
export function getMockScenario(): MockScenario { return state.scenario }
export function nextId(prefix: string): string { state.sequence += 1; return `${prefix}-${state.sequence}` }
