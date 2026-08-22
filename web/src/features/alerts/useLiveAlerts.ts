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

export function useLiveAlerts(view: 'Firing' | 'Resolved', businessSystemKey = ''): LiveAlerts {
  const stream = useAlertEventStream()
  const [items, setItems] = useState<AlertOccurrenceSummary[]>([])
  const [pending, setPending] = useState<AlertOccurrenceSummary[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const versions = useRef(new Map<string, number>())
  const atTopRef = useRef(true)
  const pendingRef = useRef<AlertOccurrenceSummary[]>([])
  const generationRef = useRef(0)
  const projectionReadyRef = useRef(false)
  const lastSeqRef = useRef(0)
  // Detail reads are serialized in event order. A later event cannot commit
  // ahead of an older read, and a failed read forces a snapshot rebuild before
  // any queued event may be treated as applied.
  const eventQueueRef = useRef<Promise<void>>(Promise.resolve())
  pendingRef.current = pending
  const viewRef = useRef(view)
  viewRef.current = view
  const filterRef = useRef(businessSystemKey)
  filterRef.current = businessSystemKey
  const projectionKey = `${view}\u0000${businessSystemKey}`
  const renderedProjectionKeyRef = useRef(projectionKey)
  // React updates refs during render before its effects run. Invalidate the
  // old projection here so a browser SSE task cannot race that small window
  // and apply an event under the newly rendered filter.
  if (renderedProjectionKeyRef.current !== projectionKey) {
    renderedProjectionKeyRef.current = projectionKey
    generationRef.current += 1
    projectionReadyRef.current = false
  }

  const loadSnapshot = useCallback(
    async (snapshotView: 'Firing' | 'Resolved', snapshotBusinessSystemKey: string, clearVisibleProjection: boolean) => {
      const generation = generationRef.current + 1
      generationRef.current = generation
      projectionReadyRef.current = false
      versions.current = new Map()
      if (clearVisibleProjection) {
        setItems([])
        setPending([])
      }
      setLoading(true)
      try {
        const snapshot = await fetchAlerts(snapshotView, snapshotBusinessSystemKey)
        if (generation !== generationRef.current) return
        versions.current = new Map(snapshot.items.map((item) => [item.id, item.rowVersion]))
        lastSeqRef.current = snapshot.snapshotSeq
        setItems(snapshot.items)
        setPending([])
        setError('')
        // Every snapshot, including a view/filter switch, owns a fresh SSE
        // boundary. Closing the old source before start makes a delayed old
        // event structurally unable to advance this projection's cursor.
        projectionReadyRef.current = true
        stream.start(snapshot.snapshotSeq)
      } catch (reason) {
        if (generation !== generationRef.current) return
        setError(reason instanceof Error ? reason.message : '告警列表加载失败')
      } finally {
        if (generation === generationRef.current) setLoading(false)
      }
    },
    [stream],
  )

  useEffect(() => {
    void loadSnapshot(view, businessSystemKey, true)
  }, [loadSnapshot, view, businessSystemKey])

  useEffect(() => {
    return stream.onResync(() => {
      // Silent full re-read (UI-ERROR-001): cursor expiry and terminal SSE
      // failures heal through a fresh snapshot rather than a stale retry.
      void loadSnapshot(viewRef.current, filterRef.current, false)
    })
  }, [stream, loadSnapshot])

  useEffect(() => {
    let cancelled = false
    const applyEvent = async (event: { seq: string; type: 'created' | 'state_changed'; occurrenceId: string; rowVersion: number }, eventGeneration: number, sourceGeneration: number) => {
      if (!projectionReadyRef.current || eventGeneration !== generationRef.current || sourceGeneration !== stream.generation) return
      const seq = Number(event.seq)
      if (seq <= lastSeqRef.current) return
      if ((versions.current.get(event.occurrenceId) ?? 0) >= event.rowVersion) {
        lastSeqRef.current = seq
        return
      }
      let detail: AlertOccurrenceSummary
      try {
        detail = await fetchOccurrence(event.occurrenceId)
      } catch {
        // Do not advance the local cursor for an unapplied event. The stream
        // cursor is only transport state, so rebuilding from an HTTP snapshot
        // is the sole authority-safe recovery path.
        if (!cancelled && eventGeneration === generationRef.current) stream.resync()
        return
      }
      if (cancelled || !projectionReadyRef.current || eventGeneration !== generationRef.current) return
      // A detail older than the change which requested it cannot be made
      // current by waiting; drop the projection and replay from a snapshot.
      if (detail.rowVersion < event.rowVersion) {
        stream.resync()
        return
      }
      const knownVersion = versions.current.get(detail.id) ?? 0
      if (knownVersion >= detail.rowVersion) {
        lastSeqRef.current = seq
        return
      }
      versions.current.set(detail.id, detail.rowVersion)
      lastSeqRef.current = seq
      // The mechanical filter mirrors the server-side businessSystemKey
      // projection: events only carry ids, so the re-read detail decides
      // membership in the filtered view (未归属 rows only match no filter).
      if (filterRef.current !== '' && (detail.businessSystemKey ?? '') !== filterRef.current) {
        setItems((previous) => previous.filter((item) => item.id !== detail.id))
        setPending((previous) => previous.filter((item) => item.id !== detail.id))
        return
      }
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
    }
    const unsubscribe = stream.onChange((event, sourceGeneration) => {
      // Capture before enqueue: an old EventSource callback must never become
      // eligible merely because a newer view has finished its snapshot.
      const eventGeneration = generationRef.current
      eventQueueRef.current = eventQueueRef.current
        .then(() => applyEvent(event, eventGeneration, sourceGeneration))
        .catch(() => {
          if (!cancelled) stream.resync()
        })
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
