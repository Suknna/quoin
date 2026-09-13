/* eslint-disable react-refresh/only-export-components, react-hooks/exhaustive-deps -- Domain view factories intentionally colocate lifecycle helpers with their route component. */
import { useEffect, useState } from "react";
import type { WorkspaceModuleProps, WorkspaceModuleView } from "@/app/module-contract";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { About } from "./About";
import { Backups } from "./Backups";
import { FeatureUnderConstruction } from "@/components/FeatureUnderConstruction";
import { Runtimes } from "./Runtimes";
import { Users } from "./Users";

type Page<T> = { items?: T[]; nextCursor?: string };
type Audit = { id: string; actorType: string; actorId: string; action: string; outcome: string; createdAt: string; domainRefType?: string; domainRefId?: string };
const failure = (reason: unknown) => reason instanceof Error ? reason.message : "暂时无法完成操作，请重试。";
async function request<T>(path: string): Promise<T> { const response = await fetch(path, { credentials: "include" }); if (!response.ok) { const body = await response.json().catch(() => null) as { message?: string; detail?: string } | null; throw new Error(body?.message ?? body?.detail ?? "暂时无法完成操作，请重试。"); } return response.status === 204 ? undefined as T : response.json() as Promise<T>; }

/** Administration projects API-backed surfaces only; Connections and system configuration remain coordinator-owned. */
export function useAdministrationModule(props: WorkspaceModuleProps): WorkspaceModuleView {
 // Routes are absolute (`/administration/users`); module selection starts after its prefix.
 const routeParts = props.route.split("/").filter(Boolean);
 const path = routeParts[0] === "administration" || routeParts[0] === "admin" ? routeParts[1] ?? "users" : routeParts[0] ?? "users";
 const labels: Record<string, string> = { about: "关于", users: "用户", model_provider: "模型提供方", backups: "备份与保留", audit: "审计", runtime: "运行时", journeys: "Journey（开发中）" };
 const list = <div className="space-y-1 p-3">{Object.entries(labels).map(([key, label]) => <Button key={key} variant={path === key ? "secondary" : "ghost"} className="w-full justify-start" disabled={key === "journeys"} title={key === "journeys" ? "浏览器巡检开发中，暂不可用" : undefined} onClick={() => props.navigate(`/admin/${key}`)}>{label}</Button>)}</div>;
 if (props.user.role !== "admin") return { title: "管理", list: null, content: <Alert variant="destructive"><AlertDescription>管理功能仅向管理员开放。</AlertDescription></Alert> };
 const content = path === "about" ? <About suspended={props.suspended} authenticationSuspended={props.authenticationSuspended} /> : path === "users" ? <Users suspended={props.suspended} /> : path === "backups" ? <Backups suspended={props.suspended} /> : path === "audit" ? <AuditLog /> : path === "runtime" ? <Runtimes suspended={props.suspended} /> : path === "journeys" ? <FeatureUnderConstruction title="浏览器巡检开发中" description="Journey 目录和浏览器巡检暂未开放；已存储的目录、身份和巡检数据不会被删除或修改。" /> : <Alert variant="destructive"><AlertDescription>未找到此管理页面。</AlertDescription></Alert>;
 return { title: labels[path] ?? "管理", list, content };
}


function AuditLog() { const [items, setItems] = useState<Audit[]>([]); const [cursor, setCursor] = useState<string>(); const [action, setAction] = useState(""); const [actorType, setActorType] = useState(""); const [error, setError] = useState(""); const load = async (more = false) => { try { const q = new URLSearchParams({ limit: "50" }); if (action) q.set("action", action); if (actorType) q.set("actorType", actorType); if (more && cursor) q.set("cursor", cursor); const page = await request<Page<Audit>>(`/api/v1/audit-events?${q}`); setItems(value => more ? [...value, ...(page.items ?? [])] : page.items ?? []); setCursor(page.nextCursor); } catch (reason) { setError(failure(reason)); } }; useEffect(() => { queueMicrotask(() => { void load(); }); }, []); return <section className="space-y-4"><h2 className="text-xl font-semibold">审计</h2><div className="flex gap-2"><Input aria-label="操作筛选" placeholder="操作" value={action} onChange={e => setAction(e.target.value)}/><Input aria-label="主体类型筛选" placeholder="主体类型" value={actorType} onChange={e => setActorType(e.target.value)}/><Button variant="secondary" onClick={() => void load()}>应用筛选</Button></div>{error && <Alert variant="destructive"><AlertDescription>{error}</AlertDescription></Alert>}<Table><TableHeader><TableRow><TableHead>时间</TableHead><TableHead>主体</TableHead><TableHead>操作</TableHead><TableHead>结果</TableHead></TableRow></TableHeader><TableBody>{items.map(item => <TableRow key={item.id}><TableCell>{item.createdAt}</TableCell><TableCell>{item.actorType} · {item.actorId}</TableCell><TableCell>{item.action}<small className="block text-muted-foreground">{item.domainRefType} {item.domainRefId}</small></TableCell><TableCell>{item.outcome}</TableCell></TableRow>)}</TableBody></Table>{cursor && <Button variant="outline" onClick={() => void load(true)}>加载更多</Button>}</section>; }
