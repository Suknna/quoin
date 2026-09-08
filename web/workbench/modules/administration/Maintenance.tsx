import { useEffect, useState } from "react";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { FieldDescription } from "@/components/ui/field";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { cancelDrainTarget, drainTargetOf, exitMaintenance, fetchMaintenanceState, prepareUpgrade, type MaintenanceStateView } from "../../../src/features/admin/maintenance/api";
import { ConfirmAction } from "./controls";

/** Maintenance actions use the server reason allowlist; no force or skip escape hatch exists. */
export function Maintenance({ authenticationSuspended = false }: { authenticationSuspended?: boolean }) {
 const [state, setState] = useState<MaintenanceStateView>(); const [error, setError] = useState("");
 const load = async () => { try { setState(await fetchMaintenanceState()); } catch (reason) { setError(reason instanceof Error ? reason.message : "暂时无法读取维护状态。"); } };
 useEffect(() => { queueMicrotask(() => { void load(); }); }, []);
 const run = async (action: () => Promise<unknown>) => { try { await action(); await load(); } catch (reason) { setError(reason instanceof Error ? reason.message : "维护操作未完成。"); } };
 const maintenanceReason = state?.reason;
 return <section className="space-y-4"><h2 className="text-xl font-semibold">维护</h2>{error && <Alert variant="destructive"><AlertDescription>{error}</AlertDescription></Alert>}{!state ? <p role="status">正在读取维护状态…</p> : <><Badge>{state.active ? `${state.reason} 维护中` : "未维护"}</Badge><Table><TableHeader><TableRow><TableHead>项目</TableHead><TableHead>安全状态</TableHead><TableHead>说明</TableHead><TableHead /></TableRow></TableHeader><TableBody>{state.items.map(item => { const target = state.reason === "Upgrade" ? drainTargetOf(item) : null; return <TableRow key={`${item.kind}-${item.objectKey}`}><TableCell>{item.kind} · {item.objectKey}</TableCell><TableCell><Badge variant={item.safeState === "Safe" ? "secondary" : "destructive"}>{item.safeState}</Badge></TableCell><TableCell>{item.detailCode}</TableCell><TableCell>{target && <ConfirmAction title="取消此升级排空工作？" description="仅执行服务端安全清单给出的取消命令，不会跳过或强制完成维护。" disabled={authenticationSuspended} onConfirm={() => void run(() => cancelDrainTarget(target))}>取消排空</ConfirmAction>}</TableCell></TableRow>; })}</TableBody></Table><FieldDescription>仅显示当前维护原因允许的修复命令；不提供 force 或 skip。</FieldDescription><div className="flex gap-2">{state.active && state.reason === "Upgrade" && <Button variant="outline" disabled={authenticationSuspended} onClick={() => void run(() => prepareUpgrade(state.rowVersion))}>继续升级排空</Button>}{state.active && maintenanceReason && <ConfirmAction title="退出维护？" description="只有所有项目均为 Safe 时，服务端才会允许退出维护。" disabled={authenticationSuspended || state.items.some(item => item.safeState === "Blocking")} onConfirm={() => void run(() => exitMaintenance(state.rowVersion, maintenanceReason))}>退出维护</ConfirmAction>}</div></>}</section>;
}
