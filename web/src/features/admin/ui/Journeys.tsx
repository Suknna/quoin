import { useEffect, useState } from "react";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { FieldDescription } from "@/components/ui/field";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { getJourneyCatalog, type JourneyCatalogView } from "@/features/admin/business-systems/api";

type CatalogEntry = { id?: string; version?: string | number; name?: string; description?: string; startUrl?: string; [key: string]: unknown };
/** The catalog schema stores entries in a JSON document; render its actual entries without inventing an alternate API. */
function entriesOf(catalog: JourneyCatalogView): CatalogEntry[] {
 const raw = catalog.catalogJson;
 const entries = Array.isArray(raw.entries) ? raw.entries : Array.isArray(raw.journeys) ? raw.journeys : [];
 return entries.filter((entry): entry is CatalogEntry => Boolean(entry) && typeof entry === "object");
}
export function Journeys() {
 const [catalog, setCatalog] = useState<JourneyCatalogView>(); const [error, setError] = useState("");
 useEffect(() => { void getJourneyCatalog().then(setCatalog).catch(reason => setError(reason instanceof Error ? reason.message : "暂时无法读取 Journey 目录。")); }, []);
 const entries = catalog ? entriesOf(catalog) : [];
 return <section className="space-y-4"><h2 className="text-xl font-semibold">Journey</h2>{error && <Alert variant="destructive"><AlertDescription>{error}</AlertDescription></Alert>}{catalog && <><p>目录版本：{catalog.version}</p><p className="break-all text-sm text-muted-foreground">Digest：{catalog.digest}</p><Table><TableHeader><TableRow><TableHead>Journey</TableHead><TableHead>版本</TableHead><TableHead>起始地址</TableHead><TableHead>说明</TableHead></TableRow></TableHeader><TableBody>{entries.map((entry, index) => <TableRow key={`${String(entry.id ?? entry.name ?? index)}-${String(entry.version ?? "")}`}><TableCell>{String(entry.id ?? entry.name ?? "未命名 Journey")}</TableCell><TableCell>{entry.version === undefined ? "—" : String(entry.version)}</TableCell><TableCell>{entry.startUrl === undefined ? "—" : String(entry.startUrl)}</TableCell><TableCell>{entry.description === undefined ? "—" : String(entry.description)}</TableCell></TableRow>)}</TableBody></Table>{entries.length === 0 && <p role="status">目录当前没有可展示的 Journey 条目。</p>}</>}<FieldDescription>Journey 目录为只读投影。</FieldDescription></section>;
}
