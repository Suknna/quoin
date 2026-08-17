import { type FormEvent, useEffect, useRef, useState } from 'react'
import type { UserSummary } from '../../api/generated/types'
import { api } from '../../app/api'

interface LoginProps {
  onAuthenticated: (user: UserSummary) => void
}

export function Login({ onAuthenticated }: LoginProps) {
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [error, setError] = useState('')
  const [submitting, setSubmitting] = useState(false)
  const errorRef = useRef<HTMLDivElement>(null)

  useEffect(() => {
    if (error) errorRef.current?.focus()
  }, [error])

  async function submit(event: FormEvent) {
    event.preventDefault()
    setError('')
    setSubmitting(true)
    try {
      onAuthenticated(await api.login({ username, password }))
      setPassword('')
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : '登录暂时不可用，请重试。')
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <main className="auth-page">
      <section className="auth-card" aria-labelledby="login-title">
        <div className="brand-mark" aria-hidden="true">Q</div>
        <p className="eyebrow">QUOIN OPERATIONS</p>
        <h1 id="login-title">登录工作台</h1>
        <p className="auth-intro">使用管理员为你创建的本地账号继续。</p>
        {error && (
          <div className="error-summary" ref={errorRef} tabIndex={-1} role="alert">
            <strong>没有登录成功</strong>
            <span>{error}</span>
          </div>
        )}
        <form onSubmit={submit} noValidate>
          <label htmlFor="username">用户名</label>
          <input id="username" name="username" autoComplete="username" value={username} onChange={(event) => setUsername(event.target.value)} required maxLength={200} autoFocus />
          <label htmlFor="password">密码</label>
          <input id="password" name="password" type="password" autoComplete="current-password" value={password} onChange={(event) => setPassword(event.target.value)} required minLength={15} maxLength={128} />
          <button className="primary-button" type="submit" disabled={submitting}>
            {submitting ? '正在登录…' : '登录'}
          </button>
        </form>
      </section>
    </main>
  )
}
