export interface AlertOccurrenceSummary {
  id: string
  state: 'Firing' | 'Resolved'
  rowVersion: number
  businessSystemKey?: string
  firstSeenAt: string
  lastStateChangeAt: string
  resolvedAt?: string
  labels: Record<string, string>
  annotations?: Record<string, string>
}

export interface AlertSnapshot {
  snapshotSeq: number
  items: AlertOccurrenceSummary[]
}

export interface ObservationSummary {
  id: string
  observedState: 'firing' | 'resolved'
  startsAt: string
  endsAt?: string
  receivedAt: string
  committedAt: string
  effect: 'initial_firing' | 'repeat_firing' | 'resolved' | 'resolved_first' | 'late_firing_after_resolved'
}

export interface AlertSourceSummary {
  key: string
  protocol: 'alertmanager'
  enabled: boolean
  rowVersion: number
  createdAt: string
  disabledAt?: string | null
}

export interface AlertSourceDetail extends AlertSourceSummary {
  credentialCount: number
}

export interface AlertSourceCredentialMetadata {
  sourceKey: string
  credentialId: string
  revealAvailable: boolean
  revealHandle?: string
}

export interface RevealCredentialResult {
  credentialId: string
  bearerToken: string
}

export interface CreateAlertSourceRequest {
  key: string
  protocol: 'alertmanager'
  clientCommandId: string
}

export interface IntakeIssue {
  id: string
  kind: 'identity_conflict' | 'fingerprint_mismatch' | 'delivery_truncated'
  issueKey: string
  detailJson: string
  firstSeenAt: string
  lastSeenAt: string
  occurrenceCount: number
  rowVersion: number
}

export async function fetchAlerts(state: 'Firing' | 'Resolved' = 'Firing'): Promise<AlertSnapshot> {
  const response = await fetch(`/api/v1/alerts?state=${state}`, { credentials: 'include' })
  if (!response.ok) throw new Error('告警列表加载失败')
  return (await response.json()) as AlertSnapshot
}

export async function fetchOccurrence(id: string): Promise<AlertOccurrenceSummary> {
  const response = await fetch(`/api/v1/alerts/${id}`, { credentials: 'include' })
  if (!response.ok) throw new Error('告警详情加载失败')
  return (await response.json()) as AlertOccurrenceSummary
}

export async function fetchObservations(id: string): Promise<{ items: ObservationSummary[] }> {
  const response = await fetch(`/api/v1/alerts/${id}/observations`, { credentials: 'include' })
  if (!response.ok) throw new Error('观测加载失败')
  return (await response.json()) as { items: ObservationSummary[] }
}

export async function fetchIntakeIssues(): Promise<{ items: IntakeIssue[] }> {
  const response = await fetch('/api/v1/alert-intake-issues', { credentials: 'include' })
  if (!response.ok) throw new Error('接入问题加载失败')
  return (await response.json()) as { items: IntakeIssue[] }
}

export async function createAlertSource(request: CreateAlertSourceRequest): Promise<AlertSourceCredentialMetadata> {
  const response = await fetch('/api/v1/alert-sources', {
    method: 'POST',
    credentials: 'include',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(request),
  })
  if (!response.ok) {
    const problem = (await response.json().catch(() => null)) as { detail?: string } | null
    throw new Error(problem?.detail ?? '告警源创建失败')
  }
  return (await response.json()) as AlertSourceCredentialMetadata
}

export async function revealCredential(handle: string): Promise<RevealCredentialResult> {
  const response = await fetch('/api/v1/alert-sources/credentials/reveal', {
    method: 'POST',
    credentials: 'include',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ revealHandle: handle }),
  })
  if (!response.ok) {
    const problem = (await response.json().catch(() => null)) as { detail?: string } | null
    throw new Error(problem?.detail ?? '凭据显示失败')
  }
  return (await response.json()) as RevealCredentialResult
}

export function newClientCommandId(): string {
  const raw = crypto.getRandomValues(new Uint8Array(18))
  return Array.from(raw, (byte) => byte.toString(16).padStart(2, '0')).join('')
}

export async function acknowledgeIntakeIssue(id: string, expectedRowVersion: number): Promise<void> {
  const response = await fetch(`/api/v1/alert-intake-issues/${id}/acknowledge`, {
    method: 'POST',
    credentials: 'include',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ clientCommandId: newClientCommandId(), expectedRowVersion }),
  })
  if (!response.ok) {
    const problem = (await response.json().catch(() => null)) as { detail?: string } | null
    throw new ApiLikeError(problem?.detail ?? '无法确认接入问题', response.status)
  }
}

export class ApiLikeError extends Error {
  constructor(message: string, readonly status: number) {
    super(message)
  }
}
