import { useEffect, useState } from 'react'
import { Accordion, AccordionContent, AccordionItem, AccordionTrigger } from '@/components/ui/accordion'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { ScrollArea } from '@/components/ui/scroll-area'
import { Separator } from '@/components/ui/separator'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import type { UserSummary } from '@/api/generated/types'
import { artifactDownloadURL, evidenceParamsText, fetchArtifactMetadata, fetchEvidence, type ArtifactSummary, type EvidenceDetail } from '@/features/analysis/tool-details/api'

const message = (reason: unknown) => reason instanceof Error ? reason.message : '证据详情加载失败。'

/** A permission-aware reading surface; inline content stays readable while artifact bytes are requested only through their authorized endpoint. */
export function EvidenceReader({ id, onClose, user }: { id: string; onClose: () => void; user: UserSummary }) {
  // Keying gives each evidence identity a fresh reading session, avoiding stale content while a prior request is aborted.
  return <EvidenceReadingSession key={id} id={id} onClose={onClose} user={user} />
}

function EvidenceReadingSession({ id, onClose, user }: { id: string; onClose: () => void; user: UserSummary }) {
  const [evidence, setEvidence] = useState<EvidenceDetail | null>(null)
  const [artifact, setArtifact] = useState<ArtifactSummary | null>(null)
  const [content, setContent] = useState('')
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(true)
  useEffect(() => {
    const controller = new AbortController()
    fetchEvidence(id).then(async (detail) => {
      if (controller.signal.aborted) return
      setEvidence(detail)
      if (detail.body.kind === 'artifact') {
        const metadata = await fetchArtifactMetadata(detail.body.artifact.id)
        if (controller.signal.aborted) return
        setArtifact(metadata)
        if (!metadata.bodyExpired) {
          const response = await fetch(artifactDownloadURL(metadata.id), { credentials: 'include', signal: controller.signal })
          if (!response.ok) throw new Error(response.status === 403 ? '你没有读取此产物内容的权限。' : '产物内容暂时不可读取。')
          setContent(await response.text())
        }
      }
    }).catch((reason: unknown) => { if (!controller.signal.aborted) setError(message(reason)) }).finally(() => { if (!controller.signal.aborted) setLoading(false) })
    return () => controller.abort()
  }, [id])
  if (loading) return <section className="p-6"><p className="text-sm text-muted-foreground">正在读取证据…</p></section>
  if (error) return <section className="space-y-4 p-6"><Alert variant="destructive"><AlertDescription>{error}</AlertDescription></Alert><Button variant="outline" onClick={onClose}>关闭</Button></section>
  if (!evidence) return null
  const warnings = Array.isArray(evidence.warnings) ? evidence.warnings : evidence.warnings ? [evidence.warnings] : []
  const errors = Array.isArray(evidence.errors) ? evidence.errors : evidence.errors ? [evidence.errors] : []
  return <section className="flex min-h-0 flex-1 flex-col p-6"><header className="flex items-start justify-between gap-3"><div><h1 className="text-xl font-semibold">证据阅读</h1><p className="mt-1 text-sm text-muted-foreground">{evidence.targetType} · {new Date(evidence.observedAt).toLocaleString()}</p></div><Button variant="outline" onClick={onClose}>关闭</Button></header><div className="mt-4 flex flex-wrap gap-2"><Badge variant={evidence.integrity === 'complete' ? 'secondary' : 'destructive'}>{evidence.integrity === 'complete' ? '完整' : '不完整'}</Badge>{evidence.connections.map((item) => <Badge variant="outline" key={item.key}>{item.key}</Badge>)}</div>{(warnings.length > 0 || errors.length > 0) && <Alert className="mt-4" variant={errors.length ? 'destructive' : 'default'}><AlertDescription>{warnings.map(String).join('；')} {errors.map(String).join('；')}</AlertDescription></Alert>}<Accordion className="mt-4" type="multiple" defaultValue={['parameters', 'content']}><AccordionItem value="parameters"><AccordionTrigger>来源与查询参数</AccordionTrigger><AccordionContent><p className="text-sm">来源：{evidence.producer.kind}</p><pre className="mt-2 overflow-x-auto rounded bg-muted p-3 text-xs">{evidenceParamsText(evidence.params)}</pre></AccordionContent></AccordionItem><AccordionItem value="content"><AccordionTrigger>证据内容</AccordionTrigger><AccordionContent>{evidence.body.kind === 'inline_json' ? <InlineEvidence value={evidence.body.value} /> : <ArtifactContent artifact={artifact} content={content} user={user} />}</AccordionContent></AccordionItem></Accordion><Separator className="my-4"/><p className="text-xs text-muted-foreground">证据 ID：{evidence.id} · 创建于 {new Date(evidence.createdAt).toLocaleString()}</p></section>
}
function InlineEvidence({ value }: { value: unknown }) {
  const fields: Array<[string, unknown]> = value && typeof value === 'object' && !Array.isArray(value) ? Object.entries(value as Record<string, unknown>) : [['value', value]]
  return <div className="space-y-3"><Table><TableHeader><TableRow><TableHead>字段</TableHead><TableHead>值</TableHead></TableRow></TableHeader><TableBody>{fields.map(([key, field]) => <TableRow key={key}><TableCell className="font-medium">{key}</TableCell><TableCell>{typeof field === 'string' || typeof field === 'number' || typeof field === 'boolean' || field === null ? String(field) : Array.isArray(field) ? `${field.length} 项` : '结构化对象'}</TableCell></TableRow>)}</TableBody></Table><Accordion type="single" collapsible><AccordionItem value="raw"><AccordionTrigger>原始证据正文</AccordionTrigger><AccordionContent><ScrollArea className="h-64 rounded border"><pre className="p-3 text-xs">{JSON.stringify(value, null, 2)}</pre></ScrollArea></AccordionContent></AccordionItem></Accordion></div>
}
function ArtifactContent({ artifact, content, user }: { artifact: ArtifactSummary | null; content: string; user: UserSummary }) {
  if (!artifact) return <p className="text-sm text-muted-foreground">正在读取产物元数据…</p>
  if (artifact.bodyExpired) return <Alert><AlertDescription>该产物正文已按保留策略清理，仍可查看元数据与完整性信息。</AlertDescription></Alert>
  return <div className="space-y-3"><p className="text-sm text-muted-foreground">{artifact.kind} · {artifact.sizeBytes} bytes · SHA-256 {artifact.sha256}</p>{artifact.sensitive && user.role !== 'admin' ? <Alert variant="destructive"><AlertDescription>此敏感产物由服务端权限策略控制；若无法显示内容，请联系管理员授权。</AlertDescription></Alert> : <ScrollArea className="h-96 rounded border"><pre className="whitespace-pre-wrap break-words p-3 text-xs">{content || '产物为空。'}</pre></ScrollArea>}</div>
}
