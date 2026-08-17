import { useEffect, useState } from 'react'
import type { AlertOccurrenceSummary, AlertSnapshot } from './api'
import { fetchAlerts } from './api'

interface AlertsProps {
  onSelect: (id: string) => void
}

export function AlertsList({ onSelect }: AlertsProps) {
  const [snapshot, setSnapshot] = useState<AlertSnapshot | null>(null)
  const [error, setError] = useState('')

  useEffect(() => {
    const cancelled = { value: false }
    void fetchAlerts()
      .then((snapshot) => { if (!cancelled.value) setSnapshot(snapshot) })
      .catch((reason: unknown) => { if (!cancelled.value) setError(reason instanceof Error ? reason.message : '告警列表加载失败') })
    return () => { cancelled.value = true }
  }, [])

  if (error) {
    return (
      <div className="inline-status" role="alert">
        <div><strong>告警列表暂时不可用</strong><p>{error}</p></div>
      </div>
    )
  }
  if (!snapshot) {
    return <div className="inline-status"><div><strong>正在读取告警…</strong></div></div>
  }
  const items = snapshot.items ?? []
  if (items.length === 0) {
    return (
      <div className="inline-status">
        <span className="status-dot waiting" />
        <div><strong>当前没有 Firing 告警</strong><p>Alertmanager 送达的新告警会出现在这里。</p></div>
      </div>
    )
  }
  return (
    <ul className="object-list-items">
      {items.map((occurrence) => (
        <AlertRow key={occurrence.id} occurrence={occurrence} onSelect={onSelect} />
      ))}
    </ul>
  )
}

function AlertRow({ occurrence, onSelect }: { occurrence: AlertOccurrenceSummary; onSelect: (id: string) => void }) {
  const alertname = occurrence.labels['alertname'] ?? '(无名称)'
  const severity = occurrence.labels['severity']
  const time = formatTime(occurrence.lastStateChangeAt)
  return (
    <li>
      <button className="object-row" onClick={() => onSelect(occurrence.id)}>
        <span className="alert-state firing" aria-label="Firing">●</span>
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
