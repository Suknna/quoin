import { useEffect, useState } from 'react'
import { listBusinessSystems, type BusinessSystemSummary } from './api'

// The module's second rail (UI-SYSTEM-002): two-line rows — display name and
// Enabled/Disabled first, current config version second. The admin-only
// upload entry opens the full-workbench layer.

interface BusinessSystemsListProps {
  onOpen: (systemKey: string) => void
  onUpload: () => void
  isAdmin: boolean
}

export function BusinessSystemsList({ onOpen, onUpload, isAdmin }: BusinessSystemsListProps) {
  const [items, setItems] = useState<BusinessSystemSummary[]>([])
  const [error, setError] = useState<string | null>(null)
  const [loading, setLoading] = useState(true)

  useEffect(() => {
    let cancelled = false
    void listBusinessSystems()
      .then((systems) => {
        if (cancelled) return
        setItems(systems)
        setError(null)
      })
      .catch((reason: unknown) => {
        if (cancelled) return
        setError(reason instanceof Error ? reason.message : '暂时无法读取业务系统列表，请重试。')
      })
      .finally(() => {
        if (!cancelled) setLoading(false)
      })
    return () => {
      cancelled = true
    }
  }, [])

  if (loading) {
    return <p className="inline-status">正在加载业务系统…</p>
  }
  if (error) {
    return (
      <div className="error-summary" role="alert">
        <strong>暂时无法读取业务系统列表</strong>
        <span>{error}</span>
      </div>
    )
  }
  if (items.length === 0) {
    return (
      <div className="list-empty">
        <p>
          还没有业务系统。
          {isAdmin ? '上传第一份配置 YAML 会创建一个 Disabled 业务系统。' : '等待管理员上传业务系统配置。'}
        </p>
        {isAdmin && (
          <button className="primary-button compact" onClick={onUpload}>
            上传配置
          </button>
        )}
      </div>
    )
  }
  return (
    <div className="business-system-list">
      <div className="list-actions">
        {isAdmin && (
          <button className="primary-button compact" onClick={onUpload}>
            上传配置
          </button>
        )}
      </div>
      {items.map((item) => (
        <button key={item.key} className="business-system-item" onClick={() => onOpen(item.key)}>
          <span className="business-system-title">
            <strong>{item.displayName}</strong>
            <span className={`status-pill ${item.enabled ? 'ok' : 'waiting'}`}>
              {item.enabled ? 'Enabled' : 'Disabled'}
            </span>
          </span>
          <span className="business-system-meta">
            {item.currentConfigVersionId ? `已发布配置 · 时区 ${item.timezone ?? '—'}` : '尚未发布配置'}
          </span>
        </button>
      ))}
    </div>
  )
}
