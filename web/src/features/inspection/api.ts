// Inspection feature API: standalone integration-scoped plans and their runs.
// Plans no longer hang off a BusinessSystem declaration — a plan selects its
// range via scope (whole integration, a business view, or explicit objects).
// Wire shapes come from the generated OpenAPI authority; failures surface the
// server's ordinary-language message (problem+json).
import type {
  AttemptSummary,
  CheckResultSummary,
  InspectionAnalysisStatus,
  InspectionRunDetail,
  InspectionRunSummary,
  PluginInspectionPlan,
  PluginInspectionPlanInput,
  PluginInspectionScope,
  ReportDetail,
  ReportSummary,
} from "@/api/generated/types";

export class InspectionApiError extends Error {
  constructor(message: string, readonly status: number, readonly code?: string) {
    super(message)
  }
}

async function failure(response: Response): Promise<InspectionApiError> {
  let message = '暂时无法完成巡检操作，请重试。'
  let code: string | undefined
  try {
    const body = (await response.json()) as { message?: string; code?: string }
    message = body.message ?? message
    code = body.code
  } catch {
    // The ordinary-language fallback remains useful for a non-JSON failure.
  }
  return new InspectionApiError(message, response.status, code)
}

/** Idempotency marker carried by every mutating inspection command. */
export function newClientCommandId(): string {
  const raw = crypto.getRandomValues(new Uint8Array(18))
  return Array.from(raw, (byte) => byte.toString(16).padStart(2, '0')).join('')
}

export type InspectionRunState = InspectionRunSummary['state']
export type InspectionCheckResult = CheckResultSummary
export type InspectionAnalysisAttempt = AttemptSummary

/**
 * Plan scope resolved at run creation: the whole integration, one versioned
 * business view, or an explicit object list ({objectType, identityKey}).
 */
export type InspectionPlanScope = PluginInspectionScope
export type InspectionPlan = PluginInspectionPlan

/** Editable plan projection sent on create; server owns rowVersion/createdAt/updatedAt. */
export type InspectionPlanInput = Omit<PluginInspectionPlanInput, 'clientCommandId' | 'expectedRowVersion'>
/** Full editable projection plus the optimistic-concurrency marker for PUT. */
export type InspectionPlanUpdate = Omit<PluginInspectionPlanInput, 'clientCommandId' | 'planKey'> & { expectedRowVersion: number }

/**
 * The server includes `id`, the immutable report locator consumed by diagnosis
 * feedback, on top of the human-facing (runId, version) route locator.
 */
export type InspectionReportDetail = ReportDetail & { id: string }
export type InspectionReportSummary = ReportSummary
export type { InspectionAnalysisStatus, InspectionRunDetail, InspectionRunSummary }

/** Lists the first server page of plans; callers filter client-side (e.g. by connectionName). */
export async function listInspectionPlans(): Promise<InspectionPlan[]> {
  const response = await fetch('/api/v1/inspections/plans?limit=100', { credentials: 'include' })
  if (!response.ok) throw await failure(response)
  const page = (await response.json()) as { items?: InspectionPlan[] }
  return page.items ?? []
}

export async function getInspectionPlan(planKey: string): Promise<InspectionPlan> {
  const response = await fetch(`/api/v1/inspections/plans/${encodeURIComponent(planKey)}`, { credentials: 'include' })
  if (!response.ok) throw await failure(response)
  return (await response.json()) as InspectionPlan
}

export async function createInspectionPlan(input: InspectionPlanInput): Promise<InspectionPlan> {
  const response = await fetch('/api/v1/inspections/plans', {
    method: 'POST', credentials: 'include', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ clientCommandId: newClientCommandId(), ...input }),
  })
  if (!response.ok) throw await failure(response)
  return (await response.json()) as InspectionPlan
}

export async function updateInspectionPlan(planKey: string, update: InspectionPlanUpdate): Promise<InspectionPlan> {
  const response = await fetch(`/api/v1/inspections/plans/${encodeURIComponent(planKey)}`, {
    method: 'PUT', credentials: 'include', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ clientCommandId: newClientCommandId(), ...update }),
  })
  if (!response.ok) throw await failure(response)
  return (await response.json()) as InspectionPlan
}

/**
 * Reads one server page of runs. Filtering by plan happens server-side;
 * callers must not walk the full cursor chain defensively.
 */
export async function listInspectionRuns(options: { planKey?: string; cursor?: string; limit?: number } = {}): Promise<{ items: InspectionRunSummary[]; nextCursor?: string }> {
  const query = new URLSearchParams({ limit: String(options.limit ?? 100) })
  if (options.planKey) query.set('planKey', options.planKey)
  if (options.cursor) query.set('cursor', options.cursor)
  const response = await fetch(`/api/v1/inspections/runs?${query.toString()}`, { credentials: 'include' })
  if (!response.ok) throw await failure(response)
  const page = (await response.json()) as { items?: InspectionRunSummary[]; nextCursor?: string }
  return { items: page.items ?? [], nextCursor: page.nextCursor }
}

/** Manual trigger: only the plan identity — scope and connection are frozen server-side. */
export async function createInspectionRun(planKey: string): Promise<InspectionRunDetail> {
  const response = await fetch('/api/v1/inspections/runs', {
    method: 'POST', credentials: 'include', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ clientCommandId: newClientCommandId(), planKey }),
  })
  if (!response.ok) throw await failure(response)
  return (await response.json()) as InspectionRunDetail
}

export async function getInspectionRun(runId: string): Promise<InspectionRunDetail> {
  const response = await fetch(`/api/v1/inspections/runs/${encodeURIComponent(runId)}`, { credentials: 'include' })
  if (!response.ok) throw await failure(response)
  return (await response.json()) as InspectionRunDetail
}

export async function cancelInspectionRun(runId: string, expectedRowVersion: number): Promise<InspectionRunDetail> {
  const response = await fetch(`/api/v1/inspections/runs/${encodeURIComponent(runId)}/cancel`, {
    method: 'POST', credentials: 'include', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ clientCommandId: newClientCommandId(), expectedRowVersion }),
  })
  if (!response.ok) throw await failure(response)
  return (await response.json()) as InspectionRunDetail
}

/** Reuses this Run's immutable collected Evidence to create its next Report version. */
export async function reanalyzeInspectionRun(runId: string): Promise<InspectionAnalysisAttempt> {
  const response = await fetch(`/api/v1/inspections/runs/${encodeURIComponent(runId)}/analyze`, {
    method: 'POST', credentials: 'include', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ clientCommandId: newClientCommandId() }),
  })
  if (!response.ok) throw await failure(response)
  return (await response.json()) as InspectionAnalysisAttempt
}

/** Starts a distinct Run that recollects evidence from this Run's frozen plan. */
export async function rerunInspection(runId: string): Promise<InspectionRunDetail> {
  const response = await fetch(`/api/v1/inspections/runs/${encodeURIComponent(runId)}/rerun`, {
    method: 'POST', credentials: 'include', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ clientCommandId: newClientCommandId() }),
  })
  if (!response.ok) throw await failure(response)
  return (await response.json()) as InspectionRunDetail
}

export async function listInspectionReports(runId: string): Promise<InspectionReportSummary[]> {
  const response = await fetch(`/api/v1/inspections/runs/${encodeURIComponent(runId)}/reports?limit=100`, { credentials: 'include' })
  if (!response.ok) throw await failure(response)
  const page = (await response.json()) as { items?: InspectionReportSummary[] }
  return page.items ?? []
}

export async function getInspectionReport(runId: string, reportVersion: number): Promise<InspectionReportDetail> {
  const response = await fetch(`/api/v1/inspections/runs/${encodeURIComponent(runId)}/reports/${reportVersion}`, { credentials: 'include' })
  if (!response.ok) throw await failure(response)
  return (await response.json()) as InspectionReportDetail
}

export const inspectionStateText: Record<InspectionRunState, string> = {
  Queued: '已排队', Running: '执行中', Completed: '已完成', CompletedWithGaps: '已完成（有缺口）',
  Failed: '失败', Cancelled: '已取消', Interrupted: '已中断', SkippedOverlap: '已跳过（重叠调度）',
}
export function inspectionActive(state: InspectionRunState): boolean { return state === 'Queued' || state === 'Running' }
export const inspectionGapReasonText: Record<string, string> = {
  runtime_unavailable: '浏览器运行时不可用', authentication_required: '需要人工登录', authentication_probe_unavailable: '登录探测不可用',
  identity_busy: '浏览器身份正忙', artifact_commit_failed: '诊断材料提交失败', journey_failed: '浏览器巡检失败',
  query_failed: '指标查询失败', partial_response: '部分响应', no_data: '无数据', cancelled: '已取消', interrupted: '已中断',
}
export function formatInspectionTime(value?: string): string { return value ? (Number.isNaN(new Date(value).getTime()) ? value : new Date(value).toLocaleString()) : '—' }

export const inspectionScopeKindText: Record<InspectionPlanScope['kind'], string> = {
  integration: '整个接入', businessView: '业务视图', objects: '指定对象',
}

/** One-line human projection of a scope for lists and tables. */
export function inspectionScopeText(scope: InspectionPlanScope): string {
  if (scope.kind === 'businessView') return `${inspectionScopeKindText.businessView} ${scope.businessViewKey}`
  if (scope.kind === 'objects') return `${inspectionScopeKindText.objects}（${scope.objects.length} 个）`
  return inspectionScopeKindText.integration
}

/** Scheduling projection: cron plus timezone, or an explicit manual-only marker. */
export function inspectionScheduleText(plan: Pick<InspectionPlan, 'cron' | 'timezone'>): string {
  return plan.cron ? `${plan.cron} · ${plan.timezone}` : '仅人工运行'
}
