import { useEffect, useState } from 'react'
import type { UserSummary } from '../api/generated/types'
import { ApiError, api } from './api'
import { ChangePassword } from '../features/auth/ChangePassword'
import { Login } from '../features/auth/Login'
import { Workbench } from '../features/shell/Workbench'

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

  async function loadCurrentUser() {
    try {
      setState({ kind: 'authenticated', user: await api.currentUser() })
    } catch (reason) {
      setState(classifyAuthFailure(reason))
    }
  }

  useEffect(() => {
    void api.currentUser().then(
      (user) => setState({ kind: 'authenticated', user }),
      (reason: unknown) => setState(classifyAuthFailure(reason)),
    )
  }, [])

  if (state.kind === 'loading') {
    return <main className="loading-page" aria-busy="true"><div className="brand-mark">Q</div><p>正在确认登录状态…</p></main>
  }
  if (state.kind === 'error') {
    return <main className="loading-page"><div className="error-summary" role="alert"><strong>暂时无法打开工作台</strong><span>{state.message}</span></div><button className="primary-button compact" onClick={() => { setState({ kind: 'loading' }); void loadCurrentUser() }}>重试</button></main>
  }
  if (state.kind === 'guest') {
    return <Login onAuthenticated={(user) => setState({ kind: 'authenticated', user })} />
  }
  if (state.user.passwordChangeRequired) {
    return <ChangePassword user={state.user} onChanged={(user) => setState({ kind: 'authenticated', user })} onLogout={() => void api.logout().finally(() => setState({ kind: 'guest' }))} />
  }
  return <Workbench user={state.user} onLogout={() => setState({ kind: 'guest' })} />
}
