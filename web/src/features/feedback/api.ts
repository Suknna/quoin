// Diagnosis feedback API (T27, DATA-KNOWLEDGE-008): the append-only
// timeline bound to one immutable diagnosis output.

export type FeedbackTargetType = 'initial_analysis_output' | 'inspection_report' | 'investigation_message'

export type FeedbackValue = 'adopted' | 'executed' | 'verified_effective' | 'rejected'

export interface FeedbackEvent {
  id: string
  targetType: FeedbackTargetType
  targetId: string
  value: FeedbackValue
  note?: string
  createdBy?: string
  createdAt: string
}

export interface FeedbackTimeline {
  latestValue?: FeedbackValue
  items: FeedbackEvent[]
  nextCursor?: string
}

export interface FeedbackTarget {
  type: FeedbackTargetType
  id: string
}

export const feedbackValueLabels: Record<FeedbackValue, string> = {
  adopted: '已采纳',
  executed: '已执行',
  verified_effective: '验证有效',
  rejected: '不采纳',
}

export function feedbackCommandId(): string {
  const raw = crypto.getRandomValues(new Uint8Array(18))
  return Array.from(raw, (byte) => byte.toString(16).padStart(2, '0')).join('')
}

async function problemMessage(response: Response): Promise<string> {
  try {
    const problem = (await response.json()) as { message?: string }
    if (problem.message) return problem.message
  } catch {
    // Keep the ordinary-language fallback.
  }
  return '暂时无法完成操作，请重试。'
}

export async function appendFeedback(target: FeedbackTarget, value: FeedbackValue, note: string): Promise<FeedbackEvent> {
  const response = await fetch('/api/v1/knowledge/feedback', {
    method: 'POST', credentials: 'include',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ clientCommandId: feedbackCommandId(), targetType: target.type, targetId: target.id, value, note }),
  })
  if (!response.ok) throw new Error(await problemMessage(response))
  return (await response.json()) as FeedbackEvent
}

export async function fetchFeedback(target: FeedbackTarget): Promise<FeedbackTimeline> {
  const query = `targetType=${target.type}&targetId=${encodeURIComponent(target.id)}&limit=50`
  const response = await fetch(`/api/v1/knowledge/feedback?${query}`, { credentials: 'include' })
  if (!response.ok) throw new Error(await problemMessage(response))
  return (await response.json()) as FeedbackTimeline
}
