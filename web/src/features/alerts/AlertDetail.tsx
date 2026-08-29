import { useEffect, useRef, useState } from 'react'
import { useOccurrenceVersions } from '../../app/realtime/hooks'
import { AnalysisDetail } from '../analysis/AnalysisDetail'
import { AnalysisPanel } from '../analysis/AnalysisPanel'
import type { InitialAnalysisDetail } from '../analysis/api'
import { fetchAnalysis } from '../analysis/api'
import type { AlertOccurrenceSummary, ObservationSummary } from './api'
import { fetchObservations, fetchOccurrence } from './api'

export function AlertDetail({ occurrenceId, openAnalysisId, onOpenAnalysis, onCloseAnalysis, onBack, onStartInvestigation, onOpenKnowledgeCandidate }: { occurrenceId: string; openAnalysisId?: string; onOpenAnalysis?: (analysisId: string) => void; onCloseAnalysis?: () => void; onBack: () => void; onStartInvestigation?: () => void; onOpenKnowledgeCandidate?: (candidateId: string) => void }) {
  const [occurrence, setOccurrence] = useState<AlertOccurrenceSummary | null>(null)
  const [observations, setObservations] = useState<ObservationSummary[]>([])
  const [error, setError] = useState('')
  const [reading, setReading] = useState<InitialAnalysisDetail | null>(null)
  const [refreshRetry, setRefreshRetry] = useState(0)
  const retryVersionRef = useRef<number | undefined>(undefined)
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
    // body is re-read here (HTTP-SSE-005). Keep the last known detail visible
    // while a bounded retry heals a transient 503 rather than silently leaving
    // a stale projection forever.
    if (liveVersion === undefined || occurrence === null) return
    if (liveVersion <= occurrence.rowVersion) return
    const retryAttempt = retryVersionRef.current === liveVersion ? refreshRetry : 0
    retryVersionRef.current = liveVersion
    let cancelled = false
    let retryTimer: ReturnType<typeof setTimeout> | undefined
    void Promise.all([fetchOccurrence(occurrenceId), fetchObservations(occurrenceId)])
      .then(([detail, observationPage]) => {
        if (cancelled) return
        if (detail.rowVersion < liveVersion) {
          throw new Error('告警详情尚未包含最新变更。')
        }
        setOccurrence(detail)
        setObservations(observationPage.items ?? [])
        setError('')
      })
      .catch(() => {
        if (cancelled) return
        if (retryAttempt < 2) {
          retryTimer = setTimeout(() => setRefreshRetry(retryAttempt + 1), 250 * (retryAttempt + 1))
          return
        }
        setError('无法读取最新告警详情，请返回列表后重试。')
      })
    return () => {
      cancelled = true
      if (retryTimer) clearTimeout(retryTimer)
    }
  }, [liveVersion, occurrenceId, occurrence, refreshRetry])

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
              {onStartInvestigation && (
                <button className="secondary-button compact" onClick={onStartInvestigation}>发起调查</button>
              )}
            </header>
            <section className="detail-section" aria-labelledby="attribution-title">
              <h2 id="attribution-title">归属</h2>
              <p className="detail-muted">
                {occurrence.businessSystemKey ? `业务系统 ${occurrence.businessSystemKey}` : '未归属（没有匹配的业务系统）'}
              </p>
            </section>
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
          <AnalysisDetail occurrenceId={occurrenceId} analysisId={reading.id} onClose={() => { setReading(null); onCloseAnalysis?.() }} onOpenCandidate={(candidateId) => onOpenKnowledgeCandidate?.(candidateId)} />
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
