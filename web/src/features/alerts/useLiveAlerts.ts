// Live alert list projection: HTTP snapshot first, then the shared SSE change
// stream (HTTP-SSE-003). Events never carry bodies — the hook re-reads the
// occurrence detail when a newer rowVersion arrives and reconciles the list
// in place (UI-LIST-003), buffering new rows behind an explicit merge so the
// reading position is never disturbed (UI-LIST-002).

import { useCallback, useEffect, useRef, useState } from 'react'
import { useAlertEventStream } from '../../app/realtime/hooks'
import { fetchAlerts, fetchOccurrence, type AlertOccurrenceSummary } from './api'

export interface LiveAlerts {
  items: AlertOccurrenceSummary[]
  loading: boolean
  error: string
  pendingNew: number
  mergePending: () => void
  setAtTop: (value: boolean) => void
}

export function useLiveAlerts(view: 'Firing' | 'Resolved'): LiveAlerts {
  const stream = useAlertEventStream()
  const [items, setItems] = useState<AlertOccurrenceSummary[]>([])
  const [pending, setPending] = useState<AlertOccurrenceSummary[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const versions = useRef(new Map<string, number>())
  const atTopRef = useRef(true)
  const pendingRef = useRef<AlertOccurrenceSummary[]>([])
  pendingRef.current = pending
  const viewRef = useRef(view)
  viewRef.current = view

  const loadSnapshot = useCallback(async () => {
    setLoading(true)
    try {
      const snapshot = await fetchAlerts(viewRef.current)
      versions.current = new Map(snapshot.items.map((item) => [item.id, item.rowVersion]))
      setItems(snapshot.items)
      setError('')
      // HTTP-SSE-003: the stream starts from the fresh snapshot cursor; a
      // stream that is already running keeps its own newer cursor.
      if (stream.phase === 'idle' || stream.phase === 'resync' || stream.phase === 'closed') {
        stream.start(snapshot.snapshotSeq)
      }
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : '告警列表加载失败')
    } finally {
      setLoading(false)
    }
  }, [stream])

  useEffect(() => {
    versions.current = new Map()
    setItems([])
    setPending([])
    void loadSnapshot()
  }, [loadSnapshot, view])

  useEffect(() => {
    return stream.onResync(() => {
      // Silent full re-read (UI-ERROR-001): cursor expiry heals via snapshot.
      void loadSnapshot()
    })
  }, [stream, loadSnapshot])

  useEffect(() => {
    let lastSeq = 0
    let cancelled = false
    const unsubscribe = stream.onChange(async (event) => {
      const seq = Number(event.seq)
      if (seq <= lastSeq) return
      lastSeq = seq
      if ((versions.current.get(event.occurrenceId) ?? 0) >= event.rowVersion) return
      let detail: AlertOccurrenceSummary
      try {
        detail = await fetchOccurrence(event.occurrenceId)
      } catch {
        return // transient read failure; resync/snapshot heals (UI-ERROR-001)
      }
      if (cancelled) return
      versions.current.set(detail.id, detail.rowVersion)
      if (detail.state !== viewRef.current) {
        // Left this view (e.g. resolved): row leaves the list; an open
        // detail URL keeps rendering with its own re-read (CONTEXT 实时投影).
        setItems((previous) => previous.filter((item) => item.id !== detail.id))
        setPending((previous) => previous.filter((item) => item.id !== detail.id))
        return
      }
      if (event.type === 'created') {
        if (atTopRef.current) {
          setItems((previous) => [detail, ...previous.filter((item) => item.id !== detail.id)])
        } else {
          // New occurrence buffers behind the merge control (UI-LIST-002).
          setPending((previous) => [detail, ...previous.filter((item) => item.id !== detail.id)])
        }
        return
      }
      // state_changed: in-place reconcile keeps the row position (UI-LIST-003);
      // an occurrence that just ENTERED this view (e.g. newly resolved rows
      // joining the history view) is added at the top instead.
      setItems((previous) => {
        if (previous.some((item) => item.id === detail.id)) {
          return previous.map((item) => (item.id === detail.id ? detail : item))
        }
        return [detail, ...previous]
      })
      setPending((previous) => previous.filter((item) => item.id !== detail.id))
    })
    return () => {
      cancelled = true
      unsubscribe()
    }
  }, [stream])

  const mergePending = useCallback(() => {
    const buffered = pendingRef.current
    if (buffered.length > 0) {
      setItems((previous) => [...buffered, ...previous.filter((item) => !buffered.some((pendingItem) => pendingItem.id === item.id))])
      setPending([])
    }
    window.scrollTo({ top: 0 })
  }, [])

  const setAtTop = useCallback((value: boolean) => {
    atTopRef.current = value
  }, [])

  return { items, loading, error, pendingNew: pending.length, mergePending, setAtTop }
}
