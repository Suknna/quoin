import { newClientCommandId, type BusinessSystemDetail, type BusinessSystemSummary, getBusinessSystem, listBusinessSystems } from '../admin/business-systems/api'

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

export type InspectionRunState = 'Queued' | 'Running' | 'Completed' | 'CompletedWithGaps' | 'Failed' | 'Cancelled' | 'Interrupted' | 'SkippedOverlap'

export interface InspectionRunSummary {
  id: string
  businessSystemKey: string
  planKey: string
  state: InspectionRunState
  rowVersion: number
  triggerKind: 'schedule' | 'manual'
  scheduledFor?: string
  evidenceAt?: string
  createdAt: string
}

/** Frozen CheckResultSummary union: ok 携带 Evidence locator；error/gap 携带缺口原因。 */
export type InspectionCheckResult =
  | { checkKey: string; status: 'ok'; evidenceId: string }
  | { checkKey: string; status: 'error' | 'gap'; gapReason: string }
  | { checkKey: string; status: 'cancelling' }

export interface InspectionRunDetail extends InspectionRunSummary {
  checks: InspectionCheckResult[]
  reportCount: number
  analysisActive: boolean
  latestAnalysis?: InspectionAnalysisStatus
}

/** Safe lifecycle projection for the latest immutable report-analysis Attempt. */
export interface InspectionAnalysisStatus {
  id: string
  state: 'Queued' | 'Assigned' | 'Running' | 'Cancelling' | 'Succeeded' | 'Failed' | 'Cancelled' | 'Interrupted'
  terminationReason?: string
}

export interface InspectionAttemptSummary {
  id: string
  type: 'inspection_analysis' | 'inspection_collection'
  state: 'Queued' | 'Assigned' | 'Running' | 'Cancelling' | 'Succeeded' | 'Failed' | 'Cancelled' | 'Interrupted'
  rowVersion: number
  createdAt: string
}

export interface InspectionReportSummary {
  version: number
  modelId: string
  createdAt: string
}

export interface InspectionReportDetail {
  runId: string
  version: number
  evidenceDigest: string
  evidenceIds: string[]
  modelId: string
  content: string
  createdAt: string
}

export { getBusinessSystem, listBusinessSystems }
export type { BusinessSystemDetail, BusinessSystemSummary }

export async function listInspectionRuns(businessSystemKey: string): Promise<InspectionRunSummary[]> {
  const query = new URLSearchParams({ businessSystemKey, limit: '100' })
  const runs: InspectionRunSummary[] = []
  let cursor = ''
  do {
    if (cursor) query.set('cursor', cursor)
    const response = await fetch(`/api/v1/inspections/runs?${query.toString()}`, { credentials: 'include' })
    if (!response.ok) throw await failure(response)
    const page = (await response.json()) as { items?: InspectionRunSummary[]; nextCursor?: string }
    runs.push(...(page.items ?? []))
    cursor = page.nextCursor ?? ''
  } while (cursor)
  return runs
}

export async function createInspectionRun(businessSystemKey: string, planKey: string): Promise<InspectionRunDetail> {
  const response = await fetch('/api/v1/inspections/runs', {
    method: 'POST', credentials: 'include', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ businessSystemKey, planKey, clientCommandId: newClientCommandId() }),
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
export async function reanalyzeInspectionRun(runId: string): Promise<InspectionAttemptSummary> {
  const response = await fetch(`/api/v1/inspections/runs/${encodeURIComponent(runId)}/analyze`, {
    method: 'POST', credentials: 'include', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ clientCommandId: newClientCommandId() }),
  })
  if (!response.ok) throw await failure(response)
  return (await response.json()) as InspectionAttemptSummary
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
