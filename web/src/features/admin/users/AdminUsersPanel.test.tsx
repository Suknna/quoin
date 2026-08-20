import '@testing-library/jest-dom/vitest'
import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { AdminUsersPanel } from './AdminUsersPanel'

const currentAdmin = {
  id: '1', username: 'admin', displayName: 'E2E Admin', role: 'admin' as const, enabled: true,
  authRevision: 2, rowVersion: 3, passwordChangeRequired: false, lastLoginAt: '2026-08-19T10:00:00Z',
}

const operator = {
  id: '2', username: 'op1', displayName: 'Operator One', role: 'operator' as const, enabled: true,
  authRevision: 1, rowVersion: 1, passwordChangeRequired: false, lastLoginAt: null,
}

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })
}

describe('AdminUsersPanel', () => {
  beforeEach(() => {
    vi.stubGlobal('fetch', vi.fn())
  })

  it('lists users and creates a new account through the real form flow', async () => {
    const fetchMock = vi.mocked(fetch)
    fetchMock.mockImplementation(async (input: RequestInfo | URL, init?: RequestInit) => {
      const path = String(input)
      const method = init?.method ?? (init?.body ? 'POST' : 'GET')
      if (path.startsWith('/api/v1/admin/users') && method === 'GET') return jsonResponse({ items: [currentAdmin, operator] })
      if (path === '/api/v1/admin/users') return jsonResponse({ ...operator, id: '3', username: 'op2' }, 201)
      if (path.startsWith('/api/v1/auth/sessions')) return jsonResponse({ items: [] })
      if (path.startsWith('/api/v1/audit-events')) return jsonResponse({ items: [] })
      return jsonResponse({}, 404)
    })
    render(<AdminUsersPanel currentUser={currentAdmin} />)
    expect(await screen.findByText(/Operator One/)).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: '+ 创建用户' }))
    fireEvent.change(screen.getByLabelText('登录名'), { target: { value: 'op2' } })
    fireEvent.change(screen.getByLabelText('显示名'), { target: { value: 'Operator Two' } })
    fireEvent.change(screen.getByLabelText('初始密码'), { target: { value: 'Operator two passphrase 2026!' } })
    fireEvent.click(screen.getByRole('button', { name: '创建' }))
    await waitFor(() => expect(screen.getByText('已创建 @op2。')).toBeInTheDocument())
    const createCall = fetchMock.mock.calls.find((call) => String(call[0]) === '/api/v1/admin/users')
    expect(createCall).toBeDefined()
    const body = JSON.parse(String(createCall?.[1]?.body)) as { clientCommandId?: string }
    expect(body.clientCommandId).toMatch(/^[A-Za-z0-9_-]{8,128}$/)
  })

  it('shows the last-admin conflict as an ordinary-language failure', async () => {
    const fetchMock = vi.mocked(fetch)
    fetchMock.mockImplementation(async (input: RequestInfo | URL) => {
      const path = String(input)
      if (path.startsWith('/api/v1/admin/users?')) return jsonResponse({ items: [currentAdmin] })
      if (path.startsWith('/api/v1/admin/users/') && path.endsWith('/revoke-sessions')) return jsonResponse({ revokedSessionCount: 0 })
      if (/^\/api\/v1\/admin\/users\/\d+$/.test(path)) {
        return jsonResponse({ code: 'active_conflict', message: '系统必须保留至少一个有效的管理员；请先创建或启用另一个管理员。', retryable: false }, 409)
      }
      if (path.startsWith('/api/v1/auth/sessions')) return jsonResponse({ items: [] })
      if (path.startsWith('/api/v1/audit-events')) return jsonResponse({ items: [] })
      return jsonResponse({}, 404)
    })
    render(<AdminUsersPanel currentUser={currentAdmin} />)
    const row = await screen.findByText(/E2E Admin/)
    fireEvent.click(row)
    fireEvent.click(screen.getByLabelText('启用该账号（当前登录账号）'))
    fireEvent.click(screen.getByRole('button', { name: '保存修改' }))
    await waitFor(() => expect(screen.getByText('系统必须保留至少一个有效的管理员；请先创建或启用另一个管理员。')).toBeInTheDocument())
  })
})
