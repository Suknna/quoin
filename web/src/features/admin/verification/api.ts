import type {
  DeploymentVerificationReceipt,
  DeploymentVerificationSummary,
  VerificationInvocationItem,
  VerificationItemResult,
  VerificationResultConflict,
  VerificationSubjectDrift,
} from '../../../api/generated/types'

export interface DeploymentVerificationDetail extends DeploymentVerificationSummary {
  items: VerificationInvocationItem[]
  results: VerificationItemResult[]
  conflicts: VerificationResultConflict[]
  subjectDrifts: VerificationSubjectDrift[]
}

export interface DeploymentVerificationPage {
  items: DeploymentVerificationSummary[]
  nextCursor?: string
}

export interface DeploymentVerificationApiError extends Error {
  status: number
  code?: string
}

class VerificationRequestError extends Error implements DeploymentVerificationApiError {
  constructor(message: string, readonly status: number, readonly code?: string) {
    super(message)
  }
}

export function newClientCommandId(): string {
  if (typeof crypto.randomUUID === 'function') return crypto.randomUUID()
  return Array.from(crypto.getRandomValues(new Uint8Array(18)), value => value.toString(16).padStart(2, '0')).join('')
}

async function failure(response: Response, fallback: string): Promise<VerificationRequestError> {
  try {
    const body = await response.json() as { message?: string; detail?: string; code?: string }
    return new VerificationRequestError(body.message ?? body.detail ?? fallback, response.status, body.code)
  } catch {
    return new VerificationRequestError(fallback, response.status)
  }
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const response = await fetch(path, {
    credentials: 'include',
    ...init,
    headers: { 'Content-Type': 'application/json', ...(init?.headers ?? {}) },
  })
  if (!response.ok) throw await failure(response, '暂时无法完成操作，请稍后重试。')
  return response.json() as Promise<T>
}

export function fetchDeploymentVerifications(cursor?: string): Promise<DeploymentVerificationPage> {
  const query = new URLSearchParams({ limit: '30' })
  if (cursor) query.set('cursor', cursor)
  return request<DeploymentVerificationPage>(`/api/v1/deployment-verifications?${query}`)
}

export function fetchDeploymentVerification(invocationId: string): Promise<DeploymentVerificationDetail> {
  return request<DeploymentVerificationDetail>(`/api/v1/deployment-verifications/${encodeURIComponent(invocationId)}`)
}

export function startDeploymentVerification(clientCommandId = newClientCommandId()): Promise<DeploymentVerificationDetail> {
  return request<DeploymentVerificationDetail>('/api/v1/deployment-verifications', {
    method: 'POST', body: JSON.stringify({ clientCommandId }),
  })
}

export function cancelDeploymentVerification(invocationId: string, clientCommandId = newClientCommandId()): Promise<DeploymentVerificationDetail> {
  return request<DeploymentVerificationDetail>(`/api/v1/deployment-verifications/${encodeURIComponent(invocationId)}/cancel`, {
    method: 'POST', body: JSON.stringify({ clientCommandId }),
  })
}

export async function downloadHelperRequest(invocationId: string): Promise<{ body: Blob; filename: string; requestDigest?: string }> {
  const response = await fetch(`/api/v1/deployment-verifications/${encodeURIComponent(invocationId)}/helper-request`, { credentials: 'include' })
  if (!response.ok) throw await failure(response, '无法导出 helper request。')
  const disposition = response.headers.get('Content-Disposition') ?? ''
  const matched = disposition.match(/filename="([^"\r\n]+)"/)
  return {
    body: await response.blob(),
    filename: matched?.[1] ?? `deployment-verification-request-${invocationId}.yaml`,
    requestDigest: response.headers.get('X-Quoin-Request-Digest') ?? undefined,
  }
}

export async function importHelperReport(invocationId: string, body: Blob): Promise<{ detail: DeploymentVerificationDetail; created: boolean }> {
  const response = await fetch(`/api/v1/deployment-verifications/${encodeURIComponent(invocationId)}/helper-reports`, {
    method: 'POST', credentials: 'include',
    headers: { 'Content-Type': 'application/yaml' }, body,
  })
  if (!response.ok) throw await failure(response, '无法导入 helper report。')
  return { detail: await response.json() as DeploymentVerificationDetail, created: response.status === 201 }
}

export interface ObservationRequest {
  itemId: string
  inputDigest: string
  visualResult: 'passed' | 'failed'
  motionResult: 'passed' | 'failed'
  focusOcclusionResult: 'passed' | 'failed'
  note?: string
  clientCommandId?: string
}

export function submitObservation(invocationId: string, body: ObservationRequest): Promise<VerificationItemResult> {
  return request<VerificationItemResult>(`/api/v1/deployment-verifications/${encodeURIComponent(invocationId)}/observations`, {
    method: 'POST', body: JSON.stringify({ ...body, clientCommandId: body.clientCommandId ?? newClientCommandId() }),
  })
}

export function receiptOf(detail: DeploymentVerificationDetail): DeploymentVerificationReceipt | undefined {
  return detail.receipt
}
