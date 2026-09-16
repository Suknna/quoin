// Read-only detail of one business view. Views describe scope; they never own
// credentials or permissions, so the page states that boundary explicitly.

import { useEffect, useState } from "react";
import { ClipboardCheck } from "lucide-react";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Empty, EmptyDescription, EmptyHeader, EmptyTitle } from "@/components/ui/empty";
import { Separator } from "@/components/ui/separator";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { messageOf } from "@/app/shared";
import { formatTime, getBusinessView, type BusinessView } from "../api";

export function ViewDetail({ viewKey, suspended, navigate }: { viewKey: string; suspended: boolean; navigate: (to: string) => void }) {
  const [prevKey, setPrevKey] = useState(viewKey);
  const [state, setState] = useState<{ loadedKey: string; view?: BusinessView; error?: string }>({ loadedKey: viewKey });
  // Resetting during render (not inside an effect) keeps the pane consistent
  // the moment the selected key changes, without cascading renders.
  if (prevKey !== viewKey) {
    setPrevKey(viewKey);
    setState({ loadedKey: viewKey });
  }

  useEffect(() => {
    let cancelled = false;
    const timer = setTimeout(() => {
      void getBusinessView(viewKey)
        .then((next) => { if (!cancelled) setState({ loadedKey: viewKey, view: next }); })
        .catch((reason) => { if (!cancelled) setState({ loadedKey: viewKey, error: messageOf(reason, "无法读取业务视图。") }); });
    }, 0);
    return () => { cancelled = true; clearTimeout(timer); };
  }, [viewKey]);

  const current = state.loadedKey === viewKey ? state : undefined;
  if (current?.error) return <div className="p-6"><Alert variant="destructive"><AlertDescription>{current.error}</AlertDescription></Alert></div>;
  if (!current?.view) return <div className="p-6"><p role="status">正在读取业务视图…</p></div>;
  const view = current.view;

  const conditions = Object.entries(view.scope.labelConditions);
  return (
    <div className="mx-auto flex w-full max-w-5xl flex-col gap-4 p-6">
      <header className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h2 className="text-xl font-semibold">{view.displayName}</h2>
          <p className="text-sm text-muted-foreground">视图标识 {view.viewKey}</p>
        </div>
        <div className="flex flex-wrap gap-2">
            {/* Usage entry: hands the view scope to the inspection planner for preselection. */}
            <Button
              variant="outline"
              onClick={() => navigate(`/inspections?businessViewKey=${encodeURIComponent(view.viewKey)}${view.scope.connectionName ? `&connectionName=${encodeURIComponent(view.scope.connectionName)}` : ""}`)}
            >
              <ClipboardCheck data-icon="inline-start" />按此视图巡检
            </Button>
            <Button variant="outline" disabled={suspended} onClick={() => navigate(`/business-views?view=${encodeURIComponent(view.viewKey)}&edit=1`)}>编辑</Button>
          </div>
      </header>
      {view.description ? <p className="text-sm">{view.description}</p> : <p className="text-sm text-muted-foreground">未填写业务说明。</p>}
      <Card>
        <CardHeader>
          <CardTitle>范围</CardTitle>
          <CardDescription>范围只是组织与说明：视图不授予新的查询权限，凭据与访问边界仍在接入侧。</CardDescription>
        </CardHeader>
        <CardContent className="flex flex-col gap-4">
          <div className="flex flex-wrap items-center gap-2">
            <span className="text-sm font-medium">来源接入</span>
            {view.scope.connectionName ? <Badge variant="default">{view.scope.connectionName}</Badge> : <Badge variant="secondary">全部候选来源</Badge>}
          </div>
          <div className="flex flex-wrap items-center gap-2">
            <span className="text-sm font-medium">告警归属</span>
            {view.scope.alertSourceKeys?.length ? (
              view.scope.alertSourceKeys.map((key) => <Badge key={key} variant="default" className="font-mono">{key}</Badge>)
            ) : (
              <Badge variant="secondary">不参与告警归属</Badge>
            )}
          </div>
          <div className="flex flex-col gap-2">
            <span className="text-sm font-medium">标签条件</span>
            {conditions.length ? (
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>标签</TableHead>
                    <TableHead>值</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {conditions.map(([key, value]) => (
                    <TableRow key={key}>
                      <TableCell className="font-mono text-xs">{key}</TableCell>
                      <TableCell className="break-all font-mono text-xs">{value}</TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            ) : (
              <Empty className="min-h-24">
                <EmptyHeader>
                  <EmptyTitle>未设置标签条件</EmptyTitle>
                  <EmptyDescription>留空表示不按标签收窄候选对象{view.scope.alertSourceKeys?.length ? "；参与告警归属的视图必须设置标签条件" : ""}。</EmptyDescription>
                </EmptyHeader>
              </Empty>
            )}
          </div>
          <Separator />
          <dl className="grid gap-2 text-sm sm:grid-cols-3">
            <div><dt className="text-muted-foreground">创建时间</dt><dd>{formatTime(view.createdAt)}</dd></div>
            <div><dt className="text-muted-foreground">更新时间</dt><dd>{formatTime(view.updatedAt)}</dd></div>
            <div><dt className="text-muted-foreground">行版本</dt><dd>{view.rowVersion}</dd></div>
          </dl>
        </CardContent>
      </Card>
    </div>
  );
}
