// Evidence/Artifact reading helpers for the analysis tool-details layer
// (frozen getEvidence / getArtifactMetadata / downloadArtifactContent).
export interface EvidenceConnection {
  key: string
  type: 'thanos' | 'kubernetes' | 'model_provider'
}

export interface ArtifactSummary {
  id: string
  kind: string
  sensitive: boolean
  retentionKind: string
  ownerType: string
  ownerId: string
  sizeBytes: number
  sha256: string
  bodyExpired: boolean
  expiresAt?: string
  createdAt: string
}

export interface EvidenceDetail {
  id: string
  targetType: string
  targetId: string
  params: unknown
  observedAt: string
  integrity: 'complete' | 'incomplete'
  warnings?: unknown
  errors?: unknown
  producer:
    | { kind: 'quoin_local' }
    | { kind: 'plinth_tool'; attemptId: string; toolCallId: string; toolName: string; toolVersion: string }
    | { kind: 'lintel_browser'; attemptId: string }
  connections: EvidenceConnection[]
  body:
    | { kind: 'inline_json'; value: unknown }
    | { kind: 'artifact'; artifact: ArtifactSummary }
  createdAt: string
}

export async function fetchEvidence(evidenceId: string): Promise<EvidenceDetail> {
  const response = await fetch(`/api/v1/evidence/${evidenceId}`, { credentials: 'include' })
  if (!response.ok) throw new Error('证据详情加载失败')
  return (await response.json()) as EvidenceDetail
}

export async function fetchArtifactMetadata(artifactId: string): Promise<ArtifactSummary> {
  const response = await fetch(`/api/v1/artifacts/${artifactId}`, { credentials: 'include' })
  if (!response.ok) throw new Error('产物信息加载失败')
  return (await response.json()) as ArtifactSummary
}

// artifactDownloadURL is the authorized download entry (the session cookie
// travels with the fetch); the location is never stored or shared.
export function artifactDownloadURL(artifactId: string): string {
  return `/api/v1/artifacts/${artifactId}/content`
}

export function toolNameLabel(toolName: string): string {
  switch (toolName) {
    case 'thanos_query': return 'Thanos 查询'
    case 'artifact_read': return '读取产物片段'
    case 'artifact_grep': return '搜索产物文本'
    case 'bash': return '工作区命令'
    case 'read': return '读取文件'
    case 'write': return '写入文件'
    case 'grep': return '文件搜索'
    default: return toolName
  }
}

// evidenceParamsText renders the frozen params of one evidence body for
// the reading layer (the program only projects deterministic facts; the
// model conclusion stays in the analysis output).
export function evidenceParamsText(params: unknown): string {
  if (params && typeof params === 'object' && 'query' in (params as Record<string, unknown>)) {
    const query = (params as { query: unknown }).query
    if (typeof query === 'string') return query
  }
  return JSON.stringify(params ?? {})
}
