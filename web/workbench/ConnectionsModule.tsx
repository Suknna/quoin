import { Checkbox } from "../templates/components/ui/checkbox";
import { type ComponentProps, type FormEvent, useEffect, useRef, useState } from "react";
import { Plus } from "lucide-react";
import { Button } from "../templates/components/ui/button";
import { Field, FieldLabel } from "../templates/components/ui/field";
import { Input } from "../templates/components/ui/input";
import { Textarea } from "./components/ui/textarea";
import {
	AlertDialog,
	AlertDialogAction,
	AlertDialogCancel,
	AlertDialogContent,
	AlertDialogDescription,
	AlertDialogFooter,
	AlertDialogHeader,
	AlertDialogTitle,
} from "../templates/components/ui/alert-dialog";
import {
	Select,
	SelectContent,
	SelectItem,
	SelectTrigger,
	SelectValue,
} from "../templates/components/ui/select";
import {
	type ConnectionDetailView,
	type ConnectionInput,
	type ConnectionRevisionView,
	type ConnectionSummaryView,
	type ConnectionType,
	type CredentialGenerationView,
	type ProbeAttemptView,
	type ProbeResultView,
	WorkbenchApiError,
	workbenchApi,
} from "./api";
import { ModelProviderEditor } from "./ModelProviderEditor";
import type { WorkspaceModuleProps, WorkspaceModuleView } from "./module-contract";
import { ErrorMessage, messageOf } from "./shared";

const terminalStates = ["Succeeded", "Failed", "Cancelled", "Interrupted"];
type EditableConnectionType = ConnectionType;

/** Keeps secret values in component memory and removes them when the workspace is suspended. */
function ConnectionFields({
	type,
	value,
	onChange,
	disabled,
}: {
	type: EditableConnectionType;
	value: Record<string, string>;
	onChange: (field: string, next: string) => void;
	disabled: boolean;
}) {
	const input = (field: string, label: string, extra: Partial<ComponentProps<typeof Input>> = {}) => (
		<Field key={field}>
			<FieldLabel htmlFor={`connection-${field}`}>{label}</FieldLabel>
			<Input id={`connection-${field}`} value={value[field] ?? ""} onChange={(event) => onChange(field, event.target.value)} disabled={disabled} {...extra} />
		</Field>
	);
	if (type === "kubernetes") return <>
		{input("contextName", "Context 名称", { required: true })}
		{input("defaultNamespace", "默认 Namespace", { required: true })}
		<Field><FieldLabel htmlFor="kubeconfig">Kubeconfig</FieldLabel><Textarea id="kubeconfig" value={value.kubeconfig ?? ""} onChange={(event) => onChange("kubeconfig", event.target.value)} disabled={disabled} required /></Field>
	</>;
	if (type === "model_provider") return <>
		{input("baseUrl", "Base URL", { type: "url", required: true })}
		{input("apiKey", "API Key", { type: "password", autoComplete: "new-password", required: true })}
		{input("chatModelId", "对话模型 ID", { required: true })}
		{input("embeddingModelId", "Embedding 模型 ID", { required: true })}
		{input("contextBudgetTokens", "Context 预算 tokens", { type: "number", min: 1, required: true })}
		{input("maxOutputTokens", "最大输出 tokens", { type: "number", min: 1, required: true })}
	</>;
	return <>
		{input("baseUrl", "Base URL", { type: "url", placeholder: "https://thanos.example", required: true })}
		{input("username", "用户名（可选）", { autoComplete: "username" })}
		{input("password", "密码（可选）", { type: "password", autoComplete: "new-password" })}
		<Field><FieldLabel htmlFor="tlsCaPem">TLS CA PEM（可选）</FieldLabel><Textarea id="tlsCaPem" value={value.tlsCaPem ?? ""} onChange={(event) => onChange("tlsCaPem", event.target.value)} disabled={disabled} /></Field>
		{input("tlsServerName", "TLS Server Name（可选）")}
		<div className="flex items-center gap-2"><Checkbox id="tlsSkipVerify" checked={value.tlsSkipVerify === "true"} onCheckedChange={(checked) => onChange("tlsSkipVerify", checked === true ? "true" : "")} disabled={disabled} /><FieldLabel htmlFor="tlsSkipVerify">跳过上游 TLS 证书验证（仅在确有必要时启用）</FieldLabel></div>
	</>;
}

function toInput(type: EditableConnectionType, fields: Record<string, string>): ConnectionInput {
	if (type === "kubernetes") return { type, contextName: fields.contextName, defaultNamespace: fields.defaultNamespace, kubeconfig: fields.kubeconfig };
	if (type === "model_provider") return {
		type, baseUrl: fields.baseUrl, apiKey: fields.apiKey, chatModelId: fields.chatModelId, embeddingModelId: fields.embeddingModelId,
		contextBudgetTokens: Number(fields.contextBudgetTokens), maxOutputTokens: Number(fields.maxOutputTokens),
	};
	return { type, baseUrl: fields.baseUrl, ...(fields.username ? { username: fields.username } : {}), ...(fields.password ? { password: fields.password } : {}), ...(fields.tlsCaPem ? { tlsCaPem: fields.tlsCaPem } : {}), ...(fields.tlsServerName ? { tlsServerName: fields.tlsServerName } : {}), ...(fields.tlsSkipVerify === "true" ? { tlsSkipVerify: true } : {}) };
}

function ConnectionEditor({ onCreated, readOnly, suspended = false }: { onCreated: (connection: ConnectionSummaryView) => void; readOnly: boolean; suspended?: boolean }) {
	const [name, setName] = useState("");
	const [type, setType] = useState<EditableConnectionType>("thanos");
	const [fields, setFields] = useState<Record<string, string>>({});
	const [error, setError] = useState("");
	const [saving, setSaving] = useState(false);
	useEffect(() => { if (suspended) setFields(current => ({ ...current, password: "", kubeconfig: "", apiKey: "" })); }, [suspended]);
	async function submit(event: FormEvent) {
		event.preventDefault(); setSaving(true); setError("");
		try { onCreated(await workbenchApi.createConnection(name, toInput(type, fields))); setFields(current => ({ ...current, password: "", kubeconfig: "", apiKey: "" })); }
		catch (reason) { setError(messageOf(reason, "暂时无法创建连接。")); }
		finally { setSaving(false); }
	}
	const disabled = readOnly || saving || suspended;
	return <form className="grid gap-4" onSubmit={submit}>
		<div><h2 className="text-lg font-semibold">添加连接</h2><p className="text-sm text-muted-foreground">秘密仅随本次请求发送，不会回显或存储在浏览器。</p></div>
		{error && <ErrorMessage>{error}</ErrorMessage>}
		<Field><FieldLabel htmlFor="connection-name">名称</FieldLabel><Input id="connection-name" value={name} onChange={(event) => setName(event.target.value)} required disabled={disabled} /></Field>
		<Field><FieldLabel>连接类型</FieldLabel><Select value={type} onValueChange={(next) => { if (next === "thanos" || next === "kubernetes" || next === "model_provider") setType(next); }} disabled={disabled}><SelectTrigger><SelectValue /></SelectTrigger><SelectContent><SelectItem value="thanos">Thanos</SelectItem><SelectItem value="kubernetes">Kubernetes</SelectItem><SelectItem value="model_provider">模型提供方</SelectItem></SelectContent></Select></Field>
		<ConnectionFields type={type} value={fields} onChange={(field, next) => setFields(current => ({ ...current, [field]: next }))} disabled={disabled} />
		<Button type="submit" disabled={disabled}>{saving ? "正在创建…" : "创建连接"}</Button>
	</form>;
}

function ConnectionDetail({ selected, onUpdate, onRefresh, readOnly, suspended = false }: { selected: ConnectionDetailView; onUpdate: (connection: ConnectionSummaryView) => void; onRefresh: () => void; readOnly: boolean; suspended?: boolean }) {
	const [attempt, setAttempt] = useState<ProbeAttemptView | undefined>(selected.activeProbeAttempt);
	const [results, setResults] = useState<ProbeResultView[]>([]);
	const [revisions, setRevisions] = useState<ConnectionRevisionView[]>([]);
	const [generations, setGenerations] = useState<CredentialGenerationView[]>([]);
	const [error, setError] = useState(""); const [busy, setBusy] = useState(false);
	const [confirmation, setConfirmation] = useState<"enable" | "disable" | undefined>();
	const [rotating, setRotating] = useState(false);
	const [rotationFields, setRotationFields] = useState<Record<string, string>>(() => Object.entries(selected.config).reduce<Record<string, string>>((fields, [key, value]) => typeof value === "string" ? { ...fields, [key]: value } : fields, {}));
	const polling = useRef(false); const alive = useRef(true);
	const editableType: EditableConnectionType = selected.type;
	async function loadHistory() {
		try { const [nextResults, nextRevisions, nextGenerations] = await Promise.all([workbenchApi.listProbeResults(selected.name), workbenchApi.listRevisions(selected.name), workbenchApi.listCredentialGenerations(selected.name)]); if (alive.current) { setResults(nextResults); setRevisions(nextRevisions); setGenerations(nextGenerations); } }
		catch (reason) { if (alive.current) setError(messageOf(reason, "连接历史读取失败。")); }
	}
	useEffect(() => { alive.current = true; setAttempt(selected.activeProbeAttempt); setError(""); void loadHistory(); return () => { alive.current = false; }; }, [selected.name]);
	useEffect(() => { if (suspended) setRotationFields(current => ({ ...current, password: "", kubeconfig: "", apiKey: "" })); }, [suspended]);
	useEffect(() => {
		if (suspended || !attempt || terminalStates.includes(attempt.state)) return;
		const timer = window.setInterval(() => { if (polling.current) return; polling.current = true; void workbenchApi.fetchProbeAttempt(selected.name, attempt.id).then(next => { if (!alive.current) return; setAttempt(current => current && current.id === next.id && current.rowVersion > next.rowVersion ? current : next); if (terminalStates.includes(next.state)) { void loadHistory(); onRefresh(); } }).catch(reason => { if (alive.current) setError(messageOf(reason, "探测状态读取失败。")); }).finally(() => { polling.current = false; }); }, 1500);
		return () => window.clearInterval(timer);
	}, [attempt?.id, attempt?.state, selected.name, suspended]);
	async function probe() { setBusy(true); setError(""); try { const next = await workbenchApi.probeConnection(selected.name); setAttempt(next); if (terminalStates.includes(next.state)) { await loadHistory(); onRefresh(); } } catch (reason) { setError(messageOf(reason, "暂时无法发起探测。")); } finally { setBusy(false); } }
	async function cancelProbe() { if (!attempt) return; setBusy(true); setError(""); try { setAttempt(await workbenchApi.cancelProbeAttempt(selected.name, attempt.id, attempt.rowVersion)); } catch (reason) { setError(messageOf(reason, "暂时无法取消探测。")); } finally { setBusy(false); } }
	async function changeEnabled() {
		setBusy(true); setError("");
		try {
			if (confirmation === "disable") onUpdate(await workbenchApi.disableConnection(selected.name, selected.rowVersion));
			else {
				const qualified = selected.type === "model_provider" ? results.filter(result => result.connectionType === "model_provider" && result.outcome === "passed" && result.connectionRevisionId === selected.currentRevisionId && result.credentialGenerationId === selected.currentCredentialGenerationId).sort((left, right) => right.finishedAt.localeCompare(left.finishedAt))[0] : undefined;
				if (selected.type === "model_provider" && (!qualified || selected.revalidationRequired)) { setError("模型提供方必须使用当前 revision 和凭据 generation 的已通过探测结果启用。"); return; }
				onUpdate(await workbenchApi.enableConnection(selected.name, selected.rowVersion, qualified?.id));
			}
			setConfirmation(undefined);
		} catch (reason) { if (reason instanceof WorkbenchApiError && reason.status === 409 && reason.code === "row_version_conflict") { setError("连接版本冲突，正在刷新详情；请核对最新版本后重试。"); onRefresh(); } else setError(messageOf(reason, "暂时无法更新连接。")); } finally { setBusy(false); }
	}
	async function rotate(event: FormEvent) { event.preventDefault(); setBusy(true); setError(""); try { onUpdate(await workbenchApi.rotateConnection(selected.name, selected.rowVersion, toInput(editableType, rotationFields))); setRotationFields(current => ({ ...current, password: "", kubeconfig: "", apiKey: "" })); setRotating(false); } catch (reason) { setError(messageOf(reason, "暂时无法轮换连接凭据。")); } finally { setBusy(false); } }
	const mutationDisabled = readOnly || busy || suspended;
	return <section className="grid gap-4"><div><h2 className="text-lg font-semibold">{selected.name}</h2><p className="text-sm text-muted-foreground">{selected.type === "model_provider" ? "模型提供方" : selected.type === "kubernetes" ? "Kubernetes" : "Thanos"} · {selected.enabled ? "已启用" : "未启用"} · 版本 {selected.rowVersion}</p></div>
		{error && <ErrorMessage>{error}</ErrorMessage>}
		<div className="rounded-md border p-3 text-sm"><strong>当前配置（非秘密）</strong><pre className="mt-2 overflow-auto text-xs">{JSON.stringify(selected.config, null, 2)}</pre></div>
		<div className="flex flex-wrap gap-2"><Button onClick={() => void probe()} disabled={mutationDisabled || Boolean(attempt && !terminalStates.includes(attempt.state))}>{busy ? "处理中…" : "探测连接"}</Button>{attempt && !terminalStates.includes(attempt.state) && <Button variant="outline" onClick={() => void cancelProbe()} disabled={mutationDisabled}>取消探测</Button>}<Button variant="secondary" onClick={() => setConfirmation(selected.enabled ? "disable" : "enable")} disabled={mutationDisabled}>{selected.enabled ? "停用连接" : "启用连接"}</Button><Button variant="outline" onClick={() => setRotating(current => !current)} disabled={mutationDisabled}>轮换凭据</Button></div>
		<AlertDialog open={Boolean(confirmation)} onOpenChange={(open) => { if (!open && !busy) setConfirmation(undefined); }}><AlertDialogContent><AlertDialogHeader><AlertDialogTitle>{confirmation === "disable" ? "停用此连接？" : "启用此连接？"}</AlertDialogTitle><AlertDialogDescription>{confirmation === "disable" ? "停用会立即阻止该连接继续被使用。" : "启用会将当前已验证的连接投入使用。"}</AlertDialogDescription></AlertDialogHeader><AlertDialogFooter><AlertDialogCancel disabled={busy}>取消</AlertDialogCancel><AlertDialogAction disabled={busy} onClick={(event) => { event.preventDefault(); void changeEnabled(); }}>{confirmation === "disable" ? "确认停用" : "确认启用"}</AlertDialogAction></AlertDialogFooter></AlertDialogContent></AlertDialog>
		{rotating && <form className="grid gap-4 rounded-md border p-4" onSubmit={rotate}><div><h3 className="font-medium">轮换凭据</h3><p className="text-sm text-muted-foreground">字段名称和创建时相同；秘密不会回显，必须重新提供。</p></div><ConnectionFields type={editableType} value={rotationFields} onChange={(field, next) => setRotationFields(current => ({ ...current, [field]: next }))} disabled={mutationDisabled} /><Button type="submit" disabled={mutationDisabled}>确认轮换</Button></form>}
		{attempt && <div className="rounded-md bg-muted p-3 text-sm">探测 {attempt.id}：{attempt.state}{attempt.terminationReason ? `（${attempt.terminationReason}）` : ""}</div>}
		<div className="grid gap-3 md:grid-cols-2"><div className="rounded-md border p-3 text-sm"><strong>配置 Revisions（{selected.revisionCount}）</strong>{revisions.map(revision => <div key={revision.id} className="mt-2 border-t pt-2">#{revision.revisionSeq} · {revision.createdAt}<pre className="mt-1 overflow-auto text-xs">{JSON.stringify(revision.config, null, 2)}</pre></div>)}</div><div className="rounded-md border p-3 text-sm"><strong>凭据 Generations（{selected.generationCount}）</strong>{generations.map(generation => <div key={generation.id} className="mt-2 border-t pt-2">#{generation.generationSeq} · {generation.createdAt}{generation.createdBy ? ` · 创建者 ${generation.createdBy}` : ""}</div>)}</div></div>
		{results.length > 0 && <div className="rounded-md border p-3 text-sm"><strong>探测历史</strong>{results.map(result => <div key={result.id} className="mt-2 border-t pt-2"><div>{result.outcome} · {result.id}</div><div className="text-muted-foreground">完成于 {result.finishedAt} · revision {result.connectionRevisionId} · generation {result.credentialGenerationId} · digest {result.resultDigest}</div><pre className="mt-1 overflow-auto text-xs">{JSON.stringify(result.details, null, 2)}</pre></div>)}</div>}
	</section>;
}

/** Owns connection service state while returning only the shell's shared sidebar module contract. */
export function useConnectionsModule({ user, route, navigate, suspended }: WorkspaceModuleProps): WorkspaceModuleView {
	const [connections, setConnections] = useState<ConnectionSummaryView[]>([]); const [selected, setSelected] = useState<ConnectionDetailView>(); const [maintenance, setMaintenance] = useState(false); const [error, setError] = useState(""); const [loading, setLoading] = useState(true); const [detailLoading, setDetailLoading] = useState(false);
	const detailController = useRef<AbortController | undefined>(undefined); const refreshing = useRef(false); const readOnly = user.role !== "admin" || maintenance || suspended; const suffix = "/admin/connections"; const selectedName = route.startsWith(`${suffix}/`) ? route.slice(suffix.length + 1) : undefined; const isNew = route === `${suffix}/new` || route.startsWith(`${suffix}/new?`); const newConnectionType = route.includes("type=model_provider") ? "model_provider" : "connection";
	async function load() { if (refreshing.current) return; refreshing.current = true; setError(""); try { const state = await workbenchApi.maintenance(); setMaintenance(state?.active === true); setConnections((await workbenchApi.listConnections()).filter(item => item.type === "thanos" || item.type === "kubernetes" || item.type === "model_provider")); } catch (reason) { setError(messageOf(reason, "连接服务当前不可用。")); } finally { setLoading(false); refreshing.current = false; } }
	async function chooseByName(name: string) { setDetailLoading(true); setSelected(undefined); setError(""); detailController.current?.abort(); const controller = new AbortController(); detailController.current = controller; try { const next = await workbenchApi.fetchConnection(name, controller.signal); if (!controller.signal.aborted) setSelected(next); } catch (reason) { if (reason instanceof DOMException && reason.name === "AbortError") return; setError(messageOf(reason, "无法读取连接详情。")); } finally { if (detailController.current === controller) setDetailLoading(false); } }
	useEffect(() => { void load(); }, []); useEffect(() => { if (!selectedName || isNew) { detailController.current?.abort(); setSelected(undefined); setDetailLoading(false); return; } try { void chooseByName(decodeURIComponent(selectedName)); } catch { setError("连接地址无效。"); } }, [route]); useEffect(() => { if (suspended) return; const timer = window.setInterval(() => void load(), 15_000); return () => window.clearInterval(timer); }, [suspended]); useEffect(() => () => detailController.current?.abort(), []);
	const choose = (connection: ConnectionSummaryView) => navigate(`${suffix}/${encodeURIComponent(connection.name)}`); const update = (connection: ConnectionSummaryView) => { setConnections(items => items.map(item => item.name === connection.name ? connection : item)); void chooseByName(connection.name); }; const labels: Record<ConnectionType, string> = { thanos: "Thanos", kubernetes: "Kubernetes", model_provider: "模型提供方" };
	const list = <div className="px-0">{(["thanos", "kubernetes", "model_provider"] as const).map(type => <div key={type}><div className="border-b px-4 py-2 text-xs font-medium text-muted-foreground">{labels[type]}</div>{connections.filter(connection => connection.type === type).map(connection => <button type="button" key={connection.name} onClick={() => choose(connection)} className="flex w-full flex-col items-start gap-2 border-b p-4 text-left text-sm leading-tight hover:bg-sidebar-accent hover:text-sidebar-accent-foreground"><div className="flex w-full items-center gap-2"><span className="font-medium">{connection.name}</span><span className="ml-auto text-xs">{connection.enabled ? "已启用" : "未启用"}</span></div><span className="line-clamp-2 w-[260px] text-xs whitespace-break-spaces text-muted-foreground">{labels[type]} · {connection.revalidationRequired ? "需要重新验证" : `版本 ${connection.rowVersion}`}</span></button>)}</div>)}{!loading && !error && !connections.length && <div className="p-4 text-sm text-muted-foreground">尚无可管理连接。</div>}</div>;
	return { title: "监控连接", list, actions: <><Button variant="outline" size="sm" onClick={() => navigate(`${suffix}/new`)} disabled={readOnly} aria-label="新建"><Plus /> 新建连接</Button><Button variant="outline" size="sm" onClick={() => navigate(`${suffix}/new?type=model_provider`)} disabled={readOnly}><Plus /> 新建模型提供方</Button></>, content: <>{maintenance && <div className="mb-4 rounded-md border border-amber-500 bg-amber-50 p-3 text-sm dark:bg-amber-950">维护模式已启用：所有写入操作已阻止。</div>}{error && <div className="mb-4"><ErrorMessage>{error}</ErrorMessage></div>}{loading || detailLoading ? <div>正在读取真实连接服务…</div> : error ? null : selected ? <ConnectionDetail key={selected.name} selected={selected} onUpdate={update} onRefresh={() => void chooseByName(selected.name)} readOnly={readOnly} suspended={suspended} /> : isNew && newConnectionType === "model_provider" ? <ModelProviderEditor onCreated={connection => { setConnections(items => [...items, connection]); choose(connection); }} readOnly={readOnly} suspended={suspended} /> : <ConnectionEditor onCreated={connection => { setConnections(items => [...items, connection]); choose(connection); }} readOnly={readOnly} suspended={suspended} />}</> };
}
