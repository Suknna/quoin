import { createContext, useContext, useEffect, useMemo, useRef, useState } from 'react'
import {
  AssistantRuntimeProvider,
  AttachmentPrimitive,
  ComposerPrimitive,
  MessagePartPrimitive,
  MessagePrimitive,
  ThreadPrimitive,
  useAuiState,
  useLocalRuntime,
  type AssistantRuntime,
  type AttachmentAdapter,
  type ChatModelAdapter,
  type PendingAttachment,
  type ThreadMessageLike,
} from '@assistant-ui/react'
import { api, type InvestigationMessage } from './api'
import { streamInvestigationMessage } from './stream'
import { uploadAttachment, attachmentCommandId } from './attachments/api'
import { pasteExceedsThreshold } from './attachments/useAttachments'
import { ToolCallTimeline, EvidenceLink } from './tools/ToolCallTimeline'
import './investigation.css'

// ChatThread owns the assistant-ui thread and the frozen two-step command
// protocol: the composer send persists the message (text and/or staged
// attachments) through sendInvestigationMessage, then the stream attaches
// to the created attempt (HTTP-STREAM-004). Attachments ride the native
// composer attachment machinery: the adapter stages each file immediately
// (errors surface on the chip in place) and the send command references
// only the durable staging ids (UI-CHAT-006/007).

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
  onTurnFinished: () => void
}

// AttachmentMeta carries the download facts of one staged attachment id
// (the durable message projection or the send response provides them).
interface AttachmentMeta {
  artifactId: string
  originalFilename: string
  sizeBytes: number
  bodyExpired: boolean
}

interface TurnExtras {
  investigationId: string
  activeAttemptId?: string
  // The head turn's attempt: the thread-tail timeline renders from it so
  // the finished turn's tool cards survive remounts deterministically.
  headAttemptId?: string
  headEvidenceIds: string[]
  attachmentMeta: (attachmentId: string) => AttachmentMeta | undefined
  durableAttachmentsFor: (messageId: string) => AttachmentMeta[]
  attemptFor: (messageId: string) => string | undefined
  evidenceFor: (messageId: string) => string[]
  live: { attachments: AttachmentMeta[]; attemptId?: string } | null
}

const TurnContext = createContext<TurnExtras | null>(null)

// durableId extracts the numeric locator from restored message ids
// (`quoin-m<id>`); live messages carry runtime-local ids.
function durableId(messageId: string): string | null {
  if (!messageId.startsWith('quoin-m')) return null
  return messageId.slice('quoin-m'.length)
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

export function ChatThread({ investigationId, messages, headMessageId, attachMessageId, activeAttemptId, onTurnFinished }: ChatThreadProps) {
  const headRef = useRef<string | null>(headMessageId)
  useEffect(() => {
    // The parent reconciles the committed head after every turn; the next
    // send fences on the authoritative value.
    headRef.current = headMessageId
  }, [headMessageId])

  const [live, setLive] = useState<TurnExtras['live']>(null)

  const initialMessages = useMemo<ThreadMessageLike[]>(
    () =>
      messages
        .filter((message) => message.status === 'active')
        .map((message) => ({
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
          const attachmentIds = (last.attachments ?? [])
            .filter((attachment) => attachment.status.type === 'complete')
            .map((attachment) => attachment.id)
          if (text.trim() === '' && attachmentIds.length === 0) throw new Error('消息不能为空。')
          const sent = await api.sendMessage(investigationId, text, headRef.current, attachmentIds)
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
    [investigationId],
  )

  const runtime = useLocalRuntime(adapter, { initialMessages, adapters: { attachments: quoinAttachmentAdapter } })

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
    }
  }, [investigationId, activeAttemptId, live, messages, byId])

  // The finished turn reconciles through ChatSurface's single subscription
  // below (running -> stopped fires onTurnFinished); the live snapshot
  // STAYS until the next send (the just-sent user message keeps its
  // attachment chips — the runtime never re-reads durable state for it,
  // and remounting would wipe the composer).

  return (
    <AssistantRuntimeProvider runtime={runtime}>
      <TurnContext.Provider value={extras}>
        <ChatSurface
          runtime={runtime}
          attachMessageId={attachMessageId}
          onTurnFinished={onTurnFinished}
        />
      </TurnContext.Provider>
    </AssistantRuntimeProvider>
  )
}

function ChatSurface({ runtime, attachMessageId, onTurnFinished }: {
  runtime: AssistantRuntime
  attachMessageId?: string
  onTurnFinished: () => void
}) {
  const extras = useContext(TurnContext)
  const [showNewReply, setShowNewReply] = useState(false)
  const [streaming, setStreaming] = useState(false)
  const scrollerRef = useRef<HTMLDivElement | null>(null)
  const followRef = useRef(true)

  // Re-attach on mount: the persisted head user message still owns an
  // active attempt, so the stream continues exactly where the last
  // observer detached (the run is keyed once per mount).
  const attached = useRef(false)
  useEffect(() => {
    if (attachMessageId && !attached.current) {
      attached.current = true
      // startRun rewinds to the parent: the parent is the thread's
      // last message (a root parent would wipe the restored history
      // before the run appends).
      const threadState = runtime.thread.getState()
      const parentId = threadState.messages.at(-1)?.id ?? null
      runtime.thread.startRun({ parentId, runConfig: { custom: { quoinAttach: attachMessageId } } })
    }
  }, [attachMessageId, runtime])

  // A finished turn reconciles the durable messages and the head fence
  // (HTTP-STREAM-003: the committed message is the authority); the
  // callback fires only on the running -> stopped transition.
  useEffect(() => {
    let wasRunning = false
    const unsubscribe = runtime.thread.subscribe(() => {
      const next = runtime.thread.getState()
      setStreaming(next.isRunning)
      if (wasRunning && !next.isRunning) onTurnFinished()
      wasRunning = next.isRunning
    })
    return unsubscribe
  }, [runtime, onTurnFinished])

  // Each new run starts in follow mode unless the reader detaches.
  useEffect(() => {
    if (!streaming) return
    setShowNewReply(!followRef.current)
  }, [streaming])

  return (
    <div
      className="chat-scroll-capture"
      onScrollCapture={(event) => {
        // The wrapper contains nothing but the thread: any scroll
        // event captured here is the viewport's own (scroll events
        // do not bubble, capture-phase only).
        const target = event.target as HTMLDivElement
        scrollerRef.current = target
        const distance = target.scrollHeight - target.scrollTop - target.clientHeight
        followRef.current = distance < 80
        // A detach during a stream surfaces the entry; it stays until
        // the reader scrolls back down (UI-CHAT-003).
        if (streaming && !followRef.current) setShowNewReply(true)
      }}
    >
      <ThreadPrimitive.Root className="chat-thread">
        <ThreadPrimitive.Viewport className="chat-viewport">
          <ThreadPrimitive.Empty>
            <div className="chat-empty">
              <p>发送第一条消息，开始生成回复。</p>
            </div>
          </ThreadPrimitive.Empty>
          <ThreadPrimitive.Messages
            components={{
              UserMessage: UserMessageShell,
              AssistantMessage: AssistantMessageShell,
            }}
          />
          {extras?.headAttemptId && (
            <div className="tool-call-timeline" aria-label="本轮工具调用">
              <ToolCallTimeline investigationId={extras.investigationId} attemptId={extras.headAttemptId} active={extras.headAttemptId === extras.activeAttemptId} />
              {extras.headEvidenceIds.length > 0 && (
                <div className="message-evidence" aria-label="本轮证据">
                  {extras.headEvidenceIds.map((evidenceId) => (
                    <EvidenceLink key={evidenceId} evidenceId={evidenceId} />
                  ))}
                </div>
              )}
            </div>
          )}
          {showNewReply && (
            <button
              className="chat-new-reply"
              onClick={() => {
                const scroller = scrollerRef.current
                scroller?.scrollTo({ top: scroller.scrollHeight, behavior: 'smooth' })
                followRef.current = true
                setShowNewReply(false)
              }}
            >
              查看新回复
            </button>
          )}
        </ThreadPrimitive.Viewport>
        <ComposerPrimitive.Root className="chat-composer">
          <ComposerPrimitive.Attachments>
            {({ attachment }) => <ComposerAttachmentChip key={attachment.id} name={attachment.name} status={attachment.status} />}
          </ComposerPrimitive.Attachments>
          <div className="chat-composer-row">
            <ComposerPrimitive.AddAttachment className="attachment-pick">附件</ComposerPrimitive.AddAttachment>
            <ComposerPrimitive.Input
              className="chat-composer-input"
              placeholder="输入消息…（Enter 发送）"
              autoFocus
              onPaste={(event) => {
                const text = event.clipboardData.getData('text')
                if (text !== '' && pasteExceedsThreshold(text)) {
                  // One large paste becomes a previewable/removable .txt
                  // attachment instead of flooding the textarea
                  // (UI-CHAT-007); typed input never converts. A staging
                  // failure (size/NUL/UTF-8) keeps the pasted body in the
                  // textarea instead of dropping it (UI-CHAT-007: 错误就近
                  // 显示并保留正文).
                  event.preventDefault()
                  const stamp = new Date()
                  const pad = (value: number) => value.toString().padStart(2, '0')
                  const filename = `粘贴-${stamp.getFullYear()}${pad(stamp.getMonth() + 1)}${pad(stamp.getDate())}-${pad(stamp.getHours())}${pad(stamp.getMinutes())}${pad(stamp.getSeconds())}.txt`
                  runtime.thread.composer.addAttachment(new File([text], filename, { type: 'text/plain' })).then(
                    () => undefined,
                    () => {
                      // The adapter already surfaced the chip error in
                      // place; restore the raw body to the composer so
                      // nothing is lost.
                      const current = runtime.thread.composer.getState().text
                      runtime.thread.composer.setText(current ? `${current}\n${text}` : text)
                    },
                  )
                }
              }}
            />
            <ComposerPrimitive.Send className="chat-send">发送</ComposerPrimitive.Send>
          </div>
        </ComposerPrimitive.Root>
      </ThreadPrimitive.Root>
    </div>
  )
}

// ComposerAttachmentChip renders one staged/drafting attachment in place
// with its staging state; removal uses the frozen primitive so keyboard
// and focus behavior follow the library's tested path.
function ComposerAttachmentChip({ name, status }: { name: string; status: { type: string; message?: string } }) {
  const uploading = status.type === 'running'
  const failed = status.type === 'incomplete'
  return (
    <span className={`attachment-chip${failed ? ' error' : ''}`}>
      <span className="attachment-chip-icon" aria-hidden="true">▤</span>
      <span className="attachment-chip-name">
        <AttachmentPrimitive.Name />
      </span>
      {uploading && <span className="attachment-chip-status">上传中…</span>}
      {failed && (
        <span className="attachment-chip-error" role="alert">
          {status.message ?? '附件上传失败，请重试。'}
        </span>
      )}
      <AttachmentPrimitive.Remove aria-label={`移除附件 ${name}`}>
        ×
      </AttachmentPrimitive.Remove>
    </span>
  )
}

function UserMessageShell() {
  // The scoped aui store resolves the current message inside the message
  // subtree (the 0.15.x surface has no useMessage hook).
  const messageId = useAuiState((state) => state.message.id)
  const extras = useContext(TurnContext)
  if (!extras) return null
  // Attachment facts render from OUR durable projection, not the runtime
  // message: the local runtime drops attachment parts when restoring
  // initialMessages, so the composer-live turn and restored turns both
  // resolve here (live in-flight snapshot vs the store projection).
  const durable = durableId(messageId)
  const metas = durable !== null ? extras.durableAttachmentsFor(durable) : extras.live?.attachments ?? []
  return (
    <MessagePrimitive.Root className="chat-message chat-message-user">
      <MessagePrimitive.Content components={{ Text: TextPart }} />
      <DurableAttachments metas={metas} />
    </MessagePrimitive.Root>
  )
}

function AssistantMessageShell() {
  const messageId = useAuiState((state) => state.message.id)
  const extras = useContext(TurnContext)
  if (!extras) return null
  const attemptId = extras.attemptFor(messageId)
  const evidenceIds = extras.evidenceFor(messageId)
  const isActiveAttempt = attemptId !== undefined && attemptId === extras.activeAttemptId
  return (
    <MessagePrimitive.Root className="chat-message chat-message-assistant">
      <MessagePrimitive.Content components={{ Text: TextPart }} />
      {attemptId !== undefined && (
        <ToolCallTimeline investigationId={extras.investigationId} attemptId={attemptId} active={isActiveAttempt} />
      )}
      {evidenceIds.length > 0 && (
        <div className="message-evidence" aria-label="本轮证据">
          {evidenceIds.map((evidenceId) => (
            <EvidenceLink key={evidenceId} evidenceId={evidenceId} />
          ))}
        </div>
      )}
    </MessagePrimitive.Root>
  )
}

// DurableAttachments renders the restored turn's ordered summaries with
// the >3 folding and the authorized artifact download (UI-CHAT-006).
function DurableAttachments({ metas }: { metas: AttachmentMeta[] }) {
  const [expanded, setExpanded] = useState(false)
  if (metas.length === 0) return null
  const collapsed = metas.length > 3 && !expanded
  const visible = collapsed ? metas.slice(0, 3) : metas
  return (
    <div className="message-attachments" aria-label="消息附件">
      {visible.map((meta) => (
        <a
          key={meta.artifactId}
          className="message-attachment"
          href={`/api/v1/artifacts/${meta.artifactId}/content`}
          download
          title={meta.bodyExpired ? '附件正文已过期' : '下载附件正文'}
        >
          <span className="attachment-chip-icon" aria-hidden="true">▤</span>
          <span className="attachment-chip-name">{meta.originalFilename}</span>
        </a>
      ))}
      {collapsed && (
        <button type="button" className="attachment-more" onClick={() => setExpanded(true)}>
          共 {metas.length} 份附件
        </button>
      )}
    </div>
  )
}

function TextPart() {
  // smooth defaults to true on MessagePartPrimitive.Text and its segment
  // animation drops the head of a replaced whole-content text; the
  // streamed arrival itself is the progressive feedback, so disable it
  // explicitly.
  return <MessagePartPrimitive.Text component="p" className="chat-text" smooth={false} />
}
