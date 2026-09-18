// Admin users feature API: typed projections and fetch helpers for the T05
// surface (users, sessions, audit events). Errors surface the frozen
// problem+json message field.
//
// Unique-admin model (docs/authentication-design.md §1): creation always
// produces an operator (the request omits `role`; the server forces it), the
// admin assigns receive targets (email/sms), and admins can never be disabled
// here. These are local DTOs: the generated contract does not yet carry
// `initialized` or the contacts command, so the wire shapes live here until
// main regenerates the shared types.

export interface AdminContactInput {
  channel: 'email' | 'sms'
  target: string
}

export interface AdminUser {
  id: string
  username: string
  displayName: string
  role: 'admin' | 'operator'
  enabled: boolean
  /** Optional until the shared generated types gain the field; absent only from stale projections. */
  initialized?: boolean
  authRevision: number
  rowVersion: number
  passwordChangeRequired: boolean
  lastLoginAt: string | null
}

export interface SessionInfo {
  id: string
  clientLabel: string
  createdAt: string
  lastActiveAt: string
  idleExpiresAt: string
  absoluteExpiresAt: string
  current: boolean
}

export interface AuditEventInfo {
  id: string
  actorType: 'user' | 'service' | 'system'
  actorId: string
  action: string
  outcome: 'success' | 'failure' | 'rejected' | 'unknown'
  clientCommandId?: string
  domainRefType?: string
  domainRefId?: string
  createdAt: string
}

export interface ResetPasswordResult {
  user: AdminUser
  revokedSessionCount: number
}

export class AdminApiError extends Error {
  constructor(message: string, readonly status: number, readonly code?: string) {
    super(message)
  }
}

async function problem(response: Response): Promise<AdminApiError> {
  let message = '暂时无法完成操作，请重试。'
  let code: string | undefined
  try {
    const body = (await response.json()) as { message?: string; detail?: string; code?: string }
    message = body.message ?? body.detail ?? message
    code = body.code
  } catch {
    // keep the ordinary-language fallback
  }
  return new AdminApiError(message, response.status, code)
}

export function newClientCommandId(): string {
  const raw = crypto.getRandomValues(new Uint8Array(18))
  return Array.from(raw, (byte) => byte.toString(16).padStart(2, '0')).join('')
}

export async function listUsers(): Promise<AdminUser[]> {
  const response = await fetch('/api/v1/admin/users?limit=100', { credentials: 'include' })
  if (!response.ok) throw await problem(response)
  const page = (await response.json()) as { items: AdminUser[] }
  return page.items ?? []
}

/** Role is deliberately absent: accounts created here are always operators. */
export async function createUser(input: {
  username: string
  displayName: string
  password: string
  contacts: AdminContactInput[]
}): Promise<AdminUser> {
  const response = await fetch('/api/v1/admin/users', {
    method: 'POST', credentials: 'include',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ ...input, clientCommandId: newClientCommandId() }),
  })
  if (!response.ok) throw await problem(response)
  return (await response.json()) as AdminUser
}

export async function updateUser(
  id: string,
  expectedRowVersion: number,
  changes: { displayName?: string; enabled?: boolean },
): Promise<AdminUser> {
  const response = await fetch(`/api/v1/admin/users/${id}`, {
    method: 'PATCH', credentials: 'include',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ ...changes, expectedRowVersion, clientCommandId: newClientCommandId() }),
  })
  if (!response.ok) throw await problem(response)
  return (await response.json()) as AdminUser
}

/** Replaces the full target set (1..2, one per channel); a replaced target
 * loses its verification and invalidates challenges bound to the old one. */
export async function configureContacts(
  id: string,
  expectedRowVersion: number,
  contacts: AdminContactInput[],
): Promise<AdminUser> {
  const response = await fetch(`/api/v1/admin/users/${id}/contacts`, {
    method: 'PUT', credentials: 'include',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ contacts, expectedRowVersion, clientCommandId: newClientCommandId() }),
  })
  if (!response.ok) throw await problem(response)
  return (await response.json()) as AdminUser
}

export async function resetPassword(id: string, expectedRowVersion: number, newPassword: string): Promise<ResetPasswordResult> {
  const response = await fetch(`/api/v1/admin/users/${id}/reset-password`, {
    method: 'POST', credentials: 'include',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ expectedRowVersion, newPassword, clientCommandId: newClientCommandId() }),
  })
  if (!response.ok) throw await problem(response)
  return (await response.json()) as ResetPasswordResult
}

export async function revokeSessions(id: string): Promise<number> {
  const response = await fetch(`/api/v1/admin/users/${id}/revoke-sessions`, {
    method: 'POST', credentials: 'include',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ clientCommandId: newClientCommandId() }),
  })
  if (!response.ok) throw await problem(response)
  const result = (await response.json()) as { revokedSessionCount: number }
  return result.revokedSessionCount
}

export async function listOwnSessions(): Promise<SessionInfo[]> {
  const response = await fetch('/api/v1/auth/sessions', { credentials: 'include' })
  if (!response.ok) throw await problem(response)
  const page = (await response.json()) as { items: SessionInfo[] }
  return page.items ?? []
}

export async function revokeOwnSession(id: string): Promise<void> {
  const response = await fetch(`/api/v1/auth/sessions/${id}/revoke`, {
    method: 'POST', credentials: 'include',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ clientCommandId: newClientCommandId() }),
  })
  if (!response.ok) throw await problem(response)
}

export async function listAuditEvents(): Promise<AuditEventInfo[]> {
  const response = await fetch('/api/v1/audit-events?limit=50', { credentials: 'include' })
  if (!response.ok) throw await problem(response)
  const page = (await response.json()) as { items: AuditEventInfo[] }
  return page.items ?? []
}

export function messageOf(reason: unknown, fallback: string): string {
  if (reason instanceof AdminApiError) return reason.message
  return reason instanceof Error ? reason.message : fallback
}

export function formatTime(timestamp: string): string {
  const date = new Date(timestamp)
  return Number.isNaN(date.getTime()) ? timestamp : date.toLocaleString()
}
