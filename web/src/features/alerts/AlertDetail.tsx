import { useEffect, useState } from 'react'
import { useOccurrenceVersions } from '../../app/realtime/hooks'
import { AnalysisDetail } from '../analysis/AnalysisDetail'
import { AnalysisPanel } from '../analysis/AnalysisPanel'
import type { InitialAnalysisDetail } from '../analysis/api'
import { fetchAnalysis } from '../analysis/api'
import type { AlertOccurrenceSummary, ObservationSummary } from './api'
import { fetchObservations, fetchOccurrence } from './api'

export function AlertDetail({ occurrenceId, openAnalysisId, onOpenAnalysis, onCloseAnalysis, onBack }: { occurrenceId: string; openAnalysisId?: string; onOpenAnalysis?: (analysisId: string) => void; onCloseAnalysis?: () => void; onBack: () => void }) {
  const [occurrence, setOccurrence] = useState<AlertOccurrenceSummary | null>(null)
  const [observations, setObservations] = useState<ObservationSummary[]>([])
  const [error, setError] = useState('')
  const [reading, setReading] = useState<InitialAnalysisDetail | null>(null)
  const versions = useOccurrenceVersions()
  const liveVersion = versions.get(occurrenceId)

  // A shared/refreshed URL restores the reading layer (UI-ROUTE-003).
  useEffect(() => {
    if (!openAnalysisId) return
    let cancelled = false
    void fetchAnalysis(occurrenceId, openAnalysisId)
      .then((detail) => { if (!cancelled) setReading(detail) })
      .catch(() => undefined)
    return () => { cancelled = true }
  }, [occurrenceId, openAnalysisId])

  useEffect(() => {
    const cancelled = { value: false }
    void Promise.all([fetchOccurrence(occurrenceId), fetchObservations(occurrenceId)])
      .then(([detail, observationPage]) => {
        if (cancelled.value) return
        setOccurrence(detail)
        setObservations(observationPage.items ?? [])
        setError('')
      })
      .catch((reason: unknown) => {
        if (cancelled.value) return
        setError(reason instanceof Error ? reason.message : '告警详情加载失败')
      })
    return () => { cancelled.value = true }
  }, [occurrenceId])

  useEffect(() => {
    // The event stream only signals "version advanced"; the authoritative
    // body is re-read here (HTTP-SSE-005). liveVersion changes identity only
    // when a strictly newer rowVersion was observed for this occurrence.
    if (liveVersion === undefined || occurrence === null) return
    if (liveVersion <= occurrence.rowVersion) return
    let cancelled = false
    void Promise.all([fetchOccurrence(occurrenceId), fetchObservations(occurrenceId)])
      .then(([detail, observationPage]) => {
        if (cancelled) return
        setOccurrence(detail)
        setObservations(observationPage.items ?? [])
      })
      .catch(() => undefined)
    return () => { cancelled = true }
  }, [liveVersion, occurrenceId, occurrence])

  if (error) {
    return (
      <div className="detail-empty">
        <div className="error-summary" role="alert"><strong>告警详情暂时不可用</strong><span>{error}</span></div>
        <button className="text-button" onClick={onBack}>返回列表</button>
      </div>
    )
  }
  if (!occurrence) {
    return <div className="detail-empty"><p>正在读取告警详情…</p></div>
  }
  const labels = Object.entries(occurrence.labels ?? {})
  return (
    <div className="detail-content">
      <button className="text-button" onClick={onBack}>← 返回列表</button>
      <header className="detail-header">
        <p className="eyebrow">告警详情</p>
        <h1>{String(occurrence.labels?.['alertname'] ?? '(无名称)')}</h1>
        {occurrence.state === 'Resolved' && <span className="status-pill resolved">已恢复</span>}
        {occurrence.state === 'Firing' && <span className="status-pill firing">Firing</span>}
      </header>
      <section className="detail-section" aria-labelledby="labels-title">
        <h2 id="labels-title">标签</h2>
        {labels.length === 0 ? <p className="detail-muted">无标签</p> : (
          <dl className="label-grid">
            {labels.map(([name, value]) => (
              <div key={name}><dt>{name}</dt><dd>{value}</dd></div>
            ))}
          </dl>
        )}
      </section>
      <section className="detail-section" aria-labelledby="timeline-title">
        <h2 id="timeline-title">时间线</h2>
        <p className="detail-muted">首次出现 {formatTime(occurrence.firstSeenAt)} · 最近变化 {formatTime(occurrence.lastStateChangeAt)}</p>
        <ol className="observation-list">
          {observations.map((observation) => (
            <li key={observation.id}>
              <span className={`observation-state ${observation.effect}`}>{observation.observedState}</span>
              <span className="observation-effect">{effectLabel(observation.effect)}</span>
              <time dateTime={observation.committedAt}>{formatTime(observation.committedAt)}</time>
            </li>
          ))}
        </ol>
      </section>
      <AnalysisPanel occurrenceId={occurrenceId} onOpenAnalysis={(detail) => { setReading(detail); onOpenAnalysis?.(detail.id) }} />
      {reading && (
        <div className="reading-overlay">
          <AnalysisDetail occurrenceId={occurrenceId} analysisId={reading.id} onClose={() => { setReading(null); onCloseAnalysis?.() }} />
        </div>
      )}
    </div>
  )
}

function effectLabel(effect: string): string {
  switch (effect) {
    case 'initial_firing': return '首次触发'
    case 'repeat_firing': return '重复触发'
    case 'resolved': return '已恢复'
    case 'resolved_first': return '首次即恢复'
    case 'late_firing_after_resolved': return '恢复后迟到触发'
    default: return effect
  }
}

function formatTime(value: string): string {
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? value : date.toLocaleString()
}
