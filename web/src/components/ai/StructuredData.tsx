import { ChevronDown, Copy } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import { ScrollArea } from "@/components/ui/scroll-area";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";

/** Bounded projections: everything beyond these limits stays readable in the preserved raw entry. */
const MAX_ROWS = 20;
const MAX_FIELDS = 30;
/** Recursion bound for nested field tables; deeper levels fall back to raw JSON. */
const MAX_DEPTH = 2;

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function descriptor(value: unknown): string {
  if (Array.isArray(value)) return `数组 · ${value.length} 项`;
  if (isRecord(value)) return `对象 · ${Object.keys(value).length} 个字段`;
  return "结构化数据";
}

function boundNote(kind: string, limit: number): string {
  return `仅显示前 ${limit} ${kind}，完整数据见原文。`;
}

/** Collapsed verbatim original with copy; long lines wrap instead of overflowing narrow screens. */
export function RawPayload({ text, label = "原始 JSON" }: { text: string; label?: string }) {
  const copy = () => { void navigator.clipboard?.writeText(text).catch(() => undefined); };
  return <Collapsible className="rounded-md border">
    <CollapsibleTrigger asChild>
      <Button variant="ghost" className="group w-full justify-between">{label}<ChevronDown className="transition-transform group-data-[state=open]:rotate-180" aria-hidden="true" /></Button>
    </CollapsibleTrigger>
    <CollapsibleContent>
      <div className="border-t px-3 py-2">
        <ScrollArea className="h-56 rounded border">
          <pre className="whitespace-pre-wrap break-words p-3 text-xs">{text}</pre>
        </ScrollArea>
        <div className="mt-2 flex justify-end"><Button size="xs" variant="outline" onClick={copy}><Copy data-icon="inline-start" />复制</Button></div>
      </div>
    </CollapsibleContent>
  </Collapsible>;
}

/**
 * Field-level display of one complete JSON value. Only real fields are shown;
 * known keys may map to readable labels while unknown keys stay verbatim, and
 * no health judgment or summary is ever invented.
 */
export function StructuredData({ value, raw, fieldLabels, depth = 0 }: { value: unknown; raw?: string; fieldLabels?: Record<string, string>; depth?: number }) {
  return <div className="flex min-w-0 flex-col gap-3">
    <StructuredValue value={value} fieldLabels={fieldLabels} depth={depth} />
    {raw !== undefined && <RawPayload text={raw} />}
  </div>;
}

function StructuredValue({ value, fieldLabels, depth }: { value: unknown; fieldLabels?: Record<string, string>; depth: number }) {
  if (Array.isArray(value)) {
    if (value.length === 0) return <p className="text-sm text-muted-foreground">空数组</p>;
    return value.every(isRecord)
      ? <RecordsTable items={value} />
      : <ValueList items={value} />;
  }
  if (isRecord(value)) {
    if (Object.keys(value).length === 0) return <p className="text-sm text-muted-foreground">空对象</p>;
    return <FieldsTable value={value} fieldLabels={fieldLabels} depth={depth} />;
  }
  return <CellValue value={value} />;
}

/** Object view: 字段/值 rows; nested structures expand in place instead of dumping a JSON blob. */
function FieldsTable({ value, fieldLabels, depth }: { value: Record<string, unknown>; fieldLabels?: Record<string, string>; depth: number }) {
  const entries = Object.entries(value);
  const bounded = entries.slice(0, MAX_FIELDS);
  return <div className="flex min-w-0 flex-col gap-2">
    <Table>
      <TableHeader><TableRow><TableHead>字段</TableHead><TableHead>值</TableHead></TableRow></TableHeader>
      <TableBody>
        {bounded.map(([key, field]) => (
          <TableRow key={key}>
            <TableCell className="font-medium whitespace-normal break-words">{fieldLabels?.[key] ?? key}</TableCell>
            <TableCell className="whitespace-normal"><FieldValue value={field} depth={depth} /></TableCell>
          </TableRow>
        ))}
      </TableBody>
    </Table>
    {entries.length > MAX_FIELDS && <p className="text-xs text-muted-foreground">{boundNote("个字段", MAX_FIELDS)}</p>}
  </div>;
}

function FieldValue({ value, depth }: { value: unknown; depth: number }) {
  if (value === null) return <span className="font-mono text-xs">null</span>;
  if (Array.isArray(value) || isRecord(value)) {
    if (Object.keys(value).length === 0) return <span className="text-sm text-muted-foreground">{Array.isArray(value) ? "空数组" : "空对象"}</span>;
    if (depth >= MAX_DEPTH) return <RawPayload text={JSON.stringify(value, null, 2)} label={descriptor(value)} />;
    return <Collapsible>
      <CollapsibleTrigger asChild>
        <Button variant="outline" size="xs" className="group">{descriptor(value)}<ChevronDown className="transition-transform group-data-[state=open]:rotate-180" aria-hidden="true" /></Button>
      </CollapsibleTrigger>
      <CollapsibleContent>
        <div className="pt-2"><StructuredValue value={value} depth={depth + 1} /></div>
      </CollapsibleContent>
    </Collapsible>;
  }
  return <CellValue value={value} />;
}

/** Array-of-records view: one honest table from the records themselves, bounded rows and columns. */
function RecordsTable({ items }: { items: Record<string, unknown>[] }) {
  const bounded = items.slice(0, MAX_ROWS);
  const columns: string[] = [];
  for (const item of bounded) for (const key of Object.keys(item)) if (!columns.includes(key)) columns.push(key);
  const shown = columns.slice(0, MAX_FIELDS);
  return <div className="flex min-w-0 flex-col gap-2">
    <Table>
      <TableHeader><TableRow>{shown.map((column) => <TableHead key={column} className="whitespace-normal break-words">{column}</TableHead>)}</TableRow></TableHeader>
      <TableBody>
        {bounded.map((item, index) => (
          <TableRow key={index}>
            {shown.map((column) => <TableCell key={column} className="whitespace-normal break-words"><CellValue value={item[column]} /></TableCell>)}
          </TableRow>
        ))}
      </TableBody>
    </Table>
    {items.length > MAX_ROWS && <p className="text-xs text-muted-foreground">{boundNote("项", MAX_ROWS)}</p>}
    {columns.length > MAX_FIELDS && <p className="text-xs text-muted-foreground">{boundNote("个字段", MAX_FIELDS)}</p>}
  </div>;
}

function ValueList({ items }: { items: unknown[] }) {
  const bounded = items.slice(0, MAX_ROWS);
  return <div className="flex min-w-0 flex-col gap-2">
    <ul className="flex flex-col gap-1">{bounded.map((item, index) => <li key={index} className="break-words"><CellValue value={item} /></li>)}</ul>
    {items.length > MAX_ROWS && <p className="text-xs text-muted-foreground">{boundNote("项", MAX_ROWS)}</p>}
  </div>;
}

function CellValue({ value }: { value: unknown }) {
  if (value === null) return <span className="font-mono text-xs">null</span>;
  if (typeof value === "string") return <span className="whitespace-pre-wrap break-words">{value}</span>;
  if (Array.isArray(value) || isRecord(value)) return <code className="break-all font-mono text-xs">{JSON.stringify(value)}</code>;
  return <span className="font-mono text-xs">{String(value)}</span>;
}
