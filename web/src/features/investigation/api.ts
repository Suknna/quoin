export interface InvestigationSourceSummary {
  id: string
  type: 'occurrence' | 'initial_analysis' | 'evidence' | 'inspection_report'
  sourceId: string
  linkedBy?: string
  linkedAt: string
}

export interface InvestigationSummary {
  id: string
  displayTitle: string
  lastActivityAt: string
  createdAt: string
  createdBy: string
  headMessageId?: string
  activeAttemptId?: string
}

export interface InvestigationDetail extends InvestigationSummary {
  messageCount: number
  attemptCount: number
  sources: InvestigationSourceSummary[]
}

export interface MessageAttachmentSummary {
  id: string
  artifactId: string
  originalFilename: string
  mediaType: string
  sizeBytes: number
  digest: string
  bodyExpired: boolean
  createdAt: string
}

export interface InvestigationMessage {
  id: string
  seq: number
  role: 'user' | 'assistant'
  status: 'active' | 'withdrawn'
  content: string
  parentMessageId?: string
  attachments: MessageAttachmentSummary[]
  attemptId?: string
  evidenceIds: string[] | null
  createdAt: string
}

export interface Page<T> {
	items: T[]
	nextCursor?: string
}

export interface StreamRequest {
	protocol: 'ui-message-stream'
}

// sourceLabel is the one display-name mapping for provenance types
// (UI-CHAT: shared by the chat page and the new-investigation entry).
export function sourceLabel(type: string): string {
	switch (type) {
		case 'occurrence': return '告警'
		case 'initial_analysis': return '初步分析'
		case 'evidence': return '证据'
		case 'inspection_report': return '巡检报告'
		default: return type
	}
}

export function commandId(): string {
  const raw = crypto.getRandomValues(new Uint8Array(18))
  return Array.from(raw, (byte) => byte.toString(16).padStart(2, '0')).join('')
}

async function read<T>(path: string): Promise<T> {
  const response = await fetch(path, { credentials: 'include' })
  if (!response.ok) {
    let message = '暂时无法读取调查数据，请重试。'
    try {
      const problem = (await response.json()) as { message?: string }
      if (problem.message) message = problem.message
    } catch {
      // The ordinary-language fallback stays authoritative.
    }
    throw new Error(message)
  }
  return (await response.json()) as T
}

async function send<T>(path: string, body: unknown): Promise<T> {
  const response = await fetch(path, {
    method: 'POST', credentials: 'include',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  })
  if (!response.ok) {
    let message = '暂时无法完成操作，请重试。'
    try {
      const problem = (await response.json()) as { message?: string }
      if (problem.message) message = problem.message
    } catch {
      // The ordinary-language fallback stays authoritative.
    }
    throw new Error(message)
  }
  return (await response.json()) as T
}

export const api = {
  list: (cursor?: string) =>
    read<Page<InvestigationSummary>>(`/api/v1/investigations${cursor ? `?cursor=${encodeURIComponent(cursor)}` : ''}`),
  get: (investigationId: string) => read<InvestigationDetail>(`/api/v1/investigations/${investigationId}`),
  listMessages: (investigationId: string, cursor?: string) =>
    read<Page<InvestigationMessage>>(`/api/v1/investigations/${investigationId}/messages${cursor ? `?cursor=${encodeURIComponent(cursor)}` : ''}`),
  create: (content: string, sources: Array<{ type: string; sourceId: string }>, attachmentIds: string[]) =>
    send<InvestigationDetail>('/api/v1/investigations', { clientCommandId: commandId(), content, sources, attachmentIds }),
	sendMessage: (investigationId: string, content: string, expectedHeadMessageId: string | null, attachmentIds: string[]) =>
		send<InvestigationMessage>(`/api/v1/investigations/${investigationId}/messages`, {
			clientCommandId: commandId(), content, expectedHeadMessageId, attachmentIds,
		}),
}
