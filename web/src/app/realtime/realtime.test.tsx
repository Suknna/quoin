import '@testing-library/jest-dom/vitest'
import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { AlertDetail } from '../../features/alerts/AlertDetail'
import { AlertsList } from '../../features/alerts/AlertsList'
import { IntakeIssuesList } from '../../features/alerts/IntakeIssuesList'
import { AlertEventStream, type AlertChangeEventData } from './stream'

// The stream singleton is stubbed per test through the factory so components
// observe a fully controlled event source.
type ChangeHandler = (event: AlertChangeEventData, sourceGeneration: number) => void

class StubStream {
  phase = 'idle' as 'idle' | 'open'
  generation = 0
  started: number[] = []
  private changeHandlers = new Set<ChangeHandler>()
  private resyncHandlers = new Set<() => void>()
  start(after: number) { this.started.push(after); this.generation += 1; this.phase = 'open' }
  stop() { /* singleton lifecycle owned by tests */ }
  onChange(listener: ChangeHandler) { this.changeHandlers.add(listener); return () => this.changeHandlers.delete(listener) }
  onResync(listener: () => void) { this.resyncHandlers.add(listener); return () => this.resyncHandlers.delete(listener) }
  onPhase() { return () => undefined }
  emit(event: AlertChangeEventData, sourceGeneration = this.generation) { for (const handler of this.changeHandlers) handler(event, sourceGeneration) }
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

  it('keeps the authoritative detail visible and retries a transient live re-read failure', async () => {
    let detailReads = 0
    vi.mocked(fetch).mockImplementation(async (input: RequestInfo | URL) => {
      const url = String(input)
      if (url === '/api/v1/alerts/7') {
        detailReads += 1
        if (detailReads === 2) return jsonResponse({ code: 'unavailable' }, 503)
        return jsonResponse(detailReads > 2 ? resolved : firing)
      }
      if (url === '/api/v1/alerts/7/observations') return jsonResponse({ items: [] })
      return jsonResponse({ detail: 'not found' }, 404)
    })
    render(<AlertDetail occurrenceId="7" onBack={() => undefined} />)
    expect(await screen.findByRole('heading', { name: 'LiveOne' })).toBeInTheDocument()
    act(() => stub.emit({ seq: '6', type: 'state_changed', occurrenceId: '7', rowVersion: 2 }))
    // A failed update does not discard the last authoritative body.
    expect(screen.getByRole('heading', { name: 'LiveOne' })).toBeInTheDocument()
    await waitFor(() => expect(screen.getByText('已恢复')).toBeInTheDocument(), { timeout: 1500 })
    expect(detailReads).toBeGreaterThanOrEqual(3)
  })

  it('reports an actionable detail error only after bounded live re-read retries', async () => {
    vi.mocked(fetch).mockImplementation(async (input: RequestInfo | URL) => {
      const url = String(input)
      if (url === '/api/v1/alerts/7') return jsonResponse({ code: 'unavailable' }, 503)
      if (url === '/api/v1/alerts/7/observations') return jsonResponse({ items: [] })
      return jsonResponse({ detail: 'not found' }, 404)
    })
    // Seed succeeds before replacement responses become permanently unavailable.
    vi.mocked(fetch).mockImplementationOnce(async () => jsonResponse(firing))
    render(<AlertDetail occurrenceId="7" onBack={() => undefined} />)
    expect(await screen.findByRole('heading', { name: 'LiveOne' })).toBeInTheDocument()
    act(() => stub.emit({ seq: '6', type: 'state_changed', occurrenceId: '7', rowVersion: 2 }))
    await waitFor(() => expect(screen.getByText('无法读取最新告警详情，请返回列表后重试。')).toBeInTheDocument(), { timeout: 2500 })
    expect(screen.getByRole('button', { name: '返回列表' })).toBeInTheDocument()
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
    render(<AlertsList view="Firing" businessSystemKey="" onFilter={() => undefined} onSelect={() => undefined} />)
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
    render(<AlertsList view="Firing" businessSystemKey="" onFilter={() => undefined} onSelect={() => undefined} />)
    expect(await screen.findByText('LiveOne')).toBeInTheDocument()
    detail = resolved
    act(() => stub.emit({ seq: '6', type: 'state_changed', occurrenceId: '7', rowVersion: 2 }))
    await waitFor(() => expect(screen.getByText('当前没有 Firing 告警')).toBeInTheDocument())
  })
})

class FakeEventSource {
  static readonly CONNECTING = 0
  static readonly CLOSED = 2

  readonly listeners = new Map<string, Set<(event: Event) => void>>()
  readyState = FakeEventSource.CONNECTING
  onerror: ((event: Event) => void) | null = null

  constructor(readonly url: string) {
    fakeSources.push(this)
  }

  addEventListener(type: string, listener: (event: Event) => void): void {
    const listeners = this.listeners.get(type) ?? new Set<(event: Event) => void>()
    listeners.add(listener)
    this.listeners.set(type, listeners)
  }

  close(): void {
    this.readyState = FakeEventSource.CLOSED
  }

  emit(type: string, data = ''): void {
    for (const listener of this.listeners.get(type) ?? []) listener(new MessageEvent(type, { data }))
  }

  fail(readyState = FakeEventSource.CLOSED): void {
    this.readyState = readyState
    this.onerror?.(new Event('error'))
  }
}

let fakeSources: FakeEventSource[] = []

describe('AlertEventStream boundary recovery (T17)', () => {
  beforeEach(() => {
    fakeSources = []
    vi.stubGlobal('EventSource', FakeEventSource)
  })

  it('ignores an event queued by the source a later snapshot replaced', () => {
    const stream = new AlertEventStream()
    const changes = vi.fn()
    stream.onChange(changes)

    stream.start(5)
    const oldSource = fakeSources[0]
    stream.start(10)
    const currentSource = fakeSources[1]

    oldSource.emit('change', JSON.stringify({ seq: '6', type: 'created', occurrenceId: 'old', rowVersion: 1 }))
    expect(changes).not.toHaveBeenCalled()

    currentSource.emit('change', JSON.stringify({ seq: '11', type: 'created', occurrenceId: 'current', rowVersion: 1 }))
    expect(changes).toHaveBeenCalledWith({ seq: '11', type: 'created', occurrenceId: 'current', rowVersion: 1 }, stream.generation)
  })

  it('uses a new snapshot after terminal EventSource failure but preserves native replay while connecting', () => {
    const stream = new AlertEventStream()
    const resync = vi.fn()
    stream.onResync(resync)

    stream.start(5)
    fakeSources[0].fail()
    expect(resync).toHaveBeenCalledTimes(1)
    expect(stream.phase).toBe('resync')

    stream.start(9)
    fakeSources[1].fail(FakeEventSource.CONNECTING)
    expect(resync).toHaveBeenCalledTimes(1)
    expect(stream.phase).toBe('recovering')
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
    render(<AlertsList view="Resolved" businessSystemKey="" onFilter={() => undefined} onSelect={() => undefined} />)
    expect(await screen.findByText('还没有已恢复的告警')).toBeInTheDocument()
    detail = resolved
    act(() => stub.emit({ seq: '6', type: 'state_changed', occurrenceId: '7', rowVersion: 2 }))
    await waitFor(() => expect(screen.getByText('LiveOne')).toBeInTheDocument())
  })
})

describe('alerts business-system filter (T17)', () => {
  const attributed = {
    id: '11', state: 'Firing', rowVersion: 1,
    firstSeenAt: '2026-09-01T10:00:00Z', lastStateChangeAt: '2026-09-01T10:00:00Z',
    businessSystemKey: 'payments',
    labels: { alertname: 'PayOnly', severity: 'critical' },
  }
  const unattributed = {
    id: '12', state: 'Firing', rowVersion: 1,
    firstSeenAt: '2026-09-01T10:01:00Z', lastStateChangeAt: '2026-09-01T10:01:00Z',
    labels: { alertname: 'NoSystem' },
  }
  const billing = {
    id: '13', state: 'Firing', rowVersion: 1,
    firstSeenAt: '2026-09-01T10:02:00Z', lastStateChangeAt: '2026-09-01T10:02:00Z',
    businessSystemKey: 'billing',
    labels: { alertname: 'BillingOnly', severity: 'warning' },
  }

  function mockStack(filtered: Array<typeof attributed | typeof unattributed>) {
    vi.mocked(fetch).mockImplementation(async (input: RequestInfo | URL) => {
      const url = String(input)
      if (url.includes('/api/v1/business-systems')) {
        return jsonResponse({ items: [{ key: 'payments', displayName: '支付系统' }, { key: 'billing', displayName: '账单系统' }] })
      }
      if (url.startsWith('/api/v1/alerts?')) return jsonResponse({ snapshotSeq: 9, items: filtered })
      if (url.startsWith('/api/v1/alerts/')) return jsonResponse(url.includes('/11') ? attributed : unattributed)
      if (url.includes('/observations')) return jsonResponse({ items: [] })
      return jsonResponse({ detail: 'not found' }, 404)
    })
  }

  it('shows attribution on rows and 未归属 for unmatched systems', async () => {
    mockStack([attributed, unattributed])
    render(<AlertsList view="Firing" businessSystemKey="" onFilter={() => undefined} onSelect={() => undefined} />)
    expect(await screen.findByText('PayOnly')).toBeInTheDocument()
    expect(screen.getByText('payments')).toBeInTheDocument()
    expect(screen.getByText('未归属')).toBeInTheDocument()
  })

  it('requests the filtered snapshot and exposes the combobox choice', async () => {
    mockStack([attributed])
    const onFilter = vi.fn()
    render(<AlertsList view="Firing" businessSystemKey="payments" onFilter={onFilter} onSelect={() => undefined} />)
    await screen.findByText('PayOnly')
    const filtered = vi.mocked(fetch).mock.calls.filter(([input]) => String(input).includes('businessSystemKey=payments'))
    expect(filtered.length).toBeGreaterThanOrEqual(1)
    // The combobox lists real systems; choosing one hands the key to the router.
    fireEvent.focus(screen.getByLabelText('按业务系统筛选'))
    fireEvent.mouseDown(await screen.findByRole('option', { name: /账单系统/ }))
    expect(onFilter).toHaveBeenCalledWith('billing')
    expect(screen.getByRole('button', { name: '清除筛选' })).toBeInTheDocument()
  })

  it('supports combobox keyboard selection with an active descendant', async () => {
    mockStack([attributed])
    const onFilter = vi.fn()
    render(<AlertsList view="Firing" businessSystemKey="" onFilter={onFilter} onSelect={() => undefined} />)
    await screen.findByText('PayOnly')
    const input = screen.getByLabelText('按业务系统筛选')
    fireEvent.focus(input)
    expect(input).toHaveAttribute('role', 'combobox')
    expect(input).toHaveAttribute('aria-expanded', 'true')
    fireEvent.keyDown(input, { key: 'ArrowDown' })
    fireEvent.keyDown(input, { key: 'ArrowDown' })
    fireEvent.keyDown(input, { key: 'ArrowDown' })
    expect(input).toHaveAttribute('aria-activedescendant', expect.stringContaining('option-2'))
    fireEvent.keyDown(input, { key: 'Enter' })
    expect(onFilter).toHaveBeenCalledWith('billing')
  })

  it('drops a live row whose attribution does not match the filter', async () => {
    mockStack([attributed])
    render(<AlertsList view="Firing" businessSystemKey="payments" onFilter={() => undefined} onSelect={() => undefined} />)
    await screen.findByText('PayOnly')
    // A new occurrence belonging to another system arrives live: the re-read
    // detail carries businessSystemKey=billing and must not enter the view.
    act(() => stub.emit({ seq: '10', type: 'created', occurrenceId: '12', rowVersion: 1 }))
    await new Promise((resolve) => setTimeout(resolve, 50))
    expect(screen.queryByText('NoSystem')).not.toBeInTheDocument()
    expect(screen.getByText('PayOnly')).toBeInTheDocument()
  })

  it('restarts at the new snapshot cursor and replays changes that arrived during a filter switch', async () => {
    let snapshots = 0
    let resolveSecondSnapshot: ((response: Response) => void) | undefined
    vi.mocked(fetch).mockImplementation((input: RequestInfo | URL) => {
      const url = String(input)
      if (url.startsWith('/api/v1/business-systems?')) {
        return Promise.resolve(jsonResponse({ items: [{ key: 'payments', displayName: '支付系统' }, { key: 'billing', displayName: '账单系统' }] }))
      }
      if (url.startsWith('/api/v1/alerts?')) {
        snapshots += 1
        if (snapshots === 1) return Promise.resolve(jsonResponse({ snapshotSeq: 5, items: [attributed] }))
        return new Promise<Response>((resolve) => { resolveSecondSnapshot = resolve })
      }
      if (url === '/api/v1/alerts/13') return Promise.resolve(jsonResponse(billing))
      return Promise.resolve(jsonResponse({ detail: 'not found' }, 404))
    })

    const { rerender } = render(<AlertsList view="Firing" businessSystemKey="payments" onFilter={() => undefined} onSelect={() => undefined} />)
    expect(await screen.findByText('PayOnly')).toBeInTheDocument()
    const replacedSourceGeneration = stub.generation
    rerender(<AlertsList view="Firing" businessSystemKey="billing" onFilter={() => undefined} onSelect={() => undefined} />)
    await waitFor(() => expect(snapshots).toBe(2))

    // This is the old EventSource's change: the new snapshot has not settled,
    // so it must be replayed from snapshotSeq rather than applied early.
    await act(async () => {
      stub.emit({ seq: '10', type: 'created', occurrenceId: '13', rowVersion: 1 })
      await Promise.resolve()
      await Promise.resolve()
    })
    if (!resolveSecondSnapshot) throw new Error('second snapshot was not requested')
    await act(async () => {
      resolveSecondSnapshot?.(jsonResponse({ snapshotSeq: 9, items: [] }))
      await Promise.resolve()
    })
    await waitFor(() => expect(stub.started.at(-1)).toBe(9))

    // A source event queued before the filter-switch snapshot must remain
    // invalid even if it runs only after the replacement stream starts.
    await act(async () => {
      stub.emit({ seq: '10', type: 'created', occurrenceId: '13', rowVersion: 1 }, replacedSourceGeneration)
      await Promise.resolve()
    })
    expect(screen.queryByText('BillingOnly')).not.toBeInTheDocument()

    // The replacement stream replays the same sequence after the snapshot.
    await act(async () => {
      stub.emit({ seq: '10', type: 'created', occurrenceId: '13', rowVersion: 1 })
      await Promise.resolve()
    })
    expect(await screen.findByText('BillingOnly')).toBeInTheDocument()
  })

  it('does not present an unknown URL filter as the unfiltered view', async () => {
    mockStack([])
    render(<AlertsList view="Firing" businessSystemKey="retired-system" onFilter={() => undefined} onSelect={() => undefined} />)
    await screen.findByText('当前没有 Firing 告警')
    await waitFor(() => expect(screen.getByLabelText('按业务系统筛选')).toHaveValue('retired-system'))
  })

  it('loads every business-system page, including the selected system beyond the first page', async () => {
    vi.mocked(fetch).mockImplementation(async (input: RequestInfo | URL) => {
      const url = String(input)
      if (url.startsWith('/api/v1/business-systems?')) {
        if (url.includes('cursor=page-2')) {
          return jsonResponse({ items: [{ key: 'late-system', displayName: '深层系统' }] })
        }
        return jsonResponse({
          items: [{ key: 'first-system', displayName: '首个系统' }],
          nextCursor: 'page-2',
        })
      }
      if (url.startsWith('/api/v1/alerts?')) return jsonResponse({ snapshotSeq: 9, items: [attributed] })
      if (url.startsWith('/api/v1/alerts/')) return jsonResponse(attributed)
      return jsonResponse({ detail: 'not found' }, 404)
    })
    const onFilter = vi.fn()
    render(<AlertsList view="Firing" businessSystemKey="late-system" onFilter={onFilter} onSelect={() => undefined} />)
    await screen.findByText('PayOnly')
    const input = screen.getByLabelText('按业务系统筛选')
    await waitFor(() => expect(input).toHaveValue('深层系统'))
    fireEvent.focus(input)
    fireEvent.mouseDown(await screen.findByRole('option', { name: /深层系统/ }))
    expect(onFilter).toHaveBeenCalledWith('late-system')
  })
})
