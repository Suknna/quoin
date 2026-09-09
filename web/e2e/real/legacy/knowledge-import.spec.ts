import { execFileSync } from 'node:child_process'
import { existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'
import { expect, test, type Page } from '@playwright/test'

test.use({ trace: 'off', screenshot: 'off', video: 'off' })

type Batch = { id: string; state: string; rowVersion: number; candidates: Array<{ id: string; state: string; draftRevision: number; draftTitle?: string; confirmedKnowledgeId?: string }> }

function command() { return `t28-${Date.now()}-${Math.random().toString(36).slice(2, 10)}` }
async function call(page: Page, url: string, init?: RequestInit) {
  return page.evaluate(async ({ url, init }) => { const response = await fetch(url, init); return { status: response.status, body: await response.text() } }, { url, init })
}
async function api<T>(page: Page, url: string, init?: RequestInit): Promise<T> {
  const result = await call(page, url, init)
  if (result.status >= 300) throw new Error(`${url}: ${result.status} ${result.body}`)
  return JSON.parse(result.body || '{}') as T
}
function rows(query: string): Array<Record<string, unknown>> {
  const mounts = JSON.parse(execFileSync('docker', ['inspect', '--format', '{{json .Mounts}}', 'quoin-quoin-1'], { encoding: 'utf8' })) as Array<{ Source: string; Destination: string }>
  const directory = mounts.find((mount) => mount.Destination.includes('data'))?.Source
  if (!directory) throw new Error('Quoin data mount unavailable')
  const script = 'import json,sqlite3,sys; c=sqlite3.connect("file:"+sys.argv[1]+"?mode=ro",uri=True); c.row_factory=sqlite3.Row; print(json.dumps([dict(x) for x in c.execute(sys.argv[2])]))'
  return JSON.parse(execFileSync('python3', ['-c', script, join(directory, 'quoin.db'), query], { encoding: 'utf8' })) as Array<Record<string, unknown>>
}
function record(value: unknown) {
  const dir = process.env.QUOIN_EVIDENCE_DIR
  if (!dir) return
  mkdirSync(dir, { recursive: true })
  writeFileSync(join(dir, 't28-knowledge-observations.json'), JSON.stringify(value, null, 2))
}

// T28 uses the established compose model fixture and reads SQLite only after
// every product operation has completed through the ordinary HTTP/runtime path.
test.describe('T28 原文导入、人工确认和版本检索 @ticket-28', () => {
  test('real Plinth extraction preserves confirmation, cancellation and FTS fences', async ({ page }) => {
    test.slow(); test.setTimeout(900_000)
    const stack = join(import.meta.dirname, '..', '..', '.artifacts', 'e2e-stack')
    expect(existsSync(join(stack, 'admin-new-password'))).toBeTruthy()
    const evidence: Record<string, unknown> = { startedAt: new Date().toISOString() }
    await page.goto('/')
    await page.fill('#username', 'admin')
    await page.fill('#password', readFileSync(join(stack, 'admin-new-password'), 'utf8').trim())
    await page.getByRole('button', { name: '登录' }).click()
    await expect(page.getByRole('navigation', { name: '全局模块' })).toBeVisible()

    const startCommand = command()
    const started = await call(page, '/api/v1/knowledge/import-batches', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ clientCommandId: startCommand, text: 'T28 导入原文：连接池请求超时，先检查连接池上限与等待超时。' }) })
    expect(started.status).toBe(202)
    const initial = JSON.parse(started.body) as Batch
    expect(initial.state).toBe('Processing')
    const replay = await call(page, '/api/v1/knowledge/import-batches', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ clientCommandId: startCommand, text: 'T28 导入原文：连接池请求超时，先检查连接池上限与等待超时。' }) })
    expect(replay.status).toBe(202)
    expect((JSON.parse(replay.body) as Batch).id).toBe(initial.id)

    let batch = initial
    for (let remaining = 180; remaining > 0 && batch.state === 'Processing'; remaining--) { await page.waitForTimeout(1000); batch = await api<Batch>(page, `/api/v1/knowledge/import-batches/${initial.id}`) }
    if (batch.state !== 'AwaitingConfirmation') {
      const failure = rows(`SELECT id,state,termination_reason,boot_id,connection_epoch FROM execution_attempts WHERE scope_id=${Number(initial.id)} AND attempt_type='knowledge_extraction'`)
      record({ ...evidence, extractionFailure: { batch, failure } })
      throw new Error(`knowledge extraction terminal state ${batch.state}: ${JSON.stringify(failure)}`)
    }
    expect(batch.candidates).toHaveLength(1)
    const candidate = batch.candidates[0]
    const detail = await api<{ originalSuggestion: { body: string }; sourceType: string; sourceId: string }>(page, `/api/v1/knowledge/candidates/${candidate.id}`)
    // The frozen Source Material endpoints are the import's provenance:
    // metadata plus the verbatim UTF-8 body the operator submitted, unchanged
    // by every later edit, confirm and revision.
    const sourceMeta = await api<{ id: string; kind: string; digest: string; sizeBytes: number }>(page, `/api/v1/source-materials/${detail.sourceId}`)
    expect(sourceMeta.kind).toBe('knowledge_import')
    const sourceContent = await call(page, `/api/v1/source-materials/${detail.sourceId}/content`)
    expect(sourceContent.status).toBe(200)
    const submittedText = 'T28 导入原文：连接池请求超时，先检查连接池上限与等待超时。'
    expect(sourceContent.body).toBe(submittedText)
    expect(sourceMeta.sizeBytes).toBe(Buffer.byteLength(submittedText, 'utf8'))
    const edited = await call(page, `/api/v1/knowledge/candidates/${candidate.id}`, { method: 'PATCH', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ clientCommandId: command(), expectedRevision: 0, title: 'T28 已审阅连接池处置' }) })
    expect(edited.status).toBe(200)
    const stale = await call(page, `/api/v1/knowledge/candidates/${candidate.id}`, { method: 'PATCH', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ clientCommandId: command(), expectedRevision: 0, title: '过期草稿' }) })
    expect(stale.status).toBe(409)
    const staleBatchConfirm = await call(page, `/api/v1/knowledge/import-batches/${batch.id}/confirm`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ clientCommandId: command(), items: [{ candidateId: candidate.id, expectedRevision: 0 }] }) })
    expect(staleBatchConfirm.status).toBe(409)
    const afterStaleBatchConfirm = await api<Batch>(page, `/api/v1/knowledge/import-batches/${batch.id}`)
    expect(afterStaleBatchConfirm.state).toBe('AwaitingConfirmation')
    expect(afterStaleBatchConfirm.candidates[0]?.draftRevision).toBe(1)
    const confirmed = await api<Batch>(page, `/api/v1/knowledge/import-batches/${batch.id}/confirm`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ clientCommandId: command(), items: [{ candidateId: candidate.id, expectedRevision: 1 }] }) })
    expect(confirmed.state).toBe('Completed')
    const confirmedCandidate = confirmed.candidates[0]
    expect(confirmedCandidate.confirmedKnowledgeId).toBeTruthy()
    const knowledgeID = confirmedCandidate.confirmedKnowledgeId as string
    const knowledge = await api<{ currentVersionId: string; currentVersionSeq: number; rowVersion: number; eligible: boolean }>(page, `/api/v1/knowledge/items/${knowledgeID}`)
    expect(knowledge.currentVersionSeq).toBe(1)
    const revision = await api<{ id: string; draftRevision: number }>(page, `/api/v1/knowledge/items/${knowledgeID}/versions`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ clientCommandId: command(), expectedCurrentVersionId: knowledge.currentVersionId, expectedRowVersion: knowledge.rowVersion }) })
    const revised = await api<{ confirmedKnowledgeId: string }>(page, `/api/v1/knowledge/candidates/${revision.id}/confirm`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ clientCommandId: command(), expectedRevision: revision.draftRevision }) })
    expect(revised.confirmedKnowledgeId).toBe(knowledgeID)
    const current = await api<{ currentVersionId: string; currentVersionSeq: number; rowVersion: number }>(page, `/api/v1/knowledge/items/${knowledgeID}`)
    expect(current.currentVersionSeq).toBe(2)
    const beforeStop = rows(`SELECT COUNT(*) AS search_docs FROM knowledge_search_docs d JOIN knowledge_versions v ON v.id=d.knowledge_version_id WHERE v.knowledge_id=${Number(knowledgeID)}`)
    expect(beforeStop[0]?.search_docs).toBe(1)
    const query = await api<{ mode: string; exactTextMatches: Array<{ knowledge: { id: string } }>; semanticMatches: unknown[] }>(page, '/api/v1/knowledge?q=T28')
    expect(query.mode).toBe('query')
    expect(query.exactTextMatches.some((hit) => hit.knowledge.id === knowledgeID)).toBeTruthy()
    await page.goto('/knowledge')
    await page.getByLabel('检索知识').fill('T28')
    await page.getByRole('button', { name: '搜索' }).click()
    await expect(page.getByText('T28 已审阅连接池处置')).toBeVisible()
    await api<void>(page, `/api/v1/knowledge/items/${knowledgeID}/versions/${current.currentVersionId}/stop-reuse`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ clientCommandId: command(), expectedRowVersion: 1 }) })

    // Fixture's slow extraction makes cancel-before-result an actual interleaving.
    const slow = await api<Batch>(page, '/api/v1/knowledge/import-batches', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ clientCommandId: command(), text: 'T28_CANCEL_SLOW：不要发布此导入。' }) })
    const cancelled = await api<Batch>(page, `/api/v1/knowledge/import-batches/${slow.id}/cancel`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ clientCommandId: command(), expectedRowVersion: slow.rowVersion }) })
    expect(cancelled.state).toBe('Cancelled')
    await page.waitForTimeout(6000)
    const afterCancel = await api<Batch>(page, `/api/v1/knowledge/import-batches/${slow.id}`)
    expect(afterCancel.state).toBe('Cancelled')
    expect(afterCancel.candidates).toHaveLength(0)

    const observed = rows(`SELECT b.id,b.state,a.state AS attempt_state,(SELECT COUNT(*) FROM knowledge_versions v WHERE v.knowledge_id=${Number(knowledgeID)}) AS versions,(SELECT COUNT(*) FROM knowledge_search_docs d JOIN knowledge_versions v ON v.id=d.knowledge_version_id WHERE v.knowledge_id=${Number(knowledgeID)}) AS search_docs FROM knowledge_import_batches b JOIN execution_attempts a ON a.scope_id=b.id AND a.attempt_type='knowledge_extraction' WHERE b.id IN (${Number(batch.id)},${Number(slow.id)}) ORDER BY b.id`)
    // The model's own suggestion stays distinct from the frozen source text
    // (it is the extraction output, not the submitted material).
    const frozenSource = await call(page, `/api/v1/source-materials/${detail.sourceId}/content`)
    expect(frozenSource.status).toBe(200)
    expect(frozenSource.body).toBe(submittedText)
    evidence.flow = { batchId: batch.id, modelSuggestionBody: detail.originalSuggestion.body, sourceMaterialId: detail.sourceId, sourceMaterialDigest: sourceMeta.digest, confirmedKnowledgeId: knowledgeID, cancelledBatchId: slow.id, rows: observed }
    expect(observed).toHaveLength(2)
    expect(observed.find((row) => String(row.id) === slow.id)?.state).toBe('Cancelled')
    expect(observed.find((row) => String(row.id) === slow.id)?.attempt_state).toBe('Cancelled')
    expect(observed.find((row) => String(row.id) === batch.id)?.search_docs).toBe(0)
    record(evidence)
  })
})
