import { type ChangeEvent, type FormEvent, useCallback, useEffect, useMemo, useRef, useState } from 'react'
import type { VerificationInvocationItem } from '../../../api/generated/types'
import {
  cancelDeploymentVerification,
  DeploymentVerificationApiError,
  type DeploymentVerificationDetail,
  downloadHelperRequest,
  fetchDeploymentVerification,
  fetchDeploymentVerifications,
  importHelperReport,
  newClientCommandId,
  startDeploymentVerification,
  submitObservation,
} from './api'
import './verification.css'

interface DeploymentVerificationPanelProps {
  invocationId?: string
  onOpen: (invocationId: string) => void
  onBack: () => void
}

type ResultValue = 'passed' | 'failed'

function localTime(value: string): string {
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? value : date.toLocaleString()
}

function shortDigest(value: string): string {
  return value.length > 16 ? `${value.slice(0, 12)}…${value.slice(-4)}` : value
}

function resultLabel(outcome: string): string {
  return outcome === 'passed' ? '通过' : outcome === 'warned' ? '警告' : outcome === 'failed' ? '失败' : outcome
}

function categoryLabel(category: string): string {
  const labels: Record<string, string> = {
    passed: '断言通过', functional_assertion_failed: '功能断言失败', cleanup_residue: '清理残留',
    verifier_conflict: '验证结果冲突', subject_drift: '验收对象已漂移', environment_unavailable: '环境不可用',
    operator_cancelled: '操作者取消', infrastructure_interrupted: '基础设施中断', cleanup_indeterminate: '清理无法确定',
    not_run: '未执行', verifier_invariant_violation: '验证器不变量违反',
  }
  return labels[category] ?? category
}

function hasDeadlinePassed(detail: DeploymentVerificationDetail): boolean {
  return !detail.receipt && Date.now() > new Date(detail.deadlineAt).getTime()
}

function progressOf(detail: DeploymentVerificationDetail): { completed: number; total: number } {
  const progress = detail.progress as { completed?: number; total?: number }
  return { completed: progress.completed ?? 0, total: progress.total ?? detail.itemCount }
}

export function DeploymentVerificationPanel({ invocationId, onOpen, onBack }: DeploymentVerificationPanelProps) {
  return invocationId ? <DeploymentVerificationDetailPanel invocationId={invocationId} onBack={onBack} /> : <DeploymentVerificationList onOpen={onOpen} />
}

function DeploymentVerificationList({ onOpen }: { onOpen: (id: string) => void }) {
  const [items, setItems] = useState<DeploymentVerificationDetail[]>([])
  const [nextCursor, setNextCursor] = useState<string | undefined>()
  const [loading, setLoading] = useState(true)
  const [starting, setStarting] = useState(false)
  const [error, setError] = useState('')
  const commandId = useRef<string | null>(null)

  const load = useCallback(async (cursor?: string) => {
    try {
      const page = await fetchDeploymentVerifications(cursor)
      setItems(current => cursor ? [...current, ...page.items as DeploymentVerificationDetail[]] : page.items as DeploymentVerificationDetail[])
      setNextCursor(page.nextCursor)
      setError('')
    } catch (reason) {
      setError(messageOf(reason, '无法读取部署验收记录。'))
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => { void load() }, [load])

  async function start() {
    setStarting(true)
    setError('')
    commandId.current ??= newClientCommandId()
    try {
      const detail = await startDeploymentVerification(commandId.current)
      commandId.current = null
      onOpen(detail.id)
    } catch (reason) {
      const error = reason as DeploymentVerificationApiError
      if (!(error instanceof Error) || (error.status !== 429 && error.status < 500)) commandId.current = null
      setError(messageOf(reason, '无法发起部署验收。'))
    } finally {
      setStarting(false)
    }
  }

  return <div className="detail-content verification-panel">
    <header className="detail-header">
      <div><p className="eyebrow">管理</p><h1>部署验收</h1></div>
      <button className="primary-button" disabled={starting} onClick={() => void start()}>{starting ? '正在冻结验收范围…' : '发起部署验收'}</button>
    </header>
    <p className="detail-muted">每次验收固定当前发布版本、部署配置、对象集合与 8 小时截止时间。它证明此时此站点的部署事实，不代表持续健康。</p>
    {error && <p className="error-summary" role="alert">{error}</p>}
    {loading ? <p role="status">正在读取部署验收历史…</p> : items.length === 0 ? <p>尚无部署验收记录。</p> : <ul className="admin-user-list verification-list">
      {items.map(item => <li key={item.id}>
        <button className="verification-list-row" onClick={() => onOpen(item.id)}>
          <strong>#{item.id} · 发布 {shortDigest(item.releaseSubjectDigest)}</strong>
          <span className="object-row-meta">开始 {localTime(item.startedAt)} · 截止 {localTime(item.deadlineAt)}</span>
          <span className="object-row-meta">进度 {progressOf(item).completed}/{progressOf(item).total} · <OutcomeBadge outcome={item.receipt?.overallOutcome} /></span>
        </button>
      </li>)}
    </ul>}
    {nextCursor && <button onClick={() => void load(nextCursor)}>加载更多部署验收记录</button>}
  </div>
}

function DeploymentVerificationDetailPanel({ invocationId, onBack }: { invocationId: string; onBack: () => void }) {
  const [detail, setDetail] = useState<DeploymentVerificationDetail | null>(null)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')
  const [busy, setBusy] = useState(false)
  const [reportFile, setReportFile] = useState<File | null>(null)
  const deadlineMissed = detail ? hasDeadlinePassed(detail) : false

  const reload = useCallback(async () => {
    try {
      const next = await fetchDeploymentVerification(invocationId)
      setDetail(next)
      setError('')
    } catch (reason) {
      setError(messageOf(reason, '无法读取部署验收详情。'))
    }
  }, [invocationId])

  // The receipt identity changes on every reload; only its stable id gates
  // the polling effect so a finalized detail cannot loop the fetch.
  const receiptId = detail?.receipt?.id ?? null
  useEffect(() => {
    void reload()
    if (receiptId !== null || deadlineMissed) return
    const timer = window.setInterval(() => {
      if (document.visibilityState === 'visible') void reload()
    }, 5000)
    return () => window.clearInterval(timer)
  }, [reload, receiptId, deadlineMissed])

  async function exportRequest() {
    setBusy(true); setError(''); setNotice('')
    try {
      const exported = await downloadHelperRequest(invocationId)
      const url = URL.createObjectURL(exported.body)
      const anchor = document.createElement('a')
      anchor.href = url; anchor.download = exported.filename
      document.body.appendChild(anchor); anchor.click(); anchor.remove()
      URL.revokeObjectURL(url)
      setNotice(`已导出 helper request${exported.requestDigest ? `（摘要 ${shortDigest(exported.requestDigest)}）` : ''}。`)
    } catch (reason) { setError(messageOf(reason, '无法导出 helper request。')) } finally { setBusy(false) }
  }

  async function importReport() {
    if (!reportFile) { setError('请选择 helper report YAML 文件。'); return }
    setBusy(true); setError(''); setNotice('')
    try {
      const imported = await importHelperReport(invocationId, reportFile)
      setDetail(imported.detail)
      setReportFile(null)
      setNotice(imported.created ? 'helper report 已导入并追加结果。' : '相同 helper report 已存在，已幂等重放。')
    } catch (reason) { setError(messageOf(reason, '无法导入 helper report。')) } finally { setBusy(false) }
  }

  async function cancel() {
    if (!detail || detail.receipt || deadlineMissed || busy) return
    if (!window.confirm('取消会把尚无结果的条目标为“操作者取消 / 未执行（警告）”；已完成的不可变结果保持不变。确定继续吗？')) return
    setBusy(true); setError(''); setNotice('')
    try {
      setDetail(await cancelDeploymentVerification(invocationId))
      setNotice('取消已提交；已派发的浏览器操作仍会按真实停止与清理状态收口。')
    } catch (reason) { setError(messageOf(reason, '无法取消部署验收。')) } finally { setBusy(false) }
  }

  return <div className="detail-content verification-panel">
    <header className="detail-header"><div><p className="eyebrow">管理 / 部署验收</p><h1>验收 #{invocationId}</h1></div><button className="back-button" onClick={onBack}>返回验收历史</button></header>
    {error && <p className="error-summary" role="alert">{error}</p>}
    {notice && <p className="admin-notice" role="status">{notice}</p>}
    {!detail ? <p role="status">正在读取不可变验收范围…</p> : <>
      {deadlineMissed && <p className="error-summary" role="alert">错过截止，需新建 invocation。该验收不会在 8 小时窗口外补写最终结论。</p>}
      <section className="detail-section verification-summary"><h2>冻结 manifest</h2>
        <p>开始 {localTime(detail.startedAt)} · 截止 {localTime(detail.deadlineAt)} · 进度 {progressOf(detail).completed}/{progressOf(detail).total} · <OutcomeBadge outcome={detail.receipt?.overallOutcome} /></p>
        <DigestFacts detail={detail} />
      </section>
      <section className="detail-section"><h2>停机 helper 交换</h2>
        <p className="detail-muted">导出的 request 不含 Session、连接或凭据。请用同一发布版本的 <code>quoin-deploy verify</code> 执行后导入 report。</p>
        <div className="admin-action-row"><button disabled={busy} onClick={() => void exportRequest()}>导出 helper request</button>
          <label className="file-label">选择 helper report YAML<input aria-label="选择 helper report YAML" type="file" accept=".yaml,.yml,application/yaml" onChange={(event: ChangeEvent<HTMLInputElement>) => setReportFile(event.target.files?.[0] ?? null)} /></label>
          <button disabled={busy || !reportFile || Boolean(detail.receipt)} onClick={() => void importReport()}>导入 helper report</button>
        </div>
        {reportFile && <p className="detail-muted">待导入：{reportFile.name}</p>}
      </section>
      <section className="detail-section"><h2>验收条目</h2><VerificationItems detail={detail} onChanged={reload} /></section>
      {detail.results.length > 0 && <section className="detail-section"><h2>不可变结果</h2><ResultTable detail={detail} /></section>}
      {detail.conflicts.length > 0 && <section className="detail-section"><h2>验证结果冲突</h2><ul className="verification-facts">{detail.conflicts.map(item => <li key={item.id}>条目 #{item.itemId}：结果 #{item.firstResultId} 与 #{item.conflictingResultId} 冲突（{localTime(item.createdAt)}）</li>)}</ul></section>}
      {detail.subjectDrifts.length > 0 && <section className="detail-section"><h2>验收对象漂移</h2><ul className="verification-facts">{detail.subjectDrifts.map((item, index) => <li key={`${item.itemId}-${index}`}>条目 #{item.itemId} · {item.objectKind}/{item.driftField}：{shortDigest(item.frozenDigest)} → {shortDigest(item.currentDigest)}（{localTime(item.observedAt)}）</li>)}</ul></section>}
      {detail.receipt ? <ReceiptCard detail={detail} /> : <section className="detail-section"><h2>关闭验收</h2><p className="detail-muted">receipt 仅在全部条目均有可定案事实后生成。关闭后不能再导入迟到结果或修改历史。</p><button className="text-button compact" disabled={busy || deadlineMissed} title={deadlineMissed ? '错过截止后必须新建 invocation。' : undefined} onClick={() => void cancel()}>{deadlineMissed ? '已错过截止，需新建 invocation' : '取消未完成验收'}</button></section>}
    </>}
  </div>
}

function DigestFacts({ detail }: { detail: DeploymentVerificationDetail }) {
  const facts: Array<[string, string]> = [
    ['发布 subject', detail.releaseSubjectDigest], ['catalog', detail.catalogDigest], ['结果 profile', detail.resultProfileDigest],
    ['部署配置', detail.deploymentConfigDigest], ['公开 Origin', detail.publicOriginDigest], ['适用集合', detail.applicableSetDigest],
    ['条目集合', detail.itemSetDigest], ['manifest', detail.manifestDigest],
  ]
  return <dl className="verification-digests">{facts.map(([label, digest]) => <div key={label}><dt>{label}</dt><dd><code title={digest}>{shortDigest(digest)}</code><CopyButton value={digest} label={`复制${label}摘要`} /></dd></div>)}</dl>
}

function CopyButton({ value, label }: { value: string; label: string }) {
  const [copied, setCopied] = useState(false)
  return <button className="text-button compact" aria-label={label} onClick={() => { void navigator.clipboard?.writeText(value); setCopied(true); window.setTimeout(() => setCopied(false), 1500) }}>{copied ? '已复制' : '复制'}</button>
}

function VerificationItems({ detail, onChanged }: { detail: DeploymentVerificationDetail; onChanged: () => Promise<void> }) {
  const byItem = useMemo(() => new Map(detail.results.map(result => [result.itemId, result])), [detail.results])
  return <div className="verification-table-wrap"><table className="verification-table"><thead><tr><th>场景 / cell</th><th>对象</th><th>冻结 locator</th><th>结果</th></tr></thead><tbody>
    {detail.items.map(item => <tr key={item.id}><td><code>{item.scenarioId}</code><br /><span>{item.cellId}</span></td><td>{item.objectKind}</td><td><LocatorSummary locator={item.locator} /></td><td>{byItem.has(item.id) ? <ResultSummary result={byItem.get(item.id)!} /> : item.objectKind === 'ui_observation' ? <ObservationForm item={item} invocationId={detail.id} onSubmitted={onChanged} /> : <span className="status-pill">等待结果</span>}</td></tr>)}
  </tbody></table></div>
}

function LocatorSummary({ locator }: { locator?: unknown }) {
  if (!locator || typeof locator !== 'object') return <span>—</span>
  const facts = Object.entries(locator as Record<string, unknown>).slice(0, 4)
  return <span className="locator-summary">{facts.map(([key, value]) => <span key={key}>{key}: {typeof value === 'string' && value.length === 64 ? shortDigest(value) : String(value)}</span>)}</span>
}

function ResultSummary({ result }: { result: { outcome: string; category: string; observedAt: string } }) {
  return <><OutcomeBadge outcome={result.outcome} /><span className="result-category">{categoryLabel(result.category)}</span><span className="object-row-meta">{localTime(result.observedAt)}</span></>
}

function ResultTable({ detail }: { detail: DeploymentVerificationDetail }) {
  return <div className="verification-table-wrap"><table className="verification-table"><thead><tr><th>条目</th><th>来源</th><th>结论</th><th>观测时间</th></tr></thead><tbody>{detail.results.map(result => <tr key={result.id}><td>#{result.itemId}</td><td>{result.producerType}</td><td><ResultSummary result={result} /></td><td>{localTime(result.observedAt)}</td></tr>)}</tbody></table></div>
}

function ObservationForm({ item, invocationId, onSubmitted }: { item: VerificationInvocationItem; invocationId: string; onSubmitted: () => Promise<void> }) {
  const [visualResult, setVisualResult] = useState<ResultValue | ''>('')
  const [motionResult, setMotionResult] = useState<ResultValue | ''>('')
  const [focusResult, setFocusResult] = useState<ResultValue | ''>('')
  const [note, setNote] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const valid = visualResult !== '' && motionResult !== '' && focusResult !== '' && note.length <= 2000
  async function submit(event: FormEvent) {
    event.preventDefault()
    if (!valid) { setError('请完成三个观察项；说明不能超过 2000 字符。'); return }
    setBusy(true); setError('')
    try {
      await submitObservation(invocationId, { itemId: item.id, inputDigest: item.inputDigest, visualResult: visualResult as ResultValue, motionResult: motionResult as ResultValue, focusOcclusionResult: focusResult as ResultValue, note: note || undefined })
      await onSubmitted()
    } catch (reason) { setError(messageOf(reason, '无法提交人工观察。')) } finally { setBusy(false) }
  }
  return <form className="observation-form" onSubmit={submit}>
    <fieldset><legend>需要人工观察</legend><Choice label="视觉理解" name={`visual-${item.id}`} value={visualResult} onChange={setVisualResult} /><Choice label="动效反馈" name={`motion-${item.id}`} value={motionResult} onChange={setMotionResult} /><Choice label="焦点遮挡" name={`focus-${item.id}`} value={focusResult} onChange={setFocusResult} /></fieldset>
    <label>备注（可选，{note.length}/2000）<textarea value={note} maxLength={2000} onChange={event => setNote(event.target.value)} /></label>
    {error && <span className="error-summary" role="alert">{error}</span>}
    <button disabled={busy}>{busy ? '正在提交…' : '提交观察'}</button>
  </form>
}

function Choice({ label, name, value, onChange }: { label: string; name: string; value: ResultValue | ''; onChange: (value: ResultValue) => void }) {
  return <div className="observation-choice"><span>{label}</span><label><input type="radio" name={name} checked={value === 'passed'} onChange={() => onChange('passed')} />通过</label><label><input type="radio" name={name} checked={value === 'failed'} onChange={() => onChange('failed')} />失败</label></div>
}

function ReceiptCard({ detail }: { detail: DeploymentVerificationDetail }) {
  const receipt = detail.receipt!
  return <section className="detail-section verification-receipt"><h2>最终 receipt</h2><p><OutcomeBadge outcome={receipt.overallOutcome} /> 快照 {localTime(receipt.snapshotAt)} · 最终提交 {localTime(receipt.finalizedAt)}</p>
    <dl className="verification-digests">{[['最终结果', receipt.finalResultDigest], ['结果集合', receipt.resultSetDigest], ['helper 导入集合', receipt.helperImportSetDigest], ['人工观察集合', receipt.typedObservationSetDigest], ['冲突集合', receipt.conflictSetDigest], ['漂移集合', receipt.subjectDriftDigest]].map(([label, digest]) => <div key={label}><dt>{label}</dt><dd><code title={digest}>{shortDigest(digest)}</code><CopyButton value={digest} label={`复制${label}摘要`} /></dd></div>)}</dl>
  </section>
}

function OutcomeBadge({ outcome }: { outcome?: string }) {
  const normalized = outcome === 'passed' || outcome === 'warned' || outcome === 'failed' ? outcome : 'waiting'
  return <span className={`status-pill verification-outcome ${normalized}`}>{outcome ? resultLabel(outcome) : '进行中'}</span>
}

function messageOf(reason: unknown, fallback: string): string {
  if (reason instanceof Error) return reason.message
  return fallback
}
