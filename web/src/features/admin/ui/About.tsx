import { useCallback, useEffect, useState } from 'react'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Separator } from '@/components/ui/separator'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { fetchAbout, type AboutStatus } from '../about/api'
import { Maintenance } from './Maintenance'
import { Runtimes } from './Runtimes'

const unknown = (value?: string) => value?.trim() || '未知'
const time = (value?: string) => {
  if (!value) return '未知'
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? '未知' : date.toLocaleString()
}

/** Admin-only About gathers bounded, non-secret platform facts and the actions that maintain them. */
export function About({ suspended, authenticationSuspended = false }: { suspended: boolean; authenticationSuspended?: boolean }) {
  const [status, setStatus] = useState<AboutStatus | null>(null)
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(false)
  const [refreshRevision, setRefreshRevision] = useState(0)

  const load = useCallback(async () => {
    if (suspended) return
    try {
      setLoading(true)
      setError('')
      setStatus(await fetchAbout())
      setRefreshRevision(current => current + 1)
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : '暂时无法读取平台关于信息。')
    } finally {
      setLoading(false)
    }
  }, [suspended])

  useEffect(() => { void load() }, [load])

  return <section className="mx-auto flex w-full max-w-6xl flex-col gap-6">
    <div className="flex flex-wrap items-start justify-between gap-3">
      <div><h2 className="text-xl font-semibold">关于平台</h2><p className="mt-1 text-sm text-muted-foreground">组件版本、连接事实和必要的平台维护。未知信息不会被推断为健康。</p></div>
      <Button variant="outline" onClick={() => void load()} disabled={suspended || loading}>{loading ? '正在刷新…' : '刷新'}</Button>
    </div>
    {error && <Alert variant="destructive"><AlertDescription>{error} <Button variant="link" className="h-auto p-0 align-baseline" onClick={() => void load()} disabled={suspended || loading}>重试</Button></AlertDescription></Alert>}
    {!status && !error && <p role="status" className="text-sm text-muted-foreground">正在读取平台事实…</p>}
    {status && <div className="flex flex-col gap-6">
      <section className="flex flex-col gap-3"><h3 className="text-sm font-medium">Quoin</h3><p className="wrap-anywhere font-mono text-sm">版本：{unknown(status.releaseVersion)}</p></section>
      <Separator />
      <section className="flex flex-col gap-3"><h3 className="text-sm font-medium">内部组件</h3><Table><TableHeader><TableRow><TableHead>组件</TableHead><TableHead>注册</TableHead><TableHead>连接</TableHead><TableHead>版本</TableHead><TableHead>最近事实</TableHead></TableRow></TableHeader><TableBody>{status.components.map((component) => <TableRow key={component.slot}><TableCell>{component.slot}</TableCell><TableCell><Badge variant={component.state === 'registered' ? 'secondary' : 'outline'}>{component.state}</Badge></TableCell><TableCell><Badge variant={component.connected ? 'secondary' : 'outline'}>{component.connected ? '已连接' : '未连接'}</Badge></TableCell><TableCell className="max-w-72 whitespace-normal wrap-anywhere font-mono text-xs">{unknown(component.releaseVersion)}</TableCell><TableCell className="whitespace-normal">{component.connected ? time(component.lastSeenAt) : '未知'}</TableCell></TableRow>)}</TableBody></Table></section>
      <Separator />
      <Runtimes suspended={suspended} refreshRevision={refreshRevision} onChanged={load} />
      <Separator />
      <Maintenance authenticationSuspended={authenticationSuspended} refreshRevision={refreshRevision} onChanged={load} />
    </div>}
  </section>
}
