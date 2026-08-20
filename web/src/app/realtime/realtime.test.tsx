import '@testing-library/jest-dom/vitest'
import { act, cleanup, render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { AlertDetail } from '../../features/alerts/AlertDetail'
import { IntakeIssuesList } from '../../features/alerts/IntakeIssuesList'
import type { AlertChangeEventData } from './stream'

// The stream singleton is stubbed per test through the factory so components
// observe a fully controlled event source.
type ChangeHandler = (event: AlertChangeEventData) => void

class StubStream {
  phase = 'idle' as 'idle' | 'open'
  started: number[] = []
  private changeHandlers = new Set<ChangeHandler>()
  private resyncHandlers = new Set<() => void>()
  start(after: number) { this.started.push(after); this.phase = 'open' }
  stop() { /* singleton lifecycle owned by tests */ }
  onChange(listener: ChangeHandler) { this.changeHandlers.add(listener); return () => this.changeHandlers.delete(listener) }
  onResync(listener: () => void) { this.resyncHandlers.add(listener); return () => this.resyncHandlers.delete(listener) }
  onPhase() { return () => undefined }
  emit(event: AlertChangeEventData) { for (const handler of this.changeHandlers) handler(event) }
  resync() { for (const handler of this.resyncHandlers) handler() }
}

const stub = new StubStream()

vi.mock('./stream', async () => {
  const actual = await vi.importActual<typeof import('./stream')>('./stream')
  return { ...actual, alertEventStreamFactory: { create: () => stub } }
})

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })
}

const firing = {
  id: '7', state: 'Firing', rowVersion: 1,
  firstSeenAt: '2026-08-18T10:00:00Z', lastStateChangeAt: '2026-08-18T10:00:00Z',
  labels: { alertname: 'LiveOne', severity: 'critical' },
}
const resolved = { ...firing, state: 'Resolved', rowVersion: 2, resolvedAt: '2026-08-18T10:05:00Z' }

let fetchCalls: string[] = []

beforeEach(() => {
  fetchCalls = []
  vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => {
    const url = String(input)
    fetchCalls.push(url)
    if (url.startsWith('/api/v1/alerts?')) return jsonResponse({ snapshotSeq: 5, items: [firing] })
    if (url === '/api/v1/alerts/7') return jsonResponse(firing)
    if (url === '/api/v1/alerts/7/observations') return jsonResponse({ items: [] })
    if (url === '/api/v1/alert-intake-issues') return jsonResponse({ items: [] })
    return jsonResponse({ detail: 'not found' }, 404)
  }))
})

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
  vi.clearAllMocks()
})

describe('alert realtime projection (T04)', () => {
  it('the detail pane re-reads the body when the row version advances', async () => {
    let detail = firing
    vi.mocked(fetch).mockImplementation(async (input: RequestInfo | URL) => {
      const url = String(input)
      if (url === '/api/v1/alerts/7') return jsonResponse(detail)
      if (url === '/api/v1/alerts/7/observations') return jsonResponse({ items: [] })
      return jsonResponse({ detail: 'not found' }, 404)
    })
    render(<AlertDetail occurrenceId="7" onBack={() => undefined} />)
    expect(await screen.findByRole('heading', { name: 'LiveOne' })).toBeInTheDocument()
    expect(screen.queryByText('已恢复')).not.toBeInTheDocument()
    detail = resolved

    act(() => stub.emit({ seq: '6', type: 'state_changed', occurrenceId: '7', rowVersion: 2 }))
    await waitFor(() => expect(screen.getByText('已恢复')).toBeInTheDocument())
    // The authoritative body was re-read, not patched from the event.
    const detailReads = vi.mocked(fetch).mock.calls.filter(([input]) => String(input) === '/api/v1/alerts/7').length
    expect(detailReads).toBeGreaterThanOrEqual(2)
  })

  it('a stale or duplicate event never triggers a re-read', async () => {
    render(<AlertDetail occurrenceId="7" onBack={() => undefined} />)
    await screen.findByRole('heading', { name: 'LiveOne' })
    const reads = fetchCalls.filter((url) => url === '/api/v1/alerts/7').length
    act(() => {
      stub.emit({ seq: '6', type: 'state_changed', occurrenceId: '7', rowVersion: 1 })
      stub.emit({ seq: '6', type: 'state_changed', occurrenceId: '7', rowVersion: 3 })
    })
    await new Promise((resolve) => setTimeout(resolve, 50))
    const detailReads = vi.mocked(fetch).mock.calls.filter(([input]) => String(input) === '/api/v1/alerts/7').length
    expect(detailReads).toBe(reads)
  })

  it('intake issue list acknowledges and refreshes with conflict handling', async () => {
    let acknowledged = false
    vi.mocked(fetch).mockImplementation(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input)
      if (url === '/api/v1/alert-intake-issues') {
        return jsonResponse(acknowledged
          ? { items: [] }
          : { items: [{ id: '3', kind: 'fingerprint_mismatch', issueKey: 'k', detailJson: '{}', firstSeenAt: '2026-08-18T10:00:00Z', lastSeenAt: '2026-08-18T10:00:00Z', occurrenceCount: 2, rowVersion: 4 }] })
      }
      if (url.endsWith('/acknowledge')) {
        const body = JSON.parse(String(init?.body)) as { expectedRowVersion: number }
        if (body.expectedRowVersion === 4) { acknowledged = true; return new Response(null, { status: 204 }) }
        return jsonResponse({ status: 409, detail: '版本已变化' }, 409)
      }
      return jsonResponse({ detail: 'x' }, 404)
    })
    render(<IntakeIssuesList isAdmin />)
    expect(await screen.findByText('已发生 2 次')).toBeInTheDocument()
    await act(async () => { screen.getByRole('button', { name: '标记已处理' }).click() })
    await waitFor(() => expect(screen.getByText('没有未处理的接入问题')).toBeInTheDocument())
  })

  it('operator sees the intake list without the acknowledge control', async () => {
    vi.mocked(fetch).mockImplementation(async (input: RequestInfo | URL) => {
      if (String(input) === '/api/v1/alert-intake-issues') {
        return jsonResponse({ items: [{ id: '3', kind: 'delivery_truncated', issueKey: 'k', detailJson: '{}', firstSeenAt: '2026-08-18T10:00:00Z', lastSeenAt: '2026-08-18T10:00:00Z', occurrenceCount: 1, rowVersion: 1 }] })
      }
      return jsonResponse({}, 404)
    })
    render(<IntakeIssuesList isAdmin={false} />)
    expect(await screen.findByText('送达截断')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '标记已处理' })).not.toBeInTheDocument()
  })

  it('the AlertsList live list resolves is the shared snapshot cursor start', async () => {
    const { AlertsList } = await import('../../features/alerts/AlertsList')
    render(<AlertsList view="Firing" onSelect={() => undefined} />)
    expect(await screen.findByText('LiveOne')).toBeInTheDocument()
    await waitFor(() => expect(stub.started).toContain(5))
  })

  it('resolved body from re-read switches the row out of the current view', async () => {
    const { AlertsList } = await import('../../features/alerts/AlertsList')
    let detail = firing
    vi.mocked(fetch).mockImplementation(async (input: RequestInfo | URL) => {
      const url = String(input)
      if (url.startsWith('/api/v1/alerts?')) return jsonResponse({ snapshotSeq: 5, items: [firing] })
      if (url === '/api/v1/alerts/7') return jsonResponse(detail)
      return jsonResponse({}, 404)
    })
    render(<AlertsList view="Firing" onSelect={() => undefined} />)
    expect(await screen.findByText('LiveOne')).toBeInTheDocument()
    detail = resolved
    act(() => stub.emit({ seq: '6', type: 'state_changed', occurrenceId: '7', rowVersion: 2 }))
    await waitFor(() => expect(screen.getByText('当前没有 Firing 告警')).toBeInTheDocument())
  })
})

describe('history view live add (T04)', () => {
  it('a newly resolved occurrence joins the open Resolved view live', async () => {
    const { AlertsList } = await import('../../features/alerts/AlertsList')
    let detail = firing
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input)
      if (url.startsWith('/api/v1/alerts?')) return jsonResponse({ snapshotSeq: 5, items: [] })
      if (url === '/api/v1/alerts/7') return jsonResponse(detail)
      return jsonResponse({}, 404)
    }))
    render(<AlertsList view="Resolved" onSelect={() => undefined} />)
    expect(await screen.findByText('还没有已恢复的告警')).toBeInTheDocument()
    detail = resolved
    act(() => stub.emit({ seq: '6', type: 'state_changed', occurrenceId: '7', rowVersion: 2 }))
    await waitFor(() => expect(screen.getByText('LiveOne')).toBeInTheDocument())
  })
})
