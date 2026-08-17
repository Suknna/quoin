import type {
  LoginRequest,
  PasswordChangeRequest,
  RuntimeStatus,
  UserSummary,
} from '../api/generated/types'

export class ApiError extends Error {
  constructor(
    readonly status: number,
    message: string,
  ) {
    super(message)
  }
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const response = await fetch(path, {
    credentials: 'include',
    headers: init?.body ? { 'Content-Type': 'application/json' } : undefined,
    ...init,
  })
  if (!response.ok) {
    let message = '暂时无法完成操作，请重试。'
    try {
      const problem = (await response.json()) as { detail?: string }
      if (problem.detail) message = problem.detail
    } catch {
      // The ordinary-language fallback remains authoritative for the UI.
    }
    throw new ApiError(response.status, message)
  }
  if (response.status === 204) return undefined as T
  return (await response.json()) as T
}

export const api = {
  currentUser: () => request<UserSummary>('/api/v1/auth/me'),
  login: (body: LoginRequest) =>
    request<UserSummary>('/api/v1/auth/login', {
      method: 'POST',
      body: JSON.stringify(body),
    }),
  changePassword: (body: PasswordChangeRequest) =>
    request<void>('/api/v1/auth/password', {
      method: 'PUT',
      body: JSON.stringify(body),
    }),
  logout: () => request<void>('/api/v1/auth/logout', { method: 'POST' }),
  runtime: () => request<RuntimeStatus>('/api/v1/runtime'),
}
