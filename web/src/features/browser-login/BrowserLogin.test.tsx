import '@testing-library/jest-dom/vitest'
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { BrowserLogin } from './BrowserLogin'

const rfb = vi.hoisted(() => ({ instances: [] as Array<{ listeners: Record<string, () => void>, focus: ReturnType<typeof vi.fn> }> }))
vi.mock('@novnc/novnc', () => ({
  default: class MockRFB {
    listeners: Record<string, () => void> = {}
    scaleViewport = false
    resizeSession = false
    constructor() { rfb.instances.push(this) }
    addEventListener(name: string, listener: () => void) { this.listeners[name] = listener }
    focus = vi.fn()
    disconnect() {}
  },
}))

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })
}

const identity = {
  id: '1', state: 'AuthenticationRequired', rowVersion: 4,
  currentRevision: { id: '2', revision: 1, name: '运营只读账号', startUrl: 'https://example.test', authenticationProbe: { journeyId: 'auth', journeyVersion: 1, params: {} }, catalogDigest: 'a'.repeat(64), catalogVersion: 'v1', createdAt: '2026-08-24T00:00:00Z' },
  currentProfile: null, lastProbe: null, currentOperation: null,
}

const operation = {
  id: '7', identityId: '1', identityRevisionId: '2', kind: 'manual_login', state: 'Queued', rowVersion: 1,
  requestedAt: '2026-08-24T00:00:00Z', canAttach: false, canPublish: false, canCancel: true,
}

describe('BrowserLogin', () => {
  beforeEach(() => vi.stubGlobal('fetch', vi.fn()))
  afterEach(() => { cleanup(); rfb.instances.splice(0) })

  it('starts an existing identity login and explains that the browser is preparing', async () => {
    vi.mocked(fetch).mockImplementation(async (input: RequestInfo | URL) => {
      const path = String(input)
      if (path === '/api/v1/business-systems/payments/browser-identity') return jsonResponse(identity)
      if (path === '/api/v1/browser-login/payments/operations') return jsonResponse(operation, 202)
      return jsonResponse({}, 404)
    })
    render(<BrowserLogin systemKey="payments" />)
    expect(screen.getByText('浏览器登录')).toBeInTheDocument()
    expect(await screen.findByText(/运营只读账号/)).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: '重新登录' }))

    await waitFor(() => expect(vi.mocked(fetch)).toHaveBeenCalledWith(
      '/api/v1/browser-login/payments/operations',
      expect.objectContaining({ method: 'POST' }),
    ))
    expect(await screen.findByText('浏览器登录已受理，正在准备安全窗口…')).toBeInTheDocument()
  })

  it('does not trap local keyboard focus when the RFB connection opens', async () => {
    const active = { ...operation, state: 'Running', canAttach: true, canPublish: true }
    vi.mocked(fetch).mockImplementation(async (input: RequestInfo | URL) => {
      if (String(input) === '/api/v1/business-systems/payments/browser-identity') return jsonResponse({ ...identity, currentOperation: active })
      return jsonResponse({}, 404)
    })
    render(<BrowserLogin systemKey="payments" />)
    await waitFor(() => expect(rfb.instances).toHaveLength(1))
    rfb.instances[0].listeners.connect()
    expect(rfb.instances[0].focus).not.toHaveBeenCalled()
    fireEvent.click(screen.getByRole('button', { name: '进入远程浏览器' }))
    expect(rfb.instances[0].focus).toHaveBeenCalledOnce()
    fireEvent.keyDown(window, { key: 'Escape', shiftKey: true })
    expect(document.activeElement).toBe(screen.getByRole('button', { name: '退出远程键盘' }))
  })

  it('applies a terminal publish response immediately and returns to the business system', async () => {
    const active = { ...operation, state: 'Running', canAttach: true, canPublish: true }
    const published = { ...active, state: 'Succeeded', rowVersion: 2, canAttach: false, canPublish: false, canCancel: false }
    const onPublished = vi.fn()
    vi.mocked(fetch).mockImplementation(async (input: RequestInfo | URL, init?: RequestInit) => {
      const path = String(input)
      if (path === '/api/v1/business-systems/payments/browser-identity') return jsonResponse({ ...identity, currentOperation: active })
      if (path === '/api/v1/browser-login/payments/operations/7/publish' && init?.method === 'POST') return jsonResponse(published)
      return jsonResponse({}, 404)
    })
    render(<BrowserLogin systemKey="payments" onPublished={onPublished} />)
    await screen.findByRole('button', { name: '完成登录并发布' })
    fireEvent.click(screen.getByRole('button', { name: '完成登录并发布' }))
    await waitFor(() => expect(onPublished).toHaveBeenCalledOnce())
    expect(screen.getByText('浏览器身份已发布并完成认证。')).toBeInTheDocument()
  })

  it('creates a new RFB attachment after a disconnect during the re-attach grace period', async () => {
    const active = { ...operation, state: 'Running', canAttach: true, canPublish: true }
    const awaitingReconnect = { ...active, state: 'AwaitingReconnect', rowVersion: 2, reconnectDeadline: '2026-08-24T00:02:00Z' }
    let operationReads = 0
    vi.mocked(fetch).mockImplementation(async (input: RequestInfo | URL) => {
      const path = String(input)
      if (path === '/api/v1/business-systems/payments/browser-identity') return jsonResponse({ ...identity, currentOperation: active })
      if (path === '/api/v1/browser-login/payments/operations/7') return jsonResponse(++operationReads === 1 ? awaitingReconnect : awaitingReconnect)
      return jsonResponse({}, 404)
    })
    render(<BrowserLogin systemKey="payments" />)
    await waitFor(() => expect(rfb.instances).toHaveLength(1))
    rfb.instances[0].listeners.disconnect()
    await waitFor(() => expect(rfb.instances).toHaveLength(2))
  })
})
