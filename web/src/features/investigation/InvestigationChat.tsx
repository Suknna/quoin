import { useCallback, useEffect, useRef, useState } from 'react'
import { api, sourceLabel, type InvestigationDetail, type InvestigationMessage } from './api'
import { ChatThread } from './ChatThread'

// InvestigationChat is the durable conversation page: it loads the
// investigation detail and the message history, restores the thread from
// the persisted active branch, and re-attaches the stream when the head
// user message still owns an active attempt (HTTP-STREAM-003/006: leaving
// the page never cancelled the task).

interface InvestigationChatProps {
  investigationId: string
  onBack: () => void
}

export function InvestigationChat({ investigationId, onBack }: InvestigationChatProps) {
  const [detail, setDetail] = useState<InvestigationDetail | null>(null)
  const [messages, setMessages] = useState<InvestigationMessage[]>([])
  const [error, setError] = useState<string | null>(null)
  const pollTimer = useRef<number | null>(null)

  const reload = useCallback(async () => {
    try {
      const [nextDetail, messagePage] = await Promise.all([
        api.get(investigationId),
        api.listMessages(investigationId),
      ])
      const all: InvestigationMessage[] = [...messagePage.items]
      let cursor = messagePage.nextCursor
      while (cursor) {
        const page = await api.listMessages(investigationId, cursor)
        all.push(...page.items)
        cursor = page.nextCursor
      }
      setDetail(nextDetail)
      setMessages(all)
      setError(null)
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : '暂时无法读取调查，请重试。')
    }
  }, [investigationId])

  const onTurnFinished = useCallback(() => {
    void reload()
  }, [reload])

  useEffect(() => {
    let cancelled = false
    // Deferred so the first load never sets state synchronously inside
    // the effect body (react-hooks/set-state-in-effect).
    queueMicrotask(() => {
      if (!cancelled) void reload()
    })
    return () => {
      cancelled = true
    }
  }, [reload])

  // While the head user message still owns an active attempt the durable
  // state changes without any client trigger; poll the store so the page
  // converges even when the stream transport detached.
  useEffect(() => {
    if (!detail?.activeAttemptId) return
    pollTimer.current = window.setInterval(() => void reload(), 2000)
    return () => {
      if (pollTimer.current !== null) window.clearInterval(pollTimer.current)
    }
  }, [detail?.activeAttemptId, reload])

  if (error) {
    return (
      <div className="investigation-pane">
        <div className="error-summary" role="alert">
          <strong>暂时无法打开调查</strong>
          <span>{error}</span>
        </div>
        <button className="primary-button compact" onClick={onBack}>返回调查列表</button>
      </div>
    )
  }
  if (!detail) {
    return <div className="investigation-pane" aria-busy="true"><p className="inline-status">正在加载调查…</p></div>
  }

	const head = messages.length > 0 ? messages[messages.length - 1] : null
	// activeAttemptId exists only while the attempt is in an active state
	// (the projection reads the Queued/Assigned/Running/Cancelling set), so
	// its presence is the attach proof.
	const attach =
		detail.activeAttemptId && head?.role === 'user' && head.status === 'active'
			? head.id
			: undefined

  return (
    <div className="investigation-pane">
      <header className="investigation-header">
        <div>
          <p className="eyebrow">调查</p>
          <h2>{detail.displayTitle}</h2>
        </div>
        <button className="secondary-button compact" onClick={onBack}>返回列表</button>
      </header>
      {detail.sources.length > 0 && (
        <div className="investigation-sources" aria-label="调查来源">
			{detail.sources.map((source) => (
				<span key={source.id} className="source-chip">{sourceLabel(source.type)} #{source.sourceId}</span>
			))}
        </div>
      )}
      {attach && (
        <p className="inline-status" role="status">
          <span className="status-dot waiting" />回复正在生成中，页面会持续更新。
        </p>
      )}
      <ChatThread
        key={investigationId}
        investigationId={investigationId}
        messages={messages}
        headMessageId={detail.headMessageId ?? null}
        attachMessageId={attach}
        activeAttemptId={detail.activeAttemptId}
        onTurnFinished={onTurnFinished}
      />
    </div>
  )
}
