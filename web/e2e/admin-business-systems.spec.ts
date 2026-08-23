import { readFileSync, existsSync } from 'node:fs'
import { join } from 'node:path'
import { expect, test } from '@playwright/test'

// T16 — the Admin upload/publish browser path over the real compose stack:
// field-error recovery that keeps the file, the valid upload landing on the
// immutable version detail, the explicit publish confirm switching the
// current pointer, and the deep-linked version detail surviving a reload.

const stackDir = join(import.meta.dirname, '..', '..', '.artifacts', 'e2e-stack')

function readAdminPassword(): string {
  const path = join(stackDir, 'admin-new-password')
  if (!existsSync(path)) throw new Error(`admin password fixture missing: ${path}`)
  return readFileSync(path, 'utf-8').trim()
}

const contractYAML = 'label_contract:\n  business_system_label: business_system\n'

const badSystemYAML = `system_key: browser-sys
display_name: 浏览器验收系统
enabled: false
timezone: Asia/Shanghai
resource_refresh_interval_seconds: 300
unknown_top_field: 1
resource_discoveries: []
inspection_plans: []
`

const goodSystemYAML = `system_key: browser-sys
display_name: 浏览器验收系统
enabled: false
timezone: Asia/Shanghai
resource_refresh_interval_seconds: 300
resource_discoveries:
  - key: web-pods
    display_name: Web Pods
    selector: 'up{business_system="browser-sys", job="web"}'
    identity_labels: [job, instance]
inspection_plans:
  - key: daily
    display_name: Daily
    cron: "30 8 * * *"
    checks:
      - key: up-instant
        display_name: Up Instant
        analysis_question: 当前可用吗？
        kind: promql
        query:
          mode: instant
          expression: 'up{business_system="browser-sys"}'
`

test.describe('T16 业务系统配置上传与发布 @ticket-16', () => {
  test('上传错误恢复、合法上传与发布的真实浏览器路径', async ({ page }) => {
    test.slow()
    await page.goto('/')
    await page.fill('#username', 'admin')
    await page.fill('#password', readAdminPassword())
    await page.getByRole('button', { name: '登录' }).click()
    await expect(page.getByRole('navigation', { name: '全局模块' })).toBeVisible({ timeout: 30_000 })

    // Fixture through the real production endpoints from inside the page, so
    // the authenticated session cookie and same-origin metadata ride along.
    const statuses = await page.evaluate(async (yaml) => {
      const commandSuffix = Date.now()
      const form = new FormData()
      form.append('file', new File([yaml], 'contract.yaml', { type: 'application/yaml' }))
      form.append('clientCommandId', `e2e-t16-contract-${commandSuffix}`)
      const create = await fetch('/api/v1/label-contracts', { method: 'POST', body: form })
      const activate = await fetch('/api/v1/label-contracts/1/activate', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          clientCommandId: `e2e-t16-activate-${commandSuffix}`,
          expectedStateRowVersion: 1,
          expectedCurrentContractVersionId: null,
          expectedTargetRowVersion: 1,
          compatibleVersions: [],
        }),
      })
      return { create: create.status, activate: activate.status }
    }, contractYAML)
    expect(statuses.create).toBe(201)
    expect(statuses.activate).toBe(200)

    // The module opens on the first-upload empty state.
    await page.getByRole('navigation', { name: '全局模块' }).getByRole('button', { name: '业务系统' }).click()
    await expect(page.getByText('还没有业务系统。')).toBeVisible({ timeout: 30_000 })
    await expect(page.getByText(/上传第一份配置 YAML 会创建一个 Disabled 业务系统/)).toBeVisible()

    // Upload layer: provenance is visible before any file is chosen.
    await page.getByRole('button', { name: '上传配置' }).click()
    const overlay = page.getByRole('dialog', { name: '上传业务系统配置' })
    await expect(overlay).toBeVisible()
    await expect(overlay.getByText(/Journey Catalog：/)).toBeVisible({ timeout: 15_000 })
    await expect(overlay.getByText(/v1-empty/)).toBeVisible()
    await expect(overlay.locator('#upload-target-contract')).toContainText('v1 · 当前激活')

    // The invalid file reports the exact unknown-field path and keeps the
    // chosen file for correction (UI-SYSTEM-004).
    await overlay.locator('input[type=file]').setInputFiles({
      name: 'bad.yaml',
      mimeType: 'application/yaml',
      buffer: Buffer.from(badSystemYAML),
    })
    await overlay.getByRole('button', { name: '上传并校验' }).click()
    await expect(overlay.getByText(/unknown_top_field/)).toBeVisible({ timeout: 15_000 })
    await expect(overlay.getByText('bad.yaml')).toBeVisible()
    await expect(overlay.getByRole('button', { name: '上传并校验' })).toBeEnabled()

    // Correcting the file in place succeeds and lands on the version detail.
    await overlay.locator('input[type=file]').setInputFiles({
      name: 'good.yaml',
      mimeType: 'application/yaml',
      buffer: Buffer.from(goodSystemYAML),
    })
    await overlay.getByRole('button', { name: '上传并校验' }).click()
    await expect(page.getByRole('heading', { name: /配置版本 v1/ })).toBeVisible({ timeout: 30_000 })
    await expect(page.getByText('草稿', { exact: true })).toBeVisible()
    await expect(page.getByText(/up\{business_system="browser-sys"\}/)).toBeVisible()

    // The explicit publish confirm switches the current pointer.
    await page.getByRole('button', { name: '发布此版本' }).click()
    await page.getByRole('button', { name: '确认发布' }).click()
    await expect(page.getByRole('heading', { name: '浏览器验收系统' })).toBeVisible({ timeout: 30_000 })
    await expect(page.getByText('已发布').first()).toBeVisible({ timeout: 15_000 })
    // Both the header status pill and the fact row show the projection; one
    // assert each to stay strict-mode safe.
    await expect(page.locator('.detail-header .status-pill', { hasText: 'Disabled' })).toBeVisible()

    // The system list row reflects the published state.
    await page.getByRole('button', { name: '返回列表' }).click()
    await expect(page.getByRole('button', { name: /浏览器验收系统/ })).toBeVisible()
    await expect(page.getByText('已发布配置 · 时区 Asia/Shanghai')).toBeVisible()

    // Deep link: the version detail survives a full reload.
    await page.goto('/business-systems/browser-sys/configs/1')
    await expect(page.getByRole('heading', { name: /配置版本 v1/ })).toBeVisible({ timeout: 30_000 })
    await expect(page.getByText('已发布').first()).toBeVisible()

    // Back returns to the system detail through browser history.
    await page.goBack()
    await expect(page.getByRole('button', { name: /浏览器验收系统/ })).toBeVisible({ timeout: 15_000 })
  })
})

test.describe('T18 配置验证执行与资源刷新浏览器路径 @ticket-18', () => {
  test('验证 Run 在 UI 内达到已通过，资源刷新受理并收敛', async ({ page }) => {
    test.slow()
    await page.goto('/')
    await page.fill('#username', 'admin')
    await page.fill('#password', readAdminPassword())
    await page.getByRole('button', { name: '登录' }).click()
    await expect(page.getByRole('navigation', { name: '全局模块' })).toBeVisible({ timeout: 30_000 })

    // Fixture the contract and a check/discovery-bearing config through the
    // real production endpoints from inside the page. The shared stack's
    // main-thanos is created but not qualified: probe it, then enable it with
    // its current row version — the same operator path the UI offers. The
    // verification then reaches Passed (a successful query passes even with
    // zero series); the refresh completes and the current-resource section
    // settles on its authoritative terminal state.
    const fixture = await page.evaluate(async (yaml) => {
      const commandSuffix = Date.now()
      const headers = { 'Content-Type': 'application/json' }
      // Reuse an already-active contract (the shared stack keeps the T16
      // activation); a standalone run creates and activates its own draft.
      let contractVersion = 1
      let contractCreate = 0
      let contractActivate = 0
      const contracts = await (await fetch('/api/v1/label-contracts?limit=100')).json()
      const active = (contracts.items ?? []).find((item: { state?: string }) => item.state === 'active')
      if (active) {
        contractVersion = active.version
      } else {
        const form = new FormData()
        form.append('file', new File([yaml.contract], 'contract.yaml', { type: 'application/yaml' }))
        form.append('clientCommandId', `e2e-t18-contract-${commandSuffix}`)
        const create = await fetch('/api/v1/label-contracts', { method: 'POST', body: form })
        contractCreate = create.status
        const draft = await create.json()
        contractVersion = draft.version
        const readiness = await (await fetch(`/api/v1/label-contracts/${contractVersion}/readiness`)).json()
        const activate = await fetch(`/api/v1/label-contracts/${contractVersion}/activate`, {
          method: 'POST',
          headers,
          body: JSON.stringify({
            clientCommandId: `e2e-t18-activate-${commandSuffix}`,
            expectedStateRowVersion: readiness.stateRowVersion,
            expectedCurrentContractVersionId: readiness.currentContractVersionId ?? null,
            expectedTargetRowVersion: readiness.targetRowVersion,
            compatibleVersions: [],
          }),
        })
        contractActivate = activate.status
      }
      // The shared stack already carries an enabled qualified Thanos (the T11
      // fixture); only a standalone run has to qualify and enable main-thanos.
      const connections = await (await fetch('/api/v1/connections?limit=100')).json()
      const enabledThanos = (connections.items ?? []).some(
        (item: { type?: string; enabled?: boolean }) => item.type === 'thanos' && item.enabled,
      )
      let probeStatus = 0
      let enabled = 0
      if (enabledThanos) {
        enabled = 200
      } else {
        const probe = await fetch('/api/v1/connections/main-thanos/probe', {
          method: 'POST',
          headers,
          body: JSON.stringify({ clientCommandId: `e2e-t18-probe-${commandSuffix}` }),
        })
        probeStatus = probe.status
        for (let attempt = 0; attempt < 60; attempt++) {
          const results = await (await fetch('/api/v1/connections/main-thanos/probe-results')).json()
          const passed = (results.items ?? []).some((item: { outcome?: string }) => item.outcome === 'passed')
          if (passed) {
            // The probe's qualification can advance the connection row between
            // the read and the enable fence; re-read and retry like the UI
            // guidance tells the operator to.
            for (let retry = 0; retry < 5; retry++) {
              const detail = await (await fetch('/api/v1/connections/main-thanos')).json()
              const enable = await fetch('/api/v1/connections/main-thanos/enable', {
                method: 'POST',
                headers,
                body: JSON.stringify({ clientCommandId: `e2e-t18-enable-${commandSuffix}`, expectedRowVersion: detail.rowVersion }),
              })
              enabled = enable.status
              if (enable.status === 200) break
              await new Promise((resolve) => setTimeout(resolve, 500))
            }
            break
          }
          await new Promise((resolve) => setTimeout(resolve, 1000))
        }
      }
      const uploadForm = new FormData()
      uploadForm.append('file', new File([yaml.system], 'config.yaml', { type: 'application/yaml' }))
      uploadForm.append('clientCommandId', `e2e-t18-upload-${commandSuffix}`)
      uploadForm.append('targetLabelContractVersion', String(contractVersion))
      const upload = await fetch('/api/v1/business-systems', { method: 'POST', body: uploadForm })
      const version = await upload.json()
      return { contractCreate, contractActivate, probe: probeStatus, enable: enabled, upload: upload.status, versionId: version.id as string }
    }, {
      contract: 'label_contract:\n  business_system_label: business_system\n',
      system: `system_key: refresh-sys
display_name: 刷新验收系统
enabled: true
timezone: Asia/Shanghai
resource_refresh_interval_seconds: 300
resource_discoveries:
  - key: web-pods
    display_name: Web Pods
    selector: 'up{business_system="refresh-sys", job="web"}'
    identity_labels: [job, instance]
inspection_plans:
  - key: daily
    display_name: Daily
    cron: "30 8 * * *"
    checks:
      - key: up-instant
        display_name: Up Instant
        analysis_question: 当前可用吗？
        kind: promql
        query:
          mode: instant
          expression: 'up{business_system="refresh-sys"}'
`,
    })
    expect([0, 201]).toContain(fixture.contractCreate)
    expect([0, 200]).toContain(fixture.contractActivate)
    expect([0, 202]).toContain(fixture.probe)
    expect(fixture.enable).toBe(200)
    expect(fixture.upload).toBe(201)
    expect(fixture.versionId).toBeTruthy()

    // The draft's Config Verification Run reaches Passed inside the UI.
    await page.getByRole('navigation', { name: '全局模块' }).getByRole('button', { name: '业务系统' }).click()
    await page.getByRole('button', { name: /刷新验收系统/ }).click()
    await page.locator('.version-history .version-item', { hasText: 'v1' }).click()
    await page.getByRole('button', { name: '运行测试' }).click()
    await expect(page.locator('.verify-run-list').getByText('已通过')).toBeVisible({ timeout: 60_000 })

    // Publish the verified draft, then trigger the manual Resource Refresh.
    await page.getByRole('button', { name: '发布此版本' }).click()
    await page.getByRole('button', { name: '确认发布' }).click()
    await expect(page.getByRole('heading', { name: '刷新验收系统' })).toBeVisible({ timeout: 30_000 })
    await expect(page.getByText('已观测资源')).toBeVisible()
    await page.getByRole('button', { name: '立即刷新' }).click()
    await expect(page.getByText(/资源刷新已开始/)).toBeVisible()
    await expect(page.getByText(/资源刷新已(Completed|CompletedWithWarnings|Failed|Cancelled|Interrupted)/)).toBeVisible({ timeout: 60_000 })
  })
})
