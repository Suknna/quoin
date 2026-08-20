// Model provider discovery API (T08): upstream /v1/models as an input
// helper with an explicit manual fallback result.

export interface DiscoveredModel {
  id: string
  metadata?: Record<string, unknown>
}

export interface DiscoveryResult {
  available: boolean
  models: DiscoveredModel[]
  detail?: string
}

export async function discoverProviderModels(baseUrl: string, apiKey: string): Promise<DiscoveryResult> {
  const response = await fetch('/api/v1/model-providers/discover', {
    method: 'POST',
    credentials: 'include',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ baseUrl, apiKey }),
  })
  if (!response.ok) {
    throw new Error(`discovery HTTP ${response.status}`)
  }
  const body = (await response.json()) as { available: boolean; items?: DiscoveredModel[]; detail?: string }
  return { available: body.available, models: body.items ?? [], detail: body.detail }
}

export function newClientCommandId(): string {
  const raw = crypto.getRandomValues(new Uint8Array(18))
  return Array.from(raw, (byte) => byte.toString(16).padStart(2, '0')).join('')
}

export interface ModelProviderFormValue {
  baseUrl: string
  apiKey: string
  chatModelId: string
  embeddingModelId: string
  contextBudgetTokens: string
  maxOutputTokens: string
}

export function buildModelProviderConnection(value: ModelProviderFormValue): Record<string, unknown> {
  return {
    type: 'model_provider',
    baseUrl: value.baseUrl.trim(),
    chatModelId: value.chatModelId.trim(),
    embeddingModelId: value.embeddingModelId.trim(),
    contextBudgetTokens: Number(value.contextBudgetTokens),
    maxOutputTokens: Number(value.maxOutputTokens),
    apiKey: value.apiKey,
    clientCommandId: newClientCommandId(),
  }
}
