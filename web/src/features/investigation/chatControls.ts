import type { InvestigationMessage, MessageAttachmentSummary, Page, InvestigationAttempt } from './api'

// chatControls owns the pure UI decisions of the T15 turn controls
// (UI-CHAT-005): which turn offers Undo, which failed turn offers Retry,
// how restored attachments merge with newly staged ones, and when the
// local thread must be rebuilt from the durable projection. The functions
// stay free of React so the rules are unit-testable.

export interface AttemptFacts {
  state: InvestigationAttempt['state']
  rowVersion: number
}

/** Latest active user message of the branch (the only Undo candidate). */
export function latestActiveUserMessage(messages: InvestigationMessage[]): InvestigationMessage | null {
  for (let index = messages.length - 1; index >= 0; index -= 1) {
    const message = messages[index]
    if (message.status !== 'active') continue
    if (message.role === 'user') return message
    // withdrawn messages never own the branch; keep scanning past assistants
  }
  return null
}

/** Undo is offered under the latest sent user turn while nothing runs. */
export function canOfferUndo(
  messages: InvestigationMessage[],
  message: InvestigationMessage,
  activeAttemptId?: string,
): boolean {
  if (activeAttemptId) return false
  if (message.status !== 'active' || message.role !== 'user') return false
  return latestActiveUserMessage(messages)?.id === message.id
}

/** Retry is offered at a user message whose attempt sealed Failed. */
export function canOfferRetry(
  attemptStates: Record<string, AttemptFacts>,
  message: InvestigationMessage,
  activeAttemptId?: string,
): boolean {
  if (activeAttemptId) return false
  if (message.status !== 'active' || message.role !== 'user' || !message.attemptId) return false
  return attemptStates[message.attemptId]?.state === 'Failed'
}

/**
 * Undo restores the withdrawn turn into the input area (UI-CHAT-005):
 * previously uploaded attachments ride along as restored references ahead
 * of newly staged chips; duplicates collapse to the first occurrence.
 */
export function mergeAttachmentIds(restored: MessageAttachmentSummary[], stagedIds: string[]): string[] {
  const merged: string[] = []
  const seen = new Set<string>()
  for (const id of [...restored.map((attachment) => attachment.id), ...stagedIds]) {
    if (seen.has(id)) continue
    seen.add(id)
    merged.push(id)
  }
  return merged
}

/**
 * The thread rebuild key: it changes exactly when the withdrawn set grows
 * (an Undo committed) so the local runtime is rebuilt from the durable
 * projection — composer drafts survive ordinary turns.
 */
export function withdrawnRevision(messages: InvestigationMessage[]): number {
  return messages.filter((message) => message.status === 'withdrawn').length
}

/** One page helper shared by the attempts projection loader. */
export function collectPages<T>(pages: Page<T>[]): T[] {
  return pages.flatMap((page) => page.items)
}
