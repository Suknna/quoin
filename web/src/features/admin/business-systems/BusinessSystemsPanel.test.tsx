import '@testing-library/jest-dom/vitest'
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { UploadOverlay } from './UploadOverlay'
import { ConfigVersionPage } from './ConfigVersionDetail'
import { BusinessSystemsList } from './BusinessSystemsList'

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })
}

const contractItems = [
  { id: '1', version: 1, state: 'active' as const, rowVersion: 2, parserVersion: 'p', schemaVersion: 's', createdAt: '2026-08-22T00:00:00Z', activatedAt: '2026-08-22T00:00:01Z' },
]

const catalogView = { version: 'v1-empty', digest: 'a'.repeat(64), catalogJson: { catalog_version: 'v1-empty', journeys: {} } }

const versionDetail = {
  id: '7',
  versionSeq: 1,
  state: 'draft' as const,
  createdAt: '2026-08-22T01:00:00Z',
  digest: 'b'.repeat(64),
  parserVersion: 'quoin-strict-yaml-1',
  schemaVersion: 'schema',
  systemKey: 'payments',
  displayName: '支付系统',
  enabled: false,
  labelContractVersionId: '1',
  journeyCatalogDigest: 'a'.repeat(64),
  journeyCatalogVersion: 'v1-empty',
  yamlBody: 'system_key: payments\n',
  timezone: 'Asia/Shanghai',
  resourceRefreshIntervalSeconds: 300,
  discoveries: [
    { discoveryKey: 'web', displayName: 'Web', selector: 'up{business_system="payments"}', identityLabels: ['job'] },
  ],
  plans: [
    {
      planKey: 'daily',
      displayName: 'Daily',
      cron: '30 8 * * *',
      checks: [
        { checkKey: 'up', displayName: 'Up', analysisQuestion: '可用吗？', kind: 'promql' as const, queryMode: 'instant' as const, expression: 'up{business_system="payments"}' },
      ],
    },
  ],
}

describe('BusinessSystemsList', () => {
  beforeEach(() => vi.stubGlobal('fetch', vi.fn()))
  afterEach(cleanup)

  it('renders two-line rows and keeps the upload entry admin-only', async () => {
    const fetchMock = vi.mocked(fetch)
    fetchMock.mockImplementation(async () =>
      jsonResponse({ items: [{ key: 'payments', displayName: '支付系统', enabled: false, rowVersion: 1, browserIdentityState: 'none', timezone: null, resourceRefreshIntervalSeconds: null }] }),
    )
    render(<BusinessSystemsList onOpen={() => undefined} onUpload={() => undefined} isAdmin={false} />)
    expect(await screen.findByText('支付系统')).toBeInTheDocument()
    expect(screen.getByText('Disabled')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '上传配置' })).not.toBeInTheDocument()
  })

  it('shows the first-upload empty state with the admin entry', async () => {
    vi.mocked(fetch).mockImplementation(async () => jsonResponse({ items: [] }))
    render(<BusinessSystemsList onOpen={() => undefined} onUpload={() => undefined} isAdmin />)
    expect(await screen.findByText(/上传第一份配置 YAML/)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '上传配置' })).toBeInTheDocument()
  })
})

describe('UploadOverlay', () => {
  beforeEach(() => vi.stubGlobal('fetch', vi.fn()))
  afterEach(cleanup)

  function stubReads() {
    const fetchMock = vi.mocked(fetch)
    fetchMock.mockImplementation(async (input: RequestInfo | URL) => {
      const path = String(input)
      if (path.startsWith('/api/v1/label-contracts')) return jsonResponse({ items: contractItems })
      if (path.startsWith('/api/v1/journey-catalog')) return jsonResponse(catalogView)
      return jsonResponse({}, 404)
    })
  }

  async function chooseFile(name: string, body: string) {
    const file = new File([body], name, { type: 'application/yaml' })
    const input = screen.getByLabelText(/拖放或选择一份 YAML 文件/) as HTMLInputElement
    fireEvent.change(input, { target: { files: [file] } })
  }

  it('renders per-path field errors and keeps the file for correction', async () => {
    stubReads()
    const upload = vi.fn(async () =>
      jsonResponse(
        {
          code: 'validation_failed',
          message: '配置内容未通过静态校验，请按逐项错误修正后重试。',
          retryable: false,
          fieldErrors: [
            { path: 'inspection_plans[0].checks[0].query.expression', reason: '每个向量选择器必须携带 business_system="payments" 的精确匹配', remediation: '在 selector 中加入 business_system: payments 精确匹配' },
          ],
        },
        422,
      ),
    )
    const fetchMock = vi.mocked(fetch)
    render(<UploadOverlay onClose={() => undefined} onUploaded={() => undefined} />)
    await screen.findByText(/Journey Catalog/)
    await chooseFile('config.yaml', 'system_key: payments\n')
    fetchMock.mockImplementation(async (input: RequestInfo | URL) => {
      const path = String(input)
      if (path === '/api/v1/business-systems') return upload()
      if (path.startsWith('/api/v1/label-contracts')) return jsonResponse({ items: contractItems })
      if (path.startsWith('/api/v1/journey-catalog')) return jsonResponse(catalogView)
      return jsonResponse({}, 404)
    })
    fireEvent.click(screen.getByRole('button', { name: '上传并校验' }))
    expect(await screen.findByText(/business_system="payments"/)).toBeInTheDocument()
    expect(screen.getByText('config.yaml')).toBeInTheDocument()
    // The failed upload keeps the chosen file and allows direct retry.
    expect(screen.getByRole('button', { name: '上传并校验' })).toBeEnabled()
  })

  it('routes to the created version on success', async () => {
    stubReads()
    const onUploaded = vi.fn()
    const fetchMock = vi.mocked(fetch)
    render(<UploadOverlay onClose={() => undefined} onUploaded={onUploaded} />)
    await screen.findByText(/Journey Catalog/)
    await chooseFile('config.yaml', 'system_key: payments\n')
    fetchMock.mockImplementation(async (input: RequestInfo | URL) => {
      if (String(input) === '/api/v1/business-systems') return jsonResponse(versionDetail, 201)
      return jsonResponse({}, 404)
    })
    fireEvent.click(screen.getByRole('button', { name: '上传并校验' }))
    await waitFor(() => expect(onUploaded).toHaveBeenCalledWith('payments', '7'))
  })
})

describe('ConfigVersionPage publish flow', () => {
  beforeEach(() => vi.stubGlobal('fetch', vi.fn()))
  afterEach(cleanup)

  it('publishes through the explicit confirm and reports conflict recovery', async () => {
    const fetchMock = vi.mocked(fetch)
    fetchMock.mockImplementation(async (input: RequestInfo | URL, init?: RequestInit) => {
      const path = String(input)
      if (path === '/api/v1/business-systems/payments/config/7') return jsonResponse(versionDetail)
      if (path === '/api/v1/business-systems/payments' && !init?.method) return jsonResponse({ key: 'payments', displayName: '支付系统', enabled: false, rowVersion: 1, currentConfigVersionId: null, browserIdentityState: 'none', configVersionCount: 1, discoveries: [], plans: [] })
      if (path.endsWith('/publish')) {
        return jsonResponse(
          { code: 'current_pointer_conflict', message: '当前已发布版本与 expectedCurrentPublishedVersionId 不匹配，请刷新后重试。', retryable: false, conflict: { code: 'current_pointer_conflict', currentVersionId: '3' } },
          409,
        )
      }
      return jsonResponse({}, 404)
    })
    const onPublished = vi.fn()
    render(<ConfigVersionPage systemKey="payments" versionId="7" isAdmin onBack={() => undefined} onPublished={onPublished} />)
    expect(await screen.findByText('配置版本 v1')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: '发布此版本' }))
    fireEvent.click(screen.getByRole('button', { name: '确认发布' }))
    expect(await screen.findByText(/不匹配，请刷新后重试/)).toBeInTheDocument()
    expect(onPublished).not.toHaveBeenCalled()
    // The confirm stays available: recovery is a re-read away, not a dead end.
    expect(screen.getByRole('button', { name: '确认发布' })).toBeInTheDocument()
  })
})
