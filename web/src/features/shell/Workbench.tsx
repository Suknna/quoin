import { useEffect, useRef, useState } from 'react'
import type { RuntimeStatus, UserSummary } from '../../api/generated/types'
import { api } from '../../app/api'
import { AlertDetail } from '../alerts/AlertDetail'
import { AlertSourceForm } from '../alerts/AlertSourceForm'
import { AlertsList } from '../alerts/AlertsList'
import { IntakeIssuesList } from '../alerts/IntakeIssuesList'

interface WorkbenchProps {
  user: UserSummary
  onLogout: () => void
}

type ModuleKey = 'alerts' | 'investigations' | 'inspections' | 'business-systems' | 'knowledge' | 'admin'

const modules: Array<{ key: ModuleKey; label: string; path: string; adminOnly?: boolean }> = [
  { key: 'alerts', label: '告警', path: '/alerts' },
  { key: 'investigations', label: '调查', path: '/investigations' },
  { key: 'inspections', label: '巡检', path: '/inspections' },
  { key: 'business-systems', label: '业务系统', path: '/business-systems' },
  { key: 'knowledge', label: '知识', path: '/knowledge' },
  { key: 'admin', label: '管理', path: '/admin', adminOnly: true },
]

function moduleFromPath(): ModuleKey {
  const match = modules.find((item) => window.location.pathname.startsWith(item.path))
  return match?.key ?? 'alerts'
}

export function Workbench({ user, onLogout }: WorkbenchProps) {
  const [active, setActive] = useState<ModuleKey>(moduleFromPath)
  const [drawerOpen, setDrawerOpen] = useState(false)
  const [profileOpen, setProfileOpen] = useState(false)
  const [runtime, setRuntime] = useState<RuntimeStatus | null>(null)
  const [runtimeError, setRuntimeError] = useState(false)
  const [selectedOccurrence, setSelectedOccurrence] = useState<string | null>(null)
  const [intakeOpen, setIntakeOpen] = useState(false)
  const profileButton = useRef<HTMLButtonElement>(null)
  const visibleModules = modules.filter((item) => !item.adminOnly || user.role === 'admin')

  useEffect(() => {
    void api.runtime().then(setRuntime).catch(() => setRuntimeError(true))
    const sync = () => {
      setActive(moduleFromPath())
      const match = window.location.pathname.match(/^\/alerts\/(\d+)/)
      setSelectedOccurrence(match ? match[1] : null)
    }
    sync()
    window.addEventListener('popstate', sync)
    return () => window.removeEventListener('popstate', sync)
  }, [])

  function navigate(key: ModuleKey, path: string) {
    window.history.pushState({}, '', path)
    setActive(key)
    setDrawerOpen(false)
    setSelectedOccurrence(null)
  }

  function selectOccurrence(id: string) {
    window.history.pushState({}, '', `/alerts/${id}`)
    setSelectedOccurrence(id)
  }

  async function logout() {
    setProfileOpen(false)
    await api.logout().catch(() => undefined)
    onLogout()
  }

  const activeModule = visibleModules.find((item) => item.key === active) ?? visibleModules[0]
  return (
    <div className="workbench">
      <header className="mobile-header">
        <button className="icon-button" aria-label="打开模块导航" aria-expanded={drawerOpen} onClick={() => setDrawerOpen(true)}><MenuIcon /></button>
        <span className="mobile-title">{activeModule.label}</span>
        <ProfileButton user={user} open={profileOpen} buttonRef={profileButton} onToggle={() => setProfileOpen((value) => !value)} onLogout={logout} />
      </header>
      {drawerOpen && <button className="drawer-scrim" aria-label="关闭模块导航" onClick={() => setDrawerOpen(false)} />}
      <nav className={`global-nav ${drawerOpen ? 'open' : ''}`} aria-label="全局模块">
        <div className="brand-mark small" aria-label="Quoin">Q</div>
        <div className="nav-items">
          {visibleModules.map((item) => (
            <button key={item.key} className={active === item.key ? 'nav-item active' : 'nav-item'} aria-label={item.label} title={item.label} onClick={() => navigate(item.key, item.path)}>
              <ModuleIcon name={item.key} /><span>{item.label}</span>
            </button>
          ))}
        </div>
        <div className="desktop-profile"><ProfileButton user={user} open={profileOpen} buttonRef={profileButton} onToggle={() => setProfileOpen((value) => !value)} onLogout={logout} /></div>
      </nav>
      <aside className="object-list" aria-label={`${activeModule.label}列表`}>
        <div className="list-heading">
          <p className="eyebrow">{activeModule.label}</p>
          <h1>{activeModule.label}</h1>
        </div>
        {active === 'alerts' && (
          <>
            <div className="segmented" aria-label="告警视图">
              <button aria-pressed={!intakeOpen} onClick={() => setIntakeOpen(false)}>当前</button>
              <button aria-pressed={intakeOpen} onClick={() => setIntakeOpen(true)}>接入问题</button>
            </div>
            {intakeOpen ? <IntakeIssuesList /> : <AlertsList onSelect={selectOccurrence} />}
          </>
        )}
        {active !== 'alerts' && (
          <div className="inline-status" role="status">
            <span className="status-dot waiting" />
            <div><strong>此能力尚未接入</strong><p>当前安装已经就绪；后续纵向票会在这里加入真实对象。</p></div>
          </div>
        )}
        {active === 'admin' && (
          <section className="runtime-summary" aria-labelledby="runtime-title">
            <h2 id="runtime-title">运行组件</h2>
            {runtimeError && <p>暂时无法读取组件状态。</p>}
            {runtime && <>
              <RuntimeRow label="Plinth" state={runtime.plinth.state} />
              <RuntimeRow label="Lintel" state={runtime.lintel.state} />
              <RuntimeRow label="Stele" state="dependency_unavailable" />
            </>}
          </section>
        )}
      </aside>
      <main className="detail-pane" tabIndex={-1}>
        {active === 'alerts' && selectedOccurrence ? (
          <AlertDetail occurrenceId={selectedOccurrence} onBack={() => { window.history.pushState({}, '', '/alerts'); setSelectedOccurrence(null) }} />
        ) : active === 'alerts' ? (
          <div className="detail-content">
            <div className="empty-state">
              <ModuleIcon name="alerts" large />
              <h2>选择一条告警查看详情</h2>
              <p>左侧列出当前 Firing 的告警；接入问题在第二栏顶部切换。</p>
            </div>
            {user.role === 'admin' && <AlertSourceForm onCreated={() => undefined} />}
          </div>
        ) : (
          <div className="empty-state">
            <ModuleIcon name={activeModule.key} large />
            <h2>{activeModule.label}尚无可查看内容</h2>
            <p>这里不会展示虚构数据。相应能力接入后，你可以从左侧选择真实对象查看详情。</p>
          </div>
        )}
      </main>
    </div>
  )
}

function RuntimeRow({ label, state }: { label: string; state: string }) {
  const text = state === 'unregistered' ? '尚未注册' : state === 'dependency_unavailable' ? '等待依赖' : state
  return <div className="runtime-row"><span>{label}</span><span className="status-pill">{text}</span></div>
}

function ProfileButton({ user, open, buttonRef, onToggle, onLogout }: { user: UserSummary; open: boolean; buttonRef: React.RefObject<HTMLButtonElement | null>; onToggle: () => void; onLogout: () => void }) {
  return <div className="profile-wrap">
    <button ref={buttonRef} className="profile-button" aria-label={`${user.displayName} 账号菜单`} aria-expanded={open} onClick={onToggle}>{user.displayName.slice(0, 1).toUpperCase()}</button>
    {open && <div className="profile-menu" role="menu">
      <div className="profile-identity"><strong>{user.displayName}</strong><span>@{user.username} · {user.role === 'admin' ? '管理员' : '操作员'}</span></div>
      <button role="menuitem" onClick={onLogout}>退出登录</button>
    </div>}
  </div>
}

function MenuIcon() {
  return <svg viewBox="0 0 24 24" aria-hidden="true"><path d="M4 7h16M4 12h16M4 17h16" /></svg>
}

function ModuleIcon({ name, large = false }: { name: ModuleKey; large?: boolean }) {
  const paths: Record<ModuleKey, string> = {
    alerts: 'M12 3 3 19h18L12 3Zm0 5v5m0 3h.01', investigations: 'm4 19 4-1 9-9-3-3-9 9-1 4Zm9-12 2-2 3 3-2 2',
    inspections: 'M6 3h12v18H6V3Zm3 5h6m-6 4h6m-6 4h4', 'business-systems': 'M4 20V8l8-5 8 5v12H4Zm5 0v-6h6v6',
    knowledge: 'M5 4h11a3 3 0 0 1 3 3v13H8a3 3 0 0 1-3-3V4Zm3 13h11', admin: 'M12 3 4 6v5c0 5 3 8 8 10 5-2 8-5 8-10V6l-8-3Zm-3 8 2 2 4-5',
  }
  return <svg className={large ? 'module-icon large' : 'module-icon'} viewBox="0 0 24 24" aria-hidden="true"><path d={paths[name]} /></svg>
}
