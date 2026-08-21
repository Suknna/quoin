import { useEffect, useRef, useState } from 'react'
import type { RuntimeStatus, UserSummary } from '../../api/generated/types'
import { api } from '../../app/api'
import { AlertDetail } from '../alerts/AlertDetail'
import { AlertSourceForm } from '../alerts/AlertSourceForm'
import { AlertsList } from '../alerts/AlertsList'
import { IntakeIssuesList } from '../alerts/IntakeIssuesList'
import { ConnectionsPanel } from '../admin/connections/ConnectionsPanel'
import { AdminUsersPanel } from '../admin/users/AdminUsersPanel'
import { RuntimesPanel } from '../admin/runtimes/RuntimesPanel'
import { InvestigationChat } from '../investigation/InvestigationChat'
import { InvestigationsList } from '../investigation/InvestigationsList'
import { NewInvestigation, type InvestigationSourceRef } from '../investigation/NewInvestigation'

interface WorkbenchProps {
  user: UserSummary
  onLogout: () => void
}

type ModuleKey = 'alerts' | 'investigations' | 'inspections' | 'business-systems' | 'knowledge' | 'admin'

type InvestigationView = { kind: 'list' } | { kind: 'new'; sources: InvestigationSourceRef[] } | { kind: 'chat'; investigationId: string }

function investigationViewFromPath(): InvestigationView {
  const match = window.location.pathname.match(/^\/investigations\/(\d+)/)
  if (match) return { kind: 'chat', investigationId: match[1] }
  if (window.location.pathname === '/investigations/new') {
    const params = new URLSearchParams(window.location.search)
    const sources: InvestigationSourceRef[] = []
    for (const occurrence of params.getAll('occurrence')) {
      if (/^\d+$/.test(occurrence)) sources.push({ type: 'occurrence', sourceId: occurrence })
    }
    for (const analysis of params.getAll('initialAnalysis')) {
      if (/^\d+$/.test(analysis)) sources.push({ type: 'initial_analysis', sourceId: analysis })
    }
    return { kind: 'new', sources }
  }
  return { kind: 'list' }
}

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
  const [openAnalysisId, setOpenAnalysisId] = useState<string | null>(null)
  const [alertSegment, setAlertSegment] = useState<'current' | 'history' | 'intake'>('current')
  const [adminSegment, setAdminSegment] = useState<'users' | 'connections' | 'runtimes'>('users')
  const [investigationView, setInvestigationView] = useState<InvestigationView>(investigationViewFromPath)
  const profileButton = useRef<HTMLButtonElement>(null)
  const visibleModules = modules.filter((item) => !item.adminOnly || user.role === 'admin')

  useEffect(() => {
    void api.runtime().then(setRuntime).catch(() => setRuntimeError(true))
    const sync = () => {
      setActive(moduleFromPath())
      const match = window.location.pathname.match(/^\/alerts\/(\d+)/)
      setSelectedOccurrence(match ? match[1] : null)
      const analysisMatch = window.location.pathname.match(/^\/alerts\/(\d+)\/analyses\/(\d+)/)
      setOpenAnalysisId(analysisMatch ? analysisMatch[2] : null)
      setInvestigationView(investigationViewFromPath())
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

	function openInvestigation(investigationId: string) {
		window.history.pushState({}, '', `/investigations/${investigationId}`)
		setActive('investigations')
		setInvestigationView({ kind: 'chat', investigationId })
	}

	function newInvestigation(sources: InvestigationSourceRef[]) {
		const params = new URLSearchParams()
		for (const source of sources) {
			if (source.type === 'occurrence') params.append('occurrence', source.sourceId)
			if (source.type === 'initial_analysis') params.append('initialAnalysis', source.sourceId)
		}
		const query = params.size > 0 ? `?${params.toString()}` : ''
		window.history.pushState({}, '', `/investigations/new${query}`)
		// pushState fires no popstate event: the module must switch here or
		// the entry point from an alert detail stays on the alerts module.
		setActive('investigations')
		setInvestigationView({ kind: 'new', sources })
	}

  function backToInvestigations() {
    window.history.pushState({}, '', '/investigations')
    setInvestigationView({ kind: 'list' })
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
              <button aria-pressed={alertSegment === 'current'} onClick={() => setAlertSegment('current')}>当前</button>
              <button aria-pressed={alertSegment === 'history'} onClick={() => setAlertSegment('history')}>历史</button>
              <button aria-pressed={alertSegment === 'intake'} onClick={() => setAlertSegment('intake')}>接入问题</button>
            </div>
            {alertSegment === 'intake' ? <IntakeIssuesList isAdmin={user.role === 'admin'} /> : <AlertsList view={alertSegment === 'history' ? 'Resolved' : 'Firing'} onSelect={selectOccurrence} />}
          </>
        )}
        {active !== 'alerts' && (
          <div className="inline-status" role="status">
            <span className="status-dot waiting" />
            <div><strong>此能力尚未接入</strong><p>当前安装已经就绪；后续纵向票会在这里加入真实对象。</p></div>
          </div>
        )}
        {active === 'admin' && (
          <>
          <div className="segmented admin-segment" aria-label="管理视图">
            <button aria-selected={adminSegment === 'users'} onClick={() => setAdminSegment('users')}>用户与会话</button>
            <button aria-selected={adminSegment === 'connections'} onClick={() => setAdminSegment('connections')}>连接</button>
            <button aria-selected={adminSegment === 'runtimes'} onClick={() => setAdminSegment('runtimes')}>运行组件</button>
          </div>
          <section className="runtime-summary" aria-labelledby="runtime-title">
            <h2 id="runtime-title">运行组件</h2>
            {runtimeError && <p>暂时无法读取组件状态。</p>}
            {runtime && <>
              <RuntimeRow label="Plinth" state={runtime.plinth.state} />
              <RuntimeRow label="Lintel" state={runtime.lintel.state} />
              <RuntimeRow label="Stele" state="dependency_unavailable" />
            </>}
          </section>
          </>
        )}
		{active === 'investigations' && (
			<InvestigationsList
				key={investigationView.kind}
				onOpen={openInvestigation}
				onNew={() => newInvestigation([])}
			/>
		)}
      </aside>
      <main className="detail-pane" tabIndex={-1}>
        {active === 'admin' && user.role === 'admin' ? (
          adminSegment === 'runtimes' ? <RuntimesPanel /> : adminSegment === 'connections' ? <ConnectionsPanel /> : <AdminUsersPanel currentUser={user} />
        ) : active === 'alerts' && selectedOccurrence ? (
          <AlertDetail
            occurrenceId={selectedOccurrence}
            openAnalysisId={openAnalysisId ?? undefined}
            onOpenAnalysis={(analysisId) => { window.history.pushState({}, '', `/alerts/${selectedOccurrence}/analyses/${analysisId}`); setOpenAnalysisId(analysisId) }}
            onCloseAnalysis={() => { window.history.pushState({}, '', `/alerts/${selectedOccurrence}`); setOpenAnalysisId(null) }}
            onStartInvestigation={() => newInvestigation([{ type: 'occurrence', sourceId: selectedOccurrence }])}
            onBack={() => { window.history.pushState({}, '', '/alerts'); setSelectedOccurrence(null); setOpenAnalysisId(null) }}
          />
        ) : active === 'alerts' ? (
          <div className="detail-content">
            <div className="empty-state">
              <ModuleIcon name="alerts" large />
              <h2>选择一条告警查看详情</h2>
              <p>左侧列出当前 Firing 的告警；接入问题在第二栏顶部切换。</p>
            </div>
            {user.role === 'admin' && <AlertSourceForm onCreated={() => undefined} />}
          </div>
        ) : active === 'investigations' && investigationView.kind === 'new' ? (
          <NewInvestigation
            sources={investigationView.sources}
            onCreated={(investigationId) => openInvestigation(investigationId)}
            onBack={backToInvestigations}
          />
        ) : active === 'investigations' && investigationView.kind === 'chat' ? (
          <InvestigationChat investigationId={investigationView.investigationId} onBack={backToInvestigations} />
        ) : active === 'investigations' ? (
          <div className="detail-content">
            <div className="empty-state">
              <ModuleIcon name="investigations" large />
              <h2>从一次调查开始</h2>
              <p>新建空白调查直接描述问题，或从告警/初步分析进入并自动携带来源。</p>
              <button className="primary-button" onClick={() => newInvestigation([])}>新建调查</button>
            </div>
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
