import type { WorkspaceModuleProps, WorkspaceModuleView } from "@/app/module-contract";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { AuditPage } from "@/features/audit/ui";
import { About } from "./About";
import { Backups } from "./Backups";
import { Runtimes } from "./Runtimes";
import { Users } from "./Users";

/** Administration projects API-backed surfaces only; Connections and system configuration remain coordinator-owned.
 * Platform entries like browser/Journey are plugin-capability driven and live under /integrations —
 * this module deliberately carries no default placeholder navigation for them.
 * Audit is the single consolidated surface (docs/audit-design.md §6); personal account keeps no audit entry. */
export function useAdministrationModule(props: WorkspaceModuleProps): WorkspaceModuleView {
 // Routes are absolute (`/administration/users`); module selection starts after its prefix.
 const routeParts = props.route.split("/").filter(Boolean);
 const path = routeParts[0] === "administration" || routeParts[0] === "admin" ? routeParts[1] ?? "users" : routeParts[0] ?? "users";
 const labels: Record<string, string> = { about: "关于", users: "用户", model_provider: "模型提供方", backups: "备份与保留", audit: "审计", runtime: "运行时" };
 const list = <div className="space-y-1 p-3">{Object.entries(labels).map(([key, label]) => <Button key={key} variant={path === key ? "secondary" : "ghost"} className="w-full justify-start" onClick={() => props.navigate(`/admin/${key}`)}>{label}</Button>)}</div>;
 if (props.user.role !== "admin") return { title: "管理", list: null, content: <Alert variant="destructive"><AlertDescription>管理功能仅向管理员开放。</AlertDescription></Alert> };
 const content = path === "about" ? <About suspended={props.suspended} /> : path === "users" ? <Users suspended={props.suspended} /> : path === "backups" ? <Backups suspended={props.suspended} /> : path === "audit" ? <AuditPage suspended={props.suspended} /> : path === "runtime" ? <Runtimes suspended={props.suspended} /> : <Alert variant="destructive"><AlertDescription>未找到此管理页面。</AlertDescription></Alert>;
 return { title: labels[path] ?? "管理", list, content };
}
