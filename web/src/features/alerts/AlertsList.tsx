import { useEffect } from 'react'
import { useStreamPhase } from '../../app/realtime/hooks'
import type { AlertOccurrenceSummary } from './api'
import { useLiveAlerts } from './useLiveAlerts'

interface AlertsProps {
  view: 'Firing' | 'Resolved'
  onSelect: (id: string) => void
}

export function AlertsList({ view, onSelect }: AlertsProps) {
  const live = useLiveAlerts(view)
  const phase = useStreamPhase()

  useEffect(() => {
    const onScroll = () => live.setAtTop(window.scrollY < 80)
    onScroll()
    window.addEventListener('scroll', onScroll, { passive: true })
    return () => window.removeEventListener('scroll', onScroll)
  }, [live])

  if (live.error) {
    return (
      <div className="inline-status" role="alert">
        <div><strong>告警列表暂时不可用</strong><p>{live.error}</p></div>
      </div>
    )
  }
  if (live.loading && live.items.length === 0) {
    return <div className="inline-status"><div><strong>正在读取告警…</strong></div></div>
  }
  if (live.items.length === 0 && live.pendingNew === 0) {
    return (
      <div className="inline-status">
        <span className="status-dot waiting" />
        <div>
          <strong>{view === 'Firing' ? '当前没有 Firing 告警' : '还没有已恢复的告警'}</strong>
          <p>{view === 'Firing' ? 'Alertmanager 送达的新告警会实时出现在这里。' : '告警恢复后会进入历史视图。'}</p>
        </div>
      </div>
    )
  }
  return (
    <div className="live-list" aria-busy={phase === 'recovering' || undefined}>
      {live.pendingNew > 0 && (
        <button className="new-content-pill" onClick={live.mergePending}>
          有 {live.pendingNew} 条新告警，点击查看
        </button>
      )}
      <ul className="object-list-items">
        {live.items.map((occurrence) => (
          <AlertRow key={occurrence.id} occurrence={occurrence} onSelect={onSelect} />
        ))}
      </ul>
    </div>
  )
}

function AlertRow({ occurrence, onSelect }: { occurrence: AlertOccurrenceSummary; onSelect: (id: string) => void }) {
  const alertname = occurrence.labels['alertname'] ?? '(无名称)'
  const severity = occurrence.labels['severity']
  const time = formatTime(occurrence.lastStateChangeAt)
  return (
    <li>
      <button className="object-row" onClick={() => onSelect(occurrence.id)}>
        <span className={`alert-state ${occurrence.state === 'Firing' ? 'firing' : 'resolved'}`} aria-label={occurrence.state}>{occurrence.state === 'Firing' ? '●' : '○'}</span>
        <span className="object-row-main">
          <strong>{alertname}</strong>
          <span className="object-row-meta">
            {severity ? <em>{severity}</em> : null}
            <time dateTime={occurrence.lastStateChangeAt}>{time}</time>
          </span>
        </span>
      </button>
    </li>
  )
}

function formatTime(value: string): string {
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return value
  return date.toLocaleString()
}
