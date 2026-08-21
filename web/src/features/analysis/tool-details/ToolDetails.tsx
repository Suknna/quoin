import { useEffect, useState } from 'react'
import { artifactDownloadURL, evidenceParamsText, fetchEvidence, toolNameLabel, type EvidenceDetail } from './api'
import './ToolDetails.css'

// ToolDetails is the analysis reading layer's evidence/tool section
// (UI-READING-001, UI-CHAT-009): the sealed Evidence of the succeeded
// output, each bound to the tool call that produced it, with the real
// observation params, the collection time, the integrity and exactly one
// body projection — the bounded inline JSON or the Artifact download
// entry. The program renders deterministic facts only; the model's
// conclusion lives in the analysis output above.
export function ToolDetails({ evidenceIds }: { evidenceIds: string[] }) {
  const [loaded, setLoaded] = useState<{ key: string; items: EvidenceDetail[]; error: string }>({ key: '', items: [], error: '' })
  const key = evidenceIds.join(',')

  useEffect(() => {
    let cancelled = false
    void Promise.all(evidenceIds.map((id) => fetchEvidence(id)))
      .then((next) => { if (!cancelled) setLoaded({ key, items: next, error: '' }) })
      .catch((reason: unknown) => {
        if (!cancelled) setLoaded({ key, items: [], error: reason instanceof Error ? reason.message : '证据详情加载失败' })
      })
    return () => { cancelled = true }
  }, [evidenceIds, key])

  const items = loaded.key === key ? loaded.items : []
  const error = loaded.key === key ? loaded.error : ''

  if (evidenceIds.length === 0) {
    return <p className="detail-muted">该结论没有引用外部观测证据。</p>
  }
  if (error) {
    return <div className="error-summary" role="alert"><strong>证据暂时不可用</strong><span>{error}</span></div>
  }
  if (items.length === 0) {
    return <p>正在读取证据…</p>
  }
  return (
    <section className="tool-details" aria-labelledby="tool-details-title">
      <h2 id="tool-details-title">采集证据</h2>
      <ol className="tool-evidence-list">
        {items.map((evidence) => (
          <li key={evidence.id} className="tool-evidence-item">
            <header>
              <span className="status-pill succeeded">已封存</span>
              {evidence.producer.kind === 'plinth_tool' ? (
                <strong>{toolNameLabel(evidence.producer.toolName)}</strong>
              ) : (
                <strong>采集证据</strong>
              )}
              <time dateTime={evidence.observedAt} className="detail-muted">{formatTime(evidence.observedAt)}</time>
            </header>
            <dl className="tool-evidence-facts">
              <div><dt>查询</dt><dd><code>{evidenceParamsText(evidence.params)}</code></dd></div>
              <div><dt>完整性</dt><dd>{evidence.integrity === 'complete' ? '完整' : '不完整'}</dd></div>
              {evidence.connections.length > 0 && (
                <div><dt>数据来源</dt><dd>{evidence.connections.map((connection) => connection.key).join('、')}</dd></div>
              )}
            </dl>
            <EvidenceBody evidence={evidence} />
          </li>
        ))}
      </ol>
    </section>
  )
}

function EvidenceBody({ evidence }: { evidence: EvidenceDetail }) {
  if (evidence.body.kind === 'artifact') {
    const artifact = evidence.body.artifact
    return (
      <div className="tool-evidence-body">
        <p className="detail-muted">
          完整响应共 {artifact.sizeBytes.toLocaleString()} 字节；预览之外的正文保存在产物中。
          {artifact.bodyExpired ? '该产物正文已过期，元数据仍保留。' : ''}
        </p>
        {!artifact.bodyExpired && (
          <a className="text-button" href={artifactDownloadURL(artifact.id)} download>
            下载完整产物（{formatBytes(artifact.sizeBytes)}）
          </a>
        )}
      </div>
    )
  }
  const value = evidence.body.value
  if (value && typeof value === 'object' && 'output' in (value as Record<string, unknown>)) {
    const output = (value as { output: unknown }).output
    if (typeof output === 'string') {
      return <pre className="tool-output-preview">{output}</pre>
    }
  }
  return <pre className="tool-output-preview">{JSON.stringify(value, null, 2)}</pre>
}

function formatTime(value: string): string {
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? value : date.toLocaleString()
}

function formatBytes(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KiB`
  return `${(bytes / (1024 * 1024)).toFixed(1)} MiB`
}
