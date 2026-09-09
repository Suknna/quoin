import '@testing-library/jest-dom/vitest'
import { act, cleanup, render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { useOccurrenceVersions } from './hooks'
import { AlertEventStream, type AlertChangeEventData } from './stream'
import { useLiveAlerts } from '../../features/alerts/useLiveAlerts'

// The stream singleton is replaced at its factory boundary so hook tests can
// exercise snapshot/cursor semantics without depending on deleted UI modules.
type ChangeHandler = (event: AlertChangeEventData, sourceGeneration: number) => void

class StubStream {
  phase = 'idle' as 'idle' | 'open'
  generation = 0
  started: number[] = []
  private changeHandlers = new Set<ChangeHandler>()
  private resyncHandlers = new Set<() => void>()

  start(after: number) { this.started.push(after); this.generation += 1; this.phase = 'open' }
  stop() { /* The hook owns this shared stream lifecycle. */ }
  onChange(listener: ChangeHandler) { this.changeHandlers.add(listener); return () => this.changeHandlers.delete(listener) }
  onResync(listener: () => void) { this.resyncHandlers.add(listener); return () => this.resyncHandlers.delete(listener) }
  onPhase() { return () => undefined }
  emit(event: AlertChangeEventData, sourceGeneration = this.generation) { for (const handler of this.changeHandlers) handler(event, sourceGeneration) }
  resync() { for (const handler of this.resyncHandlers) handler() }
  reset() {
    this.phase = 'idle'
    this.generation = 0
    this.started = []
    this.changeHandlers.clear()
    this.resyncHandlers.clear()
  }
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

/** Minimal observer for the public hook result; it deliberately asserts no UI. */
function LiveAlertsHarness({ view, businessSystemKey = '', enabled = true }: { view: 'Firing' | 'Resolved'; businessSystemKey?: string; enabled?: boolean }) {
  const alerts = useLiveAlerts(view, businessSystemKey, enabled)
  return <><output data-testid="projection">{alerts.items.map((item) => `${item.id}:${item.state}:${item.rowVersion}`).join(',')}</output><button type="button" onClick={alerts.refresh}>refresh</button></>
}

function VersionsHarness() {
  const versions = useOccurrenceVersions()
  return <output data-testid="versions">{JSON.stringify([...versions.entries()])}</output>
}

beforeEach(() => {
  stub.reset()
  vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => {
    const url = String(input)
    if (url.startsWith('/api/v1/alerts?')) return jsonResponse({ snapshotSeq: 5, items: [firing] })
    if (url === '/api/v1/alerts/7') return jsonResponse(firing)
    return jsonResponse({ detail: 'not found' }, 404)
  }))
})

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
  vi.clearAllMocks()
})

describe('alert realtime hook projection', () => {
  it('starts from the authoritative snapshot cursor', async () => {
    render(<LiveAlertsHarness view="Firing" />)

    await waitFor(() => expect(stub.started).toEqual([5]))
    expect(screen.getByTestId('projection')).toHaveTextContent('7:Firing:1')
  })

  it('advances occurrence versions monotonically by sequence and row version', async () => {
    render(<VersionsHarness />)
    await act(async () => { await Promise.resolve() })

    act(() => {
      stub.emit({ seq: '6', type: 'state_changed', occurrenceId: '7', rowVersion: 2 })
      stub.emit({ seq: '6', type: 'state_changed', occurrenceId: '7', rowVersion: 3 })
      stub.emit({ seq: '7', type: 'state_changed', occurrenceId: '7', rowVersion: 1 })
    })

    await waitFor(() => expect(screen.getByTestId('versions')).toHaveTextContent('[["7",2]]'))
  })

  it('re-reads a newer occurrence and removes it when it leaves the current projection', async () => {
    let detail = firing
    vi.mocked(fetch).mockImplementation(async (input: RequestInfo | URL) => {
      const url = String(input)
      if (url.startsWith('/api/v1/alerts?')) return jsonResponse({ snapshotSeq: 5, items: [firing] })
      if (url === '/api/v1/alerts/7') return jsonResponse(detail)
      return jsonResponse({ detail: 'not found' }, 404)
    })
    render(<LiveAlertsHarness view="Firing" />)
    await screen.findByText('7:Firing:1')
    detail = resolved

    act(() => stub.emit({ seq: '6', type: 'state_changed', occurrenceId: '7', rowVersion: 2 }))

    await waitFor(() => expect(screen.getByTestId('projection')).toBeEmptyDOMElement())
    const detailReads = vi.mocked(fetch).mock.calls.filter(([input]) => String(input) === '/api/v1/alerts/7').length
    expect(detailReads).toBe(1)
  })

  it('does not re-read a stale or duplicate list event', async () => {
    render(<LiveAlertsHarness view="Firing" />)
    await screen.findByText('7:Firing:1')

    act(() => {
      stub.emit({ seq: '5', type: 'state_changed', occurrenceId: '7', rowVersion: 2 })
      stub.emit({ seq: '6', type: 'state_changed', occurrenceId: '7', rowVersion: 1 })
    })
    await act(async () => { await Promise.resolve() })

    expect(vi.mocked(fetch).mock.calls.filter(([input]) => String(input) === '/api/v1/alerts/7')).toHaveLength(0)
  })

  it('does not start or reconcile its projection while locally paused', async () => {
    render(<LiveAlertsHarness view="Firing" enabled={false} />)
    await act(async () => { await Promise.resolve() })
    expect(stub.started).toEqual([])

    act(() => stub.emit({ seq: '6', type: 'created', occurrenceId: '7', rowVersion: 2 }))
    expect(screen.getByTestId('projection')).toBeEmptyDOMElement()
  })

  it('allows a paused list to take a manual snapshot without starting SSE', async () => {
    render(<LiveAlertsHarness view="Firing" enabled={false} />)
    await act(async () => { await Promise.resolve() })
    expect(stub.started).toEqual([])

    await act(async () => { screen.getByRole('button', { name: 'refresh' }).click() })
    await waitFor(() => expect(screen.getByTestId('projection')).toHaveTextContent('7:Firing:1'))
    expect(stub.started).toEqual([])
  })

  it('rebuilds from a new snapshot following a resync request', async () => {
    let snapshots = 0
    vi.mocked(fetch).mockImplementation(async (input: RequestInfo | URL) => {
      if (String(input).startsWith('/api/v1/alerts?')) {
        snapshots += 1
        return jsonResponse({ snapshotSeq: snapshots === 1 ? 5 : 9, items: snapshots === 1 ? [firing] : [{ ...firing, rowVersion: 2 }] })
      }
      return jsonResponse({ detail: 'not found' }, 404)
    })
    render(<LiveAlertsHarness view="Firing" />)
    await waitFor(() => expect(stub.started).toEqual([5]))

    act(() => stub.resync())

    await waitFor(() => expect(stub.started).toEqual([5, 9]))
    expect(screen.getByTestId('projection')).toHaveTextContent('7:Firing:2')
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

describe('AlertEventStream boundary recovery', () => {
  beforeEach(() => {
    fakeSources = []
    vi.stubGlobal('EventSource', FakeEventSource)
  })

  it('ignores events from a source that a later snapshot replaced', () => {
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

  it('requests a snapshot after terminal failure but preserves native replay while connecting', () => {
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
