// Admin runtimes feature API (T06): typed projections for runtime slots and
// the prepare/reveal/retire flows.

export interface RuntimeSlotView {
  slot: 'plinth' | 'lintel'
  state: 'unregistered' | 'registered' | 'revoked'
  currentGeneration: number
  pendingGeneration?: number
  retiringGeneration?: number
  retirementState?: 'AwaitingFirstUse' | 'PendingRetirement'
  rowVersion: number
  connected: boolean
  bootId?: string
  connectionEpoch?: number
  lastSeenAt?: string
  currentFirstAuthenticatedAt?: string
}

export interface RuntimeStatusView {
  plinth: RuntimeSlotView
  lintel: RuntimeSlotView
}

export interface RegistrationPreparation {
  slot: 'plinth' | 'lintel'
  state: 'unregistered' | 'revoked'
  currentGeneration: number
  rowVersion: number
  registrationTokenAvailable: boolean
  registrationTokenHandle?: string
}

export interface RegistrationTokenRevealResult {
  slot: 'plinth' | 'lintel'
  generation: number
  registrationToken: string
}

export class RuntimesApiError extends Error {
  constructor(message: string, readonly status: number, readonly code?: string) {
    super(message)
  }
}

async function failure(response: Response, fallback: string): Promise<RuntimesApiError> {
  try {
    const problem = (await response.json()) as { message?: string; detail?: string; code?: string }
    return new RuntimesApiError(problem.message ?? problem.detail ?? fallback, response.status, problem.code)
  } catch {
    return new RuntimesApiError(fallback, response.status)
  }
}

export function newClientCommandId(): string {
  const raw = crypto.getRandomValues(new Uint8Array(18))
  return Array.from(raw, (byte) => byte.toString(16).padStart(2, '0')).join('')
}

export async function fetchRuntimeStatus(): Promise<RuntimeStatusView> {
  const response = await fetch('/api/v1/runtime', { credentials: 'include' })
  if (!response.ok) throw await failure(response, '暂时无法读取组件状态。')
  return (await response.json()) as RuntimeStatusView
}

export async function prepareRegistration(slot: 'plinth' | 'lintel', expectedRowVersion: number): Promise<RegistrationPreparation> {
  const response = await fetch(`/api/v1/runtime-slots/${slot}/registration/prepare`, {
    method: 'POST', credentials: 'include',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ clientCommandId: newClientCommandId(), expectedRowVersion }),
  })
  if (!response.ok) throw await failure(response, '没有完成注册准备，请刷新后重试。')
  return (await response.json()) as RegistrationPreparation
}

export async function revealRegistrationToken(handle: string): Promise<RegistrationTokenRevealResult> {
  const response = await fetch('/api/v1/runtime-slots/registration-token/reveal', {
    method: 'POST', credentials: 'include',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ registrationTokenHandle: handle }),
  })
  if (!response.ok) throw await failure(response, '注册令牌已失效或已消费，请重新准备注册。')
  return (await response.json()) as RegistrationTokenRevealResult
}

export async function retireRuntimeCredential(slot: 'plinth' | 'lintel', expectedRowVersion: number): Promise<RuntimeSlotView> {
  const response = await fetch(`/api/v1/runtime-slots/${slot}/retiring-credential/retire`, {
    method: 'POST', credentials: 'include',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ clientCommandId: newClientCommandId(), expectedRowVersion }),
  })
  if (!response.ok) throw await failure(response, '没有完成退休，请刷新后重试。')
  return (await response.json()) as RuntimeSlotView
}

export function formatRuntimeTime(timestamp: string | undefined): string {
  if (!timestamp) return '—'
  const date = new Date(timestamp)
  return Number.isNaN(date.getTime()) ? timestamp : date.toLocaleString()
}
