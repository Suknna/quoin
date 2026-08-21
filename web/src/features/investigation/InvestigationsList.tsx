import { useEffect, useState } from 'react'
import { api, type InvestigationSummary } from './api'

// InvestigationsList is the module's second rail (UI-CHAT-001): the
// server-derived displayTitle and lastActivityAt only — no writable
// titles, no model title calls.

interface InvestigationsListProps {
  onOpen: (investigationId: string) => void
  onNew: () => void
}

export function InvestigationsList({ onOpen, onNew }: InvestigationsListProps) {
  const [items, setItems] = useState<InvestigationSummary[]>([])
  const [error, setError] = useState<string | null>(null)
  const [loading, setLoading] = useState(true)

  useEffect(() => {
    let cancelled = false
    void api.list()
      .then((page) => {
        if (cancelled) return
        setItems(page.items)
        setError(null)
      })
      .catch((reason: unknown) => {
        if (cancelled) return
        setError(reason instanceof Error ? reason.message : '暂时无法读取调查列表，请重试。')
      })
      .finally(() => {
        if (!cancelled) setLoading(false)
      })
    return () => {
      cancelled = true
    }
  }, [])

  if (loading) {
    return <p className="inline-status">正在加载调查…</p>
  }
  if (error) {
    return (
      <div className="error-summary" role="alert">
        <strong>暂时无法读取调查列表</strong>
        <span>{error}</span>
      </div>
    )
  }
  if (items.length === 0) {
    return (
      <div className="list-empty">
        <p>还没有调查。从空白调查开始，或从告警/初步分析进入。</p>
        <button className="primary-button compact" onClick={onNew}>新建调查</button>
      </div>
    )
  }
  return (
    <div className="investigation-list">
      <div className="list-actions">
        <button className="primary-button compact" onClick={onNew}>新建调查</button>
      </div>
      {items.map((item) => (
        <button key={item.id} className="investigation-item" onClick={() => onOpen(item.id)}>
          <span className="investigation-title">{item.displayTitle}</span>
          <span className="investigation-activity" title={item.lastActivityAt}>
            {new Date(item.lastActivityAt).toLocaleString()}
          </span>
        </button>
      ))}
    </div>
  )
}
