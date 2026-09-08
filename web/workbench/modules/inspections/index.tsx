import { useCallback, useEffect, useRef, useState } from "react";
import { Button } from "@/components/ui/button";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle, DialogTrigger } from "@/components/ui/dialog";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { ScrollArea } from "@/components/ui/scroll-area";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { Accordion, AccordionContent, AccordionItem, AccordionTrigger } from "@/components/ui/accordion";
import { Field, FieldLabel } from "@/components/ui/field";
import type { WorkspaceModuleProps, WorkspaceModuleView } from "../../module-contract";
import { messageOf } from "../../shared";
import {
  cancelInspectionRun, createInspectionRun, getBusinessSystem, getInspectionReport, getInspectionRun,
  inspectionActive, inspectionGapReasonText, inspectionStateText, listBusinessSystems, listInspectionReports,
  listInspectionRuns, reanalyzeInspectionRun, rerunInspection, formatInspectionTime,
  type BusinessSystemDetail, type BusinessSystemSummary, type InspectionReportDetail, type InspectionRunDetail, type InspectionRunSummary,
} from "../../../src/features/inspection/api";
import { api as knowledgeApi } from "../../../src/features/knowledge/api";
import { appendFeedback, fetchFeedback, feedbackValueLabels, type FeedbackTimeline, type FeedbackValue } from "../../../src/features/feedback/api";

const terminal = new Set(["Completed", "CompletedWithGaps", "Failed", "Cancelled", "Interrupted", "SkippedOverlap"]);
const statusClass = (state: string) => inspectionActive(state as never) ? "text-amber-700" : state.startsWith("Completed") ? "text-emerald-700" : "text-muted-foreground";

/** Splits a report into readable paragraphs; report content is never exposed as a JSON blob. */
export function ReportBody({ content, evidenceIds, openEvidence }: { content: string; evidenceIds: string[]; openEvidence: (id: string) => void }) {
  const refs = new Set(evidenceIds);
  return <div className="space-y-4 text-sm leading-6">{content.split(/\n\s*\n/).filter(Boolean).map((paragraph, index) => {
    const pieces = paragraph.split(/(#[A-Za-z0-9_-]+)/g);
    return <p key={index}>{pieces.map((piece, pieceIndex) => {
      const id = piece.startsWith("#") ? piece.slice(1) : "";
      return id && refs.has(id) ? <Button key={pieceIndex} variant="link" className="h-auto p-0 align-baseline" onClick={() => openEvidence(id)}>#{id}</Button> : piece;
    })}</p>;
  })}</div>;
}

function Feedback({ reportId, suspended }: { reportId: string; suspended: boolean }) {
  const [timeline, setTimeline] = useState<FeedbackTimeline>();
  const [note, setNote] = useState("");
  const [error, setError] = useState("");
  useEffect(() => { if (!suspended) void fetchFeedback({ type: "inspection_report", id: reportId }).then(setTimeline).catch((e) => setError(messageOf(e, "无法读取反馈。"))); }, [reportId, suspended]);
  async function record(value: FeedbackValue) {
    if (suspended) return;
    setError("");
    try { await appendFeedback({ type: "inspection_report", id: reportId }, value, note); setNote(""); setTimeline(await fetchFeedback({ type: "inspection_report", id: reportId })); }
    catch (e) { setError(messageOf(e, "无法记录反馈。")); }
  }
  return <section className="space-y-3 border-t pt-4"><h3 className="font-medium">实际反馈</h3><div className="flex flex-wrap gap-2">{(Object.keys(feedbackValueLabels) as FeedbackValue[]).map((value) => <Button key={value} size="sm" variant="outline" disabled={suspended} onClick={() => void record(value)}>{feedbackValueLabels[value]}</Button>)}</div><textarea aria-label="反馈备注" className="min-h-16 w-full rounded-md border bg-transparent px-3 py-2 text-sm" value={note} onChange={(e) => setNote(e.target.value)} disabled={suspended} placeholder="可选备注" />{error && <Alert variant="destructive"><AlertDescription>{error}</AlertDescription></Alert>}{timeline?.items.length ? <p className="text-xs text-muted-foreground">最近：{feedbackValueLabels[timeline.items[0].value]} · {formatInspectionTime(timeline.items[0].createdAt)}</p> : null}</section>;
}

function RunDetail({ runId, props, onBack, onOpenRun }: { runId: string; props: WorkspaceModuleProps; onBack: () => void; onOpenRun: (id: string) => void }) {
  const [detail, setDetail] = useState<InspectionRunDetail>(); const [report, setReport] = useState<InspectionReportDetail>();
  const [error, setError] = useState(""); const [busy, setBusy] = useState(false); const alive = useRef(true);
  const load = useCallback(async () => { try { const current = await getInspectionRun(runId); if (!alive.current) return; setDetail(current); if (current.reportCount) { const reports = await listInspectionReports(runId); if (reports[0]) setReport(await getInspectionReport(runId, reports[0].version)); } else setReport(undefined); } catch (e) { if (alive.current) setError(messageOf(e, "无法读取巡检 Run。")); } }, [runId]);
  useEffect(() => { alive.current = true; void load(); return () => { alive.current = false; }; }, [load]);
  useEffect(() => { if (props.suspended || !detail || (terminal.has(detail.state) && !detail.analysisActive)) return; const timer = window.setTimeout(() => void load(), 2000); return () => clearTimeout(timer); }, [detail, load, props.suspended]);
  async function action(kind: "cancel" | "analyze" | "rerun") { if (!detail || props.suspended) return; setBusy(true); setError(""); try { if (kind === "cancel") await cancelInspectionRun(detail.id, detail.rowVersion); else if (kind === "analyze") await reanalyzeInspectionRun(detail.id); else { onOpenRun((await rerunInspection(detail.id)).id); return; } await load(); } catch (e) { setError(messageOf(e, "操作未完成。")); await load(); } finally { setBusy(false); } }
  async function candidate() { if (!report || props.suspended) return; try { props.navigate(`/knowledge/candidates/${(await knowledgeApi.createReportCandidate(runId, report.version)).id}`); } catch (e) { setError(messageOf(e, "无法创建知识候选。")); } }
  if (!detail) return <div className="p-6">正在读取 Run…</div>;
  const canAnalyze = detail.state === "Completed" || detail.state === "CompletedWithGaps";
  return <div className="space-y-6 p-6"><Button variant="ghost" onClick={onBack}>返回巡检记录</Button><header className="flex flex-wrap items-start justify-between gap-3"><div><h2 className="text-xl font-semibold">{detail.planKey} · Run {detail.id}</h2><p className="text-sm text-muted-foreground">采证冻结于 {formatInspectionTime(detail.evidenceAt)}，报告版本不可修改。</p></div><span className={statusClass(detail.state)}>{inspectionStateText[detail.state]}</span></header>{error && <Alert variant="destructive"><AlertTitle>操作失败</AlertTitle><AlertDescription>{error}</AlertDescription></Alert>}<div className="flex flex-wrap gap-2">{inspectionActive(detail.state) || detail.analysisActive ? <Button variant="outline" disabled={busy || props.suspended} onClick={() => void action("cancel")}>取消</Button> : null}{canAnalyze ? <Button variant="outline" disabled={busy || props.suspended || detail.analysisActive} onClick={() => void action("analyze")}>分析冻结证据</Button> : null}{terminal.has(detail.state) ? <Button variant="outline" disabled={busy || props.suspended} onClick={() => void action("rerun")}>重新采证（新 Run）</Button> : null}</div><Accordion type="multiple" defaultValue={["checks", "phase"]}><AccordionItem value="checks"><AccordionTrigger>检查项与缺口</AccordionTrigger><AccordionContent><Table><TableHeader><TableRow><TableHead>检查</TableHead><TableHead>状态</TableHead><TableHead>证据 / 原因</TableHead></TableRow></TableHeader><TableBody>{detail.checks.map((check) => <TableRow key={check.checkKey}><TableCell>{check.checkKey}</TableCell><TableCell>{check.status}</TableCell><TableCell>{check.status === "ok" ? <Button variant="link" className="h-auto p-0" onClick={() => props.openEvidence(check.evidenceId)}>#{check.evidenceId}</Button> : check.status === "cancelling" ? "正在停止" : inspectionGapReasonText[check.gapReason] ?? check.gapReason}</TableCell></TableRow>)}</TableBody></Table></AccordionContent></AccordionItem><AccordionItem value="phase"><AccordionTrigger>运行阶段与事件</AccordionTrigger><AccordionContent><dl className="grid gap-2 text-sm sm:grid-cols-2"><div><dt className="text-muted-foreground">触发</dt><dd>{detail.triggerKind}</dd></div><div><dt className="text-muted-foreground">创建</dt><dd>{formatInspectionTime(detail.createdAt)}</dd></div><div><dt className="text-muted-foreground">分析</dt><dd>{detail.latestAnalysis?.state ?? "尚未开始"}</dd></div><div><dt className="text-muted-foreground">报告版本</dt><dd>{detail.reportCount}</dd></div></dl></AccordionContent></AccordionItem></Accordion>{report ? <section className="space-y-4"><div className="flex flex-wrap items-center justify-between gap-2"><div><h3 className="font-semibold">报告 v{report.version}</h3><p className="text-xs text-muted-foreground">模型 {report.modelId} · {formatInspectionTime(report.createdAt)}</p></div><Button variant="outline" disabled={props.suspended} onClick={() => void candidate()}>整理为知识候选</Button></div><ScrollArea className="max-h-[28rem] rounded-md border p-4"><ReportBody content={report.content} evidenceIds={report.evidenceIds} openEvidence={props.openEvidence} /></ScrollArea><div className="text-sm">证据引用：{report.evidenceIds.map((id) => <Button key={id} variant="link" className="h-auto p-1" onClick={() => props.openEvidence(id)}>#{id}</Button>)}</div><Feedback reportId={`${report.runId}:${report.version}`} suspended={props.suspended} /></section> : <Alert><AlertDescription>{detail.analysisActive ? "分析正在生成报告。" : "该 Run 尚无报告版本。"}</AlertDescription></Alert>}</div>;
}

export function useInspectionsModule(props: WorkspaceModuleProps): WorkspaceModuleView {
  const [systems, setSystems] = useState<BusinessSystemSummary[]>([]); const [system, setSystem] = useState<BusinessSystemDetail>(); const [runs, setRuns] = useState<InspectionRunSummary[]>([]); const [selected, setSelected] = useState<string>(); const [error, setError] = useState(""); const [createOpen, setCreateOpen] = useState(false); const [plan, setPlan] = useState("");
  const load = useCallback(async () => { try { const items = (await listBusinessSystems()).filter((item) => item.enabled && item.currentConfigVersionId); setSystems(items); const key = system?.key && items.some((x) => x.key === system.key) ? system.key : items[0]?.key; if (!key) { setSystem(undefined); setRuns([]); return; } const [detail, itemsRuns] = await Promise.all([getBusinessSystem(key), listInspectionRuns(key)]); setSystem(detail); setRuns(itemsRuns); setPlan((current) => detail.plans.some((item) => item.planKey === current) ? current : detail.plans[0]?.planKey ?? ""); } catch (e) { setError(messageOf(e, "无法读取巡检计划。")); } }, [system]);
  useEffect(() => { const timer = window.setTimeout(() => void load(), 0); return () => clearTimeout(timer); }, [load]); useEffect(() => { if (props.suspended || !runs.some((run) => inspectionActive(run.state))) return; const timer = window.setTimeout(() => void load(), 2000); return () => clearTimeout(timer); }, [load, props.suspended, runs]);
  async function create() { if (!system || !plan || props.suspended) return; try { const run = await createInspectionRun(system.key, plan); setCreateOpen(false); setSelected(run.id); } catch (e) { setError(messageOf(e, "无法创建巡检。")); } }
  const list = <div className="flex h-full flex-col gap-3 p-3"><div className="font-medium">巡检记录</div><Select value={system?.key ?? ""} onValueChange={async (key) => { try { const [detail, items] = await Promise.all([getBusinessSystem(key), listInspectionRuns(key)]); setSystem(detail); setRuns(items); } catch (e) { setError(messageOf(e, "无法切换业务系统。")); } }}><SelectTrigger><SelectValue placeholder="选择系统" /></SelectTrigger><SelectContent>{systems.map((item) => <SelectItem value={item.key} key={item.key}>{item.displayName}</SelectItem>)}</SelectContent></Select><ScrollArea className="min-h-0 flex-1">{runs.map((run) => <Button key={run.id} variant={selected === run.id ? "secondary" : "ghost"} className="mb-1 h-auto w-full justify-start whitespace-normal text-left" onClick={() => setSelected(run.id)}><span>{run.planKey}<br /><small className={statusClass(run.state)}>{inspectionStateText[run.state]}</small></span></Button>)}{!runs.length && <p className="p-2 text-sm text-muted-foreground">没有巡检记录</p>}</ScrollArea></div>;
  const actions = <Dialog open={createOpen} onOpenChange={setCreateOpen}><DialogTrigger asChild><Button disabled={!system || !plan || props.suspended}>创建巡检</Button></DialogTrigger><DialogContent><DialogHeader><DialogTitle>发布计划巡检</DialogTitle><DialogDescription>将创建独立 Run 并冻结当前已发布配置与采证时间。</DialogDescription></DialogHeader><Field><FieldLabel>计划</FieldLabel><Select value={plan} onValueChange={setPlan}><SelectTrigger><SelectValue /></SelectTrigger><SelectContent>{system?.plans.map((item) => <SelectItem key={item.planKey} value={item.planKey}>{item.displayName}</SelectItem>)}</SelectContent></Select></Field><DialogFooter><Button onClick={() => void create()} disabled={!plan || props.suspended}>开始巡检</Button></DialogFooter></DialogContent></Dialog>;
  return { title: selected ? "巡检 Run" : "巡检", list, actions, content: selected ? <RunDetail runId={selected} props={props} onBack={() => setSelected(undefined)} onOpenRun={setSelected} /> : <div className="space-y-4 p-6"><h2 className="text-xl font-semibold">巡检</h2><p className="text-muted-foreground">选择已发布系统的历史 Run，或创建一次新的采证。</p>{error && <Alert variant="destructive"><AlertDescription>{error}</AlertDescription></Alert>}</div> };
}
