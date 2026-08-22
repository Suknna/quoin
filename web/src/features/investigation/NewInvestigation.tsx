import { useState } from 'react'
import { api, sourceLabel } from './api'
import { AttachmentsInput } from './attachments/AttachmentsInput'
import { useAttachments } from './attachments/useAttachments'
import { pasteExceedsThreshold } from './attachments/useAttachments'

// NewInvestigation is the first-message entry (UI-CHAT-002): the "新建"
// click only opens this local blank input — no empty Investigation is
// persisted. The first message (text and/or staged attachments) is
// accepted atomically with the Investigation, its sources and the
// Execution Attempt (DATA-INVEST-001). Entry points from alerts/analyses
// carry immutable source references via the URL
// (?occurrence=&initialAnalysis=).

export interface InvestigationSourceRef {
  type: 'occurrence' | 'initial_analysis'
  sourceId: string
}

interface NewInvestigationProps {
  sources: InvestigationSourceRef[]
  onCreated: (investigationId: string) => void
  onBack: () => void
}

export function NewInvestigation({ sources, onCreated, onBack }: NewInvestigationProps) {
  const [content, setContent] = useState('')
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const draft = useAttachments()

  async function submit() {
    const attachmentIds = draft.readyIds()
    if (content.trim() === '' && attachmentIds.length === 0) {
      setError('请先输入要调查的问题或添加附件。')
      return
    }
    if (draft.hasBlocking) {
      setError('附件还在上传中，请稍候或先移除失败的附件。')
      return
    }
    setPending(true)
    setError(null)
    try {
      const detail = await api.create(content, sources.map((source) => ({ type: source.type, sourceId: source.sourceId })), attachmentIds)
      onCreated(detail.id)
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : '暂时无法发起调查，请重试。')
      setPending(false)
    }
  }

  const canSubmit = (content.trim() !== '' || draft.readyIds().length > 0) && !draft.hasBlocking && !pending

  return (
    <div className="investigation-pane">
      <header className="investigation-header">
        <div>
          <p className="eyebrow">调查</p>
          <h2>新调查</h2>
        </div>
        <button className="secondary-button compact" onClick={onBack}>返回列表</button>
      </header>
      {sources.length > 0 && (
        <div className="investigation-sources" aria-label="调查来源">
          {sources.map((source) => (
			<span key={`${source.type}-${source.sourceId}`} className="source-chip">
				{sourceLabel(source.type)} #{source.sourceId}
			</span>
          ))}
        </div>
      )}
      <div className="new-investigation">
        <label className="chat-input-label" htmlFor="first-message">描述你要调查的问题</label>
        <AttachmentsInput draft={draft} />
        <textarea
          id="first-message"
          className="chat-input"
          value={content}
          autoFocus
          placeholder="例如：数据库实例最近频繁出现连接超时，帮我排查可能的原因。"
          onChange={(event) => setContent(event.target.value)}
          onPaste={(event) => {
            const text = event.clipboardData.getData('text')
            if (text !== '' && pasteExceedsThreshold(text)) {
              // One large paste converts to a previewable/removable .txt
              // attachment (UI-CHAT-007); a staging failure keeps the body
              // in the textarea (错误就近显示并保留正文).
              event.preventDefault()
              const before = content
              draft.addPastedText(text)
              // If staging rejected the body (oversize/NUL/UTF-8), the chip
              // shows the error and the textarea keeps the pasted text.
              queueMicrotask(() => {
                const failed = draft.items.some((item) => item.error)
                setContent(failed ? (before ? `${before}\n${text}` : text) : before)
              })
            }
          }}
          onKeyDown={(event) => {
            if (event.key === 'Enter' && (event.metaKey || event.ctrlKey)) void submit()
          }}
        />
        {error && <p className="field-error" role="alert">{error}</p>}
        <div className="chat-actions">
          <span className="inline-status">{pending ? '正在受理第一条消息…' : '发送后将原子创建调查并开始生成回复。'}</span>
          <button className="primary-button" disabled={!canSubmit} onClick={() => void submit()}>
            {pending ? '发送中…' : '发送'}
          </button>
        </div>
      </div>
    </div>
  )
}
