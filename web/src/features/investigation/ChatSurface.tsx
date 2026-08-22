import { useContext, useEffect, useRef, useState } from 'react'
import {
  ComposerPrimitive,
  ThreadPrimitive,
  type AssistantRuntime,
} from '@assistant-ui/react'
import type { MessageAttachmentSummary } from './api'
import { pasteExceedsThreshold } from './attachments/useAttachments'
import { UserMessageShell, AssistantMessageShell, ComposerAttachmentChip } from './MessageShells'
import { ToolCallTimeline, EvidenceLink } from './tools/ToolCallTimeline'
import { TurnContext } from './turnContext'
import './investigation.css'

// ChatSurface renders the thread viewport and the composer with the T15
// turn controls: the in-place Stop button (UI-FORM-005: the control goes
// non-repeatable until the fence confirms), the restored-attachment chips
// and the transient control error line.

interface ChatSurfaceProps {
  runtime: AssistantRuntime
  attachMessageId?: string
  restoredAttachments: MessageAttachmentSummary[]
  onRestoredRemove: (attachmentId: string) => void
  onTurnFinished: () => void
}

export function ChatSurface({ runtime, attachMessageId, restoredAttachments, onRestoredRemove, onTurnFinished }: ChatSurfaceProps) {
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

  // The stop control appears in place of the send button once the accepted
  // attempt id is known (UI-CHAT-005: requestPending keeps Send disabled
  // and never shows a cancellable attempt).
  const stopAttemptId = extras?.live?.attemptId ?? extras?.activeAttemptId

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
        {extras?.controlError && (
          <p className="chat-control-error" role="alert">{extras.controlError}</p>
        )}
        <ComposerPrimitive.Root className="chat-composer">
          {restoredAttachments.length > 0 && (
            <div className="restored-attachments" aria-label="已撤回回合恢复的附件">
              {restoredAttachments.map((attachment) => (
                <span key={attachment.id} className="attachment-chip restored">
                  <span className="attachment-chip-icon" aria-hidden="true">▤</span>
                  <span className="attachment-chip-name">{attachment.originalFilename}</span>
                  <button type="button" className="attachment-chip-remove" aria-label={`移除恢复的附件 ${attachment.originalFilename}`} onClick={() => onRestoredRemove(attachment.id)}>×</button>
                </span>
              ))}
            </div>
          )}
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
            {streaming && stopAttemptId && extras ? (
              <button
                type="button"
                className={`chat-stop${extras.stopping ? ' stopping' : ''}`}
                onClick={extras.onStop}
                disabled={extras.stopping}
              >
                {extras.stopping ? '正在停止…' : '停止'}
              </button>
            ) : (
              <ComposerPrimitive.Send className="chat-send">发送</ComposerPrimitive.Send>
            )}
          </div>
        </ComposerPrimitive.Root>
      </ThreadPrimitive.Root>
    </div>
  )
}
