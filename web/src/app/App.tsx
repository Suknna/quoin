import { useEffect, useState } from 'react'
import type { UserSummary } from '../api/generated/types'
import { ApiError, api } from './api'
import { ChangePassword } from '../features/auth/ChangePassword'
import { Login } from '../features/auth/Login'
import { MaintenancePage } from '../features/admin/maintenance/MaintenancePage'
import { Workbench } from '../features/shell/Workbench'
import type { MaintenanceState } from './api'

type State =
  | { kind: 'loading' }
  | { kind: 'guest' }
  | { kind: 'error'; message: string }
  | { kind: 'authenticated'; user: UserSummary }

// 401 means "not signed in"; anything else is an availability problem.
function classifyAuthFailure(reason: unknown): State {
  if (reason instanceof ApiError && reason.status === 401) return { kind: 'guest' }
  return { kind: 'error', message: reason instanceof Error ? reason.message : '工作台暂时不可用。' }
}

export function App() {
  const [state, setState] = useState<State>({ kind: 'loading' })
  const [maintenance, setMaintenance] = useState<MaintenanceState | null>(null)

  async function establishSession(user: UserSummary) {
    if (user.passwordChangeRequired) {
      setMaintenance(null)
      setState({ kind: 'authenticated', user })
      return
    }
    try {
      setMaintenance(await api.maintenance())
      setState({ kind: 'authenticated', user })
    } catch (reason) {
      setState({ kind: 'error', message: reason instanceof Error ? reason.message : '工作台暂时不可用。' })
    }
  }

  async function loadCurrentUser() {
    try { await establishSession(await api.currentUser()) } catch (reason) { setState(classifyAuthFailure(reason)) }
  }

  useEffect(() => {
    void api.currentUser().then(
      (user) => {
        if (user.passwordChangeRequired) {
          setMaintenance(null)
          setState({ kind: 'authenticated', user })
          return
        }
        void api.maintenance().then(
          (nextMaintenance) => { setMaintenance(nextMaintenance); setState({ kind: 'authenticated', user }) },
          (reason: unknown) => setState({ kind: 'error', message: reason instanceof Error ? reason.message : '工作台暂时不可用。' }),
        )
      },
      (reason: unknown) => setState(classifyAuthFailure(reason)),
    )
  }, [])

  useEffect(() => {
    if (state.kind !== 'authenticated' || state.user.passwordChangeRequired || maintenance?.active) return
    let cancelled = false
    const refreshMaintenance = () => {
      void api.maintenance().then((nextMaintenance) => {
        if (!cancelled) setMaintenance(nextMaintenance)
      }, () => {
        // A transient read failure must not replace the usable current screen.
      })
    }
    const timer = window.setInterval(refreshMaintenance, 5_000)
    return () => { cancelled = true; window.clearInterval(timer) }
  }, [state, maintenance?.active])

  if (state.kind === 'loading') {
    return <main className="loading-page" aria-busy="true"><div className="brand-mark">Q</div><p>正在确认登录状态…</p></main>
  }
  if (state.kind === 'error') {
    return <main className="loading-page"><div className="error-summary" role="alert"><strong>暂时无法打开工作台</strong><span>{state.message}</span></div><button className="primary-button compact" onClick={() => { setState({ kind: 'loading' }); void loadCurrentUser() }}>重试</button></main>
  }
  if (state.kind === 'guest') {
    return <Login onAuthenticated={(user) => { void establishSession(user) }} />
  }
  if (state.user.passwordChangeRequired) {
    return <ChangePassword user={state.user} onChanged={(user) => { void establishSession(user) }} onLogout={() => void api.logout().finally(() => setState({ kind: 'guest' }))} />
  }
  if (maintenance?.active) {
    return <MaintenancePage user={state.user} state={maintenance} onLogout={() => void api.logout().finally(() => setState({ kind: 'guest' }))} />
  }
  return <Workbench user={state.user} onLogout={() => setState({ kind: 'guest' })} />
}
