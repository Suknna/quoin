// Admin connections feature API (T07): typed projections for connections,
// probe attempts and immutable probe results.

export interface ConnectionSummaryView {
  name: string
  type: 'thanos' | 'kubernetes' | 'model_provider'
  enabled: boolean
  revalidationRequired: boolean
  currentRevisionId?: string
  currentCredentialGenerationId?: string
  rowVersion: number
  config: Record<string, unknown>
}

export interface ConnectionDetailView extends ConnectionSummaryView {
  revisionCount: number
  generationCount: number
  activeProbeAttempt?: ProbeAttemptView
}

export interface ProbeAttemptView {
  id: string
  type: 'connection_probe'
  state: 'Queued' | 'Assigned' | 'Running' | 'Cancelling' | 'Succeeded' | 'Failed' | 'Cancelled' | 'Interrupted'
  rowVersion: number
  createdAt: string
  startedAt?: string
  endedAt?: string
  terminationReason?: string
}

export interface ProbeResultView {
  id: string
  attemptId: string
  connectionType: 'thanos' | 'kubernetes' | 'model_provider'
  outcome: 'passed' | 'failed' | 'cancelled' | 'interrupted'
  actionSetId: string
  actionSetVersion: number
  resultDigest: string
  startedAt: string
  finishedAt: string
  details: Record<string, unknown>
}

export class ConnectionsApiError extends Error {
  constructor(message: string, readonly status: number, readonly code?: string) {
    super(message)
  }
}

async function failure(response: Response, fallback: string): Promise<ConnectionsApiError> {
  try {
    const problem = (await response.json()) as { message?: string; detail?: string; code?: string }
    return new ConnectionsApiError(problem.message ?? problem.detail ?? fallback, response.status, problem.code)
  } catch {
    return new ConnectionsApiError(fallback, response.status)
  }
}

export function newClientCommandId(): string {
  const raw = crypto.getRandomValues(new Uint8Array(18))
  return Array.from(raw, (byte) => byte.toString(16).padStart(2, '0')).join('')
}

export async function listConnections(): Promise<ConnectionSummaryView[]> {
  const response = await fetch('/api/v1/connections?limit=100', { credentials: 'include' })
  if (!response.ok) {
    throw await failure(response, '暂时无法读取连接列表。')
  }
  const body = (await response.json()) as { items?: ConnectionSummaryView[] }
  return body.items ?? []
}

export async function fetchConnection(name: string): Promise<ConnectionDetailView> {
  const response = await fetch(`/api/v1/connections/${encodeURIComponent(name)}`, { credentials: 'include' })
  if (!response.ok) {
    throw await failure(response, '暂时无法读取连接详情。')
  }
  return (await response.json()) as ConnectionDetailView
}

export interface CreateConnectionInput {
  type: 'thanos' | 'kubernetes'
  baseUrl?: string
  username?: string
  password?: string
  contextName?: string
  defaultNamespace?: string
  kubeconfig?: string
}

export async function createConnection(name: string, connection: CreateConnectionInput): Promise<ConnectionSummaryView> {
  const response = await fetch('/api/v1/connections', {
    method: 'POST',
    credentials: 'include',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ clientCommandId: newClientCommandId(), name, connection }),
  })
  if (!response.ok) {
    throw await failure(response, '暂时无法创建连接。')
  }
  return (await response.json()) as ConnectionSummaryView
}

export async function probeConnection(name: string): Promise<ProbeAttemptView> {
  const response = await fetch(`/api/v1/connections/${encodeURIComponent(name)}/probe`, {
    method: 'POST',
    credentials: 'include',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ clientCommandId: newClientCommandId() }),
  })
  if (!response.ok) {
    throw await failure(response, '暂时无法发起探测。')
  }
  const body = (await response.json()) as { id: string; state: ProbeAttemptView['state'] }
  return { id: body.id, type: 'connection_probe', state: body.state, rowVersion: 1, createdAt: new Date().toISOString() }
}

export async function fetchProbeAttempt(name: string, attemptId: string): Promise<ProbeAttemptView> {
  const response = await fetch(`/api/v1/connections/${encodeURIComponent(name)}/probe-attempts/${encodeURIComponent(attemptId)}`, { credentials: 'include' })
  if (!response.ok) {
    throw await failure(response, '暂时无法读取探测任务。')
  }
  return (await response.json()) as ProbeAttemptView
}

export async function cancelProbeAttempt(name: string, attemptId: string, expectedRowVersion: number): Promise<ProbeAttemptView> {
  const response = await fetch(`/api/v1/connections/${encodeURIComponent(name)}/probe-attempts/${encodeURIComponent(attemptId)}/cancel`, {
    method: 'POST',
    credentials: 'include',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ clientCommandId: newClientCommandId(), expectedRowVersion }),
  })
  if (!response.ok) {
    throw await failure(response, '暂时无法取消探测任务。')
  }
  return (await response.json()) as ProbeAttemptView
}

export async function listProbeResults(name: string): Promise<ProbeResultView[]> {
  const response = await fetch(`/api/v1/connections/${encodeURIComponent(name)}/probe-results?limit=50`, { credentials: 'include' })
  if (!response.ok) {
    throw await failure(response, '暂时无法读取探测结果。')
  }
  const body = (await response.json()) as { items?: ProbeResultView[] }
  return body.items ?? []
}

export async function enableConnection(name: string, expectedRowVersion: number, qualifiedProbeResultId?: string): Promise<ConnectionSummaryView> {
  const response = await fetch(`/api/v1/connections/${encodeURIComponent(name)}/enable`, {
    method: 'POST',
    credentials: 'include',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ clientCommandId: newClientCommandId(), expectedRowVersion, qualifiedProbeResultId }),
  })
  if (!response.ok) {
    throw await failure(response, '暂时无法启用连接。')
  }
  return (await response.json()) as ConnectionSummaryView
}

export async function disableConnection(name: string, expectedRowVersion: number): Promise<ConnectionSummaryView> {
  const response = await fetch(`/api/v1/connections/${encodeURIComponent(name)}/disable`, {
    method: 'POST',
    credentials: 'include',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ clientCommandId: newClientCommandId(), expectedRowVersion }),
  })
  if (!response.ok) {
    throw await failure(response, '暂时无法停用连接。')
  }
  return (await response.json()) as ConnectionSummaryView
}
