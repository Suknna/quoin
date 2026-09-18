import { messageOf, notify } from "@/app/shared";
import { LoaderCircle } from "lucide-react";
import { useCallback, useEffect, useState } from "react";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { RefreshButton } from "@/components/workbench/RefreshButton";
import { Button } from "@/components/ui/button";
import { Empty, EmptyDescription, EmptyHeader, EmptyTitle } from "@/components/ui/empty";
import { FieldDescription } from "@/components/ui/field";
import { Skeleton } from "@/components/ui/skeleton";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { cancelDrainTarget, drainTargetOf, exitMaintenance, fetchMaintenanceState, prepareUpgrade, type MaintenanceStateView } from "@/features/settings/platform/maintenance/api";
import { ConfirmAction } from "../controls";

const safeStateLabels: Record<string, string> = { Safe: "安全", Blocking: "阻塞" };

/** Maintenance actions use the server reason allowlist; no force or skip escape hatch exists. */
export function Maintenance({ refreshRevision = 0, onChanged }: { refreshRevision?: number; onChanged?: () => Promise<void> }) {
 const [state, setState] = useState<MaintenanceStateView>(); const [error, setError] = useState(""); const [loading, setLoading] = useState(false); const [pending, setPending] = useState(false);
 const load = useCallback(async () => { try { setLoading(true); setError(""); setState(await fetchMaintenanceState()); } catch (reason) { setError(messageOf(reason, "暂时无法读取维护状态。")); } finally { setLoading(false); } }, []);
 useEffect(() => { void load(); }, [load, refreshRevision]);
 const refresh = async () => { await load(); await onChanged?.(); };
 const run = async (action: () => Promise<unknown>, successMessage: string) => { if (pending) return; try { setPending(true); setError(""); await action(); notify.success(successMessage); await load(); await onChanged?.(); } catch (reason) { notify.error(reason, "维护操作未完成。"); } finally { setPending(false); } };
 const disabled = pending || loading;
 const maintenanceReason = state?.reason;
 return <section className="flex flex-col gap-4"><div className="flex flex-wrap items-start justify-between gap-3"><div><h3 className="text-sm font-medium">维护</h3><p className="mt-1 text-sm text-muted-foreground">仅显示服务端允许的安全修复命令。</p></div><RefreshButton size="sm" loading={loading} disabled={disabled} onClick={() => void refresh()} /></div>{error && <Alert variant="destructive"><AlertDescription>{error} <Button variant="link" className="h-auto p-0 align-baseline" onClick={() => void load()} disabled={disabled}>重试</Button></AlertDescription></Alert>}{!state ? <div className="flex flex-col gap-3" role="status" aria-label="正在读取维护状态"><Skeleton className="h-6 w-1/3" /><Skeleton className="h-24 w-full" /><Skeleton className="h-4 w-2/3" /></div> : <><Badge variant={state.active ? "destructive" : "secondary"}>{state.active ? `${state.reason ?? "未知原因"} 维护中` : "未维护"}</Badge>{state.items.length === 0 ? <Empty><EmptyHeader><EmptyTitle>没有维护清单项目</EmptyTitle><EmptyDescription>当前没有需要处理的安全检查项。</EmptyDescription></EmptyHeader></Empty> : <Table><TableHeader><TableRow><TableHead>项目</TableHead><TableHead>安全状态</TableHead><TableHead>说明</TableHead><TableHead /></TableRow></TableHeader><TableBody>{state.items.map(item => { const target = state.reason === "Upgrade" ? drainTargetOf(item) : null; return <TableRow key={`${item.kind}-${item.objectKey}`}><TableCell className="max-w-72 whitespace-normal wrap-anywhere">{item.kind} · {item.objectKey}</TableCell><TableCell><Badge variant={item.safeState === "Safe" ? "secondary" : "destructive"}>{safeStateLabels[item.safeState] ?? item.safeState}</Badge></TableCell><TableCell className="max-w-96 whitespace-normal wrap-anywhere">{item.detailCode || "未知"}</TableCell><TableCell>{target && <ConfirmAction title="取消此升级排空工作？" description="仅执行服务端安全清单给出的取消命令，不会跳过或强制完成维护。" disabled={disabled} onConfirm={() => void run(() => cancelDrainTarget(target), "已取消排空")}>取消排空</ConfirmAction>}</TableCell></TableRow>; })}</TableBody></Table>}<FieldDescription>仅显示当前维护原因允许的修复命令；不提供 force 或 skip。</FieldDescription><div className="flex gap-2">{!state.active && <ConfirmAction title="准备升级维护？" description="将由服务端创建升级维护清单；未满足安全条件时不会直接进入或跳过维护。" disabled={disabled} onConfirm={() => void run(() => prepareUpgrade(state.rowVersion), "已准备升级")}>准备升级</ConfirmAction>}{state.active && state.reason === "Upgrade" && <Button variant="outline" disabled={disabled} onClick={() => void run(() => prepareUpgrade(state.rowVersion), "已准备升级")}>{pending ? <><LoaderCircle className="animate-spin" data-icon="inline-start" aria-hidden="true" />继续中…</> : "继续升级排空"}</Button>}{state.active && maintenanceReason && <ConfirmAction title="退出维护？" description="只有所有项目均为 Safe 时，服务端才会允许退出维护。" disabled={disabled || state.items.some(item => item.safeState === "Blocking")} onConfirm={() => void run(() => exitMaintenance(state.rowVersion, maintenanceReason), "已退出维护")}>退出维护</ConfirmAction>}</div></>}</section>;
}
