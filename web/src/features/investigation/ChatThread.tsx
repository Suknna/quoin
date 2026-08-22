import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import {
  AssistantRuntimeProvider,
  useLocalRuntime,
  type AttachmentAdapter,
  type ChatModelAdapter,
  type PendingAttachment,
  type ThreadMessageLike,
} from '@assistant-ui/react'
import { api, type InvestigationMessage, type MessageAttachmentSummary } from './api'
import { canOfferRetry, canOfferUndo, mergeAttachmentIds, type AttemptFacts } from './chatControls'
import { uploadAttachment, attachmentCommandId } from './attachments/api'
import { ChatSurface } from './ChatSurface'
import { streamInvestigationMessage } from './stream'
import { TurnContext, durableId, type AttachmentMeta, type TurnExtras, type TurnRestore } from './turnContext'

// ChatThread owns the assistant-ui thread and the frozen two-step command
// protocol: the composer send persists the message (text and/or staged
// attachments) through sendInvestigationMessage, then the stream attaches
// to the created attempt (HTTP-STREAM-004). T15 adds the explicit turn
// controls: the Stop fence, Retry on failed turns and Undo under the
// latest user turn, with the withdrawn branch kept read-only and the
// withdrawn turn restored into the input area.

export type { TurnRestore }

interface ChatThreadProps {
  investigationId: string
  messages: InvestigationMessage[]
  headMessageId: string | null
  // attachMessageId is set when the head user message still has an active
  // attempt on load: the runtime re-attaches without sending
  // (HTTP-STREAM-006 transport detach never cancels the task).
  attachMessageId?: string
  // activeAttemptId marks the head attempt as still running (the tool
  // timeline refreshes while the turn streams).
  activeAttemptId?: string
  attemptStates: Record<string, AttemptFacts>
  // restore carries a withdrawn turn back into the input area after Undo;
  // the parent keeps it across the thread rebuild.
  restore: TurnRestore | null
  onRestoreConsumed: () => void
  onUndo: (message: InvestigationMessage) => void
  onTurnFinished: () => void
}

// quoinAttachmentAdapter stages every selected/dropped/pasted file
// immediately; the staged id becomes the composer attachment id the send
// command references (removal never deletes the server object — staged
// uploads stay reusable, DATA-ATTACH-001).
const quoinAttachmentAdapter: AttachmentAdapter = {
  accept: '*',
  add: async ({ file }) => {
    const pending: PendingAttachment = {
      id: `staging-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 8)}`,
      type: 'document',
      name: file.name,
      contentType: file.type || 'text/plain',
      file,
      status: { type: 'running', reason: 'uploading', progress: 0.5 },
    }
    try {
      const summary = await uploadAttachment(file, attachmentCommandId())
      return {
        ...pending,
        id: summary.id,
        name: summary.originalFilename,
        contentType: summary.mediaType,
        status: { type: 'requires-action', reason: 'composer-send' },
      }
    } catch (reason) {
      // Deterministic staging failures stay on a removable chip in place
      // (UI-CHAT-007); the error rides the frozen incomplete status.
      return {
        ...pending,
        status: { type: 'incomplete', reason: 'error', message: reason instanceof Error ? reason.message : '附件上传失败，请重试。' },
      }
    }
  },
  remove: async () => {
    // The staged object stays server-side for reuse and later cleanup
    // (DATA-TX-015); removal only drops the composer reference.
  },
  send: async (attachment) => ({
    id: attachment.id,
    type: attachment.type,
    name: attachment.name,
    contentType: attachment.contentType,
    content: [],
    status: { type: 'complete' },
  }),
}

export function ChatThread({ investigationId, messages, headMessageId, attachMessageId, activeAttemptId, attemptStates, restore, onRestoreConsumed, onUndo, onTurnFinished }: ChatThreadProps) {
  const headRef = useRef<string | null>(headMessageId)
  useEffect(() => {
    // The parent reconciles the committed head after every turn; the next
    // send fences on the authoritative value.
    headRef.current = headMessageId
  }, [headMessageId])

  const [live, setLive] = useState<TurnExtras['live']>(null)
  const [stopping, setStopping] = useState(false)
  const [controlError, setControlError] = useState<string | null>(null)
  // Restored references ride the next send ahead of newly staged chips;
  // the ref keeps the memoized adapter free of stale closures.
  const [restoredAttachments, setRestoredAttachments] = useState<MessageAttachmentSummary[]>([])
  const restoredRef = useRef<MessageAttachmentSummary[]>([])
  const clearRestored = useCallback(() => {
    restoredRef.current = []
    setRestoredAttachments([])
  }, [])

  const initialMessages = useMemo<ThreadMessageLike[]>(
    () =>
      messages.map((message) => ({
        id: `quoin-m${message.id}`,
        role: message.role,
        content: [{ type: 'text', text: message.content }],
        ...(message.role === 'user' && message.attachments.length > 0
          ? {
              attachments: message.attachments.map((attachment) => ({
                id: attachment.id,
                type: 'document' as const,
                name: attachment.originalFilename,
                contentType: attachment.mediaType,
                content: [],
                status: { type: 'complete' as const },
              })),
            }
          : {}),
      })),
    [messages],
  )

  const byId = useMemo(() => {
    const map = new Map<string, InvestigationMessage>()
    for (const message of messages) map.set(message.id, message)
    return map
  }, [messages])

  const adapter = useMemo<ChatModelAdapter>(
    () => ({
      run: async function* ({ messages: threadMessages, runConfig, abortSignal }) {
        setControlError(null)
        // Re-attach path: the run was triggered for the persisted head
        // user message, not a new composer send.
        const attachId = (runConfig?.custom?.quoinAttach as string | undefined) ?? null
        let streamMessageId: string
        if (attachId === null) {
          const last = [...threadMessages].reverse().find((message) => message.role === 'user')
          if (!last) throw new Error('消息不能为空。')
          const text = last.content
            .filter((part): part is { type: 'text'; text: string } => part.type === 'text')
            .map((part) => part.text)
            .join('')
          const stagedIds = (last.attachments ?? [])
            .filter((attachment) => attachment.status.type === 'complete')
            .map((attachment) => attachment.id)
          const attachmentIds = mergeAttachmentIds(restoredRef.current, stagedIds)
          if (text.trim() === '' && attachmentIds.length === 0) throw new Error('消息不能为空。')
          const sent = await api.sendMessage(investigationId, text, headRef.current, attachmentIds)
          clearRestored()
          // The send response carries the durable attachment facts (and
          // the attempt id); they feed the live turn's chips until the
          // finished-turn remount renders the store projection.
          setLive({
            attachments: sent.attachments.map((attachment) => ({
              artifactId: attachment.artifactId,
              originalFilename: attachment.originalFilename,
              sizeBytes: attachment.sizeBytes,
              bodyExpired: attachment.bodyExpired,
            })),
            attemptId: sent.attemptId,
          })
          // The head fence stays on the durable projection: the commit
          // moves it to the assistant message and the parent reconciles
          // it through headMessageId — the user message id is never a
          // valid fence for the next send.
          streamMessageId = sent.id
        } else {
          streamMessageId = attachId
        }
        yield* streamInvestigationMessage(investigationId, streamMessageId, abortSignal)
      },
    }),
    [investigationId, clearRestored],
  )

  const runtime = useLocalRuntime(adapter, { initialMessages, adapters: { attachments: quoinAttachmentAdapter } })

  // A withdrawn turn restored by Undo seeds the composer once per thread
  // rebuild (the parent clears its copy after consumption).
  const seededRestore = useRef(false)
  useEffect(() => {
    if (seededRestore.current || !restore) return
    seededRestore.current = true
    runtime.thread.composer.setText(restore.content)
    restoredRef.current = restore.attachments
    setRestoredAttachments(restore.attachments)
    onRestoreConsumed()
  }, [restore, runtime, onRestoreConsumed])

  // Stop (UI-FORM-005): the domain fence is an explicit command; the local
  // reader keeps consuming until the server's terminal frame closes it.
  const stop = useCallback(async () => {
    const attemptId = live?.attemptId ?? activeAttemptId
    if (!attemptId || stopping) return
    setStopping(true)
    try {
      const page = await api.listAttempts(investigationId)
      const attempt = page.items.find((item) => item.id === attemptId)
      if (!attempt) throw new Error('执行记录不存在')
      await api.cancelAttempt(investigationId, attempt.id, attempt.rowVersion)
    } catch (reason) {
      // A deterministic conflict (the turn finished first) or a transient
      // failure: surface the reason and converge through a reload.
      setControlError(reason instanceof Error ? reason.message : '暂时无法停止，请重试。')
      onTurnFinished()
    } finally {
      setStopping(false)
    }
  }, [investigationId, live, activeAttemptId, stopping, onTurnFinished])

  // Retry re-answers the failed turn's user message through a NEW attempt
  // (DATA-INVEST-002); the stream attaches to the active attempt.
  const retry = useCallback(async (messageId: string) => {
    const message = byId.get(messageId)
    if (!message?.attemptId) return
    try {
      setControlError(null)
      const next = await api.retryAttempt(investigationId, message.attemptId)
      setLive((current) => ({ attachments: current?.attachments ?? [], attemptId: next.id }))
      const parentId = runtime.thread.getState().messages.at(-1)?.id ?? null
      runtime.thread.startRun({ parentId, runConfig: { custom: { quoinAttach: message.id } } })
    } catch (reason) {
      setControlError(reason instanceof Error ? reason.message : '暂时无法重试，请稍后重试。')
    }
  }, [investigationId, runtime, byId])

  // A finished turn that committed no durable assistant message (failed or
  // cancelled) leaves a local error bubble. Rebuild the thread in place
  // from the durable projection once nothing runs; the composer draft is
  // captured and re-seeded around the reset (UI-FORM-005: 等待期间保护
  // 已输入内容).
  useEffect(() => {
    if (activeAttemptId) return
    const state = runtime.thread.getState()
    if (state.messages.length <= messages.length) return
    const draft = runtime.thread.composer.getState().text
    runtime.thread.reset(initialMessages)
    if (draft !== '') runtime.thread.composer.setText(draft)
  }, [messages, activeAttemptId, runtime, initialMessages])

  const extras = useMemo<TurnExtras>(() => {
    const metaOf = (message: InvestigationMessage | undefined, attachmentId: string): AttachmentMeta | undefined =>
      message?.attachments.find((attachment) => attachment.id === attachmentId)
    let headAttemptId: string | undefined
    let headEvidenceIds: string[] = []
    for (let index = messages.length - 1; index >= 0; index -= 1) {
      const message = messages[index]
      if (!headAttemptId && message.attemptId) {
        headAttemptId = message.attemptId
      }
      if (message.role === 'assistant' && (message.evidenceIds ?? []).length > 0) {
        headEvidenceIds = message.evidenceIds ?? []
        break
      }
    }
    return {
      investigationId,
      activeAttemptId,
      headAttemptId,
      headEvidenceIds,
      live,
      stopping,
      controlError,
      onStop: () => void stop(),
      onRetry: (messageId) => void retry(messageId),
      onUndo: (messageId) => {
        const message = byId.get(messageId)
        if (message) onUndo(message)
      },
      attachmentMeta: (attachmentId) => {
        for (const message of messages) {
          const found = metaOf(message, attachmentId)
          if (found) return found
        }
        return live?.attachments.find((item) => item.artifactId === attachmentId)
      },
      durableAttachmentsFor: (messageId) =>
        (byId.get(messageId)?.attachments ?? []).map((attachment) => ({
          artifactId: attachment.artifactId,
          originalFilename: attachment.originalFilename,
          sizeBytes: attachment.sizeBytes,
          bodyExpired: attachment.bodyExpired,
        })),
      attemptFor: (messageId) => {
        const id = durableId(messageId)
        if (id) return byId.get(id)?.attemptId
        return live?.attemptId
      },
      evidenceFor: (messageId) => {
        const id = durableId(messageId)
        return id ? byId.get(id)?.evidenceIds ?? [] : []
      },
      withdrawnFor: (messageId) => {
        const id = durableId(messageId)
        return id ? byId.get(id)?.status === 'withdrawn' : false
      },
      canUndoFor: (messageId) => {
        const id = durableId(messageId)
        const message = id ? byId.get(id) : undefined
        return message ? canOfferUndo(messages, message, activeAttemptId) : false
      },
      canRetryFor: (messageId) => {
        const id = durableId(messageId)
        const message = id ? byId.get(id) : undefined
        return message ? canOfferRetry(attemptStates, message, activeAttemptId) : false
      },
    }
  }, [investigationId, activeAttemptId, live, messages, byId, attemptStates, stopping, controlError, stop, retry, onUndo])

  return (
    <AssistantRuntimeProvider runtime={runtime}>
      <TurnContext.Provider value={extras}>
        <ChatSurface
          runtime={runtime}
          attachMessageId={attachMessageId}
          restoredAttachments={restoredAttachments}
          onRestoredRemove={(attachmentId) => {
            const next = restoredRef.current.filter((item) => item.id !== attachmentId)
            restoredRef.current = next
            setRestoredAttachments(next)
          }}
          onTurnFinished={onTurnFinished}
        />
      </TurnContext.Provider>
    </AssistantRuntimeProvider>
  )
}
