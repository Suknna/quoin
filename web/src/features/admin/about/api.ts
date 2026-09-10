export interface AboutComponent {
  slot: 'plinth' | 'lintel'
  state: 'unregistered' | 'registered' | 'revoked'
  currentGeneration: number
  rowVersion: number
  connected: boolean
  lastSeenAt?: string
  releaseVersion?: string
}

export interface AboutStatus {
  releaseVersion: string
  maintenance: { active: boolean; reason?: string; rowVersion: number }
  components: AboutComponent[]
}

export async function fetchAbout(): Promise<AboutStatus> {
  const response = await fetch('/api/v1/admin/about', { credentials: 'include' })
  if (!response.ok) throw new Error('暂时无法读取平台关于信息。')
  return response.json() as Promise<AboutStatus>
}
