import { type FormEvent, useCallback, useEffect, useState } from 'react'
import { createUser, formatTime, listAuditEvents, listOwnSessions, listUsers, messageOf, resetPassword, revokeOwnSession, revokeSessions, updateUser, type AdminUser, type AuditEventInfo, type SessionInfo } from './api'

// Light refresh cadence for admin lists (same approach as the intake list):
// keeps user/session state current without a change channel.
const ADMIN_LIST_REFRESH_MS = 15_000

// AdminUsersPanel owns the /admin user management surface (T05): the user
// list with create/rename/enable/role/reset/revoke flows, the caller's own
// session list, and the audit trail — each with explicit waiting, failure
// and success states (UI-ADMIN waiting feedback).

interface AdminUsersPanelProps {
  currentUser: AdminUser
}

export function AdminUsersPanel({ currentUser }: AdminUsersPanelProps) {
  const [users, setUsers] = useState<AdminUser[] | null>(null)
  const [loadError, setLoadError] = useState(false)
  const [selected, setSelected] = useState<string | null>(null)
  const [notice, setNotice] = useState('')
  const [error, setError] = useState('')

  const reload = useCallback(async () => {
    try {
      const page = await listUsers()
      setUsers(page)
      setLoadError(false)
    } catch {
      setLoadError(true)
      setUsers(null)
    }
  }, [])

  useEffect(() => {
    // Same initial-load + visibility-gated refresh shape as IntakeIssuesList;
    // every setState inside reload() runs after its first await.
    // eslint-disable-next-line react-hooks/set-state-in-effect -- rule's static analysis false-positives on this async loader
    void reload()
    const timer = window.setInterval(() => {
      if (document.visibilityState === 'visible') void reload()
    }, ADMIN_LIST_REFRESH_MS)
    return () => window.clearInterval(timer)
  }, [reload])

  const active = users?.find((user) => user.id === selected) ?? null

  return (
    <div className="detail-content">
      <header className="detail-header">
        <div>
          <p className="eyebrow">管理</p>
          <h1>用户与会话</h1>
        </div>
      </header>
      <p className="detail-muted">这里管理本地账号：创建、停用、调整角色、重置密码和撤销登录。系统始终保留至少一个有效的管理员。</p>
      {loadError && <div className="error-summary" role="alert"><strong>用户列表没有加载</strong><span>请重试；如果持续失败，请检查服务状态。</span></div>}
      {users === null && !loadError && <p className="detail-muted" role="status">正在读取用户…</p>}
      {error && <div className="error-summary" role="alert"><strong>操作没有完成</strong><span>{error}</span></div>}
      {notice && <p className="admin-notice" role="status">{notice}</p>}
      {users !== null && (
        <section className="detail-section" aria-labelledby="admin-user-list-title">
          <h2 id="admin-user-list-title">用户（{users.length}）</h2>
          <ul className="admin-user-list">
            {users.map((user) => (
              <li key={user.id}>
                <button
                  className={`object-row${selected === user.id ? ' selected' : ''}`}
                  aria-pressed={selected === user.id}
                  onClick={() => setSelected(user.id)}
                >
                  <span className="object-row-main">
                    <strong>{user.displayName}（@{user.username}）</strong>
                    <span className="object-row-meta">
                      <em>{user.role === 'admin' ? '管理员' : '操作员'}</em>
                      {user.enabled ? <span>已启用</span> : <span className="admin-muted">已停用</span>}
                      {user.passwordChangeRequired && <span className="admin-warn">待改密</span>}
                      <span>{user.lastLoginAt ? `最近登录 ${formatTime(user.lastLoginAt)}` : '从未登录'}</span>
                    </span>
                  </span>
                </button>
              </li>
            ))}
          </ul>
        </section>
      )}
      {active && <UserActions key={active.id + ':' + active.rowVersion} user={active} isSelf={active.id === currentUser.id} onChanged={reload} onNotice={setNotice} onError={setError} />}
      <CreateUserCard onCreated={reload} />
      <OwnSessionsCard />
      <AuditCard />
    </div>
  )
}

interface UserActionsProps {
  user: AdminUser
  isSelf: boolean
  onChanged: () => Promise<void>
  onNotice: (message: string) => void
  onError: (message: string) => void
}

function UserActions({ user, isSelf, onChanged, onNotice, onError }: UserActionsProps) {
  const [displayName, setDisplayName] = useState(user.displayName)
  const [role, setRole] = useState<'admin' | 'operator'>(user.role)
  const [enabled, setEnabled] = useState(user.enabled)
  const [busy, setBusy] = useState<'save' | 'reset' | 'revoke' | null>(null)
  const [resetOpen, setResetOpen] = useState(false)
  const [newPassword, setNewPassword] = useState('')
  const [confirmRevoke, setConfirmRevoke] = useState(false)

  async function save() {
    onError('')
    onNotice('')
    setBusy('save')
    try {
      const changes: { displayName?: string; enabled?: boolean; role?: 'admin' | 'operator' } = {}
      if (displayName.trim() !== user.displayName) changes.displayName = displayName.trim()
      if (role !== user.role) changes.role = role
      if (enabled !== user.enabled) changes.enabled = enabled
      if (Object.keys(changes).length > 0) {
        await updateUser(user.id, user.rowVersion, changes)
        onNotice('已保存。')
        await onChanged()
      } else {
        onNotice('没有需要保存的修改。')
      }
    } catch (reason) {
      onError(messageOf(reason, '没有保存成功，请刷新后重试。'))
      await onChanged()
    } finally {
      setBusy(null)
    }
  }

  async function submitResetPassword(event: FormEvent) {
    event.preventDefault()
    onError('')
    onNotice('')
    setBusy('reset')
    try {
      const result = await resetPassword(user.id, user.rowVersion, newPassword)
      onNotice(`已重置密码；本次撤销了 ${result.revokedSessionCount} 个登录会话。`)
      setResetOpen(false)
      setNewPassword('')
      await onChanged()
    } catch (reason) {
      onError(messageOf(reason, '没有重置成功，请刷新后重试。'))
      await onChanged()
    } finally {
      setBusy(null)
    }
  }

  async function performRevokeSessions() {
    onError('')
    onNotice('')
    setBusy('revoke')
    try {
      const revoked = await revokeSessions(user.id)
      onNotice(`已撤销 ${revoked} 个登录会话。`)
      setConfirmRevoke(false)
      await onChanged()
    } catch (reason) {
      onError(messageOf(reason, '没有撤销成功，请重试。'))
      await onChanged()
    } finally {
      setBusy(null)
    }
  }

  return (
    <section className="detail-section admin-user-actions" aria-labelledby="admin-user-actions-title">
      <h2 id="admin-user-actions-title">编辑：{user.username}</h2>
      <form onSubmit={(event: FormEvent) => { event.preventDefault(); void save() }}>
        <label htmlFor="admin-display-name">显示名</label>
        <input id="admin-display-name" value={displayName} maxLength={200} onChange={(event) => setDisplayName(event.target.value)} />
        <label htmlFor="admin-role">角色</label>
        <select id="admin-role" value={role} onChange={(event) => setRole(event.target.value as 'admin' | 'operator')}>
          <option value="operator">操作员</option>
          <option value="admin">管理员</option>
        </select>
        <label className="admin-checkbox">
          <input type="checkbox" checked={enabled} onChange={(event) => setEnabled(event.target.checked)} />
          启用该账号{isSelf && '（当前登录账号）'}
        </label>
        <div className="admin-action-row">
          <button className="primary-button compact" type="submit" disabled={busy !== null}>
            {busy === 'save' ? '正在保存…' : '保存修改'}
          </button>
          {!resetOpen && (
            <button className="text-button compact" type="button" disabled={busy !== null} onClick={() => { setResetOpen(true); onNotice(''); onError('') }}>
              {busy === 'reset' ? '正在重置…' : '重置密码'}
            </button>
          )}
          {!confirmRevoke ? (
            <button className="text-button compact" type="button" disabled={busy !== null} onClick={() => { setConfirmRevoke(true); onNotice(''); onError('') }}>
              {busy === 'revoke' ? '正在撤销…' : '撤销全部会话'}
            </button>
          ) : (
            <>
              <button className="text-button compact admin-danger" type="button" disabled={busy !== null} onClick={() => void performRevokeSessions()}>确认撤销</button>
              <button className="text-button compact" type="button" disabled={busy !== null} onClick={() => setConfirmRevoke(false)}>取消</button>
            </>
          )}
        </div>
      </form>
      {resetOpen && (
        <form className="admin-reset-form" onSubmit={submitResetPassword}>
          <label htmlFor="admin-new-password">新的临时密码</label>
          <input id="admin-new-password" type="password" value={newPassword} minLength={15} maxLength={128} required autoComplete="new-password" onChange={(event) => setNewPassword(event.target.value)} />
          <p className="detail-muted">重置后该用户全部登录失效，首次登录需先修改此临时密码。</p>
          <div className="admin-action-row">
            <button className="primary-button compact" type="submit" disabled={busy !== null}>{busy === 'reset' ? '正在重置…' : '确认重置'}</button>
            <button className="text-button compact" type="button" onClick={() => setResetOpen(false)}>取消</button>
          </div>
        </form>
      )}
    </section>
  )
}

function CreateUserCard({ onCreated }: { onCreated: () => Promise<void> }) {
  const [open, setOpen] = useState(false)
  const [username, setUsername] = useState('')
  const [displayName, setDisplayName] = useState('')
  const [role, setRole] = useState<'admin' | 'operator'>('operator')
  const [password, setPassword] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')

  async function submit(event: FormEvent) {
    event.preventDefault()
    setError('')
    setNotice('')
    setBusy(true)
    try {
      const createdName = username.trim()
      await createUser({ username: createdName, displayName: displayName.trim(), role, password })
      setNotice(`已创建 @${createdName}。`)
      setUsername('')
      setDisplayName('')
      setPassword('')
      setOpen(false)
      await onCreated()
    } catch (reason) {
      setError(messageOf(reason, '没有创建成功，请重试。'))
    } finally {
      setBusy(false)
    }
  }

  if (!open) {
    return (
      <section className="detail-section">
        <button className="text-button compact" onClick={() => { setOpen(true); setNotice(''); setError('') }}>+ 创建用户</button>
        {notice && <p className="admin-notice" role="status">{notice}</p>}
      </section>
    )
  }
  return (
    <section className="detail-section admin-create-form" aria-labelledby="admin-create-title">
      <h2 id="admin-create-title">创建用户</h2>
      {error && <div className="error-summary" role="alert"><strong>没有创建成功</strong><span>{error}</span></div>}
      <form onSubmit={submit}>
        <label htmlFor="new-username">登录名</label>
        <input id="new-username" value={username} maxLength={200} required onChange={(event) => setUsername(event.target.value)} />
        <label htmlFor="new-display-name">显示名</label>
        <input id="new-display-name" value={displayName} maxLength={200} required onChange={(event) => setDisplayName(event.target.value)} />
        <label htmlFor="new-role">角色</label>
        <select id="new-role" value={role} onChange={(event) => setRole(event.target.value as 'admin' | 'operator')}>
          <option value="operator">操作员</option>
          <option value="admin">管理员</option>
        </select>
        <label htmlFor="new-user-password">初始密码</label>
        <input id="new-user-password" type="password" value={password} minLength={15} maxLength={128} required autoComplete="new-password" onChange={(event) => setPassword(event.target.value)} />
        <p className="detail-muted">初始密码请通过安全渠道告知使用者；用户可自行修改。</p>
        <div className="admin-action-row">
          <button className="primary-button compact" type="submit" disabled={busy}>{busy ? '正在创建…' : '创建'}</button>
          <button className="text-button compact" type="button" onClick={() => setOpen(false)}>收起</button>
        </div>
      </form>
    </section>
  )
}

function OwnSessionsCard() {
  const [sessions, setSessions] = useState<SessionInfo[] | null>(null)
  const [error, setError] = useState('')
  const [busyID, setBusyID] = useState<string | null>(null)

  const reload = useCallback(async () => {
    try {
      const page = await listOwnSessions()
      setSessions(page)
      setError('')
    } catch {
      setError('暂时无法读取登录设备，请刷新重试。')
    }
  }, [])

  useEffect(() => {
    void reload()
    const timer = window.setInterval(() => {
      if (document.visibilityState === 'visible') void reload()
    }, ADMIN_LIST_REFRESH_MS)
    return () => window.clearInterval(timer)
  }, [reload])

  async function revoke(id: string) {
    setBusyID(id)
    try {
      await revokeOwnSession(id)
      await reload()
    } catch (reason) {
      setError(messageOf(reason, '没有撤销成功，请重试。'))
    } finally {
      setBusyID(null)
    }
  }

  return (
    <section className="detail-section" aria-labelledby="admin-sessions-title">
      <h2 id="admin-sessions-title">我的登录设备</h2>
      {error && <div className="error-summary" role="alert"><strong>会话列表没有加载</strong><span>{error}</span></div>}
      {sessions === null && !error && <p className="detail-muted" role="status">正在读取登录设备…</p>}
      {sessions !== null && sessions.length === 0 && <p className="detail-muted">没有有效的登录设备。</p>}
      <ul className="admin-session-list">
        {(sessions ?? []).map((session) => (
          <li key={session.id}>
            <span className="object-row-main">
              <strong>{session.clientLabel}{session.current ? ' · 当前设备' : ''}</strong>
              <span className="object-row-meta"><span>登录于 {formatTime(session.createdAt)}</span></span>
            </span>
            {!session.current && (
              <button className="text-button compact" disabled={busyID === session.id} onClick={() => void revoke(session.id)}>
                {busyID === session.id ? '正在撤销…' : '下线'}
              </button>
            )}
          </li>
        ))}
      </ul>
    </section>
  )
}

function AuditCard() {
  const [events, setEvents] = useState<AuditEventInfo[] | null>(null)
  const [error, setError] = useState(false)

  useEffect(() => {
    let cancelled = false
    listAuditEvents()
      .then((items) => {
        if (!cancelled) setEvents(items)
      })
      .catch(() => {
        if (!cancelled) setError(true)
      })
    return () => {
      cancelled = true
    }
  }, [])

  return (
    <section className="detail-section" aria-labelledby="admin-audit-title">
      <h2 id="admin-audit-title">审计事件（最近）</h2>
      {error && <p className="detail-muted">暂时无法读取审计事件，请刷新重试。</p>}
      {events === null && !error && <p className="detail-muted" role="status">正在读取审计事件…</p>}
      <ul className="admin-audit-list">
        {(events ?? []).map((event) => (
          <li key={event.id}>
            <span className="admin-audit-action">{event.action}</span>
            <span className="object-row-meta">
              <em>{event.outcome === 'success' ? '成功' : event.outcome === 'rejected' ? '已拒绝' : event.outcome}</em>
              <span>{formatTime(event.createdAt)}</span>
            </span>
          </li>
        ))}
      </ul>
    </section>
  )
}
