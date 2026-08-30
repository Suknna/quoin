import { useEffect, useState } from 'react'
import { api, type ImportBatchDetail, type ImportBatchSummary } from './api'

const importBatchStateLabels: Record<ImportBatchSummary['state'], string> = {
  Processing: '正在整理',
  AwaitingConfirmation: '等待人工确认',
  Failed: '整理失败',
  Completed: '已完成',
  Cancelled: '已取消',
}

function formatTime(value: string): string {
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? '时间暂不可用' : date.toLocaleString()
}

// ImportBatchList is the workbench's second-column import-batch segment.
export function ImportBatchList({ onOpen, onNew }: { onOpen: (id: string) => void; onNew: () => void }) {
  const [batches, setBatches] = useState<ImportBatchSummary[] | null>(null)
  const [error, setError] = useState('')
  const [nextCursor, setNextCursor] = useState<string | undefined>()
  const [loadingMore, setLoadingMore] = useState(false)
  useEffect(() => {
    let cancelled = false
    void api.listImportBatches().then((page) => { if (!cancelled) { setBatches(page.items); setNextCursor(page.nextCursor) } }).catch((reason: unknown) => { if (!cancelled) setError(reason instanceof Error ? reason.message : '暂时无法读取导入批次。') })
    return () => { cancelled = true }
  }, [])
  async function loadMore() {
    if (!nextCursor) return
    setLoadingMore(true)
    try { const page = await api.listImportBatches(nextCursor); setBatches((current) => [...(current ?? []), ...page.items]); setNextCursor(page.nextCursor) } catch (reason) { setError(reason instanceof Error ? reason.message : '暂时无法读取更多导入批次。') } finally { setLoadingMore(false) }
  }
  if (error) return <p className="field-error" role="alert">{error}</p>
  if (!batches) return <p className="inline-status">正在读取导入批次…</p>
  return <>
    <button className="primary-button" onClick={onNew}>导入原文</button>
    {batches.length === 0 ? <p className="detail-muted">还没有导入批次。粘贴运行记录、操作说明或复盘原文开始整理。</p> : <><ul className="knowledge-list" aria-label="导入批次">{batches.map((item) => <li key={item.id}><button className="knowledge-item" onClick={() => onOpen(item.id)}><strong>{formatTime(item.createdAt)} 的导入</strong><span className="detail-muted">{importBatchStateLabels[item.state]}</span></button></li>)}</ul>{nextCursor && <button className="secondary-button" disabled={loadingMore} onClick={() => void loadMore()}>{loadingMore ? '正在加载…' : '加载更多'}</button>}</>}
  </>
}

// ImportKnowledge keeps the selected batch server-authoritative: Processing is
// polled and a batch can be reopened from the adjacent import-batch list.
export function ImportKnowledge({ batchId, onOpenCandidate, onSelectBatch, onBack }: { batchId?: string; onOpenCandidate: (id: string, batchID: string) => void; onSelectBatch: (id: string) => void; onBack: () => void }) {
  const [text, setText] = useState('')
  const [batch, setBatch] = useState<ImportBatchDetail | null>(null)
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)

  useEffect(() => {
    let cancelled = false
    // Clear the former authority before reading another batch: no command may
    // target stale data while the selected batch is loading or unavailable.
    queueMicrotask(() => { if (!cancelled) { setBatch(null); setError('') } })
    if (!batchId) return () => { cancelled = true }
    void api.getImportBatch(batchId).then((next) => { if (!cancelled) setBatch(next) }).catch((reason: unknown) => { if (!cancelled) setError(reason instanceof Error ? reason.message : '暂时无法读取导入批次。') })
    return () => { cancelled = true }
  }, [batchId])

  const processingBatchID = batch?.state === 'Processing' ? batch.id : undefined
  useEffect(() => {
    if (!processingBatchID) return
    let cancelled = false
    const refresh = () => void api.getImportBatch(processingBatchID)
      .then((next) => { if (!cancelled) setBatch(next) })
      .catch((reason: unknown) => { if (!cancelled) setError(reason instanceof Error ? reason.message : '暂时无法读取导入进度。') })
    const timer = window.setInterval(refresh, 1000)
    refresh()
    return () => { cancelled = true; window.clearInterval(timer) }
  }, [processingBatchID])

  async function submit() {
    if (!text.trim()) { setError('请粘贴需要整理的原文。'); return }
    setBusy(true); setError('')
    try { const next = await api.startImport(text); setBatch(next); setText(''); onSelectBatch(next.id) } catch (reason) { setError(reason instanceof Error ? reason.message : '暂时无法开始整理。') } finally { setBusy(false) }
  }
  async function cancel() {
    if (!batch) return
    setBusy(true); setError('')
    try { setBatch(await api.cancelBatch(batch.id, batch.rowVersion)) } catch (reason) { setError(reason instanceof Error ? reason.message : '暂时无法取消。') } finally { setBusy(false) }
  }
  async function confirmAll() {
    if (!batch) return
    const requested = batch.candidates.filter((candidate) => candidate.state === 'AwaitingConfirmation').map((candidate) => ({ candidateId: candidate.id, expectedRevision: candidate.draftRevision }))
    setBusy(true); setError('')
    try {
      setBatch(await api.confirmBatch(batch.id, requested))
    } catch (reason) {
      // A batch confirm is all-or-nothing. Re-read the authority and name the
      // changed draft so the operator can review the latest content in place.
      try {
        const latest = await api.getImportBatch(batch.id)
        setBatch(latest)
        const expected = new Map(requested.map((item) => [item.candidateId, item.expectedRevision]))
        const changed = latest.candidates.find((candidate) => expected.get(candidate.id) !== undefined && expected.get(candidate.id) !== candidate.draftRevision)
        setError(changed ? `“${changed.draftTitle || '未命名草稿'}”已被更新，已保留本批次所有草稿；请检查最新版本后重试。` : '本批次有草稿已更新，未确认任何候选；已刷新到最新内容。')
      } catch {
        setError(reason instanceof Error ? reason.message : '暂时无法确认本批候选。')
      }
    } finally { setBusy(false) }
  }
  return <div className="detail-content" aria-label="导入知识">
    {batch && <button className="back-button" onClick={onBack}>返回导入批次</button>}
    <h1>{batch ? `${formatTime(batch.createdAt)} 的导入` : '从原文整理知识'}</h1>
    {error && <p className="field-error" role="alert">{error}</p>}
    {!batch && <><p className="detail-muted">粘贴已有的运行记录、操作说明或复盘原文。系统会生成待人工确认的知识草稿，原文不会被直接发布为知识。</p><label className="field"><span>原文</span><textarea rows={14} value={text} disabled={busy} onChange={(event) => setText(event.target.value)} /></label><button className="primary-button" disabled={busy} onClick={() => void submit()}>{busy ? '正在提交…' : '开始整理'}</button></>}
    {batch && <section aria-live="polite">
      <h2>{importBatchStateLabels[batch.state]}</h2>
      <p className="detail-muted">可以离开此页面；导入批次会保留进度。完成后请逐条检查并确认草稿。</p>
      {batch.state === 'Processing' && <button className="text-button" disabled={busy} onClick={() => void cancel()}>取消整理</button>}
      {batch.state === 'AwaitingConfirmation' && <button className="secondary-button" disabled={busy || batch.candidates.length === 0} onClick={() => void confirmAll()}>确认当前全部</button>}
      {batch.candidates.length > 0 && <ul className="knowledge-list" aria-label="本次导入的候选">{batch.candidates.map((candidate) => <li key={candidate.id}><button className="knowledge-item" onClick={() => onOpenCandidate(candidate.id, batch.id)}><strong>{candidate.draftTitle || '未命名草稿'}</strong><span className="detail-muted">打开并人工确认</span></button></li>)}</ul>}
    </section>}
  </div>
}
