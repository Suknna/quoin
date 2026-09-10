import { afterEach, beforeEach, describe, expect, test } from 'vitest'
import { setupServer } from 'msw/node'
import { handlers, getMockScenario, resetMockState, setMockScenario } from './handlers'

const server = setupServer(...handlers)

beforeEach(() => { setMockScenario('administrator'); server.listen({ onUnhandledRequest: 'error' }) })
afterEach(() => { server.resetHandlers(); server.close(); resetMockState() })

async function response(path: string, init?: RequestInit) {
  return fetch(`http://localhost${path}`, init)
}

describe('offline domain mock handlers', () => {
  test('exposes representative linked alert, investigation, evidence, and inspection data', async () => {
    const alerts = await (await response('/api/v1/alerts?state=Firing')).json() as { items: Array<{ id: string }> }
    expect(alerts.items[0].id).toBe('alert-checkout-latency')
    const investigation = await (await response('/api/v1/investigations/investigation-checkout')).json() as { sources: Array<{ sourceId: string }> }
    expect(investigation.sources[0].sourceId).toBe(alerts.items[0].id)
    const evidence = await (await response('/api/v1/evidence/evidence-latency')).json() as { connections: Array<{ key: string }> }
    expect(evidence.connections[0].key).toBe('thanos-primary')
    const report = await (await response('/api/v1/inspections/runs/inspection-run-1/reports/1')).json() as { evidenceIds: string[] }
    expect(report.evidenceIds).toContain('evidence-latency')
  })

  test('mutates investigation head consistently and streams exact ui-message framing', async () => {
    const created = await response('/api/v1/investigations/investigation-checkout/messages', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ content: '请继续', expectedHeadMessageId: 'message-2', attachmentIds: [] }) })
    expect(created.status).toBe(201)
    const message = await created.json() as { id: string }
    const detail = await (await response('/api/v1/investigations/investigation-checkout')).json() as { headMessageId: string }
    expect(detail.headMessageId).toBe(message.id)
    const stream = await response(`/api/v1/investigations/investigation-checkout/messages/${message.id}/stream`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ protocol: 'ui-message-stream' }) })
    expect(stream.headers.get('content-type')).toContain('text/event-stream')
    expect(await stream.text()).toContain('data: [DONE]')
  })

  test('auth scenarios and fixed demo credentials follow the generated user contract', async () => {
    setMockScenario('unauthenticated')
    expect((await response('/api/v1/auth/me')).status).toBe(401)
    const login = await response('/api/v1/auth/login', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ username: 'operator', password: 'demo-operator-password' }) })
    expect(login.status).toBe(200)
    expect((await login.json() as { id: string; role: string }).id).toBe('user-operator')
    expect(getMockScenario()).toBe('unauthenticated')
    setMockScenario('password-change')
    expect((await (await response('/api/v1/auth/me')).json() as { passwordChangeRequired: boolean }).passwordChangeRequired).toBe(true)
  })

  test('persists password changes only after the actual current demo password is verified', async () => {
    const wrong = await response('/api/v1/auth/password', { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ currentPassword: 'incorrect-demo-password', newPassword: 'changed-demo-password' }) })
    expect(wrong.status).toBe(401)
    const changed = await response('/api/v1/auth/password', { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ currentPassword: 'demo-admin-password', newPassword: 'changed-demo-password' }) })
    expect(changed.status).toBe(204)
    await response('/api/v1/auth/logout', { method: 'POST' })
    expect((await response('/api/v1/auth/login', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ username: 'admin', password: 'demo-admin-password' }) })).status).toBe(401)
    expect((await response('/api/v1/auth/login', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ username: 'admin', password: 'changed-demo-password' }) })).status).toBe(200)
  })

  test('gates administration writes and platform facts while preserving mock-only runtime data', async () => {
    setMockScenario('operator')
    expect((await response('/api/v1/admin/users', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ username: 'blocked', displayName: 'Blocked', role: 'operator' }) })).status).toBe(403)
    expect((await response('/api/v1/admin/about')).status).toBe(403)
    setMockScenario('administrator')
    const about = await (await response('/api/v1/admin/about')).json() as { releaseVersion: string; components: Array<{ slot: string; releaseVersion: string }> }
    expect(about).toEqual(expect.objectContaining({ releaseVersion: 'mock-preview', components: [expect.objectContaining({ slot: 'plinth', releaseVersion: 'mock-plinth' }), expect.objectContaining({ slot: 'lintel', releaseVersion: 'mock-lintel' })] }))
    const source = await response('/api/v1/alert-sources', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ key: 'stateful-source', protocol: 'alertmanager' }) })
    expect(source.status).toBe(201)
    const createdSource = await source.json() as { credentialId: string }
    const credentials = await (await response('/api/v1/alert-sources/stateful-source/credentials')).json() as { items: Array<{ id: string; rowVersion: number }> }
    expect(credentials.items[0].id).toBe(createdSource.credentialId)
    expect((await response(`/api/v1/alert-sources/stateful-source/credentials/${createdSource.credentialId}/retire`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ expectedRowVersion: 1 }) })).status).toBe(200)
    const mapping = await response('/api/v1/business-systems/checkout/kubernetes-connections', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ connectionId: 'connection-kubernetes-prod' }) })
    expect(mapping.status).toBe(201)
    expect((await (await response('/api/v1/business-systems/checkout/kubernetes-connections')).json() as Array<{ connectionId: string }>).some(item => item.connectionId === 'connection-kubernetes-prod')).toBe(true)
  })

  test('supplies admin-only platform boundary fixtures without changing production API behavior', async () => {
    setMockScenario('maintenance')
    expect((await response('/api/v1/admin/about')).status).toBe(200)
    setMockScenario('platform-one')
    expect((await (await response('/api/v1/maintenance')).json() as { items: unknown[] }).items).toHaveLength(1)
    setMockScenario('platform-boundary')
    const about = await (await response('/api/v1/admin/about')).json() as { releaseVersion: string }
    const maintenance = await (await response('/api/v1/maintenance')).json() as { items: Array<{ objectKey: string; detailCode: string }> }
    expect(about.releaseVersion).toContain('intentionally-long')
    expect(maintenance.items).toHaveLength(50)
    expect(maintenance.items[49]).toEqual(expect.objectContaining({ objectKey: expect.stringContaining('intentionally-long'), detailCode: expect.stringContaining('intentionally-long') }))
  })

  test('persists feedback, inspection analysis, settings, and empty-domain reads', async () => {
    const feedback = await response('/api/v1/knowledge/feedback', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ targetType: 'inspection_report', targetId: 'report-1', value: 'executed', note: 'checked' }) })
    expect(feedback.status).toBe(201)
    expect((await (await response('/api/v1/knowledge/feedback?targetType=inspection_report&targetId=report-1')).json() as { items: Array<{ value: string }> }).items[0].value).toBe('executed')
    const analysis = await response('/api/v1/inspections/runs/inspection-run-1/analyze', { method: 'POST' })
    const attempt = await analysis.json() as { id: string }
    expect((await (await response('/api/v1/inspections/runs/inspection-run-1')).json() as { latestAnalysis: { id: string } }).latestAnalysis.id).toBe(attempt.id)
    const settings = await (await response('/api/v1/backups/settings')).json() as { rowVersion: number; retentionCount: number }
    expect((await response('/api/v1/backups/settings', { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ expectedRowVersion: settings.rowVersion, enabled: true, scheduleCron: null, timezone: 'UTC', retentionCount: 9 }) })).status).toBe(200)
    expect((await (await response('/api/v1/backups/settings')).json() as { timezone: string; retentionCount: number }).timezone).toBe('UTC')
    expect((await response('/api/v1/backups', { method: 'POST' })).status).toBe(501)
    setMockScenario('empty')
    expect((await (await response('/api/v1/connections')).json() as { items: unknown[] }).items).toEqual([])
    expect((await (await response('/api/v1/business-systems')).json() as { items: unknown[] }).items).toEqual([])
    expect((await (await response('/api/v1/alert-intake-issues')).json() as { items: unknown[] }).items).toEqual([])
  })

  test('serves a linked import batch detail and clears it in the empty scenario', async () => {
    const batch = await response('/api/v1/knowledge/import-batches/import-1')
    expect(batch.status).toBe(200)
    expect((await batch.json() as { candidates: Array<{ id: string; targetKnowledgeId?: string }> }).candidates).toEqual([expect.objectContaining({ id: 'candidate-import-1', targetKnowledgeId: 'knowledge-1' })])
    setMockScenario('empty')
    expect((await response('/api/v1/knowledge/import-batches/import-1')).status).toBe(404)
  })

  test('honors conflict, unavailable, and unsupported boundaries without passthrough', async () => {
    setMockScenario('conflict')
    const conflict = await response('/api/v1/connections/thanos-primary/disable', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ expectedRowVersion: 3 }) })
    expect(conflict.status).toBe(409)
    setMockScenario('unavailable')
    expect((await response('/api/v1/alerts?state=Firing')).status).toBe(503)
    setMockScenario('administrator')
    const unsupported = await response('/api/v1/backups/remote', { method: 'POST' })
    expect(unsupported.status).toBe(501)
    expect((await unsupported.json() as { code: string }).code).toBe('mock_handler_missing')
  })

  test('reset restores mutable local data', async () => {
    await response('/api/v1/connections/thanos-primary/disable', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ expectedRowVersion: 3 }) })
    expect((await (await response('/api/v1/connections/thanos-primary')).json() as { enabled: boolean }).enabled).toBe(false)
    resetMockState()
    expect((await (await response('/api/v1/connections/thanos-primary')).json() as { enabled: boolean }).enabled).toBe(true)
  })
})
