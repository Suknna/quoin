import '@testing-library/jest-dom/vitest'
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { UploadOverlay } from './UploadOverlay'
import { ConfigVersionPage } from './ConfigVersionDetail'
import { BusinessSystemsList } from './BusinessSystemsList'
import { BusinessSystemDetailPage } from './BusinessSystemDetail'

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
    render(<BusinessSystemsList onOpen={() => undefined} onUpload={() => undefined} onOpenContracts={() => undefined} isAdmin={false} />)
    expect(await screen.findByText('支付系统')).toBeInTheDocument()
    expect(screen.getByText('Disabled')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '上传配置' })).not.toBeInTheDocument()
  })

  it('shows the first-upload empty state with the admin entry', async () => {
    vi.mocked(fetch).mockImplementation(async () => jsonResponse({ items: [] }))
    render(<BusinessSystemsList onOpen={() => undefined} onUpload={() => undefined} onOpenContracts={() => undefined} isAdmin />)
    expect(await screen.findByText(/上传第一份配置 YAML/)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '上传配置' })).toBeInTheDocument()
  })
})

describe('BusinessSystemDetailPage resources', () => {
  beforeEach(() => vi.stubGlobal('fetch', vi.fn()))
  afterEach(cleanup)

  it('shows current resources and lets an Admin start one refresh', async () => {
    vi.mocked(fetch).mockImplementation(async (input: RequestInfo | URL) => {
      const path = String(input)
      if (path.includes('resources:refresh')) return jsonResponse({ id: '11', state: 'Running' })
      if (path.includes('/resources')) return jsonResponse({ items: [{ id: '5', discoveryKey: 'web', identityLabels: { job: 'web' }, observedAt: '2026-08-22T01:00:00Z', current: true, stale: false }] })
      if (path.includes('/config')) return jsonResponse({ items: [] })
      return jsonResponse({ key: 'payments', displayName: '支付系统', enabled: true, rowVersion: 1, browserIdentityState: 'none', timezone: 'Asia/Shanghai', resourceRefreshIntervalSeconds: 300, configVersionCount: 1, currentConfigVersionId: '7', discoveries: [], plans: [] })
    })
    render(<BusinessSystemDetailPage systemKey="payments" isAdmin onBack={() => undefined} onOpenVersion={() => undefined} />)
    expect(await screen.findByText('job=web')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: '立即刷新' }))
    await waitFor(() => expect(vi.mocked(fetch)).toHaveBeenCalledWith(expect.stringContaining('resources:refresh'), expect.objectContaining({ method: 'POST' })))
    expect(await screen.findByText(/资源刷新已开始/)).toBeInTheDocument()
  })

  it('keeps the refresh entry admin-only while resources stay readable', async () => {
    vi.mocked(fetch).mockImplementation(async (input: RequestInfo | URL) => {
      const path = String(input)
      if (path.includes('/resources')) return jsonResponse({ items: [] })
      if (path.includes('/config')) return jsonResponse({ items: [] })
      return jsonResponse({ key: 'payments', displayName: '支付系统', enabled: true, rowVersion: 1, browserIdentityState: 'none', timezone: 'Asia/Shanghai', resourceRefreshIntervalSeconds: 300, configVersionCount: 1, currentConfigVersionId: '7', discoveries: [], plans: [] })
    })
    render(<BusinessSystemDetailPage systemKey="payments" isAdmin={false} onBack={() => undefined} onOpenVersion={() => undefined} />)
    expect(await screen.findByText('已观测资源')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '立即刷新' })).not.toBeInTheDocument()
  })
})

describe('BusinessSystemDetailPage Kubernetes mappings', () => {
  beforeEach(() => vi.stubGlobal('fetch', vi.fn()))
  afterEach(cleanup)

  it('retries connection options independently when mappings are already available', async () => {
    let optionRequests = 0
    vi.mocked(fetch).mockImplementation(async (input: RequestInfo | URL) => {
      const path = String(input)
      if (path.includes('/kubernetes-connections')) return jsonResponse([])
      if (path === '/api/v1/connections?limit=100') {
        optionRequests += 1
        if (optionRequests === 1) return jsonResponse({ code: 'unavailable', message: 'temporary' }, 503)
        return jsonResponse({ items: [{ id: '9007199254740993', name: 'production-k8s', type: 'kubernetes', enabled: true }] })
      }
      if (path.includes('/resources')) return jsonResponse({ items: [] })
      if (path.includes('/config')) return jsonResponse({ items: [] })
      return jsonResponse({ key: 'payments', displayName: '支付系统', enabled: true, rowVersion: 1, browserIdentityState: 'none', timezone: 'Asia/Shanghai', resourceRefreshIntervalSeconds: 300, configVersionCount: 1, currentConfigVersionId: '7', discoveries: [], plans: [] })
    })
    render(<BusinessSystemDetailPage systemKey="payments" isAdmin onBack={() => undefined} onOpenVersion={() => undefined} />)
    expect(await screen.findByText(/无法读取可绑定的 Kubernetes 连接/)).toBeInTheDocument()
    expect(screen.getByText('尚未绑定 Kubernetes 连接。')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: '重试' }))
    expect(await screen.findByRole('option', { name: 'production-k8s' })).toBeInTheDocument()
    expect(screen.getByText('尚未绑定 Kubernetes 连接。')).toBeInTheDocument()
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

  it('loads every Config Verification Run history page', async () => {
    const fetchMock = vi.mocked(fetch)
    fetchMock.mockImplementation(async (input: RequestInfo | URL) => {
      const path = String(input)
      if (path === '/api/v1/business-systems/payments/config/7') return jsonResponse(versionDetail)
      if (path.startsWith('/api/v1/business-systems/payments/config/7/verifications?')) {
        if (path.includes('cursor=run-1')) {
          return jsonResponse({ items: [{ id: '2', state: 'Passed', rowVersion: 3, createdAt: '2026-08-22T02:00:00Z' }] })
        }
        return jsonResponse({
          items: [{ id: '1', state: 'Cancelled', rowVersion: 2, createdAt: '2026-08-22T01:00:00Z' }],
          nextCursor: 'run-1',
        })
      }
      return jsonResponse({}, 404)
    })
    render(<ConfigVersionPage systemKey="payments" versionId="7" isAdmin onBack={() => undefined} onPublished={() => undefined} />)
    expect(await screen.findByText('Run #1')).toBeInTheDocument()
    expect(await screen.findByText('Run #2')).toBeInTheDocument()
    expect(fetchMock.mock.calls.some(([input]) => String(input).includes('cursor=run-1'))).toBe(true)
  })

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


describe('BusinessSystemDetailPage Kubernetes mapping state', () => {
  beforeEach(() => vi.stubGlobal('fetch', vi.fn()))
  afterEach(cleanup)

  it('does not present a failed mapping read as an empty mapping state and offers retry', async () => {
    const fetchMock = vi.mocked(fetch)
    let mappingReads = 0
    fetchMock.mockImplementation(async (input: RequestInfo | URL) => {
      const path = String(input)
      if (path === '/api/v1/business-systems/payments') return jsonResponse({ key: 'payments', displayName: '支付系统', enabled: true, rowVersion: 1, currentConfigVersionId: null, browserIdentityState: 'none', configVersionCount: 0, discoveries: [], plans: [] })
      if (path === '/api/v1/business-systems/payments/kubernetes-connections') {
        mappingReads++
        return jsonResponse({ message: 'mapping API unavailable' }, 503)
      }
      if (path === '/api/v1/connections?limit=100') return jsonResponse({ items: [] })
      if (path === '/api/v1/business-systems/payments/resources') return jsonResponse({ items: [] })
      if (path === '/api/v1/business-systems/payments/config') return jsonResponse({ items: [] })
      return jsonResponse({}, 404)
    })

    render(<BusinessSystemDetailPage systemKey="payments" isAdmin onBack={() => undefined} onOpenVersion={() => undefined} />)
    expect(await screen.findByRole('alert')).toHaveTextContent(/无法读取 Kubernetes 连接绑定/)
    expect(screen.queryByText('尚未绑定 Kubernetes 连接。')).not.toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: '重试' }))
    await waitFor(() => expect(mappingReads).toBeGreaterThan(1))
  })
})


describe('BusinessSystemDetailPage Kubernetes mapping creation', () => {
  beforeEach(() => vi.stubGlobal('fetch', vi.fn()))
  afterEach(cleanup)

  it('paginates options and creates before exactly one mapping refresh', async () => {
    const fetchMock = vi.mocked(fetch)
    fetchMock.mockImplementation(async (input: RequestInfo | URL, init?: RequestInit) => {
      const path = String(input)
      if (path === '/api/v1/connections?limit=100') return jsonResponse({ items: [{ id: '9007199254740993', name: 'first-k8s', type: 'kubernetes', enabled: true }], nextCursor: 'next-page' })
      if (path === '/api/v1/connections?limit=100&cursor=next-page') return jsonResponse({ items: [{ id: '9007199254740994', name: 'second-k8s', type: 'kubernetes', enabled: true }] })
      if (path.includes('/kubernetes-connections') && init?.method === 'POST') return jsonResponse({ id: '1', connectionId: '9007199254740993', connectionName: 'first-k8s', state: 'Active', rowVersion: 1, createdBy: '1', createdAt: '2026-08-24T00:00:00Z', retiredBy: null })
      if (path.includes('/kubernetes-connections')) return jsonResponse([])
      if (path.includes('/resources')) return jsonResponse({ items: [] })
      if (path.includes('/config')) return jsonResponse({ items: [] })
      return jsonResponse({ key: 'payments', displayName: '支付系统', enabled: true, rowVersion: 1, browserIdentityState: 'none', timezone: 'Asia/Shanghai', resourceRefreshIntervalSeconds: 300, configVersionCount: 1, currentConfigVersionId: '7', discoveries: [], plans: [] })
    })
    render(<BusinessSystemDetailPage systemKey="payments" isAdmin onBack={() => undefined} onOpenVersion={() => undefined} />)
    expect(await screen.findByRole('option', { name: 'second-k8s' })).toBeInTheDocument()
    const beforeCreate = fetchMock.mock.calls.length
    fireEvent.change(screen.getByRole('combobox', { name: '选择 Kubernetes 连接' }), { target: { value: '9007199254740993' } })
    fireEvent.click(screen.getByRole('button', { name: '绑定' }))
    await screen.findByText('Kubernetes 连接已绑定。')
    const afterCreate = fetchMock.mock.calls.slice(beforeCreate)
    expect(afterCreate).toHaveLength(2)
    expect(String(afterCreate[0][0])).toContain('/kubernetes-connections')
    expect(afterCreate[0][1]).toEqual(expect.objectContaining({ method: 'POST' }))
    expect(String(afterCreate[1][0])).toContain('/kubernetes-connections')
    expect(afterCreate[1][1]?.method).toBeUndefined()
  })
})


describe('BusinessSystemDetailPage Kubernetes connection option state', () => {
  beforeEach(() => vi.stubGlobal('fetch', vi.fn()))
  afterEach(cleanup)

  it('distinguishes option loading from a successfully empty option list', async () => {
    let resolveOptions!: (response: Response) => void
    const options = new Promise<Response>((resolve) => { resolveOptions = resolve })
    const fetchMock = vi.mocked(fetch)
    fetchMock.mockImplementation((input: RequestInfo | URL) => {
      const path = String(input)
      if (path === '/api/v1/business-systems/payments') return Promise.resolve(jsonResponse({ key: 'payments', displayName: '支付系统', enabled: true, rowVersion: 1, currentConfigVersionId: null, browserIdentityState: 'none', configVersionCount: 0, discoveries: [], plans: [] }))
      if (path === '/api/v1/business-systems/payments/kubernetes-connections') return Promise.resolve(jsonResponse([]))
      if (path === '/api/v1/connections?limit=100') return options
      if (path.includes('/resources') || path.includes('/config')) return Promise.resolve(jsonResponse({ items: [] }))
      return Promise.resolve(jsonResponse({}, 404))
    })

    render(<BusinessSystemDetailPage systemKey="payments" isAdmin onBack={() => undefined} onOpenVersion={() => undefined} />)
    const select = await screen.findByRole('combobox', { name: '选择 Kubernetes 连接' })
    expect(select).toBeDisabled()
    expect(screen.getByRole('option', { name: '正在读取 Kubernetes 连接…' })).toBeInTheDocument()

    resolveOptions(jsonResponse({ items: [] }))
    expect(await screen.findByRole('option', { name: '没有可绑定的 Kubernetes 连接' })).toBeInTheDocument()
    expect(select).not.toBeDisabled()
  })
})
