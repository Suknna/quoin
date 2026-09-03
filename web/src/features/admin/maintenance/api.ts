// Admin maintenance/upgrade feature API (T36): the Upgrade maintenance
// projection, the coordinated-upgrade prepare command, the frozen drain
// cancels and the versioned exit. The drain endpoint templates are the
// web-side mirror of the server's upgrade directive vocabulary.

export interface MaintenanceItem {
  kind: string
  objectKey: string
  safeState: 'Safe' | 'Blocking'
  detailCode: string
}

export interface MaintenanceStateView {
  active: boolean
  reason?: 'Restore' | 'Upgrade' | 'RootKeyRebind'
  rowVersion: number
  items: MaintenanceItem[]
}

class RequestError extends Error {
  constructor(message: string, readonly status: number) { super(message) }
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const response = await fetch(path, {
    credentials: 'include',
    ...init,
    headers: { 'Content-Type': 'application/json', ...(init?.headers ?? {}) },
  })
  if (!response.ok) {
    const body = await response.json().catch(() => null) as { message?: string } | null
    throw new RequestError(body?.message ?? '请求没有完成。', response.status)
  }
  return response.json() as Promise<T>
}

export function commandID() {
  return Array.from(crypto.getRandomValues(new Uint8Array(18)), value => value.toString(16).padStart(2, '0')).join('')
}

export async function fetchMaintenanceState(): Promise<MaintenanceStateView> {
  return request<MaintenanceStateView>('/api/v1/maintenance')
}

export async function prepareUpgrade(expectedRowVersion: number, clientCommandId = commandID()): Promise<MaintenanceStateView> {
  return request<MaintenanceStateView>('/api/v1/maintenance/upgrade/prepare', {
    method: 'POST',
    body: JSON.stringify({ clientCommandId, expectedRowVersion }),
  })
}

export async function exitMaintenance(expectedRowVersion: number, reason: 'Restore' | 'Upgrade' | 'RootKeyRebind', clientCommandId = commandID()): Promise<MaintenanceStateView> {
  return request<MaintenanceStateView>('/api/v1/maintenance/exit', {
    method: 'POST',
    body: JSON.stringify({ clientCommandId, expectedRowVersion, expectedReason: reason }),
  })
}

// drainRoute maps one server drain directive onto its frozen cancel command.
// detail codes are `<state>|cancel:<endpointKey>:<path params>:<rowVersion>`
// or `<state>|converge`; objectKey carries the item identity only.
const drainRoutes: Record<string, (params: string[]) => string> = {
  analysis: params => `/api/v1/alerts/${params[0]}/analyses/${params[1]}/cancel`,
  investigation: params => `/api/v1/investigations/${params[0]}/attempts/${params[1]}/cancel`,
  inspection_run: params => `/api/v1/inspections/runs/${params[0]}/cancel`,
  knowledge_batch: params => `/api/v1/knowledge/import-batches/${params[0]}/cancel`,
  connection_probe: params => `/api/v1/connections/${params[0]}/probe-attempts/${params[1]}/cancel`,
  config_verification: params => `/api/v1/business-systems/${params[0]}/config/${params[1]}/verifications/${params[2]}/cancel`,
  browser_operation: params => `/api/v1/browser-login/${params[0]}/operations/${params[1]}/cancel`,
}

export interface DrainTarget {
  state: string
  endpointKey: string
  route: string
  rowVersion: number
}

export function drainTargetOf(item: MaintenanceItem): DrainTarget | null {
  const [state, directive] = item.detailCode.split('|')
  if (!directive || !directive.startsWith('cancel:')) return null
  const [, endpointKey, paramPath, rowVersion] = directive.split(':')
  const builder = drainRoutes[endpointKey]
  if (!builder) return null
  const params = paramPath.split('/')
  if (rowVersion === undefined || params.some(value => value.length === 0)) return null
  return { state, endpointKey, route: builder(params), rowVersion: Number(rowVersion) }
}

export async function cancelDrainTarget(target: DrainTarget, clientCommandId = commandID()): Promise<void> {
  // The browser login fence carries its own expectedOperationRowVersion field
  // (CancelBrowserOperationRequest); every other drain command shares
  // CancelRequest's expectedRowVersion.
  const body = target.endpointKey === 'browser_operation'
    ? { clientCommandId, expectedOperationRowVersion: target.rowVersion }
    : { clientCommandId, expectedRowVersion: target.rowVersion }
  await request<unknown>(target.route, {
    method: 'POST',
    body: JSON.stringify(body),
  })
}
