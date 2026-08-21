import { useEffect, useState } from 'react'
import type { InitialAnalysisDetail } from './api'
import { fetchAnalysis, stateLabel } from './api'
import { ToolDetails } from './tool-details/ToolDetails'

// AnalysisDetail is the full-workbench reading layer for one immutable
// Initial Analysis (UI-READING-001, UI-ROUTE-003): the sealed output with
// its provenance, the attempt list and the closed state. Closing returns
// to the alert detail.
export function AnalysisDetail({ occurrenceId, analysisId, onClose }: { occurrenceId: string; analysisId: string; onClose: () => void }) {
  const [detail, setDetail] = useState<InitialAnalysisDetail | null>(null)
  const [error, setError] = useState('')

  useEffect(() => {
    let cancelled = false
    void fetchAnalysis(occurrenceId, analysisId)
      .then((next) => { if (!cancelled) setDetail(next) })
      .catch((reason: unknown) => { if (!cancelled) setError(reason instanceof Error ? reason.message : '初步分析详情加载失败') })
    return () => { cancelled = true }
  }, [occurrenceId, analysisId])

  return (
    <div className="reading-layer" role="document" aria-label="初步分析详情">
      <button className="text-button" onClick={onClose}>← 返回告警详情</button>
      {error && <div className="error-summary" role="alert"><strong>详情暂时不可用</strong><span>{error}</span></div>}
      {!detail && !error && <p>正在读取初步分析…</p>}
      {detail && (
        <>
          <header className="detail-header">
            <p className="eyebrow">初步分析 · {stateLabel(detail.state)}</p>
            <h1>告警初步分析结论</h1>
            <p className="detail-muted">创建于 {formatTime(detail.createdAt)} · 共 {detail.attemptCount} 次执行 Attempt{detail.output ? ` · 模型 ${detail.output.modelId}` : ''}</p>
          </header>
          {detail.output ? (
            <>
              <article className="analysis-output" aria-label="分析结论">
                {detail.output.content.split('\n').map((line, index) => <p key={index}>{line || '\u00A0'}</p>)}
                <footer className="detail-muted">结论不可变地封存于 {formatTime(detail.output.createdAt)}；Succeeded 只表示模型分析完成，不表示告警正常或诊断已验证。</footer>
              </article>
              <ToolDetails evidenceIds={detail.output.evidenceIds ?? []} />
            </>
          ) : (
            <div className="inline-status" role="status">
              <span className="status-dot waiting" />
              <div><strong>{stateLabel(detail.state)}</strong><p>{detail.state === 'Failed' ? '该分析以技术失败结束；其 Attempt 记录保留在历史中。' : '该分析没有产生成功输出。'}</p></div>
            </div>
          )}
        </>
      )}
    </div>
  )
}

function formatTime(value: string): string {
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? value : date.toLocaleString()
}
