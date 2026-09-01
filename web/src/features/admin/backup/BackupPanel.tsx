import { useCallback, useEffect, useRef, useState } from 'react'
import { formatBackupBytes, formatBackupTimestamp } from './backupFormat'

interface Backup {
  id: string
  status: string
  stage: string
  triggerKind: string
  executionMode: string
  createdAt: string
  completedAt: string | null
  dbSha256: string | null
  manifestSha256: string | null
  artifactCount: number | null
  sizeBytes: number
  errorCode: string | null
  errorDetail: string | null
}

interface Settings {
  enabled: boolean
  scheduleCron?: string
  timezone: string
  backupTarget: string
  retentionCount: number
  rowVersion: number
}

interface Retention { generatedRetentionDays: number; rowVersion: number }
interface BackupPage { items: Backup[]; nextCursor?: string }
type SettingsDraft = Pick<Settings, 'enabled' | 'scheduleCron' | 'timezone' | 'retentionCount'>
type Conflict = 'settings' | 'retention' | null

class RequestError extends Error {
  constructor(message: string, readonly status: number) { super(message) }
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const response = await fetch(path, {
    credentials: 'include',
    ...init,
    headers: { 'Content-Type': 'application/json', ...(init?.headers ?? {}) },
  })
  if (!response.ok) {
    const body = await response.json().catch(() => null) as { message?: string } | null
    throw new RequestError(body?.message ?? '请求没有完成。', response.status)
  }
  return response.json() as Promise<T>
}

function commandID() {
  return Array.from(crypto.getRandomValues(new Uint8Array(18)), value => value.toString(16).padStart(2, '0')).join('')
}

function isTransientFailure(reason: unknown) {
  return !(reason instanceof RequestError) || reason.status === 429 || reason.status >= 500
}

async function retryWrite<T>(operation: () => Promise<T>): Promise<T> {
  let lastError: unknown
  for (let attempt = 0; attempt < 3; attempt++) {
    try {
      return await operation()
    } catch (reason) {
      lastError = reason
      if (!isTransientFailure(reason) || attempt === 2) throw reason
      await new Promise<void>(resolve => window.setTimeout(resolve, 1000 * (attempt + 1)))
    }
  }
  throw lastError
}

function settingsDraft(value: Settings): SettingsDraft {
  return { enabled: value.enabled, scheduleCron: value.scheduleCron ?? '', timezone: value.timezone, retentionCount: value.retentionCount }
}

function BackupRuns({ items, more, onMore }: { items: Backup[]; more: boolean; onMore: () => void }) {
  if (items.length === 0) return <p>尚无备份记录。</p>
  return <>
    <ul className="admin-user-list">
      {items.map(item => <li key={item.id}>
        <strong>#{item.id} · {item.status} · {item.stage}</strong>
        <span className="object-row-meta">{item.executionMode}/{item.triggerKind} · {formatBackupTimestamp(item.createdAt)}</span>
        <span className="object-row-meta">大小 {formatBackupBytes(item.sizeBytes)} · 产物 {item.artifactCount ?? '—'} · 数据库校验 {item.dbSha256 ?? '—'} · manifest 校验 {item.manifestSha256 ?? '—'}</span>
        {item.errorCode && <span className="error-summary">{item.errorCode}: {item.errorDetail ?? '无详情'}</span>}
        {item.status === 'succeeded' && <a href={`/api/v1/backups/${item.id}/download`}>下载归档</a>}
      </li>)}
    </ul>
    {more && <button onClick={onMore}>加载更多备份记录</button>}
  </>
}

export function BackupPanel() {
  const [backups, setBackups] = useState<Backup[] | null>(null)
  const [settings, setSettings] = useState<Settings | null>(null)
  const [retention, setRetention] = useState<Retention | null>(null)
  const [settingsDraftValue, setSettingsDraftValue] = useState<SettingsDraft | null>(null)
  const [retentionDraft, setRetentionDraft] = useState<number | null>(null)
  const [incomingSettings, setIncomingSettings] = useState<Settings | null>(null)
  const [incomingRetention, setIncomingRetention] = useState<Retention | null>(null)
  const [conflict, setConflict] = useState<Conflict>(null)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')
  const [busy, setBusy] = useState(false)
  const [nextCursor, setNextCursor] = useState<string | null>(null)
  const loadedContinuation = useRef(false)
  const settingsDirtyRef = useRef(false)
  const retentionDirtyRef = useRef(false)
  const atTopRef = useRef(true)
  const pendingHeadRef = useRef<Backup[]>([])
  const [pendingCount, setPendingCount] = useState(0)
  const commandIDs = useRef<Record<'trigger' | 'settings' | 'retention', string | null>>({ trigger: null, settings: null, retention: null })

  const reload = useCallback(async () => {
    try {
      const [page, nextSettings, nextRetention] = await Promise.all([
        request<BackupPage>('/api/v1/backups?limit=30'),
        request<Settings>('/api/v1/backups/settings'),
        request<Retention>('/api/v1/artifacts/retention-settings'),
      ])
      setBackups(current => {
        const firstPage = new Set(page.items.map(item => item.id))
        if (current && !atTopRef.current) {
          const refreshed = new Map(page.items.map(item => [item.id, item]))
          const unseen = page.items.filter(item => !current.some(currentItem => currentItem.id === item.id))
          if (unseen.length > 0) {
            pendingHeadRef.current = page.items
            setPendingCount(unseen.length)
          }
          // Do not move an away-from-top reader, but always reconcile the
          // known rows in place so an active run visibly becomes terminal.
          return current.map(item => refreshed.get(item.id) ?? item)
        }
        return [...page.items, ...(current ?? []).filter(item => !firstPage.has(item.id))]
      })
      if (!loadedContinuation.current) setNextCursor(page.nextCursor ?? null)
      if (settingsDirtyRef.current) setIncomingSettings(nextSettings)
      else { setSettings(nextSettings); setSettingsDraftValue(settingsDraft(nextSettings)) }
      if (retentionDirtyRef.current) setIncomingRetention(nextRetention)
      else { setRetention(nextRetention); setRetentionDraft(nextRetention.generatedRetentionDays) }
      setError('')
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : '无法读取备份设置。')
    }
  }, [])

  useEffect(() => {
    const updateScrollPosition = () => { atTopRef.current = window.scrollY <= 0 }
    updateScrollPosition()
    window.addEventListener('scroll', updateScrollPosition, { passive: true })
    void reload()
    const timer = window.setInterval(() => { if (document.visibilityState === 'visible') void reload() }, 5000)
    return () => { window.clearInterval(timer); window.removeEventListener('scroll', updateScrollPosition) }
  }, [reload])

  async function loadMore() {
    if (!nextCursor || busy) return
    setBusy(true)
    try {
      const page = await request<BackupPage>(`/api/v1/backups?limit=30&cursor=${encodeURIComponent(nextCursor)}`)
      setBackups(current => {
        const known = new Set((current ?? []).map(item => item.id))
        return [...(current ?? []), ...page.items.filter(item => !known.has(item.id))]
      })
      loadedContinuation.current = true
      setNextCursor(page.nextCursor ?? null)
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : '无法读取更多备份记录。')
    } finally {
      setBusy(false)
    }
  }

  async function trigger() {
    setBusy(true)
    setError('')
    const id = commandIDs.current.trigger ?? commandID()
    commandIDs.current.trigger = id
    try {
      await retryWrite(() => request<Backup>('/api/v1/backups', { method: 'POST', body: JSON.stringify({ clientCommandId: id }) }))
      commandIDs.current.trigger = null
      setNotice('备份任务已创建，完成前可离开此页；返回后会继续显示真实阶段。')
      await reload()
    } catch (reason) {
      if (!isTransientFailure(reason)) commandIDs.current.trigger = null
      setError(reason instanceof Error ? reason.message : '无法创建备份。再次点击会安全重试同一请求。')
    } finally {
      setBusy(false)
    }
  }

  function updateSettingsDraft(update: Partial<SettingsDraft>) {
    commandIDs.current.settings = null
    settingsDirtyRef.current = true
    setSettingsDraftValue(current => current ? { ...current, ...update } : current)
  }

  async function saveSettings(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (!settings || !settingsDraftValue) return
    const id = commandIDs.current.settings ?? commandID()
    commandIDs.current.settings = id
    setError('')
    try {
      const next = await retryWrite(() => request<Settings>('/api/v1/backups/settings', { method: 'PUT', body: JSON.stringify({
        clientCommandId: id, expectedRowVersion: settings.rowVersion, ...settingsDraftValue,
      }) }))
      commandIDs.current.settings = null
      setSettings(next)
      setSettingsDraftValue(settingsDraft(next))
      settingsDirtyRef.current = false
      setIncomingSettings(null)
      setConflict(null)
      setNotice('备份设置已保存。')
      await reload()
    } catch (reason) {
      if (reason instanceof RequestError && (reason.status === 409 || reason.status === 422)) setConflict('settings')
      if (!isTransientFailure(reason)) commandIDs.current.settings = null
      setError(reason instanceof Error ? reason.message : '无法保存设置。再次提交会安全重试同一请求。')
    }
  }

  function updateRetentionDraft(value: number) {
    commandIDs.current.retention = null
    retentionDirtyRef.current = true
    setRetentionDraft(value)
  }

  async function saveRetention(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (!retention || retentionDraft === null) return
    const id = commandIDs.current.retention ?? commandID()
    commandIDs.current.retention = id
    setError('')
    try {
      const next = await retryWrite(() => request<Retention>('/api/v1/artifacts/retention-settings', { method: 'PUT', body: JSON.stringify({
        clientCommandId: id, expectedRowVersion: retention.rowVersion, generatedRetentionDays: retentionDraft,
      }) }))
      commandIDs.current.retention = null
      setRetention(next)
      setRetentionDraft(next.generatedRetentionDays)
      retentionDirtyRef.current = false
      setIncomingRetention(null)
      setConflict(null)
      setNotice('生成型产物保留设置已保存。')
      await reload()
    } catch (reason) {
      if (reason instanceof RequestError && (reason.status === 409 || reason.status === 422)) setConflict('retention')
      if (!isTransientFailure(reason)) commandIDs.current.retention = null
      setError(reason instanceof Error ? reason.message : '无法保存产物保留设置。再次提交会安全重试同一请求。')
    }
  }

  async function acceptIncomingSettings() {
    try {
      const latest = incomingSettings ?? await request<Settings>('/api/v1/backups/settings')
      setSettings(latest)
      setSettingsDraftValue(settingsDraft(latest))
      settingsDirtyRef.current = false
      setIncomingSettings(null)
      commandIDs.current.settings = null
      setConflict(null)
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : '无法重新读取远端备份设置。')
    }
  }

  async function acceptIncomingRetention() {
    try {
      const latest = incomingRetention ?? await request<Retention>('/api/v1/artifacts/retention-settings')
      setRetention(latest)
      setRetentionDraft(latest.generatedRetentionDays)
      retentionDirtyRef.current = false
      setIncomingRetention(null)
      commandIDs.current.retention = null
      setConflict(null)
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : '无法重新读取远端产物保留设置。')
    }
  }

  function mergePendingRuns() {
    const firstPage = new Set(pendingHeadRef.current.map(item => item.id))
    setBackups(current => [...pendingHeadRef.current, ...(current ?? []).filter(item => !firstPage.has(item.id))])
    pendingHeadRef.current = []
    setPendingCount(0)
    window.scrollTo({ top: 0 })
    atTopRef.current = true
  }

  const latestSuccess = backups?.find(item => item.status === 'succeeded')
  return <div className="detail-content">
    <header className="detail-header"><div><p className="eyebrow">管理</p><h1>备份与保留</h1></div><button disabled={busy} onClick={() => void trigger()}>{busy ? '正在创建…' : '立即备份'}</button></header>
    <p className="detail-muted">备份目标：{settings?.backupTarget ?? '正在读取…'}。最近成功：{latestSuccess ? formatBackupTimestamp(latestSuccess.completedAt) : '尚无'}。恢复必须在 Quoin 停机时使用部署命令完成。</p>
    {error && <div className="error-summary" role="alert">{error}</div>}
    {notice && <p className="admin-notice" role="status">{notice}</p>}
    {conflict === 'settings' && <p className="error-summary" role="alert">远端备份设置已更新；当前草稿未被覆盖。<button onClick={() => void acceptIncomingSettings()}>重新加载远端设置</button></p>}
    {conflict === 'retention' && <p className="error-summary" role="alert">远端产物保留设置已更新；当前草稿未被覆盖。<button onClick={() => void acceptIncomingRetention()}>重新加载远端设置</button></p>}
    {settings && settingsDraftValue && <form className="detail-section" onSubmit={saveSettings}>
      <h2>自动备份</h2><label><input name="enabled" type="checkbox" checked={settingsDraftValue.enabled} onChange={event => updateSettingsDraft({ enabled: event.target.checked })} /> 启用计划</label>
      <label>Cron <input name="scheduleCron" value={settingsDraftValue.scheduleCron ?? ''} onChange={event => updateSettingsDraft({ scheduleCron: event.target.value })} placeholder="留空仅手动" /></label>
      <label>时区 <input name="timezone" value={settingsDraftValue.timezone} onChange={event => updateSettingsDraft({ timezone: event.target.value })} required /></label>
      <label>保留份数 <input name="retentionCount" type="number" min="1" value={settingsDraftValue.retentionCount} onChange={event => updateSettingsDraft({ retentionCount: Number(event.target.value) })} required /></label><button>保存备份设置</button>
    </form>}
    {retention && retentionDraft !== null && <form className="detail-section" onSubmit={saveRetention}><h2>生成型产物在线保留</h2><p className="detail-muted">该设置只影响后续新建的生成型产物；既有产物的过期时间不会被改写。</p><label>天数 <input name="generatedRetentionDays" type="number" min="1" value={retentionDraft} onChange={event => updateRetentionDraft(Number(event.target.value))} required /></label><button>保存产物保留</button></form>}
    <section className="detail-section"><h2>备份记录</h2>{pendingCount > 0 && <button onClick={mergePendingRuns}>显示 {pendingCount} 条新备份记录</button>}{backups === null ? <p role="status">正在读取备份记录…</p> : <BackupRuns items={backups} more={nextCursor !== null} onMore={() => void loadMore()} />}</section>
  </div>
}
