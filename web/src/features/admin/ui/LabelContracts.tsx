import { useEffect, useState } from "react";
import { Accordion, AccordionContent, AccordionItem, AccordionTrigger } from "@/components/ui/accordion";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent, AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle, AlertDialogTrigger } from "@/components/ui/alert-dialog";
import { Button } from "@/components/ui/button";
import { Field, FieldDescription, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { notifyUnauthorized, newClientCommandId } from "@/api/workbench";
import {
  activateLabelContract, ConfigApiError, fetchLabelContractReadiness, type LabelContractReadiness,
  type LabelContractSummary, listLabelContracts, type ReadinessSystem,
} from "@/features/admin/business-systems/api";

type ContractDetail = LabelContractSummary & { yamlBody: string };

/** This feature owns only contract lifecycle; business-system editing remains in Systems. */
async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const response = await fetch(path, { credentials: "include", ...init });
  if (!response.ok) {
    if (response.status === 401) notifyUnauthorized();
    let body: { message?: string; detail?: string; code?: string; fieldErrors?: { path: string; reason: string }[] } = {};
    try { body = await response.json() as typeof body; } catch { /* ordinary fallback below */ }
    throw new ConfigApiError(body.message ?? body.detail ?? "暂时无法完成操作，请重试。", response.status, body.code, body.fieldErrors);
  }
  return response.json() as Promise<T>;
}
const message = (reason: unknown) => reason instanceof Error ? reason.message : "暂时无法完成操作，请重试。";
const isConflict = (reason: unknown) => reason instanceof ConfigApiError && reason.status === 409;

function contractVersionID(value?: string | null): string | null { return value ?? null; }
function readinessItem(system: ReadinessSystem, selected: Record<string, string>) {
  const candidate = system.activationCandidates.find(item => item.configVersionId === selected[system.businessSystemKey]) ?? system.activationCandidates[0];
  return candidate && {
    businessSystemKey: system.businessSystemKey,
    configVersionId: candidate.configVersionId,
    verificationRunId: candidate.passedVerificationRunId,
    expectedCurrentConfigVersionId: contractVersionID(system.currentConfigVersionId),
    expectedBusinessSystemRowVersion: system.businessSystemRowVersion,
  };
}

export function LabelContracts({ suspended }: { suspended: boolean }) {
  const [contracts, setContracts] = useState<LabelContractSummary[]>([]);
  const [selected, setSelected] = useState<ContractDetail>();
  const [readiness, setReadiness] = useState<LabelContractReadiness>();
  const [choices, setChoices] = useState<Record<string, string>>({});
  const [file, setFile] = useState<File>();
  const [error, setError] = useState("");
  const [uploading, setUploading] = useState(false);

  const load = async () => {
    try { setContracts(await listLabelContracts()); } catch (reason) { setError(message(reason)); }
  };
  useEffect(() => { void load(); }, []);
  useEffect(() => { if (suspended) setFile(undefined); }, [suspended]);

  async function open(version: number) {
    try {
      const [detail, nextReadiness] = await Promise.all([
        request<ContractDetail>(`/api/v1/label-contracts/${version}`),
        fetchLabelContractReadiness(version),
      ]);
      setSelected(detail); setReadiness(nextReadiness); setChoices(Object.fromEntries(nextReadiness.systems.map(system => [system.businessSystemKey, system.activationCandidates[0]?.configVersionId ?? ""])));
    } catch (reason) { setError(message(reason)); }
  }
  async function upload() {
    if (!file) return;
    setUploading(true); setError("");
    try {
      const form = new FormData(); form.append("file", file, file.name); form.append("clientCommandId", newClientCommandId());
      const created = await request<ContractDetail>("/api/v1/label-contracts", { method: "POST", body: form });
      setFile(undefined); await load(); await open(created.version);
    } catch (reason) {
      if (reason instanceof ConfigApiError && reason.fieldErrors.length) setError(reason.fieldErrors.map(field => `${field.path}: ${field.reason}`).join("；"));
      else setError(message(reason));
    } finally { setUploading(false); }
  }
  async function activate() {
    if (!selected || !readiness) return;
    const compatibleVersions = readiness.systems.map(system => readinessItem(system, choices)).filter(Boolean);
    if (compatibleVersions.length !== readiness.systems.length) { setError("每个启用的业务系统都必须选择已通过验证的兼容配置。"); return; }
    try {
      await activateLabelContract(selected.version, {
        expectedStateRowVersion: readiness.stateRowVersion,
        expectedCurrentContractVersionId: readiness.currentContractVersionId ?? null,
        expectedTargetRowVersion: readiness.targetRowVersion,
        compatibleVersions,
      });
      await load(); await open(selected.version);
    } catch (reason) { setError(isConflict(reason) ? "激活围栏已变化。请重新读取就绪性并确认当前选择。" : message(reason)); }
  }
  const canActivate = !suspended && !!selected && selected.state === "draft" && !!readiness && readiness.systems.every(system => !!readinessItem(system, choices));
  return <section className="space-y-4">
    <h2 className="text-xl font-semibold">标签契约</h2>
    {error && <Alert variant="destructive"><AlertDescription>{error}</AlertDescription></Alert>}
    <Field><FieldLabel htmlFor="label-contract-file">Label Contract YAML</FieldLabel><Input id="label-contract-file" type="file" accept=".yaml,.yml,application/yaml,text/yaml" disabled={suspended || uploading} onChange={event => setFile(event.target.files?.[0])}/><FieldDescription>上传会创建草稿；服务端会校验 YAML 并返回逐字段错误。</FieldDescription></Field>
    <Button disabled={suspended || uploading || !file} onClick={() => void upload()}>{uploading ? "正在上传…" : "上传契约"}</Button>
    <Table><TableHeader><TableRow><TableHead>版本</TableHead><TableHead>状态</TableHead><TableHead>创建时间</TableHead><TableHead /></TableRow></TableHeader><TableBody>{contracts.map(contract => <TableRow key={contract.id}><TableCell>v{contract.version}</TableCell><TableCell>{contract.state}</TableCell><TableCell>{contract.createdAt}</TableCell><TableCell><Button size="sm" variant="outline" onClick={() => void open(contract.version)}>查看与就绪性</Button></TableCell></TableRow>)}</TableBody></Table>
    {selected && <ContractReadiness contract={selected} readiness={readiness} choices={choices} suspended={suspended} onChoice={(key, value) => setChoices(current => ({ ...current, [key]: value }))} canActivate={canActivate} onActivate={activate}/>}
  </section>;
}

function ContractReadiness({ contract, readiness, choices, suspended, onChoice, canActivate, onActivate }: { contract: ContractDetail; readiness?: LabelContractReadiness; choices: Record<string, string>; suspended: boolean; onChoice: (key: string, value: string) => void; canActivate: boolean; onActivate: () => void }) {
  return <Accordion type="single" collapsible defaultValue="readiness"><AccordionItem value="readiness"><AccordionTrigger>v{contract.version} · {contract.state} · 激活就绪性</AccordionTrigger><AccordionContent className="space-y-4">
    <FieldDescription>原子激活将同时切换契约和以下每个业务系统配置，并使用读取时的版本围栏。</FieldDescription>
    {!readiness ? <p role="status">正在读取就绪性…</p> : readiness.systems.map(system => <div className="rounded border p-3 space-y-2" key={system.businessSystemKey}><strong>{system.businessSystemKey}</strong>{system.blockers.length > 0 && <Alert variant="destructive"><AlertDescription>{system.blockers.join("；")}</AlertDescription></Alert>}<Field><FieldLabel>兼容系统配置</FieldLabel><Select value={choices[system.businessSystemKey]} disabled={suspended || system.activationCandidates.length === 0} onValueChange={value => onChoice(system.businessSystemKey, value)}><SelectTrigger><SelectValue placeholder="选择已验证配置" /></SelectTrigger><SelectContent>{system.activationCandidates.map(candidate => <SelectItem value={candidate.configVersionId} key={candidate.configVersionId}>配置 {candidate.configVersionId} · 验证 {candidate.passedVerificationRunId}</SelectItem>)}</SelectContent></Select><FieldDescription>所选配置会携带已通过的 verification ID。</FieldDescription></Field></div>)}
    <AlertDialog><AlertDialogTrigger asChild><Button disabled={!canActivate}>原子激活</Button></AlertDialogTrigger><AlertDialogContent><AlertDialogHeader><AlertDialogTitle>确认激活 v{contract.version}？</AlertDialogTitle><AlertDialogDescription>这会用当前就绪性围栏原子切换标签契约和所有选择的业务系统配置。若任何版本已变化，操作会整体拒绝，不会部分生效。</AlertDialogDescription></AlertDialogHeader><AlertDialogFooter><AlertDialogCancel>取消</AlertDialogCancel><AlertDialogAction onClick={onActivate}>确认激活</AlertDialogAction></AlertDialogFooter></AlertDialogContent></AlertDialog>
  </AccordionContent></AccordionItem></Accordion>;
}
