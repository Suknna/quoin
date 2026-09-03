import '@testing-library/jest-dom/vitest'
import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { PrepareUpgradeCard, UpgradeMaintenancePanel } from './UpgradeMaintenancePanel'
import type { MaintenanceStateView } from './api'

function response(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })
}

const admin = { id: '1', username: 'admin', displayName: 'Admin', role: 'admin', enabled: true, createdAt: '', passwordChangeRequired: false } as never

const blocking: MaintenanceStateView = {
  active: true,
  reason: 'Upgrade',
  rowVersion: 2,
  items: [
    { kind: 'ActiveAttempt', objectKey: 'attempt/9', safeState: 'Blocking', detailCode: 'queued|cancel:connection_probe:prod-thanos/9:3' },
    { kind: 'BackupPreflight', objectKey: 'pre_upgrade_backup', safeState: 'Blocking', detailCode: 'backup_pending' },
  ],
}

const preparedState: MaintenanceStateView = {
  active: true,
  reason: 'Upgrade',
  rowVersion: 2,
  items: [{ kind: 'BackupPreflight', objectKey: 'pre_upgrade_backup', safeState: 'Safe', detailCode: 'backup_verified' }],
}

describe('UpgradeMaintenancePanel', () => {
  beforeEach(() => { vi.stubGlobal('fetch', vi.fn()) })
  afterEach(() => { cleanup(); vi.useRealTimers(); vi.unstubAllGlobals() })

  it('drains blocking work through the frozen cancel command', async () => {
    const posts: { path: string; body: Record<string, unknown> }[] = []
    let state = blocking
    vi.mocked(fetch).mockImplementation(async (input: RequestInfo | URL, init?: RequestInit) => {
      const path = String(input)
      if (path === '/api/v1/maintenance') return response(state)
      posts.push({ path, body: JSON.parse(String(init?.body)) })
      state = { ...blocking, items: [blocking.items[1]] }
      return response({})
    })
    render(<UpgradeMaintenancePanel user={admin} initialState={blocking} />)
    const cancel = screen.getByRole('button', { name: '取消' })
    await act(async () => { fireEvent.click(cancel) })
    await waitFor(() => expect(posts).toHaveLength(1))
    expect(posts[0].path).toBe('/api/v1/connections/prod-thanos/probe-attempts/9/cancel')
    expect(posts[0].body.expectedRowVersion).toBe(3)
    expect(screen.getByRole('status').textContent).toContain('已提交取消')
  })

  it('renders the browser operation fence with its own row version field', async () => {
    const posts: { path: string; body: Record<string, unknown> }[] = []
    const browserBlocking: MaintenanceStateView = {
      active: true, reason: 'Upgrade', rowVersion: 2,
      items: [{ kind: 'ActiveBrowserOperation', objectKey: 'operation/4', safeState: 'Blocking', detailCode: 'running|cancel:browser_operation:payments/4:7' }],
    }
    vi.mocked(fetch).mockImplementation(async (input: RequestInfo | URL, init?: RequestInit) => {
      const path = String(input)
      if (path === '/api/v1/maintenance') return response(browserBlocking)
      posts.push({ path, body: JSON.parse(String(init?.body)) })
      return response({})
    })
    render(<UpgradeMaintenancePanel user={admin} initialState={browserBlocking} />)
    await act(async () => { fireEvent.click(screen.getByRole('button', { name: '取消' })) })
    await waitFor(() => expect(posts).toHaveLength(1))
    expect(posts[0].path).toBe('/api/v1/browser-login/payments/operations/4/cancel')
    expect(posts[0].body.expectedOperationRowVersion).toBe(7)
    expect(posts[0].body.expectedRowVersion).toBeUndefined()
  })

  it('offers the abort exit only when every item is Safe', async () => {
    const posts: string[] = []
    const reload = vi.fn()
    vi.stubGlobal('location', { ...window.location, reload })
    let state = preparedState
    vi.mocked(fetch).mockImplementation(async (input: RequestInfo | URL, init?: RequestInit) => {
      const path = String(input)
      if (path === '/api/v1/maintenance') return response(state)
      posts.push(`${path} ${(String(init?.body))}`)
      state = { active: false, rowVersion: 3, items: [] }
      return response(state)
    })
    render(<UpgradeMaintenancePanel user={admin} initialState={preparedState} />)
    expect(screen.getByRole('status').textContent).toContain('quoin_upgrade_prepared=1')
    const exit = screen.getByRole('button', { name: '取消升级并退出维护' })
    expect(exit).toBeEnabled()
    await act(async () => { fireEvent.click(exit) })
    await waitFor(() => expect(posts).toHaveLength(1))
    expect(posts[0]).toContain('/api/v1/maintenance/exit')
    await waitFor(() => expect(reload).toHaveBeenCalled())
  })
})

describe('PrepareUpgradeCard', () => {
  beforeEach(() => { vi.stubGlobal('fetch', vi.fn()) })
  afterEach(() => { cleanup(); vi.unstubAllGlobals() })

  it('submits the versioned prepare command', async () => {
    const posts: Record<string, unknown>[] = []
    const onPrepared = vi.fn()
    vi.mocked(fetch).mockImplementation(async (input: RequestInfo | URL, init?: RequestInit) => {
      const path = String(input)
      if (path === '/api/v1/maintenance') return response({ active: false, rowVersion: 1, items: [] })
      posts.push(JSON.parse(String(init?.body)))
      return response(blocking, 202)
    })
    render(<PrepareUpgradeCard onPrepared={onPrepared} />)
    await act(async () => { fireEvent.click(screen.getByRole('button', { name: '准备升级维护' })) })
    await waitFor(() => expect(onPrepared).toHaveBeenCalled())
    expect(posts).toHaveLength(1)
    expect(posts[0].expectedRowVersion).toBe(1)
    expect(String(posts[0].clientCommandId)).toMatch(/^[0-9a-f]{36}$/)
  })
})
