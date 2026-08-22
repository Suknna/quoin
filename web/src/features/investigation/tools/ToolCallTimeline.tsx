import { useEffect, useState } from 'react'
import { listToolCalls, fetchEvidence, artifactDownloadURL, toolNameLabel, type ToolCallItem, type EvidenceDetail } from './api'
import '../investigation.css'

// ToolCallTimeline renders one attempt's persisted tool-call cards
// (UI-CHAT-004): tool name, real state, duration/terminal facts and an
// ordinary-language summary; raw arguments, outputs and diagnostics fold
// in place. While the attempt is active it refreshes from the durable
// timeline; evidence links expand inline (the model's conclusions stay in
// the message text — the program only projects facts).

interface ToolCallTimelineProps {
  investigationId: string
  attemptId: string
  active: boolean
}

function statusLabel(status: ToolCallItem['status']): string {
  switch (status) {
    case 'pending': return '排队中'
    case 'running': return '执行中'
    case 'succeeded': return '已完成'
    case 'failed': return '执行失败'
    case 'cancelled': return '已取消'
    default: return status
  }
}

function durationOf(item: ToolCallItem): string {
  if (!item.startedAt || !item.endedAt) return ''
  const started = Date.parse(item.startedAt)
  const ended = Date.parse(item.endedAt)
  if (Number.isNaN(started) || Number.isNaN(ended) || ended < started) return ''
  const millis = ended - started
  if (millis < 1000) return `${millis} ms`
  return `${(millis / 1000).toFixed(1)} 秒`
}

// resultSummary projects the bounded machine facts of one committed tool
// result; it never derives a verdict.
function resultSummary(item: ToolCallItem): string {
  if (item.status === 'failed' || item.status === 'cancelled') {
    return item.errorDetail || '工具执行失败。'
  }
  const result = item.result
  if (!result) return '等待执行结果。'
  const parts: string[] = []
  if (typeof result.output === 'string') {
    const text = result.output.replace(/\s+/g, ' ').trim()
    parts.push(text.length > 120 ? `${text.slice(0, 120)}…` : text)
  }
  if (typeof result.totalBytes === 'number') parts.push(`共 ${result.totalBytes} 字节`)
  if (typeof result.totalLines === 'number') parts.push(`${result.totalLines} 行`)
  if (result.truncated === true) parts.push('完整输出已存为产物')
  if (typeof result.matchCount === 'number') parts.push(`${result.matchCount} 处匹配`)
  if (typeof result.sampleCount === 'number') parts.push(`${result.sampleCount} 个样本`)
  return parts.length > 0 ? parts.join(' · ') : '已执行完成。'
}

// EvidenceLink expands one evidence id inline: observation time,
// integrity flag and the artifact download when the body is an artifact
// (UI-CHAT-009 facts; conclusions stay in the message text).
export function EvidenceLink({ evidenceId }: { evidenceId: string }) {
  const [detail, setDetail] = useState<EvidenceDetail | null>(null)
  const [open, setOpen] = useState(false)
  const [error, setError] = useState<string | null>(null)
  useEffect(() => {
    if (!open || detail || error) return
    fetchEvidence(evidenceId).then(setDetail).catch(() => setError('证据详情读取失败。'))
  }, [open, detail, error, evidenceId])
  return (
    <div className="tool-evidence">
      <button type="button" className="tool-evidence-toggle" aria-expanded={open} onClick={() => setOpen((value) => !value)}>
        证据 #{evidenceId}
      </button>
      {open && (
        <div className="tool-evidence-detail">
          {error && <p className="field-error" role="alert">{error}</p>}
          {detail && (
            <>
              <p>
                观测时间 {detail.observedAt}
                {detail.integrity === 'incomplete' && ' · 完整性标记：不完整'}
              </p>
              {detail.body.kind === 'artifact' && (
                <a className="message-attachment" href={artifactDownloadURL(detail.body.artifact.id)} download>
                  <span className="attachment-chip-icon" aria-hidden="true">▤</span>
                  <span className="attachment-chip-name">产物 #{detail.body.artifact.id}</span>
                  <span className="attachment-chip-size">{detail.body.artifact.sizeBytes} 字节</span>
                </a>
              )}
            </>
          )}
        </div>
      )}
    </div>
  )
}

function ToolCallCard({ item }: { item: ToolCallItem }) {
  const [open, setOpen] = useState(false)
  return (
    <div className={`tool-call-card ${item.status}`}>
      <button type="button" className="tool-call-summary" aria-expanded={open} onClick={() => setOpen((value) => !value)}>
        <span className="tool-call-name">{toolNameLabel(item.toolName)}</span>
        <span className="tool-call-status">{statusLabel(item.status)}</span>
        {durationOf(item) !== '' && <span className="tool-call-duration">{durationOf(item)}</span>}
        <span className="tool-call-preview">{resultSummary(item)}</span>
      </button>
      {open && (
        <div className="tool-call-details">
          <section>
            <h4>调用参数</h4>
            <pre className="tool-call-json">{JSON.stringify(item.arguments, null, 2)}</pre>
          </section>
          {item.result && (
            <section>
              <h4>执行结果</h4>
              <pre className="tool-call-json">{JSON.stringify(item.result, null, 2)}</pre>
            </section>
          )}
          {item.resultArtifactId && (
            <a className="message-attachment" href={artifactDownloadURL(item.resultArtifactId)} download>
              <span className="attachment-chip-icon" aria-hidden="true">▤</span>
              <span className="attachment-chip-name">完整输出产物 #{item.resultArtifactId}</span>
            </a>
          )}
        </div>
      )}
    </div>
  )
}

export function ToolCallTimeline({ investigationId, attemptId, active }: ToolCallTimelineProps) {
  const [items, setItems] = useState<ToolCallItem[] | null>(null)
  const [error, setError] = useState<string | null>(null)
  useEffect(() => {
    let cancelled = false
    let timer: number | undefined
    const refresh = () => {
      listToolCalls(investigationId, attemptId)
        .then((next) => {
          if (!cancelled) {
            setItems(next)
            setError(null)
          }
        })
        .catch(() => {
          if (!cancelled && !active) setError('工具调用记录读取失败。')
        })
        .finally(() => {
          if (!cancelled && active) timer = window.setTimeout(refresh, 1500)
        })
    }
    refresh()
    return () => {
      cancelled = true
      if (timer !== undefined) window.clearTimeout(timer)
    }
  }, [investigationId, attemptId, active])
  if (error) return <p className="field-error" role="alert">{error}</p>
  if (!items) return null
  if (items.length === 0) return null
  return (
    <div className="tool-call-timeline" aria-label="本轮工具调用">
      {items.map((item) => (
        <ToolCallCard key={item.id} item={item} />
      ))}
    </div>
  )
}
