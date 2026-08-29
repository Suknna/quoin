// CandidateEditor is the full-workbench edit layer for one Knowledge
// Candidate (UI-KNOWLEDGE-003): only the draft title and body are
// editable; the source diagnosis and the immutable original suggestion
// stay read-only. Draft saves carry the expected revision; a conflict
// keeps the local input and shows the authoritative revision instead of
// silently overwriting it.

import { useEffect, useState } from 'react'
import {
  api, candidateSourceLabels, candidateStateLabels, CommandConflictError,
  type CandidateDetail,
} from './api'

interface CandidateEditorProps {
  candidateId: string
  onClose: () => void
  onConfirmed: (knowledgeId: string) => void
}

export function CandidateEditor({ candidateId, onClose, onConfirmed }: CandidateEditorProps) {
  const [detail, setDetail] = useState<CandidateDetail | null>(null)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')
  const [title, setTitle] = useState('')
  const [body, setBody] = useState('')
  const [revision, setRevision] = useState(0)
  const [rowVersion, setRowVersion] = useState(1)
  const [busy, setBusy] = useState(false)
  const [confirmingExclude, setConfirmingExclude] = useState(false)

  useEffect(() => {
    let cancelled = false
    setError('')
    void api.getCandidate(candidateId)
      .then((next) => {
        if (cancelled) return
        setDetail(next)
        setTitle(next.draftTitle ?? '')
        setBody(next.draftBody ?? '')
        setRevision(next.draftRevision)
        setRowVersion(next.rowVersion)
      })
      .catch((reason: unknown) => {
        if (!cancelled) setError(reason instanceof Error ? reason.message : '候选暂时不可读。')
      })
    return () => { cancelled = true }
  }, [candidateId])

  async function saveDraft() {
    setBusy(true)
    setError('')
    setNotice('')
    try {
      const next = await api.editDraft(candidateId, revision, { title, body })
      setRevision(next.draftRevision)
      setRowVersion(next.rowVersion)
      setNotice('草稿已保存。')
    } catch (reason) {
      if (reason instanceof CommandConflictError && reason.conflict?.currentRevision !== undefined) {
        setRevision(reason.conflict.currentRevision)
        setNotice('')
        setError('草稿已被其他页面更新；你输入的内容已保留，确认后可基于最新版本重新保存。')
      } else {
        setError(reason instanceof Error ? reason.message : '暂时无法保存草稿。')
      }
    } finally {
      setBusy(false)
    }
  }

  async function confirm() {
    setBusy(true)
    setError('')
    setNotice('')
    try {
      const next = await api.confirm(candidateId, revision)
      onConfirmed(next.confirmedKnowledgeId ?? '')
    } catch (reason) {
      if (reason instanceof CommandConflictError && reason.conflict?.currentRevision !== undefined) {
        setRevision(reason.conflict.currentRevision)
        setError('草稿版本已变化，请基于最新版本重新确认。')
      } else {
        setError(reason instanceof Error ? reason.message : '暂时无法确认该候选。')
      }
    } finally {
      setBusy(false)
    }
  }

  async function exclude() {
    setBusy(true)
    setError('')
    try {
      await api.exclude(candidateId, rowVersion)
      onClose()
    } catch (reason) {
      if (reason instanceof CommandConflictError) {
        setError('候选已被其他操作更新，请刷新后重试。')
      } else {
        setError(reason instanceof Error ? reason.message : '暂时无法排除该候选。')
      }
    } finally {
      setBusy(false)
      setConfirmingExclude(false)
    }
  }

  if (!detail) {
    return (
      <div className="candidate-editor" role="document" aria-label="知识候选编辑层">
        <button className="text-button" onClick={onClose}>← 返回知识</button>
        {error ? <p className="field-error" role="alert">{error}</p> : <p className="inline-status">正在读取候选…</p>}
      </div>
    )
  }

  const operable = detail.state === 'AwaitingConfirmation'
  return (
    <div className="candidate-editor" role="document" aria-label="知识候选编辑层">
      <button className="text-button" onClick={onClose}>← 返回知识</button>
      <header className="candidate-header">
        <p className="eyebrow">{candidateSourceLabels[detail.sourceType]} · {candidateStateLabels[detail.state]}</p>
        <h2>整理为知识</h2>
        <p className="detail-muted">
          草稿版本 r{revision} · 确认由人完成，模型建议只作为起点；确认后会创建一条可复用知识。
        </p>
      </header>
      {error && <div className="error-summary" role="alert"><strong>操作未完成</strong><span>{error}</span></div>}
      {notice && <p className="detail-muted" role="status">{notice}</p>}
      <section className="candidate-source" aria-label="来源诊断（只读）">
        <h3>模型原始建议（不可修改）</h3>
        <p className="candidate-suggestion-title">{detail.originalSuggestion.title}</p>
        <pre className="candidate-suggestion-body">{detail.originalSuggestion.body}</pre>
        <p className="detail-muted">
          来源：{candidateSourceLabels[detail.originalSuggestion.source.type]}
          {detail.originalSuggestion.source.modelId ? ` · 模型 ${detail.originalSuggestion.source.modelId}` : ''}
        </p>
      </section>
      <section className="candidate-draft" aria-label="草稿编辑">
        <h3>知识草稿</h3>
        <label className="field">
          <span>标题</span>
          <input value={title} disabled={!operable || busy} onChange={(event) => setTitle(event.target.value)} />
        </label>
        <label className="field">
          <span>正文</span>
          <textarea className="candidate-body" value={body} disabled={!operable || busy} rows={10} onChange={(event) => setBody(event.target.value)} />
        </label>
        <div className="candidate-actions">
          {operable && (
            <>
              <button className="secondary-button" disabled={busy} onClick={() => void saveDraft()}>保存草稿</button>
              <button className="primary-button" disabled={busy} onClick={() => void confirm()}>确认并创建知识</button>
              <button className="text-button" disabled={busy} onClick={() => setConfirmingExclude(true)}>不使用这条建议</button>
            </>
          )}
          {!operable && (
            <p className="detail-muted">
              该候选已处于“{candidateStateLabels[detail.state]}”状态，不能再编辑或确认。
              {detail.confirmedKnowledgeId ? ' 已创建的知识可以在“知识”列表中查看。' : ''}
            </p>
          )}
        </div>
      </section>
      {confirmingExclude && (
        <div className="dialog-scrim" role="presentation" onClick={() => setConfirmingExclude(false)}>
          <div className="dialog-panel" role="dialog" aria-modal="true" aria-labelledby="candidate-exclude-title" onClick={(event) => event.stopPropagation()}>
            <h3 id="candidate-exclude-title">排除这条知识候选？</h3>
            <p>排除后该候选不再可确认；原始建议与来源记录保留在历史中。</p>
            <div className="dialog-actions">
              <button type="button" className="secondary-button" onClick={() => setConfirmingExclude(false)}>取消</button>
              <button type="button" className="primary-button danger" disabled={busy} onClick={() => void exclude()}>确认排除</button>
            </div>
          </div>
        </div>
      )}
    </div>
  )
}
