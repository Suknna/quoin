import { useEffect, useState } from "react";
import type { WorkspaceModuleProps, WorkspaceModuleView } from "../../module-contract";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent, AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle } from "@/components/ui/alert-dialog";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { Field, FieldDescription, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { ScrollArea } from "@/components/ui/scroll-area";
import { Textarea } from "@/components/ui/textarea";
import { api, candidateSourceLabels, candidateStateLabels, embeddingStateLabels, indexStateLabels, type CandidateDetail, type ImportBatchDetail, type KnowledgeDetail, type KnowledgeSearchHit, type KnowledgeVersionDetail, type KnowledgeVersionSummary } from "../../../src/features/knowledge/api";

const errorText = (reason: unknown) => reason instanceof Error ? reason.message : "暂时无法完成操作，请重试。";
const routePart = (route: string) => route.split("/").filter(Boolean);

/** Knowledge view keeps cursors tied to their server query and never polls while suspended. */
export function useKnowledgeModule(props: WorkspaceModuleProps): WorkspaceModuleView {
  const [query, setQuery] = useState("");
  const [hits, setHits] = useState<{ exactTextMatches: KnowledgeSearchHit[]; semanticMatches: KnowledgeSearchHit[] } | null>(null);
  const [items, setItems] = useState<Awaited<ReturnType<typeof api.browse>>["items"]>([]);
  const [next, setNext] = useState<string>();
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);
  // The coordinator supplies absolute workspace routes; remove the module prefix
  // before interpreting the feature-local route.
  const parts = routePart(props.route).filter((part, index) => !(index === 0 && part === "knowledge"));
  const candidateId = parts[0] === "candidates" ? parts[1] : undefined;
  const knowledgeId = parts[0] === "items" ? parts[1] : undefined;
  const importId = parts[0] === "imports" ? parts[1] : undefined;

  async function load(cursor?: string) {
    setLoading(true); setError("");
    try { const page = await api.browse(cursor); setItems(value => cursor ? [...value, ...page.items] : page.items); setNext(page.nextCursor); }
    catch (reason) { setError(errorText(reason)); } finally { setLoading(false); }
  }
  useEffect(() => { void load(); }, []);
  async function search() {
    const value = query.trim(); if (!value) { setHits(null); return; }
    setError("");
    try { const result = await api.search(value); setHits({ exactTextMatches: result.exactTextMatches, semanticMatches: result.semanticMatches }); }
    catch (reason) { setError(errorText(reason)); }
  }
  const list = <ScrollArea className="h-full px-3"><div className="space-y-3 py-3">
    <form className="flex gap-2" onSubmit={event => { event.preventDefault(); void search(); }}><Input aria-label="检索知识" value={query} onChange={event => setQuery(event.target.value)} placeholder="检索知识"/><Button type="submit" variant="secondary">搜索</Button></form>
    {error && <Alert variant="destructive"><AlertDescription>{error}</AlertDescription></Alert>}
    {hits ? <SearchGroups hits={hits} open={id => props.navigate(`/knowledge/items/${id}`)} /> : <>
      {items.map(item => <Button key={item.id} variant="ghost" className="h-auto w-full justify-start whitespace-normal text-left" onClick={() => props.navigate(`/knowledge/items/${item.id}`)}><span><strong>{item.title}</strong><small className="block text-muted-foreground">v{item.currentVersionSeq} · {item.eligible ? "可复用" : "已退出检索"}</small></span></Button>)}
      {!loading && !items.length && <p className="text-sm text-muted-foreground">尚无已确认知识。</p>}
      {next && <Button variant="outline" disabled={loading} onClick={() => void load(next)}>加载更多</Button>}
    </>}
  </div></ScrollArea>;
  let content: React.ReactNode = <section className="space-y-3"><h2 className="text-lg font-semibold">知识库</h2><p className="text-sm text-muted-foreground">搜索同时展示全文匹配和语义相似结果。</p></section>;
  if (candidateId) content = <CandidateEditor id={candidateId} suspended={props.suspended} refresh={() => void load()} navigate={props.navigate} />;
  else if (knowledgeId) content = <KnowledgeItem id={knowledgeId} suspended={props.suspended} navigate={props.navigate} openEvidence={props.openEvidence} />;
  else if (importId) content = <ImportBatch id={importId} suspended={props.suspended} navigate={props.navigate} />;
  return { title: "知识", list, content, actions: <Button disabled={props.suspended} onClick={() => props.navigate("/knowledge/imports/new")}>导入原文</Button> };
}

function SearchGroups({ hits, open }: { hits: { exactTextMatches: KnowledgeSearchHit[]; semanticMatches: KnowledgeSearchHit[] }; open: (id: string) => void }) {
  const exactIds = new Set(hits.exactTextMatches.map(hit => hit.knowledge.id));
  return <div className="space-y-4"><HitGroup title="全文匹配" hits={hits.exactTextMatches} open={open} /><HitGroup title="语义相似" hits={hits.semanticMatches.filter(hit => !exactIds.has(hit.knowledge.id))} open={open} semantic /></div>;
}
function HitGroup({ title, hits, open, semantic = false }: { title: string; hits: KnowledgeSearchHit[]; open: (id: string) => void; semantic?: boolean }) {
 return <section><h3 className="mb-1 text-sm font-medium">{title}</h3>{hits.map(hit => <Button key={hit.knowledge.id} variant="ghost" className="h-auto w-full justify-start whitespace-normal text-left" onClick={() => open(hit.knowledge.id)}><span>{hit.knowledge.title}<small className="block text-muted-foreground">分数 {hit.score.toFixed(3)}{semantic && hit.indexState ? ` · ${indexStateLabels[hit.indexState]}` : ""}</small></span></Button>) || <p className="text-sm text-muted-foreground">没有匹配项。</p>}</section>;
}
function CandidateEditor({ id, suspended, refresh, navigate }: { id: string; suspended: boolean; refresh: () => void; navigate: (route: string) => void }) {
 const [candidate, setCandidate] = useState<CandidateDetail>(); const [title, setTitle] = useState(""); const [body, setBody] = useState(""); const [error, setError] = useState(""); const [busy, setBusy] = useState(false); const [confirming, setConfirming] = useState(false);
 const load = async () => { try { const value = await api.getCandidate(id); setCandidate(value); setTitle(value.draftTitle ?? value.originalSuggestion.title); setBody(value.draftBody ?? value.originalSuggestion.body); } catch (reason) { setError(errorText(reason)); } };
 useEffect(() => { void load(); }, [id]);
 useEffect(() => { if (suspended) setBusy(false); }, [suspended]);
 if (!candidate) return <p role="status">正在读取候选…</p>;
 const currentCandidate = candidate;
 async function save() { setBusy(true); setError(""); try { const next = await api.editDraft(id, currentCandidate.draftRevision, { title, body }); setCandidate(current => current ? { ...current, ...next } : current); } catch (reason) { setError(errorText(reason)); } finally { setBusy(false); } }
 async function confirm() { setBusy(true); try { const next = await api.confirm(id, currentCandidate.draftRevision); refresh(); navigate(`/knowledge/items/${next.confirmedKnowledgeId}`); } catch (reason) { setError(errorText(reason)); } finally { setBusy(false); setConfirming(false); } }
 async function exclude() { setBusy(true); try { await api.exclude(id, currentCandidate.rowVersion); refresh(); navigate("/knowledge"); } catch (reason) { setError(errorText(reason)); } finally { setBusy(false); } }
 return <section className="space-y-5"><div><Badge>{candidateStateLabels[candidate.state]}</Badge><h2 className="mt-2 text-xl font-semibold">编辑知识候选</h2><p className="text-sm text-muted-foreground">来源：{candidateSourceLabels[candidate.sourceType]}。发生冲突时保留当前输入，刷新后重新确认。</p></div>{error && <Alert variant="destructive"><AlertDescription>{error}</AlertDescription></Alert>}<Field><FieldLabel htmlFor="candidate-title">标题</FieldLabel><Input id="candidate-title" value={title} onChange={e => setTitle(e.target.value)} disabled={busy || suspended} /></Field><Field><FieldLabel htmlFor="candidate-body">正文</FieldLabel><Textarea id="candidate-body" value={body} onChange={e => setBody(e.target.value)} disabled={busy || suspended} rows={14} /></Field><div className="flex gap-2"><Button disabled={busy || suspended} onClick={() => void save()}>保存草稿</Button><Button variant="secondary" disabled={busy || suspended || candidate.state !== "AwaitingConfirmation"} onClick={() => setConfirming(true)}>确认知识</Button><Button variant="destructive" disabled={busy || suspended || candidate.state !== "AwaitingConfirmation"} onClick={() => void exclude()}>排除</Button></div><AlertDialog open={confirming} onOpenChange={setConfirming}><AlertDialogContent><AlertDialogHeader><AlertDialogTitle>确认发布此知识？</AlertDialogTitle><AlertDialogDescription>确认后将创建不可变知识版本并进入检索。</AlertDialogDescription></AlertDialogHeader><AlertDialogFooter><AlertDialogCancel>取消</AlertDialogCancel><AlertDialogAction disabled={busy} onClick={event => { event.preventDefault(); void confirm(); }}>确认</AlertDialogAction></AlertDialogFooter></AlertDialogContent></AlertDialog></section>;
}
function KnowledgeItem({ id, suspended, navigate, openEvidence }: { id: string; suspended: boolean; navigate: (route: string) => void; openEvidence: (id: string) => void }) {
 const [detail, setDetail] = useState<KnowledgeDetail>(); const [versions, setVersions] = useState<KnowledgeVersionSummary[]>([]); const [current, setCurrent] = useState<KnowledgeVersionDetail>(); const [error, setError] = useState(""); const [stop, setStop] = useState(false);
 const load = async () => { try { const [item, page] = await Promise.all([api.getKnowledge(id), api.listVersions(id)]); setDetail(item); setVersions(page.items); setCurrent(await api.getVersion(id, item.currentVersionId)); } catch (reason) { setError(errorText(reason)); } };
 useEffect(() => { queueMicrotask(() => { void load(); }); }, [id]); if (!detail) return <p role="status">正在读取知识…</p>;
 const currentDetail = detail;
 async function revise() { try { const candidate = await api.createRevision(currentDetail.id, currentDetail.currentVersionId, currentDetail.rowVersion); navigate(`/knowledge/candidates/${candidate.id}`); } catch (reason) { setError(errorText(reason)); } }
 async function stopReuse() { const version = versions.find(value => value.id === currentDetail.currentVersionId); if (!version) return; try { await api.stopReuse(currentDetail.id, version.id, version.retrievalStateRowVersion); setStop(false); await load(); } catch (reason) { setError(errorText(reason)); } }
 return <section className="space-y-5"><div><Badge variant={detail.eligible ? "default" : "secondary"}>{detail.eligible ? "可复用" : "已退出检索"}</Badge><h2 className="mt-2 text-xl font-semibold">{detail.title}</h2></div>{error && <Alert variant="destructive"><AlertDescription>{error}</AlertDescription></Alert>}<article><h3 className="font-medium">当前正文</h3><p className="mt-2 whitespace-pre-wrap text-sm">{current?.body}</p><FieldDescription>语义索引：{current ? embeddingStateLabels[current.embeddingState] ?? current.embeddingState : "读取中"}</FieldDescription></article><div className="flex gap-2"><Button disabled={suspended} onClick={() => void revise()}>创建修订</Button>{detail.eligible && <Button variant="destructive" disabled={suspended} onClick={() => setStop(true)}>停止复用</Button>}<Button variant="outline" onClick={() => openEvidence(current?.sourceCandidateId ?? "") } disabled={!current?.sourceCandidateId}>查看来源</Button></div><section><h3 className="font-medium">版本历史</h3>{versions.map(version => <Button key={version.id} variant="ghost" className="h-auto w-full justify-start border-b py-2 text-left text-sm" onClick={() => void api.getVersion(currentDetail.id, version.id).then(setCurrent).catch(reason => setError(errorText(reason)))}>v{version.versionSeq} · {version.title} · {version.eligible ? "可检索" : "已退出"}</Button>)}</section><AlertDialog open={stop} onOpenChange={setStop}><AlertDialogContent><AlertDialogHeader><AlertDialogTitle>停止复用当前版本？</AlertDialogTitle><AlertDialogDescription>此操作会使当前版本永久退出检索。要恢复内容，必须创建新的修订。</AlertDialogDescription></AlertDialogHeader><AlertDialogFooter><AlertDialogCancel>取消</AlertDialogCancel><AlertDialogAction onClick={event => { event.preventDefault(); void stopReuse(); }}>停止复用</AlertDialogAction></AlertDialogFooter></AlertDialogContent></AlertDialog></section>;
}
function ImportBatch({ id, suspended, navigate }: { id: string; suspended: boolean; navigate: (route: string) => void }) {
 const [text, setText] = useState(""); const [batch, setBatch] = useState<ImportBatchDetail>(); const [selected, setSelected] = useState<Set<string>>(new Set()); const [error, setError] = useState(""); const [busy, setBusy] = useState(false);
 const load = async (batchId: string) => { try { const value = await api.getImportBatch(batchId); setBatch(value); setSelected(new Set(value.candidates.filter(candidate => candidate.state === "AwaitingConfirmation").map(candidate => candidate.id))); } catch (reason) { setError(errorText(reason)); } };
 useEffect(() => { if (id !== "new") void load(id); }, [id]);
 useEffect(() => { if (!batch || batch.state !== "Processing" || suspended) return; const timer = window.setInterval(() => void load(batch.id), 2000); return () => window.clearInterval(timer); }, [batch?.id, batch?.state, suspended]);
 async function start() { setBusy(true); try { const value = await api.startImport(text); setText(""); setBatch(value); navigate(`/knowledge/imports/${value.id}`); } catch (reason) { setError(errorText(reason)); } finally { setBusy(false); } }
 async function confirm() { if (!batch) return; setBusy(true); try { await api.confirmBatch(batch.id, batch.candidates.filter(c => selected.has(c.id)).map(c => ({ candidateId: c.id, expectedRevision: c.draftRevision }))); await load(batch.id); } catch (reason) { setError(errorText(reason)); } finally { setBusy(false); } }
 async function cancel() { if (!batch) return; setBusy(true); try { await api.cancelBatch(batch.id, batch.rowVersion); await load(batch.id); } catch (reason) { setError(errorText(reason)); } finally { setBusy(false); } }
 if (id === "new") return <section className="space-y-4"><h2 className="text-xl font-semibold">导入原文</h2><Field><FieldLabel htmlFor="import-text">文本</FieldLabel><Textarea id="import-text" rows={16} value={text} onChange={e => setText(e.target.value)} disabled={suspended || busy}/><FieldDescription>原文将被分解为待确认候选；不会使用通用 JSON 代替正文。</FieldDescription></Field>{error && <Alert variant="destructive"><AlertDescription>{error}</AlertDescription></Alert>}<Button disabled={!text.trim() || suspended || busy} onClick={() => void start()}>开始导入</Button></section>;
 return <section className="space-y-4"><h2 className="text-xl font-semibold">导入批次</h2>{error && <Alert variant="destructive"><AlertDescription>{error}</AlertDescription></Alert>}{!batch ? <p role="status">正在读取批次…</p> : <><Badge>{batch.state}</Badge>{batch.state === "Processing" && <p className="text-sm text-muted-foreground">正在处理；会在此页面可见时刷新。</p>}<div className="space-y-2">{batch.candidates.map(candidate => <label key={candidate.id} className="flex items-start gap-2 rounded border p-3"><Checkbox checked={selected.has(candidate.id)} disabled={candidate.state !== "AwaitingConfirmation" || busy || suspended} onCheckedChange={checked => setSelected(value => { const next = new Set(value); if (checked) next.add(candidate.id); else next.delete(candidate.id); return next; })}/><span><strong>{candidate.draftTitle ?? "未命名候选"}</strong><small className="block text-muted-foreground">r{candidate.draftRevision} · {candidateStateLabels[candidate.state]}</small></span></label>)}</div><div className="flex gap-2"><Button disabled={!selected.size || busy || suspended || batch.state !== "AwaitingConfirmation"} onClick={() => void confirm()}>事务性确认所选项</Button><Button variant="destructive" disabled={busy || suspended || !["Processing", "AwaitingConfirmation"].includes(batch.state)} onClick={() => void cancel()}>取消批次</Button></div></>}</section>;
}
