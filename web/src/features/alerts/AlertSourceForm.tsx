import { type FormEvent, useState } from 'react'
import { createAlertSource, newClientCommandId, revealCredential } from './api'

interface AlertSourceFormProps {
  onCreated: () => void
}

export function AlertSourceForm({ onCreated }: AlertSourceFormProps) {
  const [key, setKey] = useState('')
  const [error, setError] = useState('')
  const [revealed, setRevealed] = useState<string | null>(null)
  const [createdKey, setCreatedKey] = useState('')
  const [submitting, setSubmitting] = useState(false)

  async function submit(event: FormEvent) {
    event.preventDefault()
    setError('')
    setRevealed(null)
    setCreatedKey('')
    setSubmitting(true)
    try {
      const metadata = await createAlertSource({
        key: key.trim(),
        protocol: 'alertmanager',
        clientCommandId: newClientCommandId(),
      })
      if (!metadata.revealAvailable || !metadata.revealHandle) {
        throw new Error('凭据句柄不可用，请重试。')
      }
      const result = await revealCredential(metadata.revealHandle)
      setRevealed(result.bearerToken)
      setCreatedKey(metadata.sourceKey)
      setKey('')
      onCreated()
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : '创建失败，请重试。')
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <section className="alert-source-form" aria-labelledby="source-form-title">
      <h2 id="source-form-title">添加 Alertmanager 告警源</h2>
      {error && <div className="error-summary" role="alert"><strong>没有创建成功</strong><span>{error}</span></div>}
      <form onSubmit={submit} noValidate>
        <label htmlFor="source-key">逻辑告警源 key</label>
        <input id="source-key" value={key} onChange={(event) => setKey(event.target.value)} placeholder="例如 prod-alertmanager" maxLength={200} required autoComplete="off" />
        <button className="primary-button compact" type="submit" disabled={submitting || key.trim() === ''}>
          {submitting ? '正在创建…' : '创建并显示凭据'}
        </button>
      </form>
      {revealed && (
        <div className="credential-reveal" role="status">
          <strong>告警源 {createdKey || '已创建'} · 一次性凭据（只显示这一次）</strong>
          <code>{revealed}</code>
          <p>把它填入 Alertmanager 的 webhook Bearer。关闭本页或刷新后无法再次查看；需要轮换时重新创建。</p>
        </div>
      )}
    </section>
  )
}
