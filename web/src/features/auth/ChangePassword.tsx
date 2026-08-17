import { type FormEvent, useEffect, useRef, useState } from 'react'
import type { UserSummary } from '../../api/generated/types'
import { api } from '../../app/api'

interface ChangePasswordProps {
  user: UserSummary
  onChanged: (user: UserSummary) => void
  onLogout: () => void
}

export function ChangePassword({ user, onChanged, onLogout }: ChangePasswordProps) {
  const [currentPassword, setCurrentPassword] = useState('')
  const [newPassword, setNewPassword] = useState('')
  const [confirmation, setConfirmation] = useState('')
  const [error, setError] = useState('')
  const [submitting, setSubmitting] = useState(false)
  const errorRef = useRef<HTMLDivElement>(null)

  useEffect(() => {
    if (error) errorRef.current?.focus()
  }, [error])

  async function submit(event: FormEvent) {
    event.preventDefault()
    setError('')
    if (newPassword !== confirmation) {
      setError('两次输入的新密码不一致。')
      return
    }
    setSubmitting(true)
    try {
      await api.changePassword({ currentPassword, newPassword })
      onChanged(await api.currentUser())
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : '没有修改成功，请重试。')
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <main className="auth-page">
      <section className="auth-card wide" aria-labelledby="password-title">
        <p className="eyebrow">首次登录 · {user.displayName}</p>
        <h1 id="password-title">先设置你自己的密码</h1>
        <p className="auth-intro">临时密码只能用于首次登录。修改成功后，当前页面会直接进入工作台。</p>
        <div className="password-guidance">
          使用 15–128 个字符。可以使用空格和中文；无需刻意组合大小写、数字或符号。
        </div>
        {error && (
          <div className="error-summary" ref={errorRef} tabIndex={-1} role="alert">
            <strong>密码没有修改</strong>
            <span>{error}</span>
          </div>
        )}
        <form onSubmit={submit} noValidate>
          <label htmlFor="current-password">当前临时密码</label>
          <input id="current-password" type="password" autoComplete="current-password" value={currentPassword} onChange={(event) => setCurrentPassword(event.target.value)} minLength={15} maxLength={128} required autoFocus />
          <label htmlFor="new-password">新密码</label>
          <input id="new-password" type="password" autoComplete="new-password" value={newPassword} onChange={(event) => setNewPassword(event.target.value)} minLength={15} maxLength={128} required />
          <label htmlFor="confirm-password">再次输入新密码</label>
          <input id="confirm-password" type="password" autoComplete="new-password" value={confirmation} onChange={(event) => setConfirmation(event.target.value)} minLength={15} maxLength={128} required />
          <button className="primary-button" type="submit" disabled={submitting}>{submitting ? '正在保存…' : '保存并进入工作台'}</button>
          <button className="text-button" type="button" onClick={onLogout}>退出登录</button>
        </form>
      </section>
    </main>
  )
}
