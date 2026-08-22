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

type ChangeListener = (event: AlertChangeEventData, sourceGeneration: number) => void
type ResyncListener = () => void
type PhaseListener = (phase: StreamPhase) => void

export class AlertEventStream {
  private source: EventSource | null = null
  private after = 0
  private sourceGeneration = 0
  private stopped = true
  private changeListeners = new Set<ChangeListener>()
  private resyncListeners = new Set<ResyncListener>()
  private phaseListeners = new Set<PhaseListener>()
  phase: StreamPhase = 'idle'

  /** Starts (or restarts) the stream from the given snapshot cursor. */
  start(after: number): void {
    this.stopped = false
    this.after = after
    this.sourceGeneration += 1
    this.connect(this.sourceGeneration)
  }

  /** Identifies the EventSource that belongs to the current snapshot. */
  get generation(): number {
    return this.sourceGeneration
  }

  stop(): void {
    this.stopped = true
    this.closeSource()
    this.setPhase('idle')
  }

  /** Drops the current source and asks snapshot owners to rebuild projection. */
  resync(): void {
    if (this.stopped) return
    this.requestResync()
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

  private connect(sourceGeneration: number): void {
    if (this.stopped || sourceGeneration !== this.sourceGeneration) return
    this.closeSource()
    this.setPhase('connecting')
    const source = new EventSource(`/api/v1/alerts/events?after=${this.after}`)
    this.source = source
    source.addEventListener('open', () => {
      if (!this.owns(source)) return
      this.setPhase('open')
    })
    source.addEventListener('change', (event) => {
      if (!this.owns(source)) return
      const data = parseChangeEvent((event as MessageEvent<string>).data)
      if (!data) return
      this.after = Number(data.seq)
      for (const listener of this.changeListeners) listener(data, sourceGeneration)
    })
    source.addEventListener('resync_required', () => {
      if (!this.owns(source)) return
      this.requestResync()
    })
    source.onerror = () => {
      if (!this.owns(source)) return
      // CONNECTING means the browser retained the EventSource and is safely
      // replaying from Last-Event-ID itself. A terminal failure has no usable
      // status/body exposed to JavaScript (including HTTP 410), so it must
      // take the same snapshot-resync path rather than retry a stale cursor.
      if (source.readyState === EventSource.CONNECTING) {
        this.setPhase('recovering')
        return
      }
      this.requestResync()
    }
  }

  private requestResync(): void {
    this.closeSource()
    this.setPhase('resync')
    for (const listener of this.resyncListeners) listener()
  }

  private owns(source: EventSource): boolean {
    return !this.stopped && this.source === source
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
