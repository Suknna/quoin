import type { ReactNode } from "react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Empty, EmptyDescription, EmptyHeader, EmptyTitle } from "@/components/ui/empty";
import { Skeleton } from "@/components/ui/skeleton";
import { cn } from "@/lib/utils";

export type EntityListColumn = "title" | "status" | "subtitle" | "time" | "actions";
export type EntityListItem = {
  id: string;
  title: string;
  badge?: { text: string; variant?: React.ComponentProps<typeof Badge>["variant"] };
  subtitle?: ReactNode;
  time?: ReactNode;
};

type EntityListProps<T extends EntityListItem> = {
  items: T[];
  columns: EntityListColumn[];
  selectedId?: string | null;
  onSelect?: (item: T) => void;
  renderActions?: (item: T) => ReactNode;
  loading?: boolean;
  error?: string;
  onRetry?: () => void;
  emptyTitle?: string;
  emptyDescription?: string;
  controls?: ReactNode;
};

/** A presentational list shell; callers own all query, lifecycle, and domain behavior. */
export function EntityList<T extends EntityListItem>({ items, columns, selectedId, onSelect, renderActions, loading, error, onRetry, emptyTitle = "没有数据", emptyDescription, controls }: EntityListProps<T>) {
  if (loading) return <div className="space-y-3 p-4" role="status" aria-label="正在加载"><Skeleton className="h-10 w-full" /><Skeleton className="h-20 w-full" /><Skeleton className="h-20 w-full" /></div>;
  return <div className="space-y-3 p-4">{controls}{error && <div role="alert" className="rounded-md border border-destructive p-3 text-sm text-destructive">{error}{onRetry && <Button className="ml-3" size="sm" variant="outline" onClick={onRetry}>重试</Button>}</div>}{!error && items.length === 0 && <Empty><EmptyHeader><EmptyTitle>{emptyTitle}</EmptyTitle>{emptyDescription && <EmptyDescription>{emptyDescription}</EmptyDescription>}</EmptyHeader></Empty>}{items.map((item) => <div key={item.id} data-selected={selectedId === item.id || undefined} className={cn("flex w-full items-start gap-3 rounded-md border p-3 transition-colors hover:bg-accent", selectedId === item.id && "border-primary bg-accent")}><div role="button" tabIndex={0} aria-pressed={selectedId === item.id} onClick={() => onSelect?.(item)} onKeyDown={(event) => { if (event.key === "Enter" || event.key === " ") { event.preventDefault(); onSelect?.(item); } }} className="min-w-0 flex-1 text-left focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none" aria-label={item.title}>{columns.includes("title") && <p className="truncate font-medium">{item.title}</p>}{columns.includes("subtitle") && item.subtitle && <p className="mt-1 truncate text-xs text-muted-foreground">{item.subtitle}</p>}</div>{columns.includes("status") && item.badge && <Badge variant={item.badge.variant}>{item.badge.text}</Badge>}{columns.includes("time") && item.time && <span className="shrink-0 text-xs text-muted-foreground">{item.time}</span>}{columns.includes("actions") && renderActions && <div className="shrink-0">{renderActions(item)}</div>}</div>)}</div>;
}
