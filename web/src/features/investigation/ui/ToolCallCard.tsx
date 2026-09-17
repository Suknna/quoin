import { ChevronDown, Wrench } from "lucide-react";
import { StructuredData } from "@/components/ai/StructuredData";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import { toolNameLabel, type ToolCallItem } from "@/features/investigation/tools/api";

const when = (value: string) => new Date(value).toLocaleString();

/** 状态是服务端投影，只映射 API 显式返回的五种终态/阶段。 */
function statusLabel(status: ToolCallItem["status"]): string {
  switch (status) {
    case "pending": return "排队中";
    case "running": return "执行中";
    case "succeeded": return "已完成";
    case "failed": return "失败";
    case "cancelled": return "已取消";
  }
}

function statusVariant(status: ToolCallItem["status"]): "default" | "secondary" | "destructive" | "outline" {
  if (status === "succeeded") return "secondary";
  if (status === "failed") return "destructive";
  if (status === "running") return "default";
  return "outline";
}

/** 摘要只机械投影真实错误与结果形态，不做语义推断；失败、取消与空结果如实区分。 */
function resultSummary(call: ToolCallItem): string {
  if (call.status === "failed" && call.errorDetail) return `失败原因：${call.errorDetail}`;
  if (call.status === "cancelled") return "执行已取消。";
  if (call.result === undefined || call.result === null) return call.status === "succeeded" ? "成功返回，无结果内容。" : "尚无结果。";
  if (Array.isArray(call.result)) return `返回数组 · ${call.result.length} 项。`;
  if (typeof call.result === "object") return `返回对象 · ${Object.keys(call.result).length} 个字段。`;
  return String(call.result);
}

/**
 * 本回合可定位的工具状态卡片：默认展示工具名、真实阶段/终态与有依据的摘要；
 * 展开后为结构化参数/结果字段与可复制的原文，未知类型安全回退为原文。
 */
export function ToolCallCard({ call }: { call: ToolCallItem }) {
  return <Collapsible className="rounded-lg border">
    <CollapsibleTrigger asChild>
      <Button variant="ghost" className="group h-auto w-full justify-start gap-2 whitespace-normal py-2 text-left">
        <Wrench data-icon="inline-start" />
        <span className="font-medium">{toolNameLabel(call.toolName)}</span>
        <Badge variant={statusVariant(call.status)}>{statusLabel(call.status)}</Badge>
        <span className="min-w-0 flex-1 text-xs text-muted-foreground">{resultSummary(call)}</span>
        <ChevronDown className="shrink-0 transition-transform group-data-[state=open]:rotate-180" aria-hidden="true" />
      </Button>
    </CollapsibleTrigger>
    <CollapsibleContent>
      <div className="flex flex-col gap-3 border-t px-3 py-2 text-sm">
        <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1">
          <dt className="text-muted-foreground">执行模式</dt><dd className="break-words">{call.executionMode}</dd>
          <dt className="text-muted-foreground">失败模式</dt><dd className="break-words">{call.failureMode}</dd>
          {call.startedAt && <><dt className="text-muted-foreground">开始</dt><dd>{when(call.startedAt)}</dd></>}
          {call.endedAt && <><dt className="text-muted-foreground">结束</dt><dd>{when(call.endedAt)}</dd></>}
          {call.errorDetail && <><dt className="text-muted-foreground">错误</dt><dd className="break-words">{call.errorDetail}</dd></>}
        </dl>
        <section className="flex flex-col gap-2"><h4 className="text-xs font-medium text-muted-foreground">参数</h4><StructuredData value={call.arguments} raw={JSON.stringify(call.arguments, null, 2)} /></section>
        {call.result !== undefined && call.result !== null
          ? <section className="flex flex-col gap-2"><h4 className="text-xs font-medium text-muted-foreground">结果</h4><StructuredData value={call.result} raw={JSON.stringify(call.result, null, 2)} /></section>
          : call.status === "succeeded" && <p className="text-xs text-muted-foreground">工具成功返回，但没有结果内容。</p>}
      </div>
    </CollapsibleContent>
  </Collapsible>;
}
