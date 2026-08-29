import { createContext } from 'react'
import type { MessageAttachmentSummary } from './api'

// turnContext owns the shared turn-control vocabulary between the thread
// composition and the message/surface views: the durable-locator helper,
// the restored-turn payload (UI-CHAT-005) and the context the message
// shells consume for their per-turn affordances.

// TurnRestore carries a withdrawn turn back into the input area after
// Undo; the parent keeps it across the thread rebuild.
export interface TurnRestore {
  content: string
  attachments: MessageAttachmentSummary[]
}

// AttachmentMeta carries the download facts of one staged attachment id
// (the durable message projection or the send response provides them).
export interface AttachmentMeta {
  artifactId: string
  originalFilename: string
  sizeBytes: number
  bodyExpired: boolean
}

export interface TurnExtras {
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
  withdrawnFor: (messageId: string) => boolean
  canUndoFor: (messageId: string) => boolean
  canRetryFor: (messageId: string) => boolean
  live: { attachments: AttachmentMeta[]; attemptId?: string } | null
  stopping: boolean
  controlError: string | null
  onStop: () => void
  onRetry: (messageId: string) => void
  onUndo: (messageId: string) => void
  // T27: the assistant message menu's 整理为知识 action (UI-FEEDBACK-003);
  // undefined keeps older surfaces compiling without the affordance.
  onOrganizeKnowledge?: (messageId: string) => void
}

export const TurnContext = createContext<TurnExtras | null>(null)

// durableId extracts the numeric locator from restored message ids
// (`quoin-m<id>`); live messages carry runtime-local ids.
export function durableId(messageId: string): string | null {
  if (!messageId.startsWith('quoin-m')) return null
  return messageId.slice('quoin-m'.length)
}
