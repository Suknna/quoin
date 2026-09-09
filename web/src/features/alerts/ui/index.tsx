/* eslint-disable react-refresh/only-export-components -- Alert views colocate their route lifecycle. */
import { useCallback, useEffect, useRef, useState } from "react";
import { AlertTriangle, Bell, FileText } from "lucide-react";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { EntityList, type EntityListItem } from "@/components/EntityList";
import { FeatureUnderConstruction } from "@/components/FeatureUnderConstruction";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from "@/components/ui/sheet";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import type { WorkspaceModuleProps, WorkspaceModuleView } from "@/app/module-contract";
import { fetchAlerts, fetchBusinessSystems, fetchObservations, fetchOccurrence, type AlertOccurrenceSummary, type BusinessSystemOption, type ObservationSummary } from "@/features/alerts/api";
import { analysisCommandId, createAnalysis, fetchAnalyses, fetchAnalysis, fetchAttempts, isActive, reasonLabel, retryAnalysis, stateLabel, type AttemptSummary, type InitialAnalysisDetail } from "@/features/analysis/api";

const problem = (reason: unknown, fallback: string) => reason instanceof Error ? reason.message : fallback;
const time = (value?: string) => value ? new Date(value).toLocaleString() : "—";
const creating = new Map<string, Promise<InitialAnalysisDetail>>();
function parts(route: string) { const url = new URL(route, "https://workbench.invalid"); return { path: url.pathname, query: url.searchParams }; }
function listRoute(view: "current" | "history", system: string, id?: string) { const query = new URLSearchParams({ view }); if (system) query.set("system", system); if (id) query.set("id", id); return `/alerts/list?${query}`; }

/** Alert center owns query state and composes reusable presentation primitives over real APIs. */
export function useAlertsModule(props: WorkspaceModuleProps): WorkspaceModuleView {
  const { path, query } = parts(props.route);
  const postmortems = path === "/postmortems";
  const view = query.get("view") === "history" ? "history" : "current";
  const system = query.get("system") ?? "";
  const selectedId = query.get("id");
  useEffect(() => { if (path === "/alerts") props.navigate(listRoute(view, system, selectedId ?? undefined)); }, [path, props, selectedId, system, view]);
  const navigation = <nav className="space-y-1 p-3" aria-label="告警中心模块"><Button className="w-full justify-start" aria-current={!postmortems ? "page" : undefined} variant={!postmortems ? "secondary" : "ghost"} onClick={() => props.navigate(listRoute(view, system, selectedId ?? undefined))}><Bell />告警列表</Button><Button className="w-full justify-start" aria-current={postmortems ? "page" : undefined} variant={postmortems ? "secondary" : "ghost"} onClick={() => props.navigate("/postmortems")}> <FileText />故障复盘</Button></nav>;
  if (postmortems) return { title: "告警中心", list: navigation, content: <section className="p-6"><FeatureUnderConstruction description="故障复盘能力尚未开放。" /></section> };
  return { title: "告警中心", list: navigation, content: <AlertList {...props} view={view} system={system} selectedId={selectedId} /> };
}

function AlertList({ view, system, selectedId, navigate, suspended, openEvidence }: WorkspaceModuleProps & { view: "current" | "history"; system: string; selectedId: string | null }) {
  const [systems, setSystems] = useState<BusinessSystemOption[]>([]); const [items, setItems] = useState<AlertOccurrenceSummary[]>([]); const [loading, setLoading] = useState(true); const [error, setError] = useState("");
  const reload = useCallback(async () => { if (suspended) { setLoading(false); return; } setLoading(true); setError(""); try { setItems((await fetchAlerts(view === "history" ? "Resolved" : "Firing", system)).items); } catch (reason) { setError(problem(reason, "无法加载告警。")); } finally { setLoading(false); } }, [suspended, system, view]);
  useEffect(() => { void reload(); }, [reload]);
  useEffect(() => { if (!suspended) fetchBusinessSystems().then(setSystems).catch(() => undefined); }, [suspended]);
  const controls = <div className="flex flex-wrap gap-3"><Tabs value={view} onValueChange={(next) => navigate(listRoute(next as "current" | "history", system, selectedId ?? undefined))}><TabsList><TabsTrigger value="current">当前告警</TabsTrigger><TabsTrigger value="history">历史告警</TabsTrigger></TabsList></Tabs><Select value={system || "__all__"} onValueChange={(next) => navigate(listRoute(view, next === "__all__" ? "" : next, selectedId ?? undefined))}><SelectTrigger className="w-48"><SelectValue placeholder="全部业务系统" /></SelectTrigger><SelectContent><SelectItem value="__all__">全部业务系统</SelectItem>{systems.map((item) => <SelectItem key={item.key} value={item.key}>{item.displayName}</SelectItem>)}</SelectContent></Select></div>;
  const listItems: EntityListItem[] = items.map((item) => ({ id: item.id, title: item.labels.alertname ?? item.id, badge: { text: item.state, variant: item.state === "Firing" ? "destructive" : "secondary" }, subtitle: item.businessSystemKey ?? "未归属业务系统", time: time(item.lastStateChangeAt) }));
  return <section className="h-full"><div className="border-b px-6 py-4"><h1 className="text-xl font-semibold">{view === "current" ? "当前告警" : "告警历史"}</h1><p className="mt-1 text-sm text-muted-foreground">查看来自上游 Alertmanager 的真实告警记录。</p></div><EntityList items={listItems} columns={["title", "status", "subtitle", "time"]} selectedId={selectedId} onSelect={(item) => navigate(listRoute(view, system, item.id))} loading={loading} error={error} onRetry={() => void reload()} controls={controls} emptyTitle="没有告警" emptyDescription="当前筛选条件下没有告警记录。" /><AlertDetailSheet id={selectedId} onClose={() => navigate(listRoute(view, system))} suspended={suspended} openEvidence={openEvidence} /></section>;
}

function AlertDetailSheet({ id, onClose, suspended, openEvidence }: { id: string | null; onClose: () => void; suspended: boolean; openEvidence: (id: string) => void }) {
 const [occurrence, setOccurrence] = useState<AlertOccurrenceSummary | null>(null); const [observations, setObservations] = useState<ObservationSummary[]>([]); const [error, setError] = useState(""); const [analysisOpen, setAnalysisOpen] = useState(false);
 const load = useCallback(async () => { if (!id || suspended) return; setError(""); try { const [detail, timeline] = await Promise.all([fetchOccurrence(id), fetchObservations(id)]); setOccurrence(detail); setObservations(timeline.items); } catch (reason) { setOccurrence(null); setError(problem(reason, "无法加载告警详情。")); } }, [id, suspended]);
 // State reset and fetch intentionally follow the URL-selected occurrence.
 // eslint-disable-next-line react-hooks/set-state-in-effect
 useEffect(() => { setOccurrence(null); if (id) void load(); }, [id, load]);
 const annotationEntries = occurrence ? Object.entries(occurrence.annotations ?? {}).filter(([key]) => key !== "description" && key !== "summary") : [];
 return <Sheet open={Boolean(id)} onOpenChange={(open) => { if (!open) onClose(); }}><SheetContent side="right" className="flex w-full flex-col overflow-hidden sm:max-w-2xl"><SheetHeader><SheetTitle>{occurrence?.labels.alertname ?? "告警详情"}</SheetTitle><SheetDescription>查看告警的真实属性、观察记录和初步分析。</SheetDescription></SheetHeader><div className="min-h-0 flex-1 overflow-y-auto px-4 pb-6">{error ? <Alert variant="destructive"><AlertDescription>{error}</AlertDescription><Button className="mt-2" size="sm" variant="outline" onClick={() => void load()}>重试</Button></Alert> : !occurrence ? <p role="status" className="text-sm text-muted-foreground">正在加载…</p> : <><div className="flex items-center gap-2"><Badge variant={occurrence.state === "Firing" ? "destructive" : "secondary"}>{occurrence.state}</Badge><span className="text-sm text-muted-foreground">最近变更 {time(occurrence.lastStateChangeAt)}</span></div><Tabs defaultValue="overview" onValueChange={(value) => setAnalysisOpen(value === "analysis")} className="mt-5"><TabsList><TabsTrigger value="overview">概览</TabsTrigger><TabsTrigger value="timeline">时间线</TabsTrigger><TabsTrigger value="analysis">AI 分析</TabsTrigger></TabsList><TabsContent value="overview" className="grid gap-6 pt-4 md:grid-cols-2"><section><h2 className="font-medium">描述</h2><p className="mt-2 whitespace-pre-wrap text-sm">{occurrence.annotations?.description ?? occurrence.annotations?.summary ?? "没有提供描述。"}</p>{annotationEntries.length > 0 && <><h3 className="mt-5 font-medium">注释</h3><dl className="mt-2 grid gap-2 text-sm">{annotationEntries.map(([key, value]) => <div key={key}><dt className="inline font-medium">{key}: </dt><dd className="inline break-all">{value}</dd></div>)}</dl></>}</section><section><h2 className="font-medium">属性</h2><dl className="mt-2 grid gap-2 text-sm">{Object.entries(occurrence.labels).map(([key, value]) => <div key={key}><dt className="inline font-medium">{key}: </dt><dd className="inline break-all">{value}</dd></div>)}</dl></section></TabsContent><TabsContent value="timeline" className="space-y-2 pt-4">{observations.length === 0 ? <p className="text-sm text-muted-foreground">没有观察记录。</p> : observations.map((item) => <div key={item.id} className="border-l-2 pl-3 text-sm"><Badge variant="outline">{item.effect}</Badge><p>{item.observedState} · {time(item.committedAt)}</p></div>)}</TabsContent><TabsContent value="analysis">{analysisOpen && <InitialAnalysis key={occurrence.id} occurrenceId={occurrence.id} suspended={suspended} openEvidence={openEvidence} />}</TabsContent></Tabs></>}</div></SheetContent></Sheet>;
}

function InitialAnalysis({ occurrenceId, suspended, openEvidence }: { occurrenceId: string; suspended: boolean; openEvidence: (id: string) => void }) {
 const [analysis, setAnalysis] = useState<InitialAnalysisDetail | null>(null); const [attempts, setAttempts] = useState<AttemptSummary[]>([]); const [error, setError] = useState(""); const started = useRef(false);
 const load = useCallback(async (retry = false) => { if (suspended) return; setError(""); try { const summaries = await fetchAnalyses(occurrenceId); const newest = [...summaries.items].sort((a, b) => b.createdAt.localeCompare(a.createdAt)); const current = newest.find((item) => isActive(item.state)) ?? newest[0]; if (retry) { if (!analysis) return; const detail = await retryAnalysis(occurrenceId, analysis.id, analysisCommandId()); setAnalysis(detail); setAttempts((await fetchAttempts(occurrenceId, detail.id)).items); return; } if (!current) { let request = creating.get(occurrenceId); if (!request) { request = createAnalysis(occurrenceId, analysisCommandId()); creating.set(occurrenceId, request); request.finally(() => creating.delete(occurrenceId)); } setAnalysis(await request); return; } const detail = await fetchAnalysis(occurrenceId, current.id); setAnalysis(detail); setAttempts((await fetchAttempts(occurrenceId, current.id)).items); } catch (reason) { setError(problem(reason, "无法读取或发起初步分析。")); } }, [analysis, occurrenceId, suspended]);
 // Mounting the AI tab is the explicit user transition that may start analysis.
 // eslint-disable-next-line react-hooks/set-state-in-effect
 useEffect(() => { if (!started.current) { started.current = true; void load(); } }, [load]);
 // Running work is server-owned, so the open tab polls its real projection until terminal or unmounted.
 useEffect(() => { if (!analysis || !isActive(analysis.state) || suspended) return; const timer = window.setInterval(() => void load(), 2000); return () => window.clearInterval(timer); }, [analysis, analysis?.id, analysis?.state, load, suspended]);
 if (error) return <Alert variant="destructive"><AlertTriangle /><AlertDescription>{error}</AlertDescription><Button className="mt-2" size="sm" variant="outline" disabled={suspended} onClick={() => void load()}>重新读取</Button></Alert>;
 if (!analysis) return <p role="status" className="py-5 text-sm text-muted-foreground">正在准备初步分析…</p>;
 return <div className="space-y-3 py-4"><div className="flex items-center justify-between"><h2 className="font-medium">初步分析</h2><Badge>{stateLabel(analysis.state)}</Badge></div>{analysis.output ? <><p className="whitespace-pre-wrap text-sm">{analysis.output.content}</p><div className="flex flex-wrap gap-2">{analysis.output.evidenceIds.map((evidenceId) => <Button key={evidenceId} size="sm" variant="outline" onClick={() => openEvidence(evidenceId)}>证据 {evidenceId}</Button>)}</div></> : <p className="text-sm text-muted-foreground">分析正在执行，关闭详情不会取消任务。</p>}{(analysis.state === "Failed" || analysis.state === "Interrupted") && <Button variant="outline" disabled={suspended} onClick={() => void load(true)}>重试分析</Button>}<div className="space-y-1">{attempts.map((attempt) => <p className="text-xs text-muted-foreground" key={attempt.id}>{attempt.type} · {attempt.state} {reasonLabel(attempt.terminationReason)}</p>)}</div></div>;
}
