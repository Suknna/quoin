import { afterEach, beforeEach, describe, expect, test } from 'vitest'
import { setupServer } from 'msw/node'
import { handlers, resetMockState, setMockScenario } from './handlers'

const server = setupServer(...handlers)

beforeEach(() => { setMockScenario('administrator'); server.listen({ onUnhandledRequest: 'error' }) })
afterEach(() => { server.resetHandlers(); server.close(); resetMockState() })

async function response(path: string, init?: RequestInit) {
  return fetch(`http://localhost${path}`, { headers: { 'Content-Type': 'application/json' }, ...init })
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

  test('auth scenarios follow the single-step login contract', async () => {
    setMockScenario('unauthenticated')
    expect((await response('/api/v1/auth/me')).status).toBe(401)
    const login = await response('/api/v1/auth/login', { method: 'POST', body: JSON.stringify({ username: 'operator', password: 'demo-operator-password' }) })
    expect(login.status).toBe(200)
    expect(await login.json() as { completed: boolean; user: { id: string; authSource: string } }).toMatchObject({ completed: true, user: { id: 'user-operator', authSource: 'local' } })
    // The single step issued the session directly (ADR-0010).
    expect((await response('/api/v1/auth/me')).status).toBe(200)
    // The retired flow surface is gone for good (MSW answers unmatched routes with 501).
    expect([404, 501]).toContain((await response('/api/v1/auth/flow')).status)
    await response('/api/v1/auth/logout', { method: 'POST' })
    expect((await response('/api/v1/auth/me')).status).toBe(401)
  })

  test('a forced temp credential lands in the restricted state', async () => {
    setMockScenario('password-change')
    const login = await response('/api/v1/auth/login', { method: 'POST', body: JSON.stringify({ username: 'operator', password: 'demo-operator-password' }) })
    expect(login.status).toBe(200)
    expect(await login.json() as { user: { passwordChangeRequired: boolean } }).toMatchObject({ user: { passwordChangeRequired: true } })
  })

  test('a fresh deployment unlocks the bootstrap admin over the session endpoints', async () => {
    setMockScenario('empty')
    const started = await response('/api/v1/auth/login', { method: 'POST', body: JSON.stringify({ username: 'admin', password: 'demo-admin-password' }) })
    expect(started.status).toBe(200)
    expect(await started.json() as { user: { passwordChangeRequired: boolean } }).toMatchObject({ user: { passwordChangeRequired: true } })
    // The restricted session exists but cannot reach the full admission level.
    expect((await response('/api/v1/auth/me')).status).toBe(200)
    // The forced change unlocks and finishes the deployment initialization.
    const changed = await response('/api/v1/auth/password', { method: 'PUT', body: JSON.stringify({ currentPassword: 'demo-admin-password', newPassword: 'initialized-demo-password' }) })
    expect(changed.status).toBe(204)
    expect((await (await response('/api/v1/auth/me')).json() as { passwordChangeRequired: boolean }).passwordChangeRequired).toBe(false)
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
    expect(about).toEqual(expect.objectContaining({ releaseVersion: 'mock-preview', components: [expect.objectContaining({ slot: 'plinth', releaseVersion: 'mock-plinth' })] }))
    const source = await response('/api/v1/alert-sources', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ key: 'stateful-source', protocol: 'alertmanager' }) })
    expect(source.status).toBe(201)
    const createdSource = await source.json() as { credentialId: string }
    const credentials = await (await response('/api/v1/alert-sources/stateful-source/credentials')).json() as { items: Array<{ id: string; rowVersion: number }> }
    expect(credentials.items[0].id).toBe(createdSource.credentialId)
    expect((await response(`/api/v1/alert-sources/stateful-source/credentials/${createdSource.credentialId}/retire`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ expectedRowVersion: 1 }) })).status).toBe(200)
  })

  test('statefully serves business views behind the admin boundary with row-version fencing', async () => {
    setMockScenario('operator')
    expect((await response('/api/v1/business-views')).status).toBe(403)
    expect((await response('/api/v1/business-views', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ clientCommandId: 'cmd-1', viewKey: 'checkout', displayName: '结算', description: '', scope: { labelConditions: {} } }) })).status).toBe(403)
    setMockScenario('administrator')
    const list = await (await response('/api/v1/business-views')).json() as { items: Array<{ viewKey: string; scope: { connectionName?: string; labelConditions: Record<string, string> } }> }
    expect(list.items[0]).toMatchObject({ viewKey: 'checkout', scope: { connectionName: 'thanos-primary', labelConditions: { service: 'checkout' } } })
    const created = await response('/api/v1/business-views', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ clientCommandId: 'cmd-2', viewKey: 'payment', displayName: '支付', description: '支付业务范围', scope: { labelConditions: { service: 'payment' } } }) })
    expect(created.status).toBe(201)
    expect((await response('/api/v1/business-views', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ clientCommandId: 'cmd-3', viewKey: 'payment', displayName: '重复标识', description: '', scope: { labelConditions: {} } }) })).status).toBe(409)
    expect((await response('/api/v1/business-views/payment', { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ clientCommandId: 'cmd-4', displayName: '支付系统', description: '', scope: { labelConditions: {} }, expectedRowVersion: 99 }) })).status).toBe(409)
    const updated = await response('/api/v1/business-views/payment', { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ clientCommandId: 'cmd-5', displayName: '支付系统', description: '支付业务范围', scope: { connectionName: 'thanos-primary', labelConditions: { service: 'payment' } }, expectedRowVersion: 1 }) })
    expect(updated.status).toBe(200)
    expect(await updated.json() as { rowVersion: number }).toMatchObject({ rowVersion: 2 })
    expect((await (await response('/api/v1/business-views/payment')).json() as { displayName: string }).displayName).toBe('支付系统')
    expect((await response('/api/v1/business-views/missing-view')).status).toBe(404)
  })

  test('replays identical commands from the stateful cache and conflicts on changed payloads', async () => {
    const command = { clientCommandId: 'idem-view-1', viewKey: 'idem', displayName: '幂等视图', description: '', scope: { labelConditions: {} } }
    expect((await response('/api/v1/business-views', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(command) })).status).toBe(201)
    // The identical replay returns the original 201 result, not a duplicate-key conflict.
    const replay = await response('/api/v1/business-views', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(command) })
    expect(replay.status).toBe(201)
    expect(await replay.json() as { viewKey: string }).toMatchObject({ viewKey: 'idem' })
    // The same ID carrying a different request conflicts per the command contract.
    const changed = await response('/api/v1/business-views', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ ...command, displayName: '改过的请求' }) })
    expect(changed.status).toBe(409)
    expect((await changed.json() as { code: string }).code).toBe('command_id_conflict')
    // An idempotent update replay returns the stored result without re-applying it.
    const update = { clientCommandId: 'idem-put-1', displayName: '幂等视图二版', description: '', scope: { labelConditions: {} }, expectedRowVersion: 1 }
    expect((await (await response('/api/v1/business-views/idem', { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(update) })).json() as { rowVersion: number }).rowVersion).toBe(2)
    const updateReplay = await response('/api/v1/business-views/idem', { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(update) })
    expect(updateReplay.status).toBe(200)
    expect(await updateReplay.json() as { rowVersion: number }).toMatchObject({ rowVersion: 2 })
    expect((await (await response('/api/v1/business-views/idem')).json() as { rowVersion: number }).rowVersion).toBe(2)
    // Run creation replays return the original run instead of creating a duplicate.
    const run = await (await response('/api/v1/inspections/runs', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ clientCommandId: 'idem-run-1', planKey: 'checkout-latency-watch' }) })).json() as { id: string }
    const runReplay = await (await response('/api/v1/inspections/runs', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ clientCommandId: 'idem-run-1', planKey: 'checkout-latency-watch' }) })).json() as { id: string }
    expect(runReplay.id).toBe(run.id)
    expect((await (await response('/api/v1/inspections/runs?planKey=checkout-latency-watch')).json() as { items: Array<{ id: string }> }).items.filter((item) => item.id === run.id)).toHaveLength(1)
  })

  test('serves plugin inspection plans and freezes run scope from planKey only', async () => {
    // The whole inspection handler group sits behind the Admin boundary, matching the real reader.
    setMockScenario('operator')
    expect((await response('/api/v1/inspections/plans')).status).toBe(403)
    expect((await response('/api/v1/inspections/runs')).status).toBe(403)
    expect((await response('/api/v1/inspections/runs/inspection-run-1')).status).toBe(403)
    expect((await response('/api/v1/inspections/plans', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ clientCommandId: 'cmd-p0', planKey: 'operator-plan', displayName: '越权计划', enabled: true, connectionName: 'thanos-primary', pluginId: 'thanos', templateId: 'latency', params: {}, scope: { kind: 'integration' }, timezone: 'UTC' }) })).status).toBe(403)
    setMockScenario('administrator')
    const plans = await (await response('/api/v1/inspections/plans')).json() as { items: Array<{ planKey: string; scope: { kind: string; businessViewKey?: string }; connectionName: string }> }
    expect(plans.items[0]).toMatchObject({ planKey: 'checkout-latency-watch', connectionName: 'thanos-primary', scope: { kind: 'businessView', businessViewKey: 'checkout' } })
    setMockScenario('administrator')
    expect((await response('/api/v1/inspections/plans', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ clientCommandId: 'cmd-p1', planKey: 'bad-connection', displayName: '未知接入', enabled: true, connectionName: 'missing-connection', pluginId: 'thanos', templateId: 'latency', params: {}, scope: { kind: 'integration' }, timezone: 'UTC' }) })).status).toBe(422)
    expect((await response('/api/v1/inspections/plans', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ clientCommandId: 'cmd-p2', planKey: 'bad-view', displayName: '未知视图', enabled: true, connectionName: 'thanos-primary', pluginId: 'thanos', templateId: 'latency', params: {}, scope: { kind: 'businessView', businessViewKey: 'missing-view' }, timezone: 'UTC' }) })).status).toBe(422)
    const created = await response('/api/v1/inspections/plans', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ clientCommandId: 'cmd-p3', planKey: 'checkout-error-watch', displayName: '结算错误观测', enabled: true, connectionName: 'thanos-primary', pluginId: 'thanos', templateId: 'latency', templateVersion: '1', params: { query: 'up' }, scope: { kind: 'businessView', businessViewKey: 'checkout' }, cron: '*/10 * * * *', timezone: 'Asia/Shanghai' }) })
    expect(created.status).toBe(201)
    expect((await response('/api/v1/inspections/plans', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ clientCommandId: 'cmd-p4', planKey: 'checkout-error-watch', displayName: '重复计划', enabled: true, connectionName: 'thanos-primary', pluginId: 'thanos', templateId: 'latency', params: {}, scope: { kind: 'integration' }, timezone: 'UTC' }) })).status).toBe(409)
    expect((await response('/api/v1/inspections/plans/checkout-error-watch', { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ clientCommandId: 'cmd-p5', displayName: '结算错误观测二版', enabled: true, connectionName: 'thanos-primary', pluginId: 'thanos', templateId: 'latency', params: {}, scope: { kind: 'integration' }, timezone: 'UTC', expectedRowVersion: 99 }) })).status).toBe(409)
    const updated = await (await response('/api/v1/inspections/plans/checkout-error-watch', { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ clientCommandId: 'cmd-p6', displayName: '结算错误观测二版', enabled: true, connectionName: 'thanos-primary', pluginId: 'thanos', templateId: 'latency', params: {}, scope: { kind: 'integration' }, timezone: 'UTC', expectedRowVersion: 1 }) })).json() as { rowVersion: number }
    expect(updated.rowVersion).toBe(2)
    const runResponse = await response('/api/v1/inspections/runs', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ clientCommandId: 'cmd-r1', planKey: 'checkout-error-watch' }) })
    expect(runResponse.status).toBe(201)
    const run = await runResponse.json() as { id: string; planKey: string; connectionName?: string; businessSystemKey?: string | null; state: string; rowVersion: number }
    expect(run).toMatchObject({ planKey: 'checkout-error-watch', connectionName: 'thanos-primary', state: 'Completed' })
    expect(run.businessSystemKey ?? null).toBeNull()
    expect((await response('/api/v1/inspections/runs', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ clientCommandId: 'cmd-r2', planKey: 'missing-plan' }) })).status).toBe(404)
    const filtered = await (await response('/api/v1/inspections/runs?planKey=checkout-error-watch')).json() as { items: Array<{ planKey: string }> }
    expect(filtered.items.map((item) => item.planKey)).toEqual(['checkout-error-watch'])
    // Cancel fencing is exercised against the seeded active plan-scoped run, not the terminal preview result.
    expect((await response('/api/v1/inspections/runs/inspection-run-2/cancel', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ clientCommandId: 'cmd-r3', expectedRowVersion: 99 }) })).status).toBe(409)
    const cancelled = await response('/api/v1/inspections/runs/inspection-run-2/cancel', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ clientCommandId: 'cmd-r4', expectedRowVersion: 1 }) })
    expect(cancelled.status).toBe(200)
    expect(await cancelled.json() as { state: string }).toMatchObject({ state: 'Cancelled' })
    expect((await response('/api/v1/inspections/runs/inspection-run-2/cancel', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ clientCommandId: 'cmd-r5', expectedRowVersion: 2 }) })).status).toBe(409)
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
    expect((await (await response('/api/v1/business-views')).json() as { items: unknown[] }).items).toEqual([])
    expect((await (await response('/api/v1/inspections/plans')).json() as { items: unknown[] }).items).toEqual([])
    expect((await (await response('/api/v1/alert-intake-issues')).json() as { items: unknown[] }).items).toEqual([])
  })

  test('serves a linked import batch detail and clears it in the empty scenario', async () => {
    const batch = await response('/api/v1/knowledge/import-batches/import-1')
    expect(batch.status).toBe(200)
    expect((await batch.json() as { candidates: Array<{ id: string; targetKnowledgeId?: string }> }).candidates).toEqual([expect.objectContaining({ id: 'candidate-import-1', targetKnowledgeId: 'knowledge-1' })])
    setMockScenario('empty')
    expect((await response('/api/v1/knowledge/import-batches/import-1')).status).toBe(404)
  })

  test('organize-knowledge endpoints create-or-return candidates and honor rejection', async () => {
    const headers = { 'Content-Type': 'application/json' }
    // 分析输出来源:首次创建 201,同源再次创建返回已有候选(200)。
    const first = await response('/api/v1/alerts/alert-checkout-latency/analyses/analysis-1/knowledge-candidates', { method: 'POST', headers, body: JSON.stringify({ clientCommandId: 'k1' }) })
    // analysis-output-1 已挂接 candidate-1(create-or-return 命中已有候选)。
    expect(first.status).toBe(200)
    expect((await first.json() as { id: string }).id).toBe('candidate-1')
    // 调查消息来源:assistant 消息可建,用户消息 422。
    const msg = await response('/api/v1/investigations/investigation-checkout/knowledge-candidates', { method: 'POST', headers, body: JSON.stringify({ clientCommandId: 'k2', sourceType: 'investigation_message', sourceId: 'message-2' }) })
    expect(msg.status).toBe(201)
    const msgCandidate = await msg.json() as { id: string; sourceType: string }
    expect(msgCandidate.sourceType).toBe('investigation_message')
    expect((await response('/api/v1/investigations/investigation-checkout/knowledge-candidates', { method: 'POST', headers, body: JSON.stringify({ clientCommandId: 'k3', sourceType: 'investigation_message', sourceId: 'message-2' }) })).status).toBe(200)
    expect((await response('/api/v1/investigations/investigation-checkout/knowledge-candidates', { method: 'POST', headers, body: JSON.stringify({ clientCommandId: 'k4', sourceType: 'investigation_message', sourceId: 'message-1' }) })).status).toBe(422)
    // 巡检报告来源:按 run + 报告版本建候选。
    const report = await response('/api/v1/inspections/runs/inspection-run-1/reports/1/knowledge-candidates', { method: 'POST', headers, body: JSON.stringify({ clientCommandId: 'k5' }) })
    expect(report.status).toBe(201)
    // 来源被标记不采纳后,同源创建返回 409。
    await response('/api/v1/knowledge/feedback', { method: 'POST', headers, body: JSON.stringify({ targetType: 'inspection_report', targetId: 'report-1', value: 'rejected' }) })
    expect((await response('/api/v1/inspections/runs/inspection-run-1/reports/1/knowledge-candidates', { method: 'POST', headers, body: JSON.stringify({ clientCommandId: 'k6' }) })).status).toBe(409)
    // 列表过滤:state/sourceType 都生效。
    const awaiting = await (await response('/api/v1/knowledge/candidates?state=AwaitingConfirmation')).json() as { items: Array<{ state: string }> }
    expect(awaiting.items.every((item) => item.state === 'AwaitingConfirmation')).toBe(true)
    const fromMessages = await (await response('/api/v1/knowledge/candidates?sourceType=investigation_message')).json() as { items: Array<{ id: string }> }
    expect(fromMessages.items.map((item) => item.id)).toEqual([msgCandidate.id])
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
