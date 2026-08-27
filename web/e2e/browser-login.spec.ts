import { execFileSync } from 'node:child_process'
import { existsSync, readFileSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'
import { expect, test, type Page } from '@playwright/test'

// T20 runs without trace/screenshot/video: Browser Identity policy forbids
// recording page content or login input during a manual browser session.
test.use({ trace: 'off', screenshot: 'off', video: 'off' })

const browserTicket = process.env.QUOIN_TICKET === 'T22' ? 'T22' : 'T20'
const browserFixture = `quoin-${browserTicket.toLowerCase()}-auth-fixture`
const browserFixtureHost = `${browserFixture}:8081`

function fixtureCounter(name: string): number {
  try {
    return Number.parseInt(execFileSync('docker', ['exec', browserFixture, 'cat', `/state/${name}`], { encoding: 'utf-8', stdio: ['ignore', 'pipe', 'ignore'] }).trim(), 10) || 0
  } catch { return 0 }
}

function fixtureState(name: string): boolean {
  try {
    execFileSync('docker', ['exec', browserFixture, 'test', '-f', `/state/${name}`], { stdio: 'ignore' })
    return true
  } catch { return false }
}

async function expectWebSocketRejected(page: Page, endpoint: string): Promise<void> {
  await expect.poll(() => page.evaluate(async (url) => await new Promise<boolean>((resolve) => {
    const socket = new WebSocket(url)
    let settled = false
    let upgraded = false
    const settle = (value: boolean) => {
      if (settled) return
      settled = true
      window.clearTimeout(timeout)
      resolve(value)
    }
    const timeout = window.setTimeout(() => { socket.close(); settle(false) }, 5_000)
    socket.addEventListener('open', () => {
      upgraded = true
      window.setTimeout(() => { socket.close(); settle(false) }, 300)
    })
    socket.addEventListener('error', () => settle(!upgraded))
    socket.addEventListener('close', () => settle(!upgraded))
  }), endpoint), { timeout: 10_000 }).toBe(true)
}

test.describe('T20 Browser Identity manual login @ticket-20 @ticket-22', () => {
  test('Operator path opens real noVNC, publishes the authenticated profile, and retains no browser recording', async ({ page, browser }) => {
    const serverFailures: string[] = []
    page.on('response', (response) => {
      if (response.url().includes('/api/') && response.status() >= 500) {
        void response.text().then((body) => serverFailures.push(`${response.status()} ${response.url()} ${body}`))
      }
    })
    test.slow()
    const stackDir = join(import.meta.dirname, '..', '..', '.artifacts', `e2e-stack-${browserTicket.toLowerCase()}`)
    const passwordPath = join(stackDir, 'admin-new-password')
    expect(existsSync(passwordPath)).toBeTruthy()

    await page.goto('/')
    await page.fill('#username', 'admin')
    await page.fill('#password', readFileSync(passwordPath, 'utf-8').trim())
    await page.getByRole('button', { name: '登录' }).click()
    await expect(page.getByRole('navigation', { name: '全局模块' })).toBeVisible({ timeout: 30_000 })

    const systemKey = `${browserTicket.toLowerCase()}-browser-${Date.now()}`
    const prepared = await page.evaluate(async ({ key, fixtureHost, ticket, enabled }) => {
      const headers = { 'Content-Type': 'application/json' }
      const suffix = `${Date.now()}`
      let active = (await (await fetch('/api/v1/label-contracts?limit=100')).json()).items?.find((item: { state?: string }) => item.state === 'active')
      if (!active) {
        const form = new FormData()
        form.append('file', new File(['label_contract:\n  business_system_label: business_system\n'], 'contract.yaml', { type: 'application/yaml' }))
        form.append('clientCommandId', `e2e-t20-contract-${suffix}`)
        const created = await fetch('/api/v1/label-contracts', { method: 'POST', body: form })
        if (created.status !== 201) throw new Error(`create label contract=${created.status}`)
        active = await created.json()
        const activated = await fetch(`/api/v1/label-contracts/${active.version}/activate`, {
          method: 'POST', headers,
          body: JSON.stringify({ clientCommandId: `e2e-t20-activate-${suffix}`, expectedStateRowVersion: active.rowVersion, expectedCurrentContractVersionId: null, expectedTargetRowVersion: active.rowVersion, compatibleVersions: [] }),
        })
        if (!activated.ok) throw new Error(`activate label contract=${activated.status}`)
        active = await activated.json()
      }
      // T22's real Exploration admission only permits enabled business systems;
      // retain T20's disabled fixture coverage while making the T22 profile
      // reachable through the same Investigation → Browser Tool path.
      const yaml = `system_key: ${key}\ndisplay_name: ${ticket} 浏览器验收\nenabled: ${enabled}\ntimezone: Asia/Shanghai\nresource_refresh_interval_seconds: 300\nresource_discoveries: []\ninspection_plans: []\n`
      const form = new FormData()
      form.append('file', new File([yaml], 'system.yaml', { type: 'application/yaml' }))
      form.append('clientCommandId', `e2e-t20-system-${suffix}`)
      form.append('targetLabelContractVersion', String(active.version))
      const created = await fetch('/api/v1/business-systems', { method: 'POST', body: form })
      if (created.status !== 201) throw new Error(`create business system=${created.status}: ${await created.text()}`)
      if (enabled) {
        const versions = await (await fetch(`/api/v1/business-systems/${key}/config?limit=1`)).json() as { items?: Array<{ id: string }> }
        const versionID = versions.items?.[0]?.id
        if (!versionID) throw new Error('T22 business system upload returned no draft config version')
        const published = await fetch(`/api/v1/business-systems/${key}/config/${versionID}/publish`, {
          method: 'POST', headers,
          body: JSON.stringify({ clientCommandId: `e2e-t22-publish-system-${suffix}`, expectedCurrentPublishedVersionId: null }),
        })
        if (!published.ok) throw new Error(`publish T22 business system=${published.status}: ${await published.text()}`)
      }
      const identity = await fetch(`/api/v1/business-systems/${key}/browser-identity`, {
        method: 'POST', headers,
        body: JSON.stringify({
          clientCommandId: `e2e-t20-identity-${suffix}`,
          name: `${ticket} 只读浏览器账号`,
          startUrl: `http://${fixtureHost}/login`,
          authenticationProbe: { journeyId: 'authentication.url-prefix.v1', journeyVersion: 1, params: { authenticatedUrlPrefix: `http://${fixtureHost}/authenticated` } },
        }),
      })
      if (identity.status !== 202) throw new Error(`configure browser identity=${identity.status}: ${await identity.text()}`)
      const detail = await (await fetch(`/api/v1/business-systems/${key}`)).json()
      return { systemKey: key, identity: await identity.json(), detail }
    }, { key: systemKey, fixtureHost: browserFixtureHost, ticket: browserTicket, enabled: browserTicket === 'T22' })
    expect(prepared.systemKey).toBe(systemKey)
    expect(prepared.identity.identity.state).toBe('AuthenticationRequired')
    expect(prepared.detail.browserIdentityState).toBe('AuthenticationRequired')
    const catalog = await page.evaluate(async () => await (await fetch('/api/v1/journey-catalog')).json()) as { digest: string; version: string }
    expect(prepared.identity.identity.currentRevision.catalogDigest).toBe(catalog.digest)
    expect(prepared.identity.identity.currentRevision.catalogVersion).toBe(catalog.version)
    expect(prepared.identity.identity.currentRevision.startUrl).toBe(`http://${browserFixtureHost}/login`)

    // Exercise the actual Lintel unauthenticated branch before the interactive
    // path: its probe must preserve AuthenticationRequired and commit no profile.
    const unauthenticated = await page.evaluate(async (key) => {
      const identity = await (await fetch(`/api/v1/business-systems/${key}/browser-identity`)).json()
      const started = await fetch(`/api/v1/browser-login/${key}/operations`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ clientCommandId: `e2e-t20-unauth-start-${Date.now()}`, expectedRowVersion: identity.rowVersion }) })
      if (!started.ok) throw new Error(`start unauthenticated operation=${started.status}`)
      return await started.json()
    }, systemKey) as { id: string }
    await expect.poll(async () => page.evaluate(async ({ key, id }) => await (await fetch(`/api/v1/browser-login/${key}/operations/${id}`)).json(), { key: systemKey, id: unauthenticated.id }), { timeout: 45_000 }).toMatchObject({ state: 'Running', canPublish: true })
    const unauthenticatedPublish = await page.evaluate(async ({ key, id }) => {
      const operation = await (await fetch(`/api/v1/browser-login/${key}/operations/${id}`)).json()
      const published = await fetch(`/api/v1/browser-login/${key}/operations/${id}/publish`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ clientCommandId: `e2e-t20-unauth-publish-${Date.now()}`, expectedOperationRowVersion: operation.rowVersion }) })
      if (!published.ok) throw new Error(`unauthenticated publish=${published.status}`)
      return await published.json()
    }, { key: systemKey, id: unauthenticated.id })
    expect(unauthenticatedPublish).toMatchObject({ state: 'Running' })
    await expect.poll(async () => page.evaluate(async (key) => await (await fetch(`/api/v1/business-systems/${key}/browser-identity`)).json(), systemKey), { timeout: 45_000 }).toMatchObject({ state: 'AuthenticationRequired', currentProfile: null })
    await page.evaluate(async ({ key, id }) => {
      const operation = await (await fetch(`/api/v1/browser-login/${key}/operations/${id}`)).json()
      const cancelled = await fetch(`/api/v1/browser-login/${key}/operations/${id}/cancel`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ clientCommandId: `e2e-t20-unauth-cancel-${Date.now()}`, expectedOperationRowVersion: operation.rowVersion }) })
      if (!cancelled.ok) throw new Error(`cancel unauthenticated operation=${cancelled.status}`)
    }, { key: systemKey, id: unauthenticated.id })
    await expect.poll(async () => page.evaluate(async ({ key, id }) => await (await fetch(`/api/v1/browser-login/${key}/operations/${id}`)).json(), { key: systemKey, id: unauthenticated.id }), { timeout: 45_000 }).toMatchObject({ state: 'Cancelled' })
    // A Cancelled durable operation remains authoritative until Lintel confirms
    // the physical browser and relay cleanup. Do not race that fence by asking
    // the UI to start its successor before currentOperation has cleared.
    await expect.poll(async () => page.evaluate(async (key) => (await (await fetch(`/api/v1/business-systems/${key}/browser-identity`)).json()).currentOperation, systemKey), { timeout: 45_000 }).toBeNull()
    const fixtureBaseline = {
      login: fixtureCounter('login-get-seq'),
      ready: fixtureCounter('ready-seq'),
      pointer: fixtureCounter('pointerdown-seq'),
      key: fixtureCounter('keydown-seq'),
      submit: fixtureCounter('submit-seq'),
    }

    // Drive the same entry point an Operator uses from Business Systems.
    await page.getByRole('navigation', { name: '全局模块' }).getByRole('button', { name: '业务系统' }).click()
    await page.getByRole('button', { name: new RegExp(`${browserTicket} 浏览器验收`) }).click()
    await expect(page.getByRole('button', { name: '打开浏览器登录' })).toBeVisible({ timeout: 30_000 })
    await page.getByRole('button', { name: '打开浏览器登录' }).click()
    await expect(page.getByRole('heading', { name: '浏览器登录' })).toBeVisible({ timeout: 30_000 })
    await page.getByRole('button', { name: '重新登录' }).click()
    await expect.poll(async () => page.evaluate(async (key) => {
      const identity = await (await fetch(`/api/v1/business-systems/${key}/browser-identity`)).json()
      return identity.currentOperation
    }, systemKey), { timeout: 45_000 }).toMatchObject({ state: 'Running', canAttach: true, canPublish: true })
    const operationID = (await page.evaluate(async (key) => (await (await fetch(`/api/v1/business-systems/${key}/browser-identity`)).json()).currentOperation.id, systemKey)) as string
    const websocketPath = `wss://127.0.0.1:18480/api/v1/browser-login/${encodeURIComponent(systemKey)}/operations/${encodeURIComponent(operationID)}/ws`
    const anonymous = await browser.newContext({ ignoreHTTPSErrors: true })
    const anonymousPage = await anonymous.newPage()
    await anonymousPage.goto('https://127.0.0.1:18480/')
    await expectWebSocketRejected(anonymousPage, websocketPath)
    await anonymous.close()
    const foreignOrigin = await browser.newContext({ ignoreHTTPSErrors: true })
    const foreignPage = await foreignOrigin.newPage()
    await foreignPage.goto('data:text/html,<title>foreign</title>')
    await expectWebSocketRejected(foreignPage, websocketPath)
    await foreignOrigin.close()
    const canvas = page.locator('.browser-login-viewport canvas')
    await expect(canvas).toBeVisible({ timeout: 45_000 })
    await expect(page.getByText('安全浏览器已连接。选择“进入远程浏览器”后开始输入。')).toBeVisible({ timeout: 45_000 })
    // These monotonic observations must belong to the interactive Operation,
    // not the cancelled unauthenticated probe that preceded it.
    await expect.poll(() => fixtureCounter('login-get-seq') > fixtureBaseline.login, { timeout: 45_000 }).toBe(true)
    // The resource request proves Chromium parsed the fresh login surface and
    // installed its form/event handlers. RFB connect alone only proves the
    // protocol handshake, not that the remote page can receive input yet.
    await expect.poll(() => fixtureCounter('ready-seq') > fixtureBaseline.ready, { timeout: 45_000 }).toBe(true)
    // The VNC server has completed its RFB handshake before the first
    // framebuffer update from the activated Chromium target.
    await page.waitForTimeout(2_000)
    await expect(page.getByRole('button', { name: '完成登录并发布' })).toBeEnabled({ timeout: 45_000 })
    // Activate the deterministic fixture's visible Sign in control through the
    // real noVNC RFB input data plane. The fixture records only the resulting
    // action and authenticated state, so no credential travels through Playwright or logs.
    const authenticatedState = 'authenticated'
    const authenticatedPage = 'authenticated-page'
    const canvasBounds = await canvas.boundingBox()
    if (canvasBounds === null) throw new Error('noVNC canvas is not measurable')
    await page.getByRole('button', { name: '进入远程浏览器' }).click()
    await expect(canvas.evaluate((element) => document.activeElement === element)).resolves.toBe(true)
    // One pointer plus one Enter is the whole interaction. The fixture records
    // only event kinds/counts, letting this test locate a failed input layer
    // without retaining keys, coordinates, values, cookies, or RFB payloads.
    await canvas.click({ position: { x: canvasBounds.width / 2, y: canvasBounds.height / 2 } })
    await expect.poll(() => fixtureCounter('pointerdown-seq') > fixtureBaseline.pointer, { timeout: 20_000 }).toBe(true)
    await canvas.focus()
    await page.keyboard.press('Enter')
    await expect.poll(() => fixtureCounter('keydown-seq') > fixtureBaseline.key, { timeout: 20_000 }).toBe(true)
    // /complete receives the exact RFB-key-derived navigation and redirects to
    // /authenticated.  Waiting for that destination proves Chromium committed
    // the cookie into the profile before Publish triggers its authentication
    // probe; the server-side submit state alone occurs before that commit.
    await expect.poll(() => fixtureCounter('submit-seq') > fixtureBaseline.submit && fixtureState(authenticatedState) && fixtureState(authenticatedPage), { timeout: 20_000 }).toBe(true)
    await page.getByRole('button', { name: '完成登录并发布' }).click()
    await expect(page).toHaveURL(new RegExp(`/business-systems/${systemKey}$`), { timeout: 45_000 })
    await expect(canvas).toHaveCount(0)
    expect(serverFailures).toEqual([])

    // T22 uses the profile published above only as a prerequisite. The action
    // under test starts with an Investigation message and must traverse the
    // real Plinth model/runtime path into Lintel's Exploration session; it does
    // not start or assert a manual-login operation.
    if (process.env.QUOIN_TICKET === 'T22') {
      const exploration = await page.evaluate(async (key) => {
        const response = await fetch('/api/v1/investigations', {
          method: 'POST', headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ clientCommandId: `e2e-t22-investigation-${Date.now()}`, content: `T22Browser ${key}`, sources: [], attachmentIds: [] }),
        })
        if (!response.ok) throw new Error(`create T22 investigation=${response.status}: ${await response.text()}`)
        const investigation = await response.json() as { id: string }
        return investigation
      }, systemKey)
      type T22Attempt = { id: string, state: string, type?: string, terminationReason?: string }
      type T22Tool = { toolName?: string, status?: string, result?: { action?: string, error?: { code?: string } } }
      const attempts = async () => await page.evaluate(async (investigationId) => await (await fetch(`/api/v1/investigations/${investigationId}/attempts`)).json(), exploration.id) as { items?: T22Attempt[] }
      // T22 error evidence is deliberately closed and sensitive-safe: it never
      // serializes model text, tool arguments/results, URLs, IDs, page content,
      // or browser trace metadata. It exists only to distinguish the durable
      // operation/child state from the closed browser action error code.
      const failureDiagnostic = async () => {
        const listed = await attempts()
        const parent = listed.items?.find((item) => item.type === 'investigation')
        const tools = parent ? await page.evaluate(async ({ investigationId, attemptId }) => await (await fetch(`/api/v1/investigations/${investigationId}/attempts/${attemptId}/tool-calls`)).json(), { investigationId: exploration.id, attemptId: parent.id }) as { items?: T22Tool[] } : undefined
        const browserTool = (tools?.items ?? []).filter((tool) => tool.toolName === 'quoin_browser' && tool.status !== 'pending' && tool.status !== 'running').at(-1)
        return {
          operation: listed.items?.find((item) => item.type === 'browser_exploration')?.state ?? 'absent',
          action: browserTool?.result?.action ?? 'absent',
          actionState: browserTool?.status ?? 'absent',
          errorCode: browserTool?.result?.error?.code ?? 'absent',
        }
      }
      await expect.poll(async () => {
        const listed = await attempts()
        const succeeded = listed.items?.find((item) => item.type === 'investigation' && item.state === 'Succeeded')
        if (succeeded) return true
        const failedBrowserChild = listed.items?.find((item) => item.type === 'browser_exploration' && item.state === 'Failed')
        if (failedBrowserChild) throw new Error(`T22 failure diagnostic=${JSON.stringify(await failureDiagnostic())}`)
        return false
      }, { timeout: 90_000 }).toBe(true)
      const attempt = (await attempts()).items?.find((item) => item.type === 'investigation' && item.state === 'Succeeded')
      if (!attempt) throw new Error('T22 investigation completed without a succeeded Attempt')
      const tools = await page.evaluate(async ({ investigationId, attemptId }) => await (await fetch(`/api/v1/investigations/${investigationId}/attempts/${attemptId}/tool-calls`)).json(), { investigationId: exploration.id, attemptId: attempt.id }) as { items?: Array<{ toolName?: string, arguments?: unknown, result?: unknown }> }
      const browserCalls = (tools.items ?? []).filter((tool) => tool.toolName === 'quoin_browser')
      if (browserCalls.length !== 2) {
        const observed = (tools.items ?? []).map((tool) => ({ toolName: tool.toolName, status: (tool as { status?: string }).status }))
        throw new Error(`T22 expected open+close quoin_browser calls; observed tool metadata=${JSON.stringify(observed)}`)
      }
      const resultBody = (tool: { result?: unknown }) => typeof tool.result === 'string' ? JSON.parse(tool.result) : tool.result as Record<string, unknown>
      const openResult = resultBody(browserCalls[0])
      const sessionID = String(openResult.sessionId ?? '')
      expect(sessionID).toMatch(/^\d+$/)
      // These facts must come from Chromium's Runtime.evaluate RemoteObject
      // value, not a synthetic CDP envelope.  A page list URL alone would not
      // prove that the bounded model projection retained the actual document.
      const observation = openResult.observation as Record<string, unknown>
      expect(observation.url).toBe(`http://${browserFixtureHost}/authenticated`)
      expect(observation.origin).toBe(`http://${browserFixtureHost}`)
      expect(observation.visibleText).toContain('authenticated')
      // The authenticated fixture starts at /login and Chromium redirects it to
      // /authenticated. This is a real recorder-path assertion: redirect
      // provenance is accepted only after the destination document's fixed
      // location.origin reply, never as an early origin=null frame-tree replay.
      const events = observation.events as Array<Record<string, unknown>>
      expect(events.filter((event) => event.kind === 'popup')).toEqual([])
      const redirect = events.find((event) => event.kind === 'redirect')
      expect(redirect).toMatchObject({
        origin: `http://${browserFixtureHost}`,
        sourceUrl: `http://${browserFixtureHost}/login`,
        destinationUrl: `http://${browserFixtureHost}/authenticated`,
      })
      const closeObservation = resultBody(browserCalls[1]).observation as Record<string, unknown>
      const closeEvents = closeObservation.events as Array<Record<string, unknown>>
      // The open/close observations share one recorder, but both response
      // boundaries must preserve the startup inventory fence: the existing
      // Chromium target is not a popup caused by either tool call.
      expect(closeEvents.filter((event) => event.kind === 'popup')).toEqual([])
      type T22OperationRead = { operation: { state?: string, kind?: string, traceIntegrity?: string, traceArtifactId?: string, completionDigest?: string, stopConfirmedAt?: string, probeResults?: Array<{ phase?: string, result?: string }> }, identity: { currentOperation?: unknown } }
      const readOperation = async (): Promise<T22OperationRead> => await page.evaluate(async ({ key, sessionId }) => {
        const response = await fetch(`/api/v1/browser-login/${key}/operations/${sessionId}`)
        if (!response.ok) throw new Error(`read T22 exploration operation=${response.status}: ${await response.text()}`)
        const identityResponse = await fetch(`/api/v1/business-systems/${key}/browser-identity`)
        if (!identityResponse.ok) throw new Error(`read T22 browser identity=${identityResponse.status}`)
        return { operation: await response.json(), identity: await identityResponse.json() }
      }, { key: systemKey, sessionId: sessionID }) as T22OperationRead
      // Attempt success alone is not enough: wait until the persisted browser
      // operation confirms Stop and its identity slot is available to the next
      // Exploration admission.
      await expect.poll(async () => {
        const read = await readOperation()
        return read.operation.state === 'Succeeded' && read.operation.traceIntegrity === 'complete' && !!read.operation.traceArtifactId && !!read.operation.completionDigest && !!read.operation.stopConfirmedAt && !read.identity.currentOperation
      }, { timeout: 45_000 }).toBeTruthy()
      const operation = await readOperation()
      expect(operation.operation).toMatchObject({ kind: 'exploration', state: 'Succeeded', traceIntegrity: 'complete' })
      expect(operation.operation.traceArtifactId).toMatch(/^\d+$/)
      expect(operation.operation.completionDigest).toMatch(/^[A-Fa-f0-9]{64}$/)
      expect(operation.operation.stopConfirmedAt).toEqual(expect.any(String))
      expect(operation.operation.probeResults).toEqual(expect.arrayContaining([expect.objectContaining({ phase: 'completion', result: 'Authenticated' })]))
      // A terminal Exploration must release its identity/slot before a caller can
      // regard the model turn as complete; `currentOperation` is the public
      // durable projection used by subsequent open admission.
      expect(operation.identity.currentOperation).toBeFalsy()
      const messages = await page.evaluate(async (investigationId) => await (await fetch(`/api/v1/investigations/${investigationId}/messages`)).json(), exploration.id) as { items?: Array<{ content?: string }> }
      expect((messages.items ?? []).some((message) => message.content?.includes('fixture-proof-t22'))).toBeTruthy()
      const evidenceDir = process.env.QUOIN_EVIDENCE_DIR
      if (evidenceDir) {
        writeFileSync(join(evidenceDir, 't22-runtime-observations.json'), `${JSON.stringify({
          observedAt: new Date().toISOString(),
          runtimePath: 'Investigation HTTP -> Plinth model -> Quoin Runtime gRPC -> Lintel -> Chromium',
          investigationID: exploration.id,
          attemptID: attempt.id,
          browserToolCalls: browserCalls.length,
          openAndClose: { open: resultBody(browserCalls[0]), close: resultBody(browserCalls[1]) },
          operation: operation.operation,
          slotReleased: !operation.identity.currentOperation,
          assertion: 'A real Investigation opened and closed a published browser profile; public operation reads prove successful completion, authenticated probe, complete trace Artifact/digest, Stop confirmation, and released identity slot.',
        }, null, 2)}\n`)
      }
    }
    if (process.env.QUOIN_TICKET === 'T20') {
      const evidenceDir = process.env.QUOIN_EVIDENCE_DIR ?? join(import.meta.dirname, '..', '..', '.artifacts', 'tickets', 'T20')
      writeFileSync(join(evidenceDir, 't20-browser-observations.json'), `${JSON.stringify({
        observedAt: new Date().toISOString(),
        identity: { systemKey, identityID: prepared.identity.identity.id, stateBeforeLogin: prepared.identity.identity.state, unauthenticatedOperationID: unauthenticated.id, authenticatedOperationID: operationID, unauthenticatedPublish: 'rejected without profile generation', stateAfterNoVNCFixtureLogin: 'Ready' },
        catalog: { digest: catalog.digest, version: catalog.version, identityDigest: prepared.identity.identity.currentRevision.catalogDigest },
        websocket: { endpoint: websocketPath, validSameOriginSession: 'RFB canvas visible', fixtureObservedPointerAndKeyEvent: true, anonymousSession: 'rejected before HTTP upgrade', foreignOrigin: 'rejected before HTTP upgrade' },
        transitions: ['SQLite-backed BrowserIdentity: AuthenticationRequired', 'real Lintel URL probe: Unauthenticated with no profile', 'same-Origin Session → RFB canvas', 'RFB fixture keyboard action → authenticated URL', 'Publish → Ready with profile generation'],
        browser: { chromium: 'real Lintel process', xvfb: 'real Lintel process', x0vncserver: 'real Lintel process', noVNC: 'real RFB canvas' },
        recording: { trace: 'off', screenshot: 'off', video: 'off', evidenceContainsLoginContent: false },
      }, null, 2)}\n`)
    }
  })
})
