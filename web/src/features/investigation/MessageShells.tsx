import { useContext, useState } from 'react'
import {
  AttachmentPrimitive,
  MessagePartPrimitive,
  MessagePrimitive,
  useAuiState,
} from '@assistant-ui/react'
import { ToolCallTimeline, EvidenceLink } from './tools/ToolCallTimeline'
import { TurnContext, durableId, type AttachmentMeta } from './turnContext'
import { FeedbackControl } from '../feedback/FeedbackControl'

// MessageShells renders the per-message views with the T15 turn controls
// (UI-CHAT-005): Retry at a failed turn's user message, Undo under the
// latest user turn, and the read-only withdrawn presentation.

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
  const withdrawn = extras.withdrawnFor(messageId)
  const canUndo = extras.canUndoFor(messageId)
  const canRetry = extras.canRetryFor(messageId)
  return (
    <MessagePrimitive.Root className={`chat-message chat-message-user${withdrawn ? ' withdrawn' : ''}`}>
      <div className="chat-message-row">
        {canRetry && (
          <button
            type="button"
            className="message-retry"
            title="该轮回复未能生成，重试将用同一条消息重新生成"
            aria-label="重试这一轮"
            onClick={() => durable && extras.onRetry(durable)}
          >
            ↻
          </button>
        )}
        <MessagePrimitive.Content components={{ Text: TextPart }} />
      </div>
      <DurableAttachments metas={metas} />
      {withdrawn && <p className="withdrawn-badge">该回合已撤回（保留审计）</p>}
      {canUndo && (
        <button type="button" className="message-undo" onClick={() => durable && extras.onUndo(durable)}>
          撤回这一轮
        </button>
      )}
    </MessagePrimitive.Root>
  )
}

function AssistantMessageShell() {
  const messageId = useAuiState((state) => state.message.id)
  const extras = useContext(TurnContext)
  const [feedbackOpen, setFeedbackOpen] = useState(false)
  if (!extras) return null
  const withdrawn = extras.withdrawnFor(messageId)
  const attemptId = extras.attemptFor(messageId)
  const evidenceIds = extras.evidenceFor(messageId)
  const isActiveAttempt = attemptId !== undefined && attemptId === extras.activeAttemptId
  // The diagnosis affordances bind to the durable assistant message only
  // (UI-FEEDBACK-001/003): live in-flight turns and withdrawn branches
  // carry no actions.
  const durable = durableId(messageId)
  const canRecord = !withdrawn && durable !== null && extras.onOrganizeKnowledge !== undefined
  return (
    <MessagePrimitive.Root className={`chat-message chat-message-assistant${withdrawn ? ' withdrawn' : ''}`}>
      {withdrawn && <p className="withdrawn-badge">该回合已撤回（保留审计）</p>}
      {!withdrawn && <MessagePrimitive.Content components={{ Text: TextPart }} />}
      {!withdrawn && attemptId !== undefined && (
        <ToolCallTimeline investigationId={extras.investigationId} attemptId={attemptId} active={isActiveAttempt} />
      )}
      {!withdrawn && evidenceIds.length > 0 && (
        <div className="message-evidence" aria-label="本轮证据">
          {evidenceIds.map((evidenceId) => (
            <EvidenceLink key={evidenceId} evidenceId={evidenceId} />
          ))}
        </div>
      )}
      {canRecord && (
        <div className="message-diagnosis" aria-label="诊断操作">
          <button type="button" className="text-button" aria-expanded={feedbackOpen} onClick={() => setFeedbackOpen(!feedbackOpen)}>
            记录实际结果
          </button>
          <button type="button" className="text-button" onClick={() => durable && extras.onOrganizeKnowledge?.(durable)}>
            整理为知识
          </button>
        </div>
      )}
      {canRecord && feedbackOpen && durable && (
        <FeedbackControl target={{ type: 'investigation_message', id: durable }} />
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

function TextPart() {
  // smooth defaults to true on MessagePartPrimitive.Text and its segment
  // animation drops the head of a replaced whole-content text; the
  // streamed arrival itself is the progressive feedback, so disable it
  // explicitly.
  return <MessagePartPrimitive.Text component="p" className="chat-text" smooth={false} />
}

export { UserMessageShell, AssistantMessageShell, ComposerAttachmentChip }
