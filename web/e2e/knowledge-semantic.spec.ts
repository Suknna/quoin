import { execFileSync } from 'node:child_process'
import { existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'
import { expect, test, type Page } from '@playwright/test'

test.use({ trace: 'retain-on-failure', screenshot: 'only-on-failure', video: 'retain-on-failure' })

type Batch = { id: string; state: string; rowVersion: number; candidates: Array<{ id: string; state: string; draftRevision: number; draftTitle?: string; confirmedKnowledgeId?: string }> }
type SearchHit = { knowledge: { id: string; title: string }; score: number; indexState?: 'ready' | 'stale' | 'rebuilding' }
type QueryResult = { mode: 'query'; exactTextMatches: SearchHit[]; semanticMatches: SearchHit[]; nextCursor?: string }

function command() { return `t29-${Date.now()}-${Math.random().toString(36).slice(2, 10)}` }
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
  writeFileSync(join(dir, 't29-knowledge-observations.json'), JSON.stringify(value, null, 2))
}

// T29 semantic retrieval over the real stack: the confirmed knowledge embeds
// through a real Plinth embedding attempt against the deterministic fixture,
// the dual-channel search serves raw FTS and cosine results side by side,
// and a real model switch (dimension drift) rebuilds a fresh generation that
// atomically replaces the old one without mixing.
test.describe('T29 语义检索、双通道游标与换代重建 @ticket-29', () => {
  test('semantic channel builds, serves, drifts and never reactivates exited versions', async ({ page }) => {
    test.slow(); test.setTimeout(900_000)
    const stack = join(import.meta.dirname, '..', '..', '.artifacts', 'e2e-stack')
    expect(existsSync(join(stack, 'admin-new-password'))).toBeTruthy()
    const evidence: Record<string, unknown> = { startedAt: new Date().toISOString() }
    await page.goto('/')
    await page.fill('#username', 'admin')
    await page.fill('#password', readFileSync(join(stack, 'admin-new-password'), 'utf8').trim())
    await page.getByRole('button', { name: '登录' }).click()
    await expect(page.getByRole('navigation', { name: '全局模块' })).toBeVisible()

    async function importAndConfirm(text: string, title: string): Promise<string> {
      const started = await api<Batch>(page, '/api/v1/knowledge/import-batches', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ clientCommandId: command(), text }) })
      let batch = started
      for (let remaining = 180; remaining > 0 && batch.state === 'Processing'; remaining--) { await page.waitForTimeout(1000); batch = await api<Batch>(page, `/api/v1/knowledge/import-batches/${started.id}`) }
      expect(batch.state).toBe('AwaitingConfirmation')
      const candidate = batch.candidates[0]
      await api<void>(page, `/api/v1/knowledge/candidates/${candidate.id}`, { method: 'PATCH', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ clientCommandId: command(), expectedRevision: candidate.draftRevision, title }) })
      const confirmed = await api<Batch>(page, `/api/v1/knowledge/import-batches/${batch.id}/confirm`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ clientCommandId: command(), items: [{ candidateId: candidate.id, expectedRevision: candidate.draftRevision + 1 }] }) })
      expect(confirmed.state).toBe('Completed')
      const knowledgeID = confirmed.candidates[0].confirmedKnowledgeId as string
      expect(knowledgeID).toBeTruthy()
      return knowledgeID
    }

    const poolKnowledge = await importAndConfirm('T29 连接池治理：数据库连接池耗尽导致请求超时，先检查连接池上限与等待队列配置。', 'T29 连接池治理')
    const diskKnowledge = await importAndConfirm('T29 磁盘压力：节点磁盘压力持续升高触发驱逐，先排查大日志文件与未回收的临时卷。', 'T29 磁盘压力处置')
    evidence.confirmedKnowledge = { pool: poolKnowledge, disk: diskKnowledge }

    // The semantic index builds asynchronously through a real embedding
    // attempt on the connected Plinth; poll until the query mode serves it.
    let served: QueryResult | undefined
    for (let remaining = 120; remaining > 0; remaining--) {
      const query = await api<QueryResult>(page, '/api/v1/knowledge?q=' + encodeURIComponent('连接池治理'))
      if (query.semanticMatches.some((hit) => hit.knowledge.id === poolKnowledge)) { served = query; break }
      await page.waitForTimeout(1000)
    }
    if (!served) {
      const attempts = rows(`SELECT id,state,termination_reason,scope_id FROM execution_attempts WHERE attempt_type='embedding' ORDER BY id`)
      record({ ...evidence, semanticNeverServed: { attempts } })
      throw new Error(`semantic channel never served: ${JSON.stringify(attempts)}`)
    }
    expect(served.semanticMatches[0].indexState).toBe('ready')
    evidence.firstServe = {
      semantic: served.semanticMatches.map((hit) => ({ id: hit.knowledge.id, score: hit.score, indexState: hit.indexState })),
      exact: served.exactTextMatches.map((hit) => ({ id: hit.knowledge.id, score: hit.score })),
    }
    // The trigram channel ranks the exact phrase independently of cosine.
    expect(served.exactTextMatches.some((hit) => hit.knowledge.id === poolKnowledge)).toBeTruthy()

    // Dual cursor pagination: limit 1 pages both channels without loss; the
    // union of pages equals the unbounded result for each channel.
    const unbounded = await api<QueryResult>(page, '/api/v1/knowledge?q=' + encodeURIComponent('连接池治理'))
    const pageOne = await api<QueryResult>(page, '/api/v1/knowledge?limit=1&q=' + encodeURIComponent('连接池治理'))
    expect(pageOne.nextCursor).toBeTruthy()
    const pageTwo = await api<QueryResult>(page, '/api/v1/knowledge?limit=1&q=' + encodeURIComponent('连接池治理') + '&cursor=' + encodeURIComponent(pageOne.nextCursor as string))
    const mergedExact = [...pageOne.exactTextMatches, ...pageTwo.exactTextMatches].map((hit) => hit.knowledge.id).sort()
    const mergedSemantic = [...pageOne.semanticMatches, ...pageTwo.semanticMatches].map((hit) => hit.knowledge.id).sort()
    expect(mergedExact).toEqual(unbounded.exactTextMatches.map((hit) => hit.knowledge.id).sort())
    expect(mergedSemantic).toEqual(unbounded.semanticMatches.map((hit) => hit.knowledge.id).sort())
    evidence.pagination = { exact: mergedExact, semantic: mergedSemantic }

    // The UI renders both raw channels with their scores and index states.
    await page.goto('/knowledge')
    await page.getByLabel('检索知识').fill('连接池治理')
    await page.getByRole('button', { name: '搜索' }).click()
    // The frozen UI contract: a knowledge hit by both channels renders once
    // (in the exact-text section) with the dual-evidence annotation; the
    // semantic section carries the remaining cosine hits.
    await expect(page.getByRole('region', { name: '精确文本匹配' }).getByText('T29 连接池治理')).toBeVisible()
    await expect(page.getByRole('region', { name: '精确文本匹配' }).getByText('同时命中另一种检索')).toBeVisible()
    await expect(page.getByText('语义索引就绪').first()).toBeVisible()

    // First generation observations: current, dimension 16, ready vectors.
    const firstGeneration = rows(`SELECT id,model_name,state,COALESCE(vector_dim,0) AS vector_dim FROM embedding_generations ORDER BY generation`)
    const readyRows = rows(`SELECT COUNT(*) AS ready, MIN(length(vector)) AS bytes FROM embeddings WHERE state='ready'`)
    expect(Number(readyRows[0]?.ready)).toBeGreaterThanOrEqual(2)
    expect(Number(readyRows[0]?.bytes)).toBe(16 * 4)
    expect(firstGeneration.length).toBe(1)
    expect(firstGeneration[0]?.model_name).toBe('fixture-embed-1')
    expect(firstGeneration[0]?.state).toBe('current')
    evidence.firstGeneration = { rows: firstGeneration, ready: readyRows }

    // Dimension drift through the real admin path: disable the enabled
    // provider (the shared stack boots with one), qualify a wide-dimension
    // replacement and enable it.
    const connections = await api<{ items: Array<{ name: string; type: string; enabled: boolean; rowVersion: number }> }>(page, '/api/v1/connections?limit=100')
    const enabledProvider = connections.items.find((item) => item.type === 'model_provider' && item.enabled)
    expect(enabledProvider).toBeTruthy()
    await api<void>(page, `/api/v1/connections/${enabledProvider?.name}/disable`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ clientCommandId: command(), expectedRowVersion: enabledProvider?.rowVersion }) })
    const wideConfig = await api<{ items: Array<{ config: { baseUrl: string } }> }>(page, `/api/v1/connections/${enabledProvider?.name}/revisions?limit=1`)
    const baseUrl = wideConfig.items[0]?.config.baseUrl ?? ''
    expect(baseUrl).toContain('18443')
    await api<void>(page, '/api/v1/connections', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ clientCommandId: command(), name: 't29-wide-openai', connection: { type: 'model_provider', baseUrl, chatModelId: 'fixture-chat-1', embeddingModelId: 'fixture-embed-wide', contextBudgetTokens: 8192, maxOutputTokens: 1024, apiKey: 'fixture-api-key-2026' } }) })
    await api<void>(page, '/api/v1/connections/t29-wide-openai/probe', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ clientCommandId: command() }) })
    let wideProbeId = ''
    for (let remaining = 60; remaining > 0; remaining--) {
      const results = await api<{ items: Array<{ id: string; outcome: string }> }>(page, '/api/v1/connections/t29-wide-openai/probe-results')
      const passed = (results.items ?? []).find((item) => item.outcome === 'passed')
      if (passed) { wideProbeId = passed.id; break }
      await page.waitForTimeout(1000)
    }
    expect(wideProbeId).toBeTruthy()
    const wideRow = await api<{ rowVersion: number }>(page, '/api/v1/connections/t29-wide-openai')
    await api<void>(page, '/api/v1/connections/t29-wide-openai/enable', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ clientCommandId: command(), expectedRowVersion: wideRow.rowVersion, qualifiedProbeResultId: wideProbeId }) })

    // The drift sweep builds generation 2 at dimension 32 and atomically
    // retires generation 1; cosine never mixes the two.
    let drifted: Array<Record<string, unknown>> = []
    for (let remaining = 120; remaining > 0; remaining--) {
      drifted = rows(`SELECT model_name,state,COALESCE(vector_dim,0) AS vector_dim FROM embedding_generations ORDER BY generation`)
      const current = drifted.find((row) => row.state === 'current')
      if (current && current.model_name === 'fixture-embed-wide' && Number(current.vector_dim) === 32) break
      await page.waitForTimeout(1000)
    }
    const currentAfterDrift = drifted.find((row) => row.state === 'current')
    const retiredAfterDrift = drifted.filter((row) => row.state === 'retired')
    if (!currentAfterDrift || currentAfterDrift.model_name !== 'fixture-embed-wide' || Number(currentAfterDrift.vector_dim) !== 32) {
      record({ ...evidence, driftNeverSettled: { generations: drifted } })
      throw new Error(`wide generation never settled: ${JSON.stringify(drifted)}`)
    }
    expect(retiredAfterDrift.length).toBe(1)
    expect(retiredAfterDrift[0]?.model_name).toBe('fixture-embed-1')
    const wideVectors = rows(`SELECT length(vector) AS bytes FROM embeddings e JOIN embedding_generations g ON g.id=e.embedding_generation_id WHERE g.model_name='fixture-embed-wide' AND e.state='ready'`)
    expect(wideVectors.length).toBeGreaterThanOrEqual(2)
    for (const vector of wideVectors) expect(Number(vector.bytes)).toBe(32 * 4)
    evidence.drift = { generations: drifted, wideVectorBytes: wideVectors.map((vector) => Number(vector.bytes)) }

    // The semantic channel serves from the new generation after the switch.
    let postDrift: QueryResult | undefined
    for (let remaining = 60; remaining > 0; remaining--) {
      const query = await api<QueryResult>(page, '/api/v1/knowledge?q=' + encodeURIComponent('磁盘压力'))
      if (query.semanticMatches.some((hit) => hit.knowledge.id === diskKnowledge)) { postDrift = query; break }
      await page.waitForTimeout(1000)
    }
    expect(postDrift).toBeTruthy()
    expect(postDrift?.semanticMatches.every((hit) => hit.indexState === 'ready' || hit.indexState === 'rebuilding')).toBeTruthy()
    evidence.postDriftServe = postDrift?.semanticMatches.map((hit) => ({ id: hit.knowledge.id, score: hit.score, indexState: hit.indexState }))

    // No old-version reactivation: stop-reuse exits the version permanently
    // and both channels lose it immediately.
    const diskDetail = await api<{ currentVersionId: string }>(page, `/api/v1/knowledge/items/${diskKnowledge}`)
    const versions = await api<{ items: Array<{ id: string; retrievalStateRowVersion: number; eligible: boolean }> }>(page, `/api/v1/knowledge/items/${diskKnowledge}/versions`)
    const currentVersion = versions.items.find((version) => version.id === diskDetail.currentVersionId)
    expect(currentVersion?.eligible).toBeTruthy()
    await api<void>(page, `/api/v1/knowledge/items/${diskKnowledge}/versions/${diskDetail.currentVersionId}/stop-reuse`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ clientCommandId: command(), expectedRowVersion: currentVersion?.retrievalStateRowVersion ?? 1 }) })
    const afterStop = await api<QueryResult>(page, '/api/v1/knowledge?q=' + encodeURIComponent('磁盘压力'))
    expect(afterStop.exactTextMatches.some((hit) => hit.knowledge.id === diskKnowledge)).toBeFalsy()
    expect(afterStop.semanticMatches.some((hit) => hit.knowledge.id === diskKnowledge)).toBeFalsy()
    const stopRows = rows(`SELECT d.knowledge_version_id FROM knowledge_search_docs d JOIN knowledge_versions v ON v.id=d.knowledge_version_id WHERE v.knowledge_id=${Number(diskKnowledge)}`)
    expect(stopRows).toHaveLength(0)
    evidence.stopReuse = { knowledge: diskKnowledge, version: diskDetail.currentVersionId, exactHits: afterStop.exactTextMatches.length, semanticHits: afterStop.semanticMatches.length }

    evidence.finishedAt = new Date().toISOString()
    record(evidence)
  })
})
