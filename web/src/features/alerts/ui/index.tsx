/* eslint-disable react-refresh/only-export-components -- Alert views colocate their route lifecycle. */
import { Fragment, useCallback, useEffect, useMemo, useRef, useState } from "react";
import { cn } from "cn";
import { Activity, AlertTriangle, Bot, CheckCircle2, ChevronDown, ChevronRight, CircleDot, Clock3, FileText, RefreshCw } from "lucide-react";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import { Empty, EmptyDescription, EmptyHeader, EmptyMedia, EmptyTitle } from "@/components/ui/empty";
import { Item, ItemActions, ItemContent, ItemDescription, ItemMedia, ItemTitle } from "@/components/ui/item";
import { Input } from "@/components/ui/input";
import { FeatureUnderConstruction } from "@/components/FeatureUnderConstruction";
import { Select, SelectContent, SelectGroup, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { EntityList, type EntityListItem } from "@/components/EntityList";
import { Separator } from "@/components/ui/separator";
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from "@/components/ui/sheet";
import { Skeleton } from "@/components/ui/skeleton";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import type { WorkspaceModuleProps, WorkspaceModuleView } from "@/app/module-contract";
import { fetchAlerts, fetchBusinessSystems, fetchObservations, fetchOccurrence, type AlertOccurrenceSummary, type BusinessSystemOption, type ObservationSummary } from "@/features/alerts/api";
import { useLiveAlerts } from "@/features/alerts/useLiveAlerts";
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
  if (postmortems) return { title: "运维中心", list: null, content: <section className="p-6"><FeatureUnderConstruction description="故障复盘能力尚未开放。" /></section> };
  return { title: "运维中心", list: null, content: <AlertList {...props} view={view} system={system} selectedId={selectedId} /> };
}

function severityTone(value?: string) {
  switch (value?.toLowerCase()) {
    case "critical": return "bg-destructive";
    case "warning": return "bg-warning";
    case "info": return "bg-info";
    default: return "bg-muted-foreground";
  }
}

function relativeTime(value: string) {
  const seconds = Math.max(0, Math.floor((Date.now() - new Date(value).getTime()) / 1_000));
  if (seconds < 60) return "刚刚";
  if (seconds < 3_600) return `${Math.floor(seconds / 60)} 分钟前`;
  if (seconds < 86_400) return `${Math.floor(seconds / 3_600)} 小时前`;
  return `${Math.floor(seconds / 86_400)} 天前`;
}

function AlertList({ view, system, selectedId, navigate, suspended, openEvidence }: WorkspaceModuleProps & { view: "current" | "history"; system: string; selectedId: string | null }) {
  const [systems, setSystems] = useState<BusinessSystemOption[]>([]);
  const [query, setQuery] = useState("");
  // Counts are independently snapshotted so neither tab presents a misleading zero before load.
  const [counts, setCounts] = useState<{ key: string; firing?: number; resolved?: number }>({ key: "" });
  // The list is live by default; workbench suspension is the only local pause boundary.
  const live = useLiveAlerts(view === "history" ? "Resolved" : "Firing", system, !suspended);
  const { setAtTop } = live;
  useEffect(() => { if (!suspended) fetchBusinessSystems().then(setSystems).catch(() => undefined); }, [suspended]);
  useEffect(() => {
    // The workbench main area scrolls with the document, so this is the real
    // reading-position boundary used to buffer new firing occurrences.
    const updateAtTop = () => setAtTop(window.scrollY < 8);
    updateAtTop();
    window.addEventListener("scroll", updateAtTop, { passive: true });
    return () => window.removeEventListener("scroll", updateAtTop);
  }, [setAtTop]);
  const countKey = system;
  useEffect(() => {
    if (suspended) return;
    let active = true;
    void Promise.all([fetchAlerts("Firing", system), fetchAlerts("Resolved", system)]).then(([firing, resolved]) => {
      if (active) setCounts({ key: countKey, firing: firing.items.length, resolved: resolved.items.length });
    }).catch(() => undefined);
    return () => { active = false; };
  }, [countKey, suspended, system]);
  const filteredItems = useMemo(() => {
    const needle = query.trim().toLocaleLowerCase();
    if (!needle) return live.items;
    return live.items.filter((item) => [item.labels.alertname, item.annotations?.summary, item.annotations?.description, ...Object.values(item.labels)].filter(Boolean).join(" ").toLocaleLowerCase().includes(needle));
  }, [live.items, query]);
  const emptyTitle = query ? "没有匹配的告警" : view === "current" ? "当前没有告警" : "没有历史告警";
  const emptyDescription = query ? "已加载的告警中没有匹配此搜索条件的记录。" : view === "current" ? "当前没有正在触发的告警。" : "尚未加载到已恢复的告警记录。";
  const listItems: EntityListItem[] = filteredItems.map((item) => ({ id: item.id, title: item.labels.alertname ?? item.id, subtitle: item.annotations?.summary ?? item.annotations?.description ?? item.businessSystemKey ?? "未提供摘要", badge: { text: item.state, variant: item.state === "Firing" ? "destructive" : "secondary" }, media: <span className={`size-2 rounded-full ${severityTone(item.labels.severity)}`} title={item.labels.severity ? `严重性：${item.labels.severity}` : "严重性：未知"} />, time: <time dateTime={item.lastStateChangeAt} title={time(item.lastStateChangeAt)}>{relativeTime(item.lastStateChangeAt)}</time> }));
  const controls = <div className="flex flex-wrap items-center justify-between gap-3"><Tabs value={view} onValueChange={(next) => navigate(listRoute(next as "current" | "history", system, selectedId ?? undefined))}><TabsList><TabsTrigger value="current">当前告警{counts.key === countKey && counts.firing !== undefined && <Badge variant="secondary" className="ml-1 tabular-nums">{counts.firing}</Badge>}</TabsTrigger><TabsTrigger value="history">历史告警{counts.key === countKey && counts.resolved !== undefined && <Badge variant="secondary" className="ml-1 tabular-nums">{counts.resolved}</Badge>}</TabsTrigger></TabsList></Tabs><div className="flex flex-1 flex-wrap justify-end gap-3"><Select value={system || "__all__"} onValueChange={(next) => navigate(listRoute(view, next === "__all__" ? "" : next, selectedId ?? undefined))}><SelectTrigger className="w-full sm:w-52"><SelectValue placeholder="全部业务系统" /></SelectTrigger><SelectContent><SelectGroup><SelectItem value="__all__">全部业务系统</SelectItem>{systems.map((item) => <SelectItem key={item.key} value={item.key}>{item.displayName}</SelectItem>)}</SelectGroup></SelectContent></Select><Input value={query} onChange={(event) => setQuery(event.target.value)} placeholder="搜索已加载告警" className="w-full sm:max-w-xs" aria-label="搜索已加载告警" /></div></div>;
  return <section className="flex flex-col gap-6"><div><h1 className="text-2xl font-semibold tracking-tight">{view === "current" ? "当前告警" : "告警历史"}</h1><p className="mt-1 text-sm text-muted-foreground">查看来自上游 Alertmanager 的真实告警记录。</p></div>{live.pendingNew > 0 && <Button variant="secondary" size="sm" onClick={live.mergePending}>显示 {live.pendingNew} 条新告警</Button>}<EntityList items={listItems} columns={["media", "title", "subtitle", "status", "time"]} selectedId={selectedId} onSelect={(item) => navigate(listRoute(view, system, item.id))} loading={live.loading} error={live.error} onRetry={live.refresh} controls={controls} emptyTitle={emptyTitle} emptyDescription={emptyDescription} /><AlertDetailSheet id={selectedId} onClose={() => navigate(listRoute(view, system))} suspended={suspended} openEvidence={openEvidence} /></section>;
}

function DetailEmpty({ icon: Icon, title, description }: { icon: typeof FileText; title: string; description: string }) {
  return <Empty className="min-h-32 gap-3 bg-muted/50 p-5 md:p-6"><EmptyHeader><EmptyMedia variant="icon"><Icon /></EmptyMedia><EmptyTitle className="text-base">{title}</EmptyTitle><EmptyDescription>{description}</EmptyDescription></EmptyHeader></Empty>;
}

/** Machine-provided labels and annotations remain copyable while long values cannot overflow the drawer. */
function PropertyList({ entries }: { entries: [string, string][] }) {
  return <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-2">{entries.map(([key, value]) => <Fragment key={key}><dt className="text-muted-foreground">{key}</dt><dd className="min-w-0 break-words font-mono text-sm">{key.toLowerCase() === "severity" && value.toLowerCase() === "critical" ? <Badge variant="destructive">{value}</Badge> : value}</dd></Fragment>)}</dl>;
}

function ObservationIcon({ effect }: { effect: ObservationSummary["effect"] }) {
  if (effect === "resolved" || effect === "resolved_first") return <CheckCircle2 className="size-4 text-success" aria-hidden="true" />;
  if (effect === "repeat_firing") return <RefreshCw className="size-4 text-destructive" aria-hidden="true" />;
  if (effect === "late_firing_after_resolved") return <AlertTriangle className="size-4 text-destructive" aria-hidden="true" />;
  return <CircleDot className="size-4 text-destructive" aria-hidden="true" />;
}

function ObservationDot({ effect }: { effect: ObservationSummary["effect"] }) {
  const resolved = effect === "resolved" || effect === "resolved_first";
  return <span className={`absolute -left-[5px] top-5 size-2.5 rounded-full border border-background ${resolved ? "bg-success" : "bg-destructive"}`} aria-hidden="true" />;
}

/** Attempt states are server projections; only map states the API explicitly returns. */
function AttemptStateIcon({ state }: { state: AttemptSummary["state"] }) {
  if (state === "Succeeded") return <CheckCircle2 className="size-4 text-success" aria-label="已完成" />;
  if (state === "Failed" || state === "Interrupted") return <AlertTriangle className="size-4 text-destructive" aria-label={state} />;
  if (state === "Queued" || state === "Running") return <Clock3 className="size-4 text-muted-foreground" aria-label={state} />;
  return <CircleDot className="size-4 text-muted-foreground" aria-label={state} />;
}

function AlertDetailSheet({ id, onClose, suspended, openEvidence }: { id: string | null; onClose: () => void; suspended: boolean; openEvidence: (id: string) => void }) {
  const [occurrence, setOccurrence] = useState<AlertOccurrenceSummary | null>(null); const [observations, setObservations] = useState<ObservationSummary[]>([]); const [error, setError] = useState(""); const [analysisOpen, setAnalysisOpen] = useState(false);
  const load = useCallback(async () => { if (!id || suspended) return; setError(""); try { const [detail, timeline] = await Promise.all([fetchOccurrence(id), fetchObservations(id)]); setOccurrence(detail); setObservations(timeline.items); } catch (reason) { setOccurrence(null); setError(problem(reason, "无法加载告警详情。")); } }, [id, suspended]);
  // State reset and fetch intentionally follow the URL-selected occurrence.
  // eslint-disable-next-line react-hooks/set-state-in-effect
  useEffect(() => { setOccurrence(null); if (id) void load(); }, [id, load]);
  const annotationEntries = occurrence ? Object.entries(occurrence.annotations ?? {}).filter(([key]) => key !== "description" && key !== "summary") : [];
  const description = occurrence?.annotations?.description ?? occurrence?.annotations?.summary;
  return <Sheet open={Boolean(id)} onOpenChange={(open) => { if (!open) onClose(); }}><SheetContent side="right" className="flex h-dvh w-full flex-col gap-0 overflow-hidden p-0 sm:max-w-4xl"><SheetHeader className="shrink-0 border-b px-6 py-5 pr-12"><SheetTitle className="text-lg">{occurrence?.labels.alertname ?? "告警详情"}</SheetTitle><SheetDescription>查看告警的真实属性、观察记录和初步分析。</SheetDescription>{occurrence && <div className="flex flex-wrap items-center gap-2 pt-2"><Badge variant={occurrence.state === "Firing" ? "destructive" : "secondary"}>{occurrence.state}</Badge><Separator orientation="vertical" className="hidden h-4 self-center sm:block" /><Badge variant="outline">{occurrence.businessSystemKey ?? "未归属业务系统"}</Badge><Separator orientation="vertical" className="hidden h-4 self-center sm:block" /><span className="flex w-full items-center gap-1 text-xs text-muted-foreground sm:w-auto"><Clock3 className="size-3.5" aria-hidden="true" />最近变更 {time(occurrence.lastStateChangeAt)}</span></div>}</SheetHeader><div className="min-h-0 flex-1 overflow-hidden">{error ? <div className="p-6"><Alert variant="destructive"><AlertTriangle /><AlertTitle>无法加载告警详情</AlertTitle><AlertDescription>{error}</AlertDescription><Button className="mt-2" size="sm" variant="outline" onClick={() => void load()}><RefreshCw data-icon="inline-start" />重试</Button></Alert></div> : !occurrence ? <div className="flex flex-col gap-4 p-6" role="status" aria-label="正在加载告警详情"><Skeleton className="h-7 w-1/3" /><Skeleton className="h-24 w-full" /><Skeleton className="h-24 w-full" /></div> : <Tabs defaultValue="overview" onValueChange={(value) => setAnalysisOpen(value === "analysis")} className="h-full min-h-0 gap-0"><div className="shrink-0 border-b px-6"><TabsList variant="line" className="h-11"><TabsTrigger value="overview">概览</TabsTrigger><TabsTrigger value="timeline">时间线</TabsTrigger><TabsTrigger value="analysis">AI 分析</TabsTrigger></TabsList></div><TabsContent value="overview" className="min-h-0 overflow-y-auto p-4 sm:p-6"><div className="flex flex-col gap-6"><section className="flex flex-col gap-3"><h2 className="text-sm font-medium">告警说明</h2>{description ? <p className="whitespace-pre-wrap text-sm leading-6">{description}</p> : <DetailEmpty icon={FileText} title="没有提供描述" description="上游告警未附带描述或摘要。" />}</section>{annotationEntries.length > 0 && <><Separator /><section className="flex flex-col gap-3"><h2 className="text-sm font-medium">注释</h2><PropertyList entries={annotationEntries} /></section></>}<Separator /><section className="flex flex-col gap-3"><h2 className="text-sm font-medium">属性</h2><PropertyList entries={Object.entries(occurrence.labels)} /></section></div></TabsContent><TabsContent value="timeline" className="min-h-0 overflow-y-auto p-4 sm:p-6"><section aria-labelledby="observation-title"><h2 id="observation-title" className="mb-4 text-sm font-medium">观察记录</h2>{observations.length === 0 ? <DetailEmpty icon={Activity} title="没有观察记录" description="此告警尚未记录状态观察。" /> : <ul className="ml-2 border-l border-border" aria-label="观察记录时间线">{observations.map((item) => <li key={item.id} className="relative pl-5"><ObservationDot effect={item.effect} /><Item className="rounded-none border-0 px-0 py-4" size="sm"><ItemMedia variant="icon"><ObservationIcon effect={item.effect} /></ItemMedia><ItemContent><ItemTitle>{item.observedState}</ItemTitle><ItemDescription>效果：{item.effect} · 提交于 {time(item.committedAt)}</ItemDescription></ItemContent></Item></li>)}</ul>}</section></TabsContent><TabsContent value="analysis" className="min-h-0 overflow-y-auto p-4 sm:p-6">{analysisOpen && <InitialAnalysis key={occurrence.id} occurrenceId={occurrence.id} suspended={suspended} openEvidence={openEvidence} />}</TabsContent></Tabs>}</div></SheetContent></Sheet>;
}

function InitialAnalysis({ occurrenceId, suspended, openEvidence }: { occurrenceId: string; suspended: boolean; openEvidence: (id: string) => void }) {
  const [analysis, setAnalysis] = useState<InitialAnalysisDetail | null>(null); const [attempts, setAttempts] = useState<AttemptSummary[]>([]); const [error, setError] = useState(""); const [attemptsOpen, setAttemptsOpen] = useState(false); const started = useRef(false);
  const load = useCallback(async (retry = false) => { if (suspended) return; setError(""); try { const summaries = await fetchAnalyses(occurrenceId); const newest = [...summaries.items].sort((a, b) => b.createdAt.localeCompare(a.createdAt)); const current = newest.find((item) => isActive(item.state)) ?? newest[0]; if (retry) { if (!analysis) return; const detail = await retryAnalysis(occurrenceId, analysis.id, analysisCommandId()); setAnalysis(detail); setAttempts((await fetchAttempts(occurrenceId, detail.id)).items); return; } if (!current) { let request = creating.get(occurrenceId); if (!request) { request = createAnalysis(occurrenceId, analysisCommandId()); creating.set(occurrenceId, request); request.finally(() => creating.delete(occurrenceId)); } setAnalysis(await request); return; } const detail = await fetchAnalysis(occurrenceId, current.id); setAnalysis(detail); setAttempts((await fetchAttempts(occurrenceId, current.id)).items); } catch (reason) { setError(problem(reason, "无法读取或发起初步分析。")); } }, [analysis, occurrenceId, suspended]);
  // Mounting the AI tab is the explicit user transition that may start analysis.
  // eslint-disable-next-line react-hooks/set-state-in-effect
  useEffect(() => { if (!started.current) { started.current = true; void load(); } }, [load]);
  // Running work is server-owned, so the open tab polls its real projection until terminal or unmounted.
  useEffect(() => { if (!analysis || !isActive(analysis.state) || suspended) return; const timer = window.setInterval(() => void load(), 2000); return () => window.clearInterval(timer); }, [analysis, analysis?.id, analysis?.state, load, suspended]);
  if (error) return <Alert variant="destructive"><AlertTriangle /><AlertTitle>无法读取初步分析</AlertTitle><AlertDescription>{error}</AlertDescription><Button className="mt-2" size="sm" variant="outline" disabled={suspended} onClick={() => void load()}><RefreshCw data-icon="inline-start" />重新读取</Button></Alert>;
  if (!analysis) return <section className="flex flex-col gap-6" role="status" aria-label="正在准备初步分析"><h2 className="text-sm font-medium">初步分析</h2><div className="flex flex-col gap-3"><Skeleton className="h-4 w-1/4" /><Skeleton className="h-4 w-full" /><Skeleton className="h-4 w-4/5" /></div></section>;
  const running = isActive(analysis.state);
  return <section className="flex flex-col gap-6"><div className="flex items-center justify-between gap-3"><h2 className="text-sm font-medium">初步分析</h2><Badge>{stateLabel(analysis.state)}</Badge></div>{analysis.output ? <><section className="flex flex-col gap-3"><h3 className="text-sm font-medium">结论</h3><div className="flex gap-2 rounded-md bg-muted/50 px-3 py-2 text-sm leading-6"><Bot className="mt-0.5 size-4 shrink-0 text-muted-foreground" aria-hidden="true" /><p className="whitespace-pre-wrap">{analysis.output.content}</p></div></section><Separator /><section className="flex flex-col gap-3"><h3 className="text-sm font-medium">关联证据</h3>{analysis.output.evidenceIds.length > 0 ? <div className="flex flex-col">{analysis.output.evidenceIds.map((evidenceId) => <Item asChild key={evidenceId} variant="outline" size="sm" className="w-full cursor-pointer rounded-md px-3 text-left hover:bg-accent focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2"><button type="button" onClick={() => openEvidence(evidenceId)}><ItemMedia variant="default"><FileText className="size-4" aria-hidden="true" /></ItemMedia><ItemContent><ItemTitle>证据 {evidenceId}</ItemTitle></ItemContent><ItemActions className="text-muted-foreground"><span className="text-sm">查看证据</span><ChevronRight className="size-4" aria-hidden="true" /></ItemActions></button></Item>)}</div> : <DetailEmpty icon={FileText} title="没有关联证据" description="本次分析没有返回证据记录。" />}</section></> : running ? <><div className="flex items-center gap-2 text-sm text-muted-foreground"><Bot className="size-4" aria-hidden="true" /><span>分析正在执行，关闭详情不会取消任务。</span></div><div className="flex flex-col gap-3"><Skeleton className="h-4 w-full" /><Skeleton className="h-4 w-5/6" /><Skeleton className="h-4 w-2/3" /></div></> : <DetailEmpty icon={Bot} title="没有分析结果" description="此分析未返回结果内容。" />}{(analysis.state === "Failed" || analysis.state === "Interrupted") && <Button variant="outline" disabled={suspended} onClick={() => void load(true)}><RefreshCw data-icon="inline-start" />重试分析</Button>}{attempts.length > 0 && <Collapsible open={attemptsOpen} onOpenChange={setAttemptsOpen} className="rounded-md border"><CollapsibleTrigger asChild><Button className="w-full justify-between rounded-b-none" variant="ghost"><span>执行记录</span><ChevronDown className={cn("size-4 transition-transform", attemptsOpen && "rotate-180")} aria-hidden="true" /></Button></CollapsibleTrigger><CollapsibleContent className="border-t px-3 py-2"><ul className="flex flex-col gap-3">{attempts.map((attempt) => <li key={attempt.id} className="flex flex-col gap-1 text-sm"><div className="flex items-center gap-2"><AttemptStateIcon state={attempt.state} /><span className="font-mono text-xs">{attempt.type}</span><span className="text-muted-foreground">{attempt.state}</span></div>{attempt.terminationReason && <p className="text-muted-foreground">{reasonLabel(attempt.terminationReason) || attempt.terminationReason}</p>}</li>)}</ul></CollapsibleContent></Collapsible>}</section>;
}
