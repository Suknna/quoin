// Attempt tool-call projection API (T14): the durable tool-call timeline
// of one attempt pages through GET listAttemptToolCalls; Evidence and
// Artifact details reuse the frozen reading-layer endpoints the analysis
// module already consumes.

export interface ToolCallItem {
  id: string
  attemptId: string
  modelCallId: string
  callSeq: number
  toolIndex: number
  providerToolCallId: string
  toolName: string
  toolVersion: string
  arguments: Record<string, unknown>
  executionMode: string
  failureMode: string
  status: 'pending' | 'running' | 'succeeded' | 'failed' | 'cancelled'
  rowVersion: number
  result?: Record<string, unknown>
  resultArtifactId?: string
  errorDetail?: string
  startedAt?: string
  endedAt?: string
  createdAt: string
}

export interface ToolCallPage {
  items: ToolCallItem[]
  nextCursor?: string
}

async function read<T>(path: string): Promise<T> {
  const response = await fetch(path, { credentials: 'include' })
  if (!response.ok) throw new Error('工具调用记录读取失败。')
  return (await response.json()) as T
}

// listToolCalls drains the full paged timeline of one attempt (bounded by
// the tool-call count of a single turn).
export async function listToolCalls(investigationId: string, attemptId: string): Promise<ToolCallItem[]> {
  const all: ToolCallItem[] = []
  let cursor: string | undefined
  do {
    const page = await read<ToolCallPage>(
      `/api/v1/investigations/${investigationId}/attempts/${attemptId}/tool-calls${cursor ? `?cursor=${encodeURIComponent(cursor)}` : ''}`,
    )
    all.push(...page.items)
    cursor = page.nextCursor
  } while (cursor)
  return all
}

export { fetchEvidence, artifactDownloadURL, toolNameLabel, type EvidenceDetail } from '../../analysis/tool-details/api'
