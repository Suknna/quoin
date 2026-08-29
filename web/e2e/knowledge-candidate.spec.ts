import { execFileSync } from 'node:child_process'
import { existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'
import { expect, test, type Page } from '@playwright/test'

test.use({ trace: 'off', screenshot: 'off', video: 'off' })

// T27 acceptance: record diagnostic feedback on the immutable analysis
// output and an assistant message, organize each into a Knowledge
// Candidate through the real HTTP/UI path, prove the original suggestion
// immutability, the revisioned draft with its conflict envelope,
// create-or-return idempotency, and the human confirmation boundary, then
// observe the durable SQLite state read-only. Fixture code never writes
// product tables.

type Candidate = {
  id: string
  sourceType: string
  sourceId: string
  state: string
  rowVersion: number
  draftRevision: number
  draftTitle?: string
  draftBody?: string
  confirmedKnowledgeId?: string
  originalSuggestion?: { v: number; source: { type: string; id: string; modelId?: string; locator?: Record<string, unknown> }; title: string; body: string }
}

type Knowledge = { id: string; title: string; currentVersionId: string; currentVersionSeq: number; eligible: boolean }

async function rawCall(page: Page, url: string, init?: RequestInit): Promise<{ status: number; body: string }> {
  return page.evaluate(async ({ url, init }) => {
    const response = await fetch(url, init)
    const body = await response.text()
    return { status: response.status, body } as const
  }, { url, init })
}

async function api<T>(page: Page, url: string, init?: RequestInit): Promise<T> {
  const result = await rawCall(page, url, init)
  if (result.status >= 300) throw new Error(`${url}: ${result.status} ${result.body}`)
  return JSON.parse(result.body || '{}') as T
}

function command(): string {
  return `t27-${Date.now()}-${Math.random().toString(36).slice(2, 10)}`
}

function observations(value: unknown) {
  const dir = process.env.QUOIN_EVIDENCE_DIR
  if (!dir) return
  mkdirSync(dir, { recursive: true })
  writeFileSync(join(dir, 't27-knowledge-observations.json'), JSON.stringify(value, null, 2))
}

// Read-only SQLite observation of the real Quoin database (the T26
// precedent); the fixture never writes product tables.
function sqliteRows(query: string): Array<Record<string, unknown>> {
  const mountJSON = execFileSync('docker', ['inspect', '--format', '{{json .Mounts}}', 'quoin-quoin-1'], { encoding: 'utf8' }).trim()
  const mounts = JSON.parse(mountJSON) as Array<{ Source: string; Destination: string }>
  const dataDirectory = mounts.find((mount) => mount.Destination.includes('data'))?.Source
  if (!dataDirectory) throw new Error(`T27 Quoin data mount unavailable: ${mountJSON}`)
  const database = join(dataDirectory, 'quoin.db')
  const program = 'import json,sqlite3,sys; c=sqlite3.connect("file:"+sys.argv[1]+"?mode=ro", uri=True); c.row_factory=sqlite3.Row; print(json.dumps([dict(r) for r in c.execute(sys.argv[2])]))'
  return JSON.parse(execFileSync('python3', ['-c', program, database, query], { encoding: 'utf8' })) as Array<Record<string, unknown>>
}

test.describe('T27 诊断反馈与知识确认 @ticket-27', () => {
  test('记录反馈、整理为知识、修订草稿并人工确认，观察来源闭合与幂等边界', async ({ page }) => {
    test.slow(); test.setTimeout(900_000)
    const stackDir = join(import.meta.dirname, '..', '..', '.artifacts', 'e2e-stack')
    const passwordPath = join(stackDir, 'admin-new-password')
    expect(existsSync(passwordPath)).toBeTruthy()
    const evidence: Record<string, unknown> = { startedAt: new Date().toISOString() }

    await page.goto('/')
    await page.fill('#username', 'admin')
    await page.fill('#password', readFileSync(passwordPath, 'utf-8').trim())
    await page.getByRole('button', { name: '登录' }).click()
    await expect(page.getByRole('navigation', { name: '全局模块' })).toBeVisible({ timeout: 30_000 })

    // ---------- Part 1: Initial Analysis output source -----------------
    await page.getByRole('navigation', { name: '全局模块' }).getByRole('button', { name: '告警' }).click()
    await page.getByText('T11Thanosa', { exact: false }).first().click()
    await expect(page.getByRole('heading', { name: /T11Thanosa/ })).toBeVisible()
    if (await page.getByRole('button', { name: '初步分析', exact: true }).isVisible().catch(() => false)) {
      await page.getByRole('button', { name: '初步分析', exact: true }).click()
    }
    await expect(page.getByText('已完成', { exact: true }).first()).toBeVisible({ timeout: 180_000 })
    await page.getByRole('button', { name: '查看完整分析' }).click()
    await expect(page.getByRole('document', { name: '初步分析详情' })).toBeVisible()

    // 整理为知识: the editor opens with the immutable suggestion.
    await page.getByRole('button', { name: '整理为知识' }).first().click()
    await expect(page.getByRole('document', { name: '知识候选编辑层' })).toBeVisible()
    await expect(page.getByText(/模型原始建议（不可修改）/)).toBeVisible()
    const candidateId = page.url().match(/\/knowledge\/candidates\/(\d+)$/)?.[1] ?? ''
    expect(candidateId).not.toBe('')
    const detail = await api<Candidate>(page, `/api/v1/knowledge/candidates/${candidateId}`)
    const suggestionBody = detail.originalSuggestion?.body ?? ''
    expect(suggestionBody.length).toBeGreaterThan(0)
    evidence.sourceLinks = {
      candidateId,
      sourceType: detail.sourceType,
      sourceId: detail.sourceId,
      suggestionSource: detail.originalSuggestion?.source,
      matchesAnalysisOutput: detail.sourceType === 'initial_analysis_output' && detail.originalSuggestion?.source.type === 'initial_analysis_output',
    }

    // Revisioned draft: edit title and the 适用范围 rows (UI-KNOWLEDGE-003),
    // save, observe the revision bump.
    const editedTitle = `T27 连接池处置 ${Date.now()}`
    await page.getByLabel('标题').fill(editedTitle)
    await page.getByRole('button', { name: '添加范围' }).click()
    await page.getByLabel('范围键 1').fill('业务系统')
    await page.getByLabel('范围值 1').fill('payments')
    await page.getByRole('button', { name: '保存草稿' }).click()
    await expect(page.getByText('草稿已保存。')).toBeVisible({ timeout: 30_000 })
    await expect(page.getByText(/r1/).first()).toBeVisible()

    // Stale revision: a direct API edit with the old expectation returns
    // the frozen conflict envelope carrying the authoritative revision.
    const staleEdit = await rawCall(page, `/api/v1/knowledge/candidates/${candidateId}`, {
      method: 'PATCH', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ clientCommandId: command(), expectedRevision: 0, title: '过期修改' }),
    })
    evidence.staleRevisionConflict = { status: staleEdit.status, body: JSON.parse(staleEdit.body || '{}') }
    expect(staleEdit.status).toBe(409)
    expect((JSON.parse(staleEdit.body) as { conflict: { currentRevision: number } }).conflict.currentRevision).toBe(1)

    // Original suggestion immutability after edits.
    const detailAfterEdit = await api<Candidate>(page, `/api/v1/knowledge/candidates/${candidateId}`)
    evidence.originalSuggestionImmutable = {
      before: suggestionBody,
      after: detailAfterEdit.originalSuggestion?.body,
      unchanged: suggestionBody === detailAfterEdit.originalSuggestion?.body,
      draftTitleChanged: detailAfterEdit.draftTitle === editedTitle,
    }
    expect(detailAfterEdit.originalSuggestion?.body).toBe(suggestionBody)
    expect(detailAfterEdit.draftTitle).toBe(editedTitle)

    // Create-or-return: a second organize on the same source returns the
    // same candidate with the frozen 200 and no duplicate row.
    const createAgain = await page.evaluate(async (candidateId: string) => {
      // The candidate's source locator carries the analysis link.
      const detail = await (await fetch(`/api/v1/knowledge/candidates/${candidateId}`)).json()
      const locator = detail.originalSuggestion.source.locator as { occurrenceId: number; analysisId: number }
      const response = await fetch(`/api/v1/alerts/${locator.occurrenceId}/analyses/${locator.analysisId}/knowledge-candidates`, {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ clientCommandId: `t27-return-${Date.now()}` }),
      })
      return { status: response.status, body: await response.json() } as const
    }, candidateId)
    const sameSourceCount = (await api<{ items: Candidate[] }>(page, '/api/v1/knowledge/candidates?sourceType=initial_analysis_output')).items.filter((item) => item.sourceId === detail.sourceId).length
    evidence.createOrReturn = {
      firstStatus: 201,
      secondStatus: createAgain.status,
      sameCandidate: createAgain.body.id === candidateId,
      candidatesForSource: sameSourceCount,
    }
    expect(createAgain.status).toBe(200)
    expect(createAgain.body.id).toBe(candidateId)
    expect(sameSourceCount).toBe(1)

    // Diagnosis feedback on the output (UI-FEEDBACK-001): two appends,
    // the latest-value projection and the full history.
    const browseBefore = await rawCall(page, '/api/v1/knowledge')
    await page.goBack()
    await expect(page.getByRole('document', { name: '初步分析详情' })).toBeVisible()
    await page.getByRole('button', { name: '已采纳' }).click()
    await expect(page.locator('.feedback-control .status-pill.feedback-adopted')).toBeVisible({ timeout: 30_000 })
    await page.getByRole('button', { name: '验证有效' }).click()
    await expect(page.locator('.feedback-control .status-pill.feedback-verified_effective')).toBeVisible({ timeout: 30_000 })
    await page.getByRole('button', { name: /查看反馈历史/ }).click()
    await expect(page.locator('.feedback-history')).toContainText('已采纳')
    const timeline = await api<{ latestValue: string; items: Array<{ value: string }> }>(page, `/api/v1/knowledge/feedback?targetType=initial_analysis_output&targetId=${detail.sourceId}`)
    evidence.feedbackTimelineShape = { latestValue: timeline.latestValue, itemCount: timeline.items.length }
    expect(timeline.latestValue).toBe('verified_effective')
    expect(timeline.items.length).toBe(2)

    // Human confirmation boundary: knowledge exists only after the human
    // confirms through the editor; a second command conflicts.
    await page.getByRole('button', { name: '整理为知识' }).first().click()
    await expect(page.getByRole('document', { name: '知识候选编辑层' })).toBeVisible()
    await page.getByRole('button', { name: '确认并创建知识' }).click()
    await expect(page.getByRole('article', { name: '知识详情' })).toBeVisible({ timeout: 30_000 })
    const knowledgeUrl = page.url()
    const knowledgeId = knowledgeUrl.match(/\/knowledge\/items\/(\d+)$/)?.[1] ?? ''
    expect(knowledgeId).not.toBe('')
    const secondConfirm = await rawCall(page, `/api/v1/knowledge/candidates/${candidateId}/confirm`, {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ clientCommandId: command(), expectedRevision: 1 }),
    })
    const knowledgeDetail = await api<Knowledge & { versionCount: number }>(page, `/api/v1/knowledge/items/${knowledgeId}`)
    evidence.confirmationBoundary = {
      browseItemsBeforeConfirm: (JSON.parse(browseBefore.body) as { items: unknown[] }).items.length,
      knowledgeId,
      versionCount: knowledgeDetail.versionCount,
      eligible: knowledgeDetail.eligible,
      secondConfirmStatus: secondConfirm.status,
    }
    expect(knowledgeDetail.versionCount).toBe(1)
    expect(knowledgeDetail.eligible).toBe(true)
    expect(secondConfirm.status).toBe(409)
    await expect(page.getByText(/共 1 个版本/)).toBeVisible()
    // The scope draft rides into the immutable version (DATA-KNOWLEDGE-001).
    const versionDetail = await api<{ scope?: Record<string, string> }>(page, `/api/v1/knowledge/items/${knowledgeId}/versions/${knowledgeDetail.currentVersionId}`)
    evidence.versionScope = versionDetail.scope ?? null
    expect(versionDetail.scope?.['业务系统']).toBe('payments')
    await expect(page.locator('.knowledge-scope').getByText('业务系统')).toBeVisible()
    await expect(page.locator('.knowledge-scope dd')).toHaveText('payments')

    // ---------- Part 2: assistant message source ------------------------
    await page.getByRole('navigation', { name: '全局模块' }).getByRole('button', { name: '告警' }).click()
    await page.getByText('T10Probe', { exact: false }).first().click()
    await page.getByRole('button', { name: '发起调查' }).click()
    await page.fill('#first-message', '请分析连接池告警并给出处置建议。')
    await page.getByRole('button', { name: '发送', exact: true }).click()
    await expect(page.locator('.chat-message-assistant').last()).toContainText('fixture-proof-t13', { timeout: 180_000 })
    const lastAssistant = page.locator('.chat-message-assistant').last()
    // Feedback on the assistant message (UI-FEEDBACK-001).
    await lastAssistant.getByRole('button', { name: '记录实际结果' }).click()
    await lastAssistant.getByRole('button', { name: '已执行' }).click()
    await expect(lastAssistant.locator('.status-pill.feedback-executed')).toBeVisible({ timeout: 30_000 })
    // Organize the assistant message into knowledge (UI-FEEDBACK-003).
    await lastAssistant.getByRole('button', { name: '整理为知识' }).click()
    await expect(page.getByRole('document', { name: '知识候选编辑层' })).toBeVisible()
    const messageCandidateId = page.url().match(/\/knowledge\/candidates\/(\d+)$/)?.[1] ?? ''
    expect(messageCandidateId).not.toBe('')
    const messageCandidate = await api<Candidate>(page, `/api/v1/knowledge/candidates/${messageCandidateId}`)
    evidence.messageSource = {
      candidateId: messageCandidateId,
      sourceType: messageCandidate.sourceType,
      sourceId: messageCandidate.sourceId,
      matchesInvestigationMessage: messageCandidate.sourceType === 'investigation_message',
    }
    expect(messageCandidate.sourceType).toBe('investigation_message')
    // Confirm through the command API with a replayable command id: the
    // same command returns the original knowledge; a different one
    // conflicts (DATA-KNOWLEDGE-004/006).
    const confirmCommand = command()
    const firstConfirm = await rawCall(page, `/api/v1/knowledge/candidates/${messageCandidateId}/confirm`, {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ clientCommandId: confirmCommand, expectedRevision: 0 }),
    })
    const replayConfirm = await rawCall(page, `/api/v1/knowledge/candidates/${messageCandidateId}/confirm`, {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ clientCommandId: confirmCommand, expectedRevision: 0 }),
    })
    const duplicateConfirm = await rawCall(page, `/api/v1/knowledge/candidates/${messageCandidateId}/confirm`, {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ clientCommandId: command(), expectedRevision: 0 }),
    })
    evidence.commandReplay = {
      firstStatus: firstConfirm.status,
      replayStatus: replayConfirm.status,
      sameKnowledge: JSON.parse(firstConfirm.body).confirmedKnowledgeId === JSON.parse(replayConfirm.body).confirmedKnowledgeId,
      duplicateStatus: duplicateConfirm.status,
    }
    expect(firstConfirm.status).toBe(200)
    expect(replayConfirm.status).toBe(200)
    expect(JSON.parse(firstConfirm.body).confirmedKnowledgeId).toBe(JSON.parse(replayConfirm.body).confirmedKnowledgeId)
    expect(duplicateConfirm.status).toBe(409)

    // ---------- Part 3: rejected boundary -------------------------------
    await page.getByRole('navigation', { name: '全局模块' }).getByRole('button', { name: '调查' }).click()
    await page.getByRole('button', { name: '新建调查' }).click()
    await page.fill('#first-message', '请给出另一条值得保留的结论。')
    await page.getByRole('button', { name: '发送', exact: true }).click()
    await expect(page.locator('.chat-message-assistant').last()).toContainText('fixture-proof-t13', { timeout: 180_000 })
    const rejectedAssistant = page.locator('.chat-message-assistant').last()
    await rejectedAssistant.getByRole('button', { name: '整理为知识' }).click()
    await expect(page.getByRole('document', { name: '知识候选编辑层' })).toBeVisible()
    const rejectedCandidateId = page.url().match(/\/knowledge\/candidates\/(\d+)$/)?.[1] ?? ''
    expect(rejectedCandidateId).not.toBe('')
    // Back to the chat, then mark the source rejected through the
    // confirming dialog (UI-FEEDBACK-002).
    await page.goBack()
    await expect(rejectedAssistant).toBeVisible()
    await rejectedAssistant.getByRole('button', { name: '记录实际结果' }).click()
    await rejectedAssistant.getByRole('button', { name: '不采纳' }).click()
    await expect(page.getByRole('dialog', { name: '确认标记为“不采纳”？' })).toBeVisible()
    await expect(page.getByText(/永久退出检索/)).toBeVisible()
    await page.getByRole('button', { name: '确认不采纳' }).click()
    await expect(rejectedAssistant.locator('.status-pill.feedback-rejected')).toBeVisible({ timeout: 30_000 })
    // The unconfirmed candidate became SourceInvalid and cannot confirm.
    const rejectedCandidate = await api<Candidate>(page, `/api/v1/knowledge/candidates/${rejectedCandidateId}`)
    const rejectedConfirm = await rawCall(page, `/api/v1/knowledge/candidates/${rejectedCandidateId}/confirm`, {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ clientCommandId: command(), expectedRevision: rejectedCandidate.draftRevision }),
    })
    // A fresh organize on the rejected source is refused (no new
    // candidate) — the create-or-return priority still returns nothing new.
    const rejectedCreate = await page.evaluate(async (sourceId: string) => {
      const match = window.location.pathname.match(/^\/investigations\/(\d+)/)
      if (!match) throw new Error('investigation context missing')
      const response = await fetch(`/api/v1/investigations/${match[1]}/knowledge-candidates`, {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ clientCommandId: `t27-rejected-${Date.now()}`, sourceType: 'investigation_message', sourceId }),
      })
      return { status: response.status, body: await response.json() } as const
    }, rejectedCandidate.sourceId)
    evidence.rejectedBoundary = {
      candidateState: rejectedCandidate.state,
      confirmStatus: rejectedConfirm.status,
      freshCreateStatus: rejectedCreate.status,
      freshCreateReturnedExisting: rejectedCreate.body?.id === rejectedCandidateId,
    }
    expect(rejectedCandidate.state).toBe('SourceInvalid')
    expect(rejectedConfirm.status).toBe(409)
    expect(rejectedCreate.status).toBe(200)
    expect(rejectedCreate.body.id).toBe(rejectedCandidateId)

    // ---------- Knowledge module projection -----------------------------
    await page.getByRole('navigation', { name: '全局模块' }).getByRole('button', { name: '知识' }).click()
    await expect(page.getByRole('heading', { name: '知识', exact: true })).toBeVisible()
    const knowledgeItems = (await api<{ items: Knowledge[] }>(page, '/api/v1/knowledge')).items
    evidence.browseAfter = knowledgeItems.map((item) => ({ id: item.id, title: item.title, eligible: item.eligible }))
    expect(knowledgeItems.length).toBe(2)
    await page.getByRole('button', { name: '待确认' }).click()
    await expect(page.getByText('来源无效').first()).toBeVisible({ timeout: 30_000 })

    // ---------- Durable read-only state ---------------------------------
    evidence.sqlite = {
      candidates: sqliteRows(`SELECT id,source_type,source_id,state,draft_revision,confirmed_knowledge_id FROM knowledge_candidates ORDER BY id`),
      knowledge: sqliteRows(`SELECT k.id,k.current_version_id FROM reusable_knowledge k ORDER BY k.id`),
      versions: sqliteRows(`SELECT id,knowledge_id,version_seq,title,source_candidate_id FROM knowledge_versions ORDER BY id`),
      retrieval: sqliteRows(`SELECT knowledge_version_id,exited,exit_reason FROM knowledge_version_retrieval_state ORDER BY knowledge_version_id`),
      searchDocs: sqliteRows(`SELECT knowledge_version_id,title FROM knowledge_search_docs ORDER BY knowledge_version_id`),
      feedback: sqliteRows(`SELECT target_type,target_id,value FROM diagnosis_feedback ORDER BY id`),
      fts: sqliteRows(`SELECT rowid FROM knowledge_fts ORDER BY rowid`),
    }
    evidence.finishedAt = new Date().toISOString()
    observations(evidence)
  })
})
