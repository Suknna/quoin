// Knowledge candidate and knowledge API (T27): create-or-return from the
// three diagnosis sources, revisioned draft edits, the human confirmation
// boundary, and the browse/version projections.

export type CandidateState = 'AwaitingConfirmation' | 'Confirmed' | 'Excluded' | 'Superseded' | 'SourceInvalid'

export type CandidateSourceType = 'initial_analysis_output' | 'inspection_report' | 'investigation_message' | 'source_material' | 'knowledge_version'

export interface CandidateSummary {
  id: string
  sourceType: CandidateSourceType
  sourceId: string
  state: CandidateState
  rowVersion: number
  generation: number
  draftRevision: number
  draftTitle?: string
  draftBody?: string
  draftScope?: Record<string, unknown>
  targetKnowledgeId?: string
  confirmedKnowledgeId?: string
}

export interface CandidateSuggestionSource {
  type: CandidateSourceType
  id: string
  modelId?: string
  createdAt?: string
  locator?: Record<string, number | string>
}

export interface CandidateSuggestion {
  v: number
  source: CandidateSuggestionSource
  title: string
  body: string
}

export interface CandidateDetail extends CandidateSummary {
  originalSuggestion: CandidateSuggestion
}

export interface KnowledgeSummary {
  id: string
  title: string
  currentVersionId: string
  currentVersionSeq: number
  eligible: boolean
  rowVersion: number
}

export interface KnowledgeDetail extends KnowledgeSummary {
  versionCount: number
}

export interface KnowledgeVersionSummary {
  id: string
  versionSeq: number
  title: string
  sourceCandidateId: string
  embeddingState: string
  createdAt: string
  eligible: boolean
  retrievalStateRowVersion: number
}

export interface KnowledgeVersionDetail {
  id: string
  versionSeq: number
  title: string
  body: string
  scope?: Record<string, unknown>
  sourceCandidateId: string
  createdAt: string
  eligible: boolean
  retrievalStateRowVersion: number
  embeddingState: string
  exitedAt?: string
  exitReason?: string
}

export interface Page<T> {
  items: T[]
  nextCursor?: string
}

export interface ConflictInfo {
  code: string
  currentRevision?: number
  currentRowVersion?: number
  state?: string
}

export class CommandConflictError extends Error {
  constructor(readonly conflict: ConflictInfo | null) {
    super(conflict?.currentRevision !== undefined
      ? '草稿已被更新，请基于最新版本修改。'
      : '候选状态已变化，请刷新后重试。')
  }
}

export const candidateStateLabels: Record<CandidateState, string> = {
  AwaitingConfirmation: '待确认',
  Confirmed: '已确认',
  Excluded: '已排除',
  Superseded: '已取代',
  SourceInvalid: '来源无效',
}

export const candidateSourceLabels: Record<CandidateSourceType, string> = {
  initial_analysis_output: '初步分析',
  inspection_report: '巡检报告',
  investigation_message: '调查消息',
  source_material: '导入原文',
  knowledge_version: '知识修订',
}

export function knowledgeCommandId(): string {
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

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const response = await fetch(path, { credentials: 'include', ...init })
  if (!response.ok) {
    if (response.status === 409) {
      let conflict: ConflictInfo | null = null
      try {
        const problem = (await response.json()) as { conflict?: ConflictInfo }
        conflict = problem.conflict ?? null
      } catch {
        conflict = null
      }
      throw new CommandConflictError(conflict)
    }
    throw new Error(await problemMessage(response))
  }
  return (await response.json()) as T
}

function commandBody(extra: Record<string, unknown>): string {
  return JSON.stringify({ clientCommandId: knowledgeCommandId(), ...extra })
}

export const api = {
  createAnalysisCandidate: (occurrenceId: string, analysisId: string) =>
    request<CandidateSummary>(`/api/v1/alerts/${occurrenceId}/analyses/${analysisId}/knowledge-candidates`, {
      method: 'POST', headers: { 'Content-Type': 'application/json' }, body: commandBody({}),
    }),
  createMessageCandidate: (investigationId: string, messageId: string) =>
    request<CandidateSummary>(`/api/v1/investigations/${investigationId}/knowledge-candidates`, {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: commandBody({ sourceType: 'investigation_message', sourceId: messageId }),
    }),
  createReportCandidate: (runId: string, reportVersion: number) =>
    request<CandidateSummary>(`/api/v1/inspections/runs/${runId}/reports/${reportVersion}/knowledge-candidates`, {
      method: 'POST', headers: { 'Content-Type': 'application/json' }, body: commandBody({}),
    }),
  listCandidates: (query = '') =>
    request<Page<CandidateSummary>>(`/api/v1/knowledge/candidates${query}`),
  getCandidate: (candidateId: string) =>
    request<CandidateDetail>(`/api/v1/knowledge/candidates/${candidateId}`),
  editDraft: (candidateId: string, expectedRevision: number, changes: { title?: string; body?: string; scope?: Record<string, unknown> }) =>
    request<CandidateSummary>(`/api/v1/knowledge/candidates/${candidateId}`, {
      method: 'PATCH', headers: { 'Content-Type': 'application/json' },
      body: commandBody({ expectedRevision, ...changes }),
    }),
  confirm: (candidateId: string, expectedRevision: number) =>
    request<CandidateSummary>(`/api/v1/knowledge/candidates/${candidateId}/confirm`, {
      method: 'POST', headers: { 'Content-Type': 'application/json' }, body: commandBody({ expectedRevision }),
    }),
  exclude: (candidateId: string, expectedRowVersion: number) =>
    request<CandidateSummary>(`/api/v1/knowledge/candidates/${candidateId}/exclude`, {
      method: 'POST', headers: { 'Content-Type': 'application/json' }, body: commandBody({ expectedRowVersion }),
    }),
  browse: () => request<{ mode: 'browse'; items: KnowledgeSummary[]; nextCursor?: string }>('/api/v1/knowledge'),
  getKnowledge: (knowledgeId: string) =>
    request<KnowledgeDetail>(`/api/v1/knowledge/items/${knowledgeId}`),
  listVersions: (knowledgeId: string) =>
    request<Page<KnowledgeVersionSummary>>(`/api/v1/knowledge/items/${knowledgeId}/versions`),
  getVersion: (knowledgeId: string, versionId: string) =>
    request<KnowledgeVersionDetail>(`/api/v1/knowledge/items/${knowledgeId}/versions/${versionId}`),
}
