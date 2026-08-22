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

export async function getJourneyCatalog(): Promise<JourneyCatalogView> {
  const response = await fetch('/api/v1/journey-catalog', { credentials: 'include' })
  if (!response.ok) throw await problem(response)
  return (await response.json()) as JourneyCatalogView
}

export function formatTime(timestamp: string): string {
  const date = new Date(timestamp)
  return Number.isNaN(date.getTime()) ? timestamp : date.toLocaleString()
}
