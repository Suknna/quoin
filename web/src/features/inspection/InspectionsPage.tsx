import { useCallback, useEffect, useState } from 'react'
import {
  createInspectionRun, getBusinessSystem, InspectionApiError, inspectionActive, inspectionStateText,
  listBusinessSystems, listInspectionRuns, type BusinessSystemDetail, type BusinessSystemSummary, type InspectionRunSummary,
} from './api'

interface Props { onOpenRun: (runId: string) => void }

export function InspectionsPage({ onOpenRun }: Props) {
  const [systems, setSystems] = useState<BusinessSystemSummary[]>([])
  const [systemKey, setSystemKey] = useState('')
  const [detail, setDetail] = useState<BusinessSystemDetail | null>(null)
  const [planKey, setPlanKey] = useState('')
  const [runs, setRuns] = useState<InspectionRunSummary[]>([])
  const [message, setMessage] = useState('')
  const [loading, setLoading] = useState(true)
  const [submitting, setSubmitting] = useState(false)

  const loadRuns = useCallback(() => {
    if (!systemKey) return
    void listInspectionRuns(systemKey).then((items) => { setRuns(items); setMessage('') }).catch((error: unknown) => setMessage(error instanceof Error ? error.message : '暂时无法读取巡检记录。'))
  }, [systemKey])

  useEffect(() => {
    let cancelled = false
    void listBusinessSystems().then((items) => {
      if (cancelled) return
      setSystems(items.filter((item) => item.enabled && item.currentConfigVersionId))
      setSystemKey((current) => current || items.find((item) => item.enabled && item.currentConfigVersionId)?.key || '')
    }).catch((error: unknown) => { if (!cancelled) setMessage(error instanceof Error ? error.message : '暂时无法读取业务系统。') }).finally(() => { if (!cancelled) setLoading(false) })
    return () => { cancelled = true }
  }, [])

  useEffect(() => {
    if (!systemKey) { setDetail(null); setRuns([]); return }
    let cancelled = false
    void getBusinessSystem(systemKey).then((value) => {
      if (cancelled) return
      setDetail(value)
      setPlanKey((current) => value.plans.some((plan) => plan.planKey === current) ? current : value.plans[0]?.planKey ?? '')
    }).catch((error: unknown) => { if (!cancelled) setMessage(error instanceof Error ? error.message : '暂时无法读取巡检计划。') })
    loadRuns()
    return () => { cancelled = true }
  }, [systemKey, loadRuns])

  useEffect(() => {
    if (!runs.some((run) => inspectionActive(run.state))) return
    const timer = window.setInterval(loadRuns, 2000)
    return () => window.clearInterval(timer)
  }, [runs, loadRuns])

  async function start() {
    if (!systemKey || !planKey) return
    setSubmitting(true); setMessage('')
    try {
      const run = await createInspectionRun(systemKey, planKey)
      onOpenRun(run.id)
    } catch (error) {
      const text = error instanceof InspectionApiError && error.code === 'active_conflict'
        ? `${error.message} 请打开当前活动 Run 后继续查看。`
        : error instanceof Error ? error.message : '无法开始巡检。'
      setMessage(text)
      loadRuns()
    } finally { setSubmitting(false) }
  }

  if (loading) return <p className="inline-status">正在加载可用巡检计划…</p>
  return <section className="inspection-page">
    <header className="inspection-header"><div><p className="eyebrow">手工巡检</p><h2>运行已发布的巡检计划</h2><p>每次运行固定配置版本与采证时间；完成后报告不可修改。</p></div></header>
    {message && <p className="field-error" role="alert">{message}</p>}
    {systems.length === 0 ? <p className="detail-muted">没有已启用且已发布配置的业务系统，暂时无法开始巡检。</p> : <>
      <div className="inspection-controls">
        <label>业务系统<select value={systemKey} onChange={(event) => setSystemKey(event.target.value)}>{systems.map((system) => <option key={system.key} value={system.key}>{system.displayName}</option>)}</select></label>
        <label>巡检计划<select value={planKey} onChange={(event) => setPlanKey(event.target.value)} disabled={!detail?.plans.length}>{detail?.plans.map((plan) => <option key={plan.planKey} value={plan.planKey}>{plan.displayName}（{plan.planKey}）</option>)}</select></label>
        <button className="primary-button" disabled={!planKey || submitting} onClick={() => void start()}>{submitting ? '正在创建…' : '开始巡检'}</button>
      </div>
      {!detail?.plans.length && <p className="detail-muted">当前已发布配置没有 inspection plan。</p>}
      <h3>巡检记录</h3>
      {runs.length === 0 ? <p className="detail-muted">这个业务系统尚无巡检记录。</p> : <table className="data-table inspection-runs"><thead><tr><th>计划</th><th>状态</th><th>创建时间</th></tr></thead><tbody>{runs.map((run) => <tr key={run.id} className="clickable-row" onClick={() => onOpenRun(run.id)}><td>{run.planKey}</td><td><span className={`status-pill ${run.state === 'Completed' ? 'ok' : inspectionActive(run.state) ? 'waiting' : 'muted'}`}>{inspectionStateText[run.state]}</span></td><td>{new Date(run.createdAt).toLocaleString()}</td></tr>)}</tbody></table>}
    </>}
  </section>
}
