// React bindings for the alert change stream: a provider-less singleton hook
// so the list, the detail pane and the navigation badge all observe the same
// EventSource without prop drilling.

import { useEffect, useState } from 'react'
import { alertEventStreamFactory, type AlertChangeEventData, type AlertEventStream, type StreamPhase } from './stream'

let shared: AlertEventStream | null = null
let refCount = 0

function acquire(): AlertEventStream {
  if (!shared) shared = alertEventStreamFactory.create()
  return shared
}

function release(): void {
  if (refCount === 0 && shared) {
    shared.stop()
    shared = null
  }
}

/**
 * Subscribes the calling component to change events and keeps the shared
 * stream alive while at least one observer exists.
 */
export function useAlertEventStream(): AlertEventStream {
  const [stream] = useState(acquire)
  useEffect(() => {
    refCount += 1
    return () => {
      refCount -= 1
      release()
    }
  }, [])
  return stream
}

/** Latest row version per occurrence, advanced idempotently by seq/version. */
export function useOccurrenceVersions(): Map<string, number> {
  const stream = useAlertEventStream()
  const [versions, setVersions] = useState<Map<string, number>>(() => new Map())
  useEffect(() => {
    let lastSeq = 0
    return stream.onChange((event: AlertChangeEventData) => {
      const seq = Number(event.seq)
      // Idempotent apply (HTTP-SSE-003): stale or duplicate seqs never move
      // the projection backwards; only a newer rowVersion re-reads details.
      if (seq <= lastSeq) return
      lastSeq = seq
      setVersions((previous) => {
        if ((previous.get(event.occurrenceId) ?? 0) >= event.rowVersion) return previous
        const next = new Map(previous)
        next.set(event.occurrenceId, event.rowVersion)
        return next
      })
    })
  }, [stream])
  return versions
}

/** Live stream phase for status strips (never user-facing terminology). */
export function useStreamPhase(): StreamPhase {
  const stream = useAlertEventStream()
  const [phase, setPhase] = useState<StreamPhase>(stream.phase)
  useEffect(() => stream.onPhase(setPhase), [stream])
  return phase
}
