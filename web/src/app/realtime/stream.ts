// Realtime alert change stream (HTTP-SSE-003): one app-level EventSource
// manager over /api/v1/alerts/events. The stream is an observation channel
// only — events carry occurrenceId/type/rowVersion, never object bodies;
// consumers re-read details when a version advances.

export interface AlertChangeEventData {
  seq: string
  type: 'created' | 'state_changed'
  occurrenceId: string
  rowVersion: number
}

export type StreamPhase = 'idle' | 'connecting' | 'open' | 'recovering' | 'resync' | 'closed'

type ChangeListener = (event: AlertChangeEventData) => void
type ResyncListener = () => void
type PhaseListener = (phase: StreamPhase) => void

const RETRY_BASE_MS = 1000
const RETRY_MAX_MS = 8000

export class AlertEventStream {
  private source: EventSource | null = null
  private after = 0
  private retryTimer: number | null = null
  private retryAttempt = 0
  private stopped = true
  private changeListeners = new Set<ChangeListener>()
  private resyncListeners = new Set<ResyncListener>()
  private phaseListeners = new Set<PhaseListener>()
  phase: StreamPhase = 'idle'

  /** Starts (or restarts) the stream from the given snapshot cursor. */
  start(after: number): void {
    this.stopped = false
    this.after = after
    this.connect()
  }

  stop(): void {
    this.stopped = true
    this.closeSource()
    if (this.retryTimer !== null) {
      window.clearTimeout(this.retryTimer)
      this.retryTimer = null
    }
    this.setPhase('idle')
  }

  onChange(listener: ChangeListener): () => void {
    this.changeListeners.add(listener)
    return () => this.changeListeners.delete(listener)
  }

  onResync(listener: ResyncListener): () => void {
    this.resyncListeners.add(listener)
    return () => this.resyncListeners.delete(listener)
  }

  onPhase(listener: PhaseListener): () => void {
    this.phaseListeners.add(listener)
    listener(this.phase)
    return () => this.phaseListeners.delete(listener)
  }

  private connect(): void {
    if (this.stopped) return
    this.closeSource()
    this.setPhase(this.retryAttempt > 0 ? 'recovering' : 'connecting')
    const source = new EventSource(`/api/v1/alerts/events?after=${this.after}`)
    this.source = source
    source.addEventListener('open', () => {
      this.retryAttempt = 0
      this.setPhase('open')
    })
    source.addEventListener('change', (event) => {
      const data = parseChangeEvent((event as MessageEvent<string>).data)
      if (!data) return
      this.after = Number(data.seq)
      for (const listener of this.changeListeners) listener(data)
    })
    source.addEventListener('resync_required', () => {
      // Cursor expired beyond the retention window: the list owner re-reads
      // the full snapshot, then restarts the stream from its new cursor
      // (UI-ERROR-001: silent recovery, no user-visible terminology).
      this.closeSource()
      this.setPhase('resync')
      for (const listener of this.resyncListeners) listener()
    })
    source.onerror = () => {
      if (this.stopped) return
      // Capture the state BEFORE closing: EventSource reports CONNECTING
      // while the browser is auto-reconnecting (it re-sends Last-Event-ID
      // on its own) and CLOSED after a fatal failure.
      const wasConnecting = source.readyState === EventSource.CONNECTING
      if (wasConnecting) {
        this.setPhase('recovering')
        return
      }
      this.closeSource()
      // Non-retryable failure (auth loss, hard 410 the browser gave up
      // on, server down). Retry with backoff from the last seen seq; a
      // dead session surfaces through the next snapshot read as 401.
      this.scheduleRetry()
    }
  }

  private scheduleRetry(): void {
    this.setPhase(this.retryAttempt > 0 ? 'recovering' : 'connecting')
    const delay = Math.min(RETRY_BASE_MS * 2 ** this.retryAttempt, RETRY_MAX_MS)
    this.retryAttempt += 1
    this.retryTimer = window.setTimeout(() => {
      this.retryTimer = null
      this.connect()
    }, delay)
  }

  private closeSource(): void {
    if (this.source) {
      this.source.close()
      this.source = null
    }
  }

  private setPhase(phase: StreamPhase): void {
    if (this.phase === phase) return
    this.phase = phase
    for (const listener of this.phaseListeners) listener(phase)
  }
}

function parseChangeEvent(data: string): AlertChangeEventData | null {
  try {
    const parsed = JSON.parse(data) as AlertChangeEventData
    if (typeof parsed.seq !== 'string' || typeof parsed.occurrenceId !== 'string') return null
    if (parsed.type !== 'created' && parsed.type !== 'state_changed') return null
    if (typeof parsed.rowVersion !== 'number') return null
    return parsed
  } catch {
    return null
  }
}

// Test seam: tests replace the constructor with a stub.
export const alertEventStreamFactory = { create: () => new AlertEventStream() }
