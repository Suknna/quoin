import { useCallback, useEffect, useState } from 'react'
import { api, sourceLabel, type InvestigationDetail, type InvestigationMessage } from './api'
import { withdrawnRevision, type AttemptFacts } from './chatControls'
import { ChatThread, type TurnRestore } from './ChatThread'

// InvestigationChat is the durable conversation page: it loads the
// investigation detail, the message history and the attempt projection,
// restores the thread from the persisted active branch, and re-attaches
// the stream when the head user message still owns an active attempt
// (HTTP-STREAM-003/006: leaving the page never cancelled the task).
// T15: it also owns the Undo restore payload (the withdrawn turn returns
// to the input area across the thread rebuild) and the rebuild counter
// that drops local ghosts after failed/cancelled turns.

interface InvestigationChatProps {
  investigationId: string
  onBack: () => void
  onOpenCandidate?: (candidateId: string) => void
}

export function InvestigationChat({ investigationId, onBack, onOpenCandidate }: InvestigationChatProps) {
  const [detail, setDetail] = useState<InvestigationDetail | null>(null)
  const [messages, setMessages] = useState<InvestigationMessage[]>([])
  const [attemptStates, setAttemptStates] = useState<Record<string, AttemptFacts>>({})
  const [error, setError] = useState<string | null>(null)
  const [restore, setRestore] = useState<TurnRestore | null>(null)
  const [undoError, setUndoError] = useState<string | null>(null)

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
      const attempts = await api.listAttempts(investigationId)
      const states: Record<string, AttemptFacts> = {}
      for (const attempt of attempts.items) {
        states[attempt.id] = { state: attempt.state, rowVersion: attempt.rowVersion }
      }
      setDetail(nextDetail)
      setMessages(all)
      setAttemptStates(states)
      setError(null)
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : '暂时无法读取调查，请重试。')
    }
  }, [investigationId])

  const onTurnFinished = useCallback(() => {
    void reload()
  }, [reload])

  // Undo (DATA-INVEST-002): the server withdraws the turn and successors;
  // the withdrawn text and attachment references return to the input area
  // (UI-CHAT-005) and the thread rebuilds from the durable projection.
  // The restore payload is set only AFTER the reload commits: the thread
  // rebuild key derives from the withdrawn set, and setting it earlier
  // would let the outgoing thread instance consume the payload before the
  // rebuild mounts.
  const onUndo = useCallback(async (message: InvestigationMessage) => {
    if (!detail?.headMessageId) return
    setUndoError(null)
    try {
      await api.undo(investigationId, detail.headMessageId)
      await reload()
      setRestore({ content: message.content, attachments: message.attachments })
    } catch (reason) {
      setUndoError(reason instanceof Error ? reason.message : '暂时无法撤回，请刷新后重试。')
      await reload()
    }
  }, [investigationId, detail, reload])

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
    const timer = window.setInterval(() => void reload(), 2000)
    return () => {
      window.clearInterval(timer)
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
      {undoError && (
        <p className="chat-control-error" role="alert">{undoError}</p>
      )}
      {attach && (
        <p className="inline-status" role="status">
          <span className="status-dot waiting" />回复正在生成中，页面会持续更新。
        </p>
      )}
      <ChatThread
        key={`${investigationId}:w${withdrawnRevision(messages)}`}
        investigationId={investigationId}
        messages={messages}
        headMessageId={detail.headMessageId ?? null}
        attachMessageId={attach}
        activeAttemptId={detail.activeAttemptId}
        attemptStates={attemptStates}
        restore={restore}
        onRestoreConsumed={() => setRestore(null)}
        onUndo={(message) => void onUndo(message)}
        onTurnFinished={onTurnFinished}
        onOpenCandidate={onOpenCandidate}
      />
    </div>
  )
}
