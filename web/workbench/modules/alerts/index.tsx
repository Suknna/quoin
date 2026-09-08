import { useCallback, useEffect, useState } from 'react'
import { Accordion, AccordionContent, AccordionItem, AccordionTrigger } from '@/components/ui/accordion'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { Separator } from '@/components/ui/separator'
import type { WorkspaceModuleProps, WorkspaceModuleView } from '../../module-contract'
import {
  acknowledgeIntakeIssue,
  fetchAlerts,
  fetchBusinessSystems,
  fetchIntakeIssues,
  fetchObservations,
  fetchOccurrence,
  type AlertOccurrenceSummary,
  type BusinessSystemOption,
  type IntakeIssue,
  type ObservationSummary,
} from '../../../src/features/alerts/api'
import {
  analysisCommandId,
  cancelAnalysis,
  createAnalysis,
  fetchAnalyses,
  fetchAnalysis,
  fetchAttempts,
  isActive,
  reasonLabel,
  stateLabel,
  type InitialAnalysisDetail,
  type InitialAnalysisSummary,
  type AttemptSummary,
} from '../../../src/features/analysis/api'

const problem = (reason: unknown, fallback: string) => reason instanceof Error ? reason.message : fallback
const time = (value?: string) => value ? new Date(value).toLocaleString() : '—'

// The retry command creates a new attempt from this analysis's frozen input snapshot; it is not a new analysis.
async function retryAnalysis(occurrenceId: string, analysisId: string): Promise<InitialAnalysisDetail> {
  const response = await fetch(`/api/v1/alerts/${encodeURIComponent(occurrenceId)}/analyses/${encodeURIComponent(analysisId)}/retry`, {
    method: 'POST', credentials: 'include', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ clientCommandId: analysisCommandId() }),
  })
  if (!response.ok) {
    const body = (await response.json().catch(() => null)) as { detail?: string } | null
    throw new Error(body?.detail ?? '暂时无法重试初步分析')
  }
  return (await response.json()) as InitialAnalysisDetail
}

function routeParts(route: string) {
  const url = new URL(route, 'https://workbench.invalid')
  return { path: url.pathname, query: url.searchParams }
}

export function useAlertsModule(props: WorkspaceModuleProps): WorkspaceModuleView {
  const { path, query } = routeParts(props.route)
  const occurrenceId = path.match(/^\/alerts\/([^/]+)$/)?.[1]
  const view = query.get('view') === 'history' ? 'history' : query.get('view') === 'intake' ? 'intake' : 'current'
  const to = (next: string) => props.navigate(next)
  const [systems, setSystems] = useState<BusinessSystemOption[]>([])
  const system = query.get('system') ?? ''
  const [alerts, setAlerts] = useState<AlertOccurrenceSummary[]>([])
  const [issues, setIssues] = useState<IntakeIssue[]>([])
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(false)

  const reload = useCallback(async () => {
    if (props.suspended) return
    setLoading(true); setError('')
    try {
      if (view === 'intake') setIssues((await fetchIntakeIssues()).items)
      else setAlerts((await fetchAlerts(view === 'history' ? 'Resolved' : 'Firing', system)).items)
    } catch (reason) { setError(problem(reason, '无法加载告警。')) } finally { setLoading(false) }
  }, [props.suspended, system, view])
  useEffect(() => { void reload() }, [reload])
  useEffect(() => { if (!props.suspended) fetchBusinessSystems().then(setSystems).catch(() => undefined) }, [props.suspended])

  const list = <div className="space-y-3 p-3">
    <div className="flex gap-1" aria-label="告警视图">
      {([['current', '当前'], ['history', '历史'], ['intake', '接入问题']] as const).map(([key, label]) => <Button key={key} variant={view === key ? 'secondary' : 'ghost'} size="sm" onClick={() => to(`/alerts?view=${key}`)}>{label}</Button>)}
    </div>
    {view !== 'intake' && <Select value={system || '__all__'} onValueChange={(key) => to(`/alerts?view=${view}${key === '__all__' ? '' : `&system=${encodeURIComponent(key)}`}`)}><SelectTrigger><SelectValue placeholder="全部业务系统" /></SelectTrigger><SelectContent><SelectItem value="__all__">全部业务系统</SelectItem>{systems.map((item) => <SelectItem key={item.key} value={item.key}>{item.displayName}</SelectItem>)}</SelectContent></Select>}
    {loading && <p className="text-sm text-muted-foreground">正在加载…</p>}
    {error && <Alert variant="destructive"><AlertDescription>{error}</AlertDescription></Alert>}
    {view === 'intake' ? issues.length === 0 && !loading ? <p className="text-sm text-muted-foreground">没有待处理接入问题。</p> : issues.map((item) => <IntakeItem key={item.id} item={item} disabled={props.suspended} onAcknowledged={reload} />) : alerts.length === 0 && !loading ? <p className="text-sm text-muted-foreground">没有告警。</p> : alerts.map((item) => <button className="w-full rounded-md border p-3 text-left hover:bg-accent" key={item.id} onClick={() => to(`/alerts/${encodeURIComponent(item.id)}?view=${view}`)}><div className="flex justify-between gap-2"><span className="truncate font-medium">{item.labels.alertname ?? item.id}</span><Badge variant={item.state === 'Firing' ? 'destructive' : 'secondary'}>{item.state}</Badge></div><p className="mt-1 truncate text-xs text-muted-foreground">{item.businessSystemKey ?? '未归属业务系统'} · {time(item.lastStateChangeAt)}</p></button>)}
  </div>

  return { title: occurrenceId ? '告警详情' : '告警', list, content: occurrenceId ? <AlertDetail id={occurrenceId} {...props} /> : <section className="p-6"><h1 className="text-xl font-semibold">{view === 'current' ? '当前告警' : view === 'history' ? '告警历史' : '告警接入问题'}</h1><p className="mt-2 text-sm text-muted-foreground">从左侧选择一项以查看详情。</p>{view === 'intake' && props.user.role === 'admin' && <Button className="mt-4" variant="outline" onClick={() => props.navigate('/admin/alert-sources')}>在管理中维护告警源</Button>}</section> }
}

function IntakeItem({ item, disabled, onAcknowledged }: { item: IntakeIssue; disabled: boolean; onAcknowledged: () => Promise<void> }) {
  const [busy, setBusy] = useState(false); const [error, setError] = useState('')
  return <div className="rounded-md border p-3"><p className="font-medium">{item.kind}</p><p className="text-xs text-muted-foreground">{item.issueKey} · 已出现 {item.occurrenceCount} 次</p>{error && <p role="alert" className="mt-2 text-sm text-destructive">{error}</p>}<Button className="mt-2" size="sm" variant="outline" disabled={disabled || busy} onClick={() => { setBusy(true); setError(''); acknowledgeIntakeIssue(item.id, item.rowVersion).then(onAcknowledged).catch((reason: unknown) => setError(problem(reason, '确认失败。'))).finally(() => setBusy(false)) }}>{busy ? '正在确认…' : '确认'}</Button></div>
}

function AlertDetail({ id, navigate, suspended, openEvidence }: WorkspaceModuleProps & { id: string }) {
  const [occurrence, setOccurrence] = useState<AlertOccurrenceSummary | null>(null); const [observations, setObservations] = useState<ObservationSummary[]>([]); const [analyses, setAnalyses] = useState<InitialAnalysisSummary[]>([]); const [selected, setSelected] = useState<InitialAnalysisDetail | null>(null); const [attempts, setAttempts] = useState<AttemptSummary[]>([]); const [error, setError] = useState(''); const [busy, setBusy] = useState(false)
  const refresh = useCallback(async () => { if (suspended) return; setError(''); try { const [next, obs, analysis] = await Promise.all([fetchOccurrence(id), fetchObservations(id), fetchAnalyses(id)]); setOccurrence(next); setObservations(obs.items); setAnalyses(analysis.items) } catch (reason) { setError(problem(reason, '无法加载告警详情。')) } }, [id, suspended])
  useEffect(() => { void refresh() }, [refresh]); useEffect(() => { if (suspended) setSelected(null) }, [suspended])
  const choose = async (analysisId: string) => { try { const detail = await fetchAnalysis(id, analysisId); setSelected(detail); setAttempts((await fetchAttempts(id, analysisId)).items) } catch (reason) { setError(problem(reason, '无法加载分析详情。')) } }
  const create = async () => { setBusy(true); try { const detail = await createAnalysis(id, analysisCommandId()); await refresh(); await choose(detail.id) } catch (reason) { setError(problem(reason, '无法创建初步分析。')) } finally { setBusy(false) } }
  const cancel = async () => { if (!selected) return; setBusy(true); try { setSelected(await cancelAnalysis(id, selected.id, selected.rowVersion, analysisCommandId())); await refresh() } catch (reason) { setError(problem(reason, '无法取消分析。')) } finally { setBusy(false) } }
  const retry = async () => { if (!selected) return; setBusy(true); try { setSelected(await retryAnalysis(id, selected.id)); setAttempts((await fetchAttempts(id, selected.id)).items); await refresh() } catch (reason) { setError(problem(reason, '无法重试初步分析。')) } finally { setBusy(false) } }
  return <section className="space-y-5 p-6">{error && <Alert variant="destructive"><AlertDescription>{error}</AlertDescription></Alert>}{!occurrence ? <p className="text-sm text-muted-foreground">正在加载…</p> : <><header><div className="flex items-center gap-2"><h1 className="text-xl font-semibold">{occurrence.labels.alertname ?? occurrence.id}</h1><Badge variant={occurrence.state === 'Firing' ? 'destructive' : 'secondary'}>{occurrence.state}</Badge></div><p className="mt-1 text-sm text-muted-foreground">首次出现 {time(occurrence.firstSeenAt)}，最近变更 {time(occurrence.lastStateChangeAt)}</p></header><Accordion type="multiple" defaultValue={['labels', 'observations']}><AccordionItem value="labels"><AccordionTrigger>标签与注释</AccordionTrigger><AccordionContent><dl className="grid gap-2 text-sm">{Object.entries({ ...occurrence.labels, ...occurrence.annotations }).map(([key, value]) => <div key={key}><dt className="inline font-medium">{key}: </dt><dd className="inline break-all">{value}</dd></div>)}</dl></AccordionContent></AccordionItem><AccordionItem value="observations"><AccordionTrigger>观测时间线（{observations.length}）</AccordionTrigger><AccordionContent className="space-y-2">{observations.map((item) => <div key={item.id} className="border-l-2 pl-3 text-sm"><Badge variant="outline">{item.effect}</Badge><p>{item.observedState} · {time(item.committedAt)}</p></div>)}</AccordionContent></AccordionItem></Accordion><Separator/><div className="flex items-center justify-between"><h2 className="text-lg font-semibold">初步分析</h2><Button onClick={() => void create()} disabled={suspended || busy}>{busy ? '处理中…' : '发起分析'}</Button></div><div className="grid gap-3 md:grid-cols-[16rem_1fr]"> <div className="space-y-2">{analyses.length === 0 ? <p className="text-sm text-muted-foreground">暂无分析。</p> : analyses.map((item) => <button key={item.id} className="w-full rounded border p-2 text-left hover:bg-accent" onClick={() => void choose(item.id)}><Badge variant="outline">{stateLabel(item.state)}</Badge><p className="mt-1 text-xs">{time(item.createdAt)}</p></button>)}</div><div className="rounded-md border p-4">{selected ? <><div className="flex justify-between"><h3 className="font-medium">分析详情</h3><Badge>{stateLabel(selected.state)}</Badge></div>{isActive(selected.state) && <Button className="mt-3" variant="outline" size="sm" disabled={suspended || busy} onClick={() => void cancel()}>取消</Button>}{(selected.state === 'Failed' || selected.state === 'Interrupted' || selected.state === 'Cancelled') && <Button className="mt-3 ml-2" variant="outline" size="sm" disabled={suspended || busy} onClick={() => void retry()}>重试分析</Button>}{selected.output ? <div className="mt-3 space-y-3"><p className="whitespace-pre-wrap text-sm">{selected.output.content}</p>{selected.output.evidenceIds.length > 0 && <div className="flex flex-wrap gap-2">{selected.output.evidenceIds.map((evidenceId) => <Button key={evidenceId} variant="outline" size="sm" onClick={() => openEvidence(evidenceId)}>证据 {evidenceId}</Button>)}</div>}<Button variant="secondary" size="sm" onClick={() => navigate(`/investigations/new?occurrence=${encodeURIComponent(id)}&initialAnalysis=${encodeURIComponent(selected.id)}`)}>转为调查</Button></div> : <p className="mt-3 text-sm text-muted-foreground">尚未产生输出。</p>}<div className="mt-4 space-y-1">{attempts.map((attempt) => <p className="text-xs text-muted-foreground" key={attempt.id}>{attempt.type} · {attempt.state} {reasonLabel(attempt.terminationReason)}</p>)}</div></> : <p className="text-sm text-muted-foreground">选择一项分析以查看输出和执行阶段。</p>}</div></div></>}</section>
}
