// Business-systems feature API (T16): typed projections and fetch helpers
// for the config surface. Failures surface the frozen problem+json message;
// validation failures keep the complete fieldErrors list so the upload form
// can render per-path recovery (UI-SYSTEM-004).

export interface FieldError {
  path: string
  reason: string
  remediation?: string
}

export class ConfigApiError extends Error {
  constructor(
    message: string,
    readonly status: number,
    readonly code?: string,
    readonly fieldErrors: FieldError[] = [],
    readonly conflict?: { code: string; currentVersionId?: string | null },
  ) {
    super(message)
  }
}

async function problem(response: Response): Promise<ConfigApiError> {
  let message = '暂时无法完成操作，请重试。'
  let code: string | undefined
  let fieldErrors: FieldError[] = []
  let conflict: { code: string; currentVersionId?: string | null } | undefined
  try {
    const body = (await response.json()) as {
      message?: string
      code?: string
      fieldErrors?: FieldError[]
      conflict?: { code: string; currentVersionId?: string | null }
    }
    message = body.message ?? message
    code = body.code
    fieldErrors = body.fieldErrors ?? []
    conflict = body.conflict
  } catch {
    // keep the ordinary-language fallback
  }
  return new ConfigApiError(message, response.status, code, fieldErrors, conflict)
}

export function newClientCommandId(): string {
  const raw = crypto.getRandomValues(new Uint8Array(18))
  return Array.from(raw, (byte) => byte.toString(16).padStart(2, '0')).join('')
}

export interface CheckView {
  checkKey: string
  displayName: string
  analysisQuestion: string
  kind: 'promql' | 'browser'
  queryMode?: 'instant' | 'range'
  expression?: string
  rangeSeconds?: number
  stepSeconds?: number
  journeyId?: string
  journeyParams?: Record<string, unknown>
}

export interface PlanView {
  planKey: string
  displayName: string
  cron?: string
  checks: CheckView[]
}

export interface DiscoveryView {
  discoveryKey: string
  displayName: string
  selector: string
  identityLabels: string[]
}

export interface BusinessSystemSummary {
  key: string
  displayName: string
  enabled: boolean
  rowVersion: number
  currentConfigVersionId?: string
  timezone?: string | null
  resourceRefreshIntervalSeconds?: number | null
  browserIdentityState: 'Ready' | 'AuthenticationRequired' | 'none'
}

export interface BusinessSystemDetail extends BusinessSystemSummary {
  configVersionCount: number
  discoveries: DiscoveryView[]
  plans: PlanView[]
}

export type ResourceRefreshState = 'Queued' | 'Running' | 'Completed' | 'CompletedWithWarnings' | 'Failed' | 'Cancelled' | 'Interrupted'
export interface ResourceRefreshRunDetail {
  id: string
  businessSystemId: string
  configVersionId: string
  labelContractVersionId: string
  triggerKind: 'manual' | 'schedule'
  state: ResourceRefreshState
  rowVersion: number
  evidenceAt?: string
  resultDetail?: string
  createdAt: string
}
export interface ObservedResourceSummary {
  id: string
  discoveryKey: string
  identityLabels: Record<string, string>
  observedAt?: string
  current: boolean
  stale: boolean
  lastSuccessfulRefreshAt?: string
}

export interface ConfigVersionSummary {
  id: string
  versionSeq: number
  state: 'draft' | 'published' | 'superseded'
  createdAt: string
  publishedAt?: string
  digest: string
  parserVersion: string
  schemaVersion: string
  systemKey: string
  displayName: string
  enabled: boolean
  labelContractVersionId: string
  journeyCatalogDigest: string
  journeyCatalogVersion: string
}

export interface ConfigVersionDetail extends ConfigVersionSummary {
  yamlBody: string
  timezone: string
  resourceRefreshIntervalSeconds: number
  discoveries: DiscoveryView[]
  plans: PlanView[]
}

export interface LabelContractSummary {
  id: string
  version: number
  state: 'draft' | 'active' | 'retired'
  rowVersion: number
  parserVersion: string
  schemaVersion: string
  createdAt: string
  activatedAt?: string
}

export interface JourneyCatalogView {
  version: string
  digest: string
  catalogJson: Record<string, unknown>
}

export async function listBusinessSystems(): Promise<BusinessSystemSummary[]> {
  const response = await fetch('/api/v1/business-systems?limit=100', { credentials: 'include' })
  if (!response.ok) throw await problem(response)
  const page = (await response.json()) as { items?: BusinessSystemSummary[] }
  return page.items ?? []
}

export async function getBusinessSystem(key: string): Promise<BusinessSystemDetail> {
  const response = await fetch(`/api/v1/business-systems/${encodeURIComponent(key)}`, { credentials: 'include' })
  if (!response.ok) throw await problem(response)
  return (await response.json()) as BusinessSystemDetail
}

export async function listObservedResources(key: string): Promise<ObservedResourceSummary[]> {
  const response = await fetch(`/api/v1/business-systems/${encodeURIComponent(key)}/resources?current=true&limit=100`, { credentials: 'include' })
  if (!response.ok) throw await problem(response)
  return ((await response.json()) as { items?: ObservedResourceSummary[] }).items ?? []
}

export async function getResourceRefreshRun(key: string, runId: string): Promise<ResourceRefreshRunDetail> {
  const response = await fetch(`/api/v1/business-systems/${encodeURIComponent(key)}/resource-refresh-runs/${encodeURIComponent(runId)}`, { credentials: 'include' })
  if (!response.ok) throw await problem(response)
  return (await response.json()) as ResourceRefreshRunDetail
}

export async function startResourceRefresh(key: string): Promise<ResourceRefreshRunDetail> {
  const response = await fetch(`/api/v1/business-systems/${encodeURIComponent(key)}/resources:refresh`, { method: 'POST', credentials: 'include', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ clientCommandId: newClientCommandId() }) })
  if (!response.ok) throw await problem(response)
  return (await response.json()) as ResourceRefreshRunDetail
}

export async function listConfigVersions(key: string): Promise<ConfigVersionSummary[]> {
  const response = await fetch(`/api/v1/business-systems/${encodeURIComponent(key)}/config?limit=100`, { credentials: 'include' })
  if (!response.ok) throw await problem(response)
  const page = (await response.json()) as { items?: ConfigVersionSummary[] }
  return page.items ?? []
}

export async function getConfigVersion(key: string, versionId: string): Promise<ConfigVersionDetail> {
  const response = await fetch(`/api/v1/business-systems/${encodeURIComponent(key)}/config/${versionId}`, { credentials: 'include' })
  if (!response.ok) throw await problem(response)
  return (await response.json()) as ConfigVersionDetail
}

export async function uploadBusinessSystemConfig(input: {
  file: File
  targetLabelContractVersion: number
  journeyCatalogDigest?: string
}): Promise<ConfigVersionDetail> {
  const form = new FormData()
  form.append('file', input.file, input.file.name)
  form.append('clientCommandId', newClientCommandId())
  form.append('targetLabelContractVersion', String(input.targetLabelContractVersion))
  if (input.journeyCatalogDigest) form.append('journeyCatalogDigest', input.journeyCatalogDigest)
  const response = await fetch('/api/v1/business-systems', { method: 'POST', credentials: 'include', body: form })
  if (!response.ok) throw await problem(response)
  return (await response.json()) as ConfigVersionDetail
}

export async function publishBusinessSystemConfig(
  key: string,
  versionId: string,
  expectedCurrentPublishedVersionId: string | null,
): Promise<BusinessSystemDetail> {
  const response = await fetch(`/api/v1/business-systems/${encodeURIComponent(key)}/config/${versionId}/publish`, {
    method: 'POST',
    credentials: 'include',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ clientCommandId: newClientCommandId(), expectedCurrentPublishedVersionId }),
  })
  if (!response.ok) throw await problem(response)
  return (await response.json()) as BusinessSystemDetail
}

export async function listLabelContracts(): Promise<LabelContractSummary[]> {
  const response = await fetch('/api/v1/label-contracts?limit=100', { credentials: 'include' })
  if (!response.ok) throw await problem(response)
  const page = (await response.json()) as { items?: LabelContractSummary[] }
  return page.items ?? []
}

export interface VerificationCheckResultView {
  planKey: string
  checkKey: string
  status: 'ok' | 'error' | 'gap'
  evidenceId?: string
  gapReason?: string
}

export type VerificationRunState = 'Queued' | 'Running' | 'Passed' | 'Failed' | 'Cancelled' | 'Interrupted'

export interface VerificationRunSummary {
  id: string
  purpose: 'prepublish' | 'deployment_acceptance'
  configVersionId: string
  labelContractVersionId: string
  state: VerificationRunState
  rowVersion: number
  evidenceAt?: string
  createdAt: string
}

export interface VerificationRunDetail extends VerificationRunSummary {
  checkResults: VerificationCheckResultView[]
  resultDetail?: string
}

export interface ReadinessCandidate {
  configVersionId: string
  passedVerificationRunId: string
}

export type ReadinessBlocker =
  | 'no_compatible_version'
  | 'verification_run_missing'
  | 'verification_run_pending'
  | 'verification_run_failed'
  | 'verification_run_cancelled'
  | 'verification_run_interrupted'

export interface ReadinessSystem {
  businessSystemKey: string
  currentConfigVersionId?: string | null
  businessSystemRowVersion: number
  activationCandidates: ReadinessCandidate[]
  blockers: ReadinessBlocker[]
}

export interface LabelContractReadiness {
  targetContractVersion: number
  stateRowVersion: number
  targetRowVersion: number
  currentContractVersionId?: string | null
  systems: ReadinessSystem[]
}

export const readinessBlockerText: Record<ReadinessBlocker, string> = {
  no_compatible_version: '没有面向该契约的未发布草稿版本',
  verification_run_missing: '草稿还没有运行过 Config Verification Run',
  verification_run_pending: '验证 Run 还在进行中',
  verification_run_failed: '最新的验证 Run 失败了',
  verification_run_cancelled: '最新的验证 Run 被取消了',
  verification_run_interrupted: '最新的验证 Run 被中断了',
}

export const verificationStateText: Record<VerificationRunState, string> = {
  Queued: '已排队',
  Running: '正在验证',
  Passed: '已通过',
  Failed: '失败',
  Cancelled: '已取消',
  Interrupted: '已中断',
}

export async function fetchLabelContractReadiness(version: number, signal?: AbortSignal): Promise<LabelContractReadiness> {
  const response = await fetch(`/api/v1/label-contracts/${version}/readiness`, { credentials: 'include', signal })
  if (!response.ok) throw await problem(response)
  return (await response.json()) as LabelContractReadiness
}

export async function listVerificationRuns(key: string, versionId: string): Promise<VerificationRunSummary[]> {
  const runs: VerificationRunSummary[] = []
  let cursor = ''
  do {
    const params = new URLSearchParams({ limit: '100' })
    if (cursor) params.set('cursor', cursor)
    const response = await fetch(
      `/api/v1/business-systems/${encodeURIComponent(key)}/config/${versionId}/verifications?${params.toString()}`,
      { credentials: 'include' },
    )
    if (!response.ok) throw await problem(response)
    const page = (await response.json()) as { items?: VerificationRunSummary[]; nextCursor?: string }
    runs.push(...(page.items ?? []))
    cursor = page.nextCursor ?? ''
  } while (cursor)
  return runs
}

export async function runVerification(key: string, versionId: string): Promise<VerificationRunDetail> {
  const response = await fetch(
    `/api/v1/business-systems/${encodeURIComponent(key)}/config/${versionId}/verifications`,
    {
      method: 'POST',
      credentials: 'include',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ clientCommandId: newClientCommandId(), purpose: 'prepublish' }),
    },
  )
  if (!response.ok) throw await problem(response)
  return (await response.json()) as VerificationRunDetail
}

export async function cancelVerification(
  key: string,
  versionId: string,
  runId: string,
  expectedRowVersion: number,
): Promise<VerificationRunDetail> {
  const response = await fetch(
    `/api/v1/business-systems/${encodeURIComponent(key)}/config/${versionId}/verifications/${runId}/cancel`,
    {
      method: 'POST',
      credentials: 'include',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ clientCommandId: newClientCommandId(), expectedRowVersion }),
    },
  )
  if (!response.ok) throw await problem(response)
  return (await response.json()) as VerificationRunDetail
}

export interface ActivationItemInput {
  businessSystemKey: string
  configVersionId: string
  verificationRunId: string
  expectedCurrentConfigVersionId: string | null
  expectedBusinessSystemRowVersion: number
}

export async function activateLabelContract(
  version: number,
  input: {
    expectedStateRowVersion: number
    expectedCurrentContractVersionId: string | null
    expectedTargetRowVersion: number
    compatibleVersions: ActivationItemInput[]
  },
): Promise<LabelContractSummary & { yamlBody?: string }> {
  const response = await fetch(`/api/v1/label-contracts/${version}/activate`, {
    method: 'POST',
    credentials: 'include',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ clientCommandId: newClientCommandId(), ...input }),
  })
  if (!response.ok) throw await problem(response)
  return (await response.json()) as LabelContractSummary & { yamlBody?: string }
}

export async function getJourneyCatalog(): Promise<JourneyCatalogView> {
  const response = await fetch('/api/v1/journey-catalog', { credentials: 'include' })
  if (!response.ok) throw await problem(response)
  return (await response.json()) as JourneyCatalogView
}

export function formatTime(timestamp: string): string {
  const date = new Date(timestamp)
  return Number.isNaN(date.getTime()) ? timestamp : date.toLocaleString()
}
