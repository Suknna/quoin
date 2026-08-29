import { useCallback, useEffect, useRef, useState } from 'react'
import {
  cancelInspectionRun, formatInspectionTime, getInspectionReport, getInspectionRun, inspectionActive,
  inspectionGapReasonText, inspectionStateText, listInspectionReports, reanalyzeInspectionRun, rerunInspection,
  type InspectionReportDetail, type InspectionRunDetail,
} from './api'

interface Props { runId: string; onBack: () => void; onOpenRun: (runId: string) => void }

function analysisOutcomeText(detail: InspectionRunDetail): string | null {
  const latest = detail.latestAnalysis
  if (!latest || detail.analysisActive) return null
  switch (latest.state) {
    case 'Failed': return `最近一次分析未完成${latest.terminationReason ? `（${latest.terminationReason}）` : ''}。可稍后重新分析。`
    case 'Cancelled': return '最近一次分析已取消。可随时重新分析。'
    case 'Interrupted': return `最近一次分析被中断${latest.terminationReason ? `（${latest.terminationReason}）` : ''}。可重新分析。`
    default: return null
  }
}

export function RunDetailPage({ runId, onBack, onOpenRun }: Props) {
  const [detail, setDetail] = useState<InspectionRunDetail | null>(null)
  const [report, setReport] = useState<InspectionReportDetail | null>(null)
  const [message, setMessage] = useState('')
  const [cancelling, setCancelling] = useState(false)
  const [reanalyzing, setReanalyzing] = useState(false)
  const [rerunning, setRerunning] = useState(false)
  const request = useRef(0)
  const load = useCallback(async () => {
    const current = ++request.current
    try {
      const value = await getInspectionRun(runId)
      if (current !== request.current) return
      setDetail(value)
      if (value.reportCount > 0) {
        void listInspectionReports(runId).then((items) => {
          const latest = items[0]
          if (!latest) return
          void getInspectionReport(runId, latest.version).then((read) => { if (current === request.current) setReport(read) }).catch(() => { /* 报告读取失败保留现有视图 */ })
        }).catch(() => { /* 列表失败时详情仍可读 */ })
      } else {
        setReport(null)
      }
    } catch (error) {
      if (current === request.current) setMessage(error instanceof Error ? error.message : '暂时无法读取巡检 Run。')
    }
  }, [runId])
  useEffect(() => { load(); return () => { request.current += 1 } }, [load])
  // A closed collection can still be awaiting its analysis report: keep
  // re-reading for a bounded window (analysis failure produces no placeholder
  // report, so an unbounded poll would never settle).
  const reportWaitStartedAt = useRef(0)
  useEffect(() => {
    if (!detail) return
    const collectionClosed = detail.state === 'Completed' || detail.state === 'CompletedWithGaps'
    const awaitingReport = collectionClosed && detail.reportCount === 0
    if (awaitingReport && reportWaitStartedAt.current === 0) reportWaitStartedAt.current = Date.now()
    if (!awaitingReport) reportWaitStartedAt.current = 0
    const collectionCancelling = detail.checks.some((check) => check.status === 'cancelling')
    const shouldPoll = inspectionActive(detail.state) || collectionCancelling || detail.analysisActive || (awaitingReport && Date.now() - reportWaitStartedAt.current < 240_000)
    if (!shouldPoll) return
    const timer = window.setTimeout(load, 2000)
    return () => window.clearTimeout(timer)
  }, [detail, load])
  // A run that just closed may commit its report after the last active-state
  // poll: re-read once per report-count change so the card always lands.
  const lastReportCount = useRef(0)
  useEffect(() => {
    if (detail && detail.reportCount !== lastReportCount.current) {
      lastReportCount.current = detail.reportCount
      if (detail.reportCount > 0) load()
    }
  }, [detail, load])
  async function cancel() {
    if (!detail) return
    setCancelling(true); setMessage('')
    try { setDetail(await cancelInspectionRun(detail.id, detail.rowVersion)); setMessage('已受理取消请求，正在停止尚未完成的检查。') } catch (error) { setMessage(error instanceof Error ? error.message : '取消巡检失败。'); load() } finally { setCancelling(false) }
  }
  async function reanalyze() {
    if (!detail) return
    setReanalyzing(true); setMessage('')
    try {
      await reanalyzeInspectionRun(detail.id)
      await load()
      setMessage('已受理重新分析，正在复用本 Run 已收集的 Evidence 生成新报告版本。')
    } catch (error) { setMessage(error instanceof Error ? error.message : '重新分析失败。'); await load() } finally { setReanalyzing(false) }
  }
  async function rerun() {
    if (!detail) return
    setRerunning(true); setMessage('')
    try { onOpenRun((await rerunInspection(detail.id)).id) } catch (error) { setMessage(error instanceof Error ? error.message : '重新采证失败。'); load() } finally { setRerunning(false) }
  }
  if (message && !detail) return <section className="inspection-page"><button className="back-button" onClick={onBack}>返回巡检</button><p className="field-error" role="alert">{message}</p></section>
  if (!detail) return <p className="inline-status">正在读取巡检 Run…</p>
  const active = inspectionActive(detail.state)
  const canReanalyze = detail.state === 'Completed' || detail.state === 'CompletedWithGaps'
  const canRerun = canReanalyze || detail.state === 'Failed' || detail.state === 'Cancelled' || detail.state === 'Interrupted'
  const busy = cancelling || reanalyzing || rerunning
  const analysisPending = detail.analysisActive
  const analysisCancelling = detail.latestAnalysis?.state === 'Cancelling'
  const analysisOutcome = analysisOutcomeText(detail)
  return <section className="inspection-page run-detail">
    <button className="back-button" onClick={onBack}>返回巡检记录</button>
    <header className="inspection-header"><div><p className="eyebrow">{detail.businessSystemKey} · {detail.planKey}</p><h2>巡检 Run #{detail.id}</h2><p>此视图持续读取同一个不可变运行记录。</p></div><span className={`status-pill ${detail.state === 'Completed' ? 'ok' : active ? 'waiting' : 'muted'}`}>{inspectionStateText[detail.state]}</span></header>
    {message && <p className="field-error" role="alert">{message}</p>}
    {analysisOutcome && <p className="field-error" role="alert">{analysisOutcome}</p>}
    <dl className="fact-list"><div><dt>触发方式</dt><dd>{detail.triggerKind === 'manual' ? '人工触发' : '定时调度'}</dd></div><div><dt>采证时间</dt><dd>{formatInspectionTime(detail.evidenceAt)}</dd></div><div><dt>创建时间</dt><dd>{formatInspectionTime(detail.createdAt)}</dd></div></dl>
    {(active || analysisPending) && <button className="secondary-button" disabled={busy || analysisCancelling} onClick={() => void cancel()}>{cancelling || analysisCancelling ? '正在取消…' : active ? '取消巡检' : '取消进行中的分析'}</button>}
    {canRerun && <div className="inspection-controls" aria-label="巡检后续操作">{canReanalyze && <button className="secondary-button" disabled={busy || analysisPending} onClick={() => void reanalyze()}>{reanalyzing ? '正在重新分析…' : '重新分析'}</button>}<button className="secondary-button" disabled={busy} onClick={() => void rerun()}>{rerunning ? '正在重新采证…' : '重新采证'}</button><p className="detail-muted">重新分析复用本 Run 的 Evidence；重新采证会创建新的 Run。</p></div>}
    <h3>检查结果</h3>
    {detail.checks.length === 0 ? <p className="detail-muted">{active ? '检查尚未产生结果，正在自动刷新。' : '该 Run 没有检查结果。'}</p> : <table className="data-table inspection-checks"><thead><tr><th>检查</th><th>结果</th><th>Evidence</th></tr></thead><tbody>{detail.checks.map((check) => <tr key={check.checkKey}><td>{check.checkKey}</td><td>{check.status === 'ok' ? <span className="status-pill ok">通过</span> : check.status === 'cancelling' ? <span className="status-pill waiting">正在取消…</span> : check.status ? <><span className="status-pill muted">{check.status === 'gap' ? '缺口' : '错误'}</span><span className="gap-reason">{inspectionGapReasonText[check.gapReason] ?? check.gapReason}</span></> : '等待结果'}</td><td>{check.status === 'ok' ? `#${check.evidenceId}` : '—'}</td></tr>)}</tbody></table>}
    <h3>不可变报告</h3>
    {!report ? <p className="detail-muted">{active ? '检查完成后将由 Plinth 生成并原子提交报告。' : detail.reportCount > 0 ? '正在读取报告…' : '此 Run 尚未形成报告。'}</p> : <article className="inspection-report"><header><strong>报告 v{report.version}</strong><span>模型：{report.modelId}</span><time dateTime={report.createdAt}>{formatInspectionTime(report.createdAt)}</time></header><pre>{report.content}</pre><footer><span>Evidence：{report.evidenceIds.map((id) => `#${id}`).join('、') || '—'}</span></footer></article>}
  </section>
}
