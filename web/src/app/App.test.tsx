import '@testing-library/jest-dom/vitest'
import { act, cleanup, render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { App } from './App'

const user = {
  id: '1', username: 'admin', displayName: 'Admin', role: 'admin', enabled: true,
  authRevision: 1, rowVersion: 1, passwordChangeRequired: true, lastLoginAt: new Date().toISOString(),
}

describe('App authentication projection', () => {
  beforeEach(() => {
    vi.stubGlobal('fetch', vi.fn())
  })

  afterEach(() => { cleanup(); vi.useRealTimers() })

  it('shows login after an unauthenticated current-user response', async () => {
    vi.mocked(fetch).mockResolvedValue(new Response(JSON.stringify({ detail: 'unauthorized' }), { status: 401, headers: { 'Content-Type': 'application/json' } }))
    render(<App />)
    expect(screen.getByText('正在确认登录状态…')).toBeInTheDocument()
    expect(await screen.findByRole('heading', { name: '登录工作台' })).toBeInTheDocument()
  })

  it('keeps a temporary-password session out of the workbench', async () => {
    vi.mocked(fetch).mockResolvedValue(new Response(JSON.stringify(user), { status: 200, headers: { 'Content-Type': 'application/json' } }))
    render(<App />)
    expect(await screen.findByRole('heading', { name: '先设置你自己的密码' })).toBeInTheDocument()
    await waitFor(() => expect(screen.queryByLabelText('全局模块')).not.toBeInTheDocument())
  })

  it('switches an already-open workbench into maintenance when the server enters RootKeyRebind', async () => {
    vi.useFakeTimers()
    let maintenanceReads = 0
    vi.mocked(fetch).mockImplementation((input) => {
      const url = input instanceof Request ? input.url : String(input)
      if (url.endsWith('/api/v1/auth/me')) return Promise.resolve(new Response(JSON.stringify({ ...user, passwordChangeRequired: false }), { status: 200, headers: { 'Content-Type': 'application/json' } }))
      if (url.endsWith('/api/v1/maintenance')) {
        maintenanceReads++
        if (maintenanceReads === 1) return Promise.resolve(new Response(JSON.stringify({ detail: 'not in maintenance' }), { status: 404, headers: { 'Content-Type': 'application/json' } }))
        return Promise.resolve(new Response(JSON.stringify({ active: true, reason: 'RootKeyRebind', rowVersion: 8, items: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } }))
      }
      return Promise.resolve(new Response(JSON.stringify([]), { status: 200, headers: { 'Content-Type': 'application/json' } }))
    })
    render(<App />)
    await act(async () => { await Promise.resolve(); await Promise.resolve() })
    expect(screen.getByLabelText('全局模块')).toBeInTheDocument()
    await act(async () => { await vi.advanceTimersByTimeAsync(5_000); await Promise.resolve() })
    expect(screen.getByRole('heading', { name: '根密钥更换' })).toBeInTheDocument()
    expect(screen.queryByLabelText('全局模块')).not.toBeInTheDocument()
  })

  it('replaces the entire workbench with the root-key rebind maintenance page', async () => {
    vi.mocked(fetch)
      .mockResolvedValueOnce(new Response(JSON.stringify({ ...user, passwordChangeRequired: false }), { status: 200, headers: { 'Content-Type': 'application/json' } }))
      .mockResolvedValueOnce(new Response(JSON.stringify({ active: true, reason: 'RootKeyRebind', rowVersion: 7, items: [{ kind: 'Connection', objectKey: 'main-thanos', safeState: 'Blocking', detailCode: 'root_key_rebind_required' }] }), { status: 200, headers: { 'Content-Type': 'application/json' } }))
      .mockResolvedValue(new Response(JSON.stringify([]), { status: 200, headers: { 'Content-Type': 'application/json' } }))
    render(<App />)
    expect(await screen.findByRole('heading', { name: '根密钥更换' })).toBeInTheDocument()
    expect(screen.queryByLabelText('全局模块')).not.toBeInTheDocument()
  })
})
