import { ConnectionsPanel } from '../connections/ConnectionsPanel'
import type { UserSummary } from '../../../api/generated/types'
import type { MaintenanceState } from '../../../app/api'
import { UpgradeMaintenancePanel } from './UpgradeMaintenancePanel'
import type { MaintenanceStateView } from './api'

const labels: Record<NonNullable<MaintenanceState['reason']>, string> = { Restore: '恢复后信任重建', Upgrade: '升级协调', RootKeyRebind: '根密钥更换' }

// The full-page boundary is intentionally above Workbench. The server's
// maintenance allowlist is authoritative; this projection removes stale normal
// navigation rather than relying on hidden buttons for containment.
export function MaintenancePage({ user, state, onLogout }: { user: UserSummary; state: MaintenanceState; onLogout: () => void }) {
  return <main className="maintenance-page" aria-labelledby="maintenance-title">
    <header className="maintenance-page-header"><div className="brand-mark">Q</div><div><p className="eyebrow">维护中</p><h1 id="maintenance-title">{state.reason ? labels[state.reason] : '维护'}</h1></div><button className="text-button" onClick={onLogout}>退出登录</button></header>
    <section className="maintenance-page-card"><p>普通业务入口已暂停。系统会在退出维护时重新核验清单，不能跳过。</p>{state.reason === 'RootKeyRebind' ? <ConnectionsPanel maintenanceMode /> : state.reason === 'Upgrade' ? <UpgradeMaintenancePanel user={user} initialState={state as MaintenanceStateView} /> : <MaintenanceChecklist state={state} user={user} />}</section>
  </main>
}

function MaintenanceChecklist({ state, user }: { state: MaintenanceState; user: UserSummary }) {
  return <><ul className="maintenance-checklist" role="list">{state.items.map((item) => <li key={`${item.kind}:${item.objectKey}`}><span>{item.objectKey} · {item.detailCode}</span><strong>{item.safeState === 'Safe' ? '已安全' : '需要处理'}</strong></li>)}</ul>{user.role === 'admin' ? <p>请按此维护原因的允许路径完成清单项目。</p> : <p>请联系管理员完成维护。</p>}</>
}
