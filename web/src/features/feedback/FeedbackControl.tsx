import { useEffect, useState } from 'react'
import {
  appendFeedback, fetchFeedback, feedbackValueLabels,
  type FeedbackEvent, type FeedbackTarget, type FeedbackValue,
} from './api'

// FeedbackControl is the “记录实际结果” control under every immutable
// diagnosis output (UI-FEEDBACK-001/002): four closed values, an optional
// bounded note, the latest value in place, and the full append-only
// history behind one toggle. Only “不采纳” confirms first — it permanently
// retires source-derived knowledge (DATA-TX-011).

interface FeedbackControlProps {
  target: FeedbackTarget
}

const valueOrder: FeedbackValue[] = ['adopted', 'executed', 'verified_effective', 'rejected']

export function FeedbackControl({ target }: FeedbackControlProps) {
  const [latest, setLatest] = useState<FeedbackValue | null>(null)
  const [items, setItems] = useState<FeedbackEvent[] | null>(null)
  const [expanded, setExpanded] = useState(false)
  const [note, setNote] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [confirmingRejection, setConfirmingRejection] = useState(false)
  const [rejectionNote, setRejectionNote] = useState('')
  const [loaded, setLoaded] = useState(false)

  useEffect(() => {
    let cancelled = false
    setLoaded(false)
    void fetchFeedback(target)
      .then((timeline) => {
        if (cancelled) return
        setLatest(timeline.latestValue ?? null)
        setItems(timeline.items)
        setLoaded(true)
      })
      .catch(() => {
        if (!cancelled) setLoaded(true)
      })
    return () => { cancelled = true }
  }, [target.type, target.id]) // eslint-disable-line react-hooks/exhaustive-deps

  async function record(value: FeedbackValue, withNote: string) {
    setBusy(true)
    setError('')
    try {
      await appendFeedback(target, value, withNote)
      const timeline = await fetchFeedback(target)
      setLatest(timeline.latestValue ?? null)
      setItems(timeline.items)
      setNote('')
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : '暂时无法记录反馈，请重试。')
    } finally {
      setBusy(false)
    }
  }

  return (
    <section className="feedback-control" aria-label="记录实际结果">
      <div className="feedback-heading">
        <strong>实际结果</strong>
        {latest && <span className={`status-pill feedback-${latest}`}>{feedbackValueLabels[latest]}</span>}
      </div>
      {loaded && items === null && <p className="detail-muted">反馈记录暂时不可读。</p>}
      {loaded && items?.length === 0 && <p className="detail-muted">还没有记录过这条诊断的实际结果。</p>}
      <div className="feedback-values" role="group" aria-label="选择实际结果">
        {valueOrder.map((value) => (
          <button
            key={value}
            type="button"
            className={`feedback-value${latest === value ? ' active' : ''}`}
            disabled={busy}
            onClick={() => {
              if (value === 'rejected') {
                setConfirmingRejection(true)
                return
              }
              void record(value, note.trim())
            }}
          >
            {feedbackValueLabels[value]}
          </button>
        ))}
      </div>
      <input
        className="feedback-note"
        value={note}
        maxLength={4096}
        placeholder="可选说明（会随本次记录一起保存）"
        aria-label="反馈说明"
        onChange={(event) => setNote(event.target.value)}
      />
      {error && <p className="field-error" role="alert">{error}</p>}
      {items && items.length > 0 && (
        <button type="button" className="text-button" aria-expanded={expanded} onClick={() => setExpanded(!expanded)}>
          {expanded ? '收起反馈历史' : `查看反馈历史（${items.length} 条）`}
        </button>
      )}
      {expanded && items && (
        <ol className="feedback-history" aria-label="反馈历史">
          {items.map((event) => (
            <li key={event.id}>
              <span className={`status-pill feedback-${event.value}`}>{feedbackValueLabels[event.value]}</span>
              {event.note && <span className="feedback-history-note">{event.note}</span>}
              <time dateTime={event.createdAt}>{formatTime(event.createdAt)}</time>
            </li>
          ))}
        </ol>
      )}
      {confirmingRejection && (
        <div className="dialog-scrim" role="presentation" onClick={() => setConfirmingRejection(false)}>
          <div className="dialog-panel" role="dialog" aria-modal="true" aria-labelledby="feedback-reject-title" onClick={(event) => event.stopPropagation()}>
            <h3 id="feedback-reject-title">确认标记为“不采纳”？</h3>
            <p>
              相关未确认的知识候选会立即变为“来源无效”；已确认的知识版本将永久退出检索，
              不会自动恢复。如需再次使用，需要重新整理并确认新知识。
            </p>
            <textarea
              className="feedback-note"
              value={rejectionNote}
              maxLength={4096}
              placeholder="可选：说明不采纳的原因"
              aria-label="不采纳说明"
              onChange={(event) => setRejectionNote(event.target.value)}
            />
            <div className="dialog-actions">
              <button type="button" className="secondary-button" onClick={() => setConfirmingRejection(false)}>取消</button>
              <button
                type="button"
                className="primary-button danger"
                disabled={busy}
                onClick={() => {
                  setConfirmingRejection(false)
                  void record('rejected', rejectionNote.trim()).then(() => setRejectionNote(''))
                }}
              >
                {busy ? '正在记录…' : '确认不采纳'}
              </button>
            </div>
          </div>
        </div>
      )}
    </section>
  )
}

function formatTime(value: string): string {
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? value : date.toLocaleString()
}
