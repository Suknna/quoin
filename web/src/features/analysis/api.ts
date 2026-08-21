export interface InitialAnalysisSummary {
  id: string
  state: 'Queued' | 'Running' | 'Succeeded' | 'Failed' | 'Cancelled' | 'Interrupted'
  rowVersion: number
  createdAt: string
}

export interface AnalysisOutput {
  id: string
  modelId: string
  content: string
  evidenceIds: string[]
  createdAt: string
}

export interface InitialAnalysisDetail extends InitialAnalysisSummary {
  attemptCount: number
  output?: AnalysisOutput
}

export interface AttemptSummary {
  id: string
  type: string
  state: string
  rowVersion: number
  startedAt?: string
  endedAt?: string
  terminationReason?: string
  createdAt: string
}

export async function fetchAnalyses(occurrenceId: string): Promise<{ items: InitialAnalysisSummary[] }> {
  const response = await fetch(`/api/v1/alerts/${occurrenceId}/analyses`, { credentials: 'include' })
  if (!response.ok) throw new Error('初步分析历史加载失败')
  return (await response.json()) as { items: InitialAnalysisSummary[] }
}

export async function fetchAnalysis(occurrenceId: string, analysisId: string): Promise<InitialAnalysisDetail> {
  const response = await fetch(`/api/v1/alerts/${occurrenceId}/analyses/${analysisId}`, { credentials: 'include' })
  if (!response.ok) throw new Error('初步分析详情加载失败')
  return (await response.json()) as InitialAnalysisDetail
}

export async function createAnalysis(occurrenceId: string, clientCommandId: string): Promise<InitialAnalysisDetail> {
  const response = await fetch(`/api/v1/alerts/${occurrenceId}/analyses`, {
    method: 'POST', credentials: 'include',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ clientCommandId }),
  })
  if (!response.ok) {
    const problem = (await response.json().catch(() => null)) as { detail?: string } | null
    throw new Error(problem?.detail ?? '暂时无法发起初步分析')
  }
  return (await response.json()) as InitialAnalysisDetail
}

export async function cancelAnalysis(occurrenceId: string, analysisId: string, expectedRowVersion: number, clientCommandId: string): Promise<InitialAnalysisDetail> {
  const response = await fetch(`/api/v1/alerts/${occurrenceId}/analyses/${analysisId}/cancel`, {
    method: 'POST', credentials: 'include',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ clientCommandId, expectedRowVersion }),
  })
  if (!response.ok) {
    const problem = (await response.json().catch(() => null)) as { detail?: string } | null
    throw new Error(problem?.detail ?? '暂时无法取消初步分析')
  }
  return (await response.json()) as InitialAnalysisDetail
}

export function analysisCommandId(): string {
  const raw = crypto.getRandomValues(new Uint8Array(18))
  return Array.from(raw, (byte) => byte.toString(16).padStart(2, '0')).join('')
}

export function stateLabel(state: InitialAnalysisSummary['state']): string {
  switch (state) {
    case 'Queued': return '排队中'
    case 'Running': return '分析中'
    case 'Succeeded': return '已完成'
    case 'Failed': return '失败'
    case 'Cancelled': return '已取消'
    case 'Interrupted': return '已中断'
  }
}

export function isActive(state: InitialAnalysisSummary['state']): boolean {
  return state === 'Queued' || state === 'Running'
}

// fetchAttempts reads the immutable attempt history of one analysis
// (listInitialAnalysisAttempts); the latest attempt's terminationReason
// explains technical failures and interruptions to the operator.
export async function fetchAttempts(occurrenceId: string, analysisId: string): Promise<{ items: AttemptSummary[] }> {
  const response = await fetch(`/api/v1/alerts/${occurrenceId}/analyses/${analysisId}/attempts`, { credentials: 'include' })
  if (!response.ok) throw new Error('分析执行历史加载失败')
  return (await response.json()) as { items: AttemptSummary[] }
}

// reasonLabel projects one machine termination reason onto the plain
// language an operator reads (UI copy stays Chinese; the raw enum is only
// a secondary diagnostic detail).
export function reasonLabel(reason?: string): string {
  switch (reason) {
    case 'lease_expired': return '运行时连接丢失且租约到期'
    case 'replaced': return '运行时被替换'
    case 'revoked': return '运行时凭据被吊销'
    case 'provider_unavailable': return '模型供应商暂时不可用'
    case 'timeout': return '模型调用超时'
    case 'rate_limited': return '模型调用被限流'
    case 'invalid_response': return '模型返回无法解析'
    case 'context_too_large': return '输入超出模型上下文'
    case 'sandbox_unavailable': return '沙箱不可用'
    case 'worker_protocol_error': return '执行进程异常退出'
    case 'cancelled': return '操作者取消'
    default: return ''
  }
}
