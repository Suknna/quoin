import '@testing-library/jest-dom/vitest'
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { useInvestigationsModule } from './index'
import { streamInvestigationMessage } from '@/features/investigation/stream'
import { type ToolCallItem } from '@/features/investigation/tools/api'

const { api, uploadAttachment } = vi.hoisted(() => ({ api: {
  list: vi.fn(), get: vi.fn(), listMessages: vi.fn(), listAttempts: vi.fn(), create: vi.fn(), sendMessage: vi.fn(), cancelAttempt: vi.fn(), retryAttempt: vi.fn(), undo: vi.fn(),
}, uploadAttachment: vi.fn() }))
vi.mock('@/features/investigation/api', () => ({ api, sourceLabel: (type: string) => type }))
vi.mock('@/features/investigation/attachments/api', () => ({ attachmentCommandId: () => 'upload-command', uploadAttachment }))
vi.mock('@/features/investigation/tools/api', async (original) => ({ ...await original<typeof import('@/features/investigation/tools/api')>(), listToolCalls: vi.fn() }))
vi.mock('@/features/investigation/stream', () => ({ streamInvestigationMessage: vi.fn() }))
vi.mock('@/features/feedback/api', () => ({ appendFeedback: vi.fn(), fetchFeedback: vi.fn(), feedbackValueLabels: {} }))
vi.mock('@/features/knowledge/api', () => ({ api: { createMessageCandidate: vi.fn() } }))
const user = { id: 'u', username: 'operator', displayName: 'Operator', role: 'operator' as const, passwordChangeRequired: false, authRevision: 1, enabled: true, initialized: true, lastLoginAt: null, rowVersion: 1 }
function View({ route, suspended = false, navigate = vi.fn() }: { route: string; suspended?: boolean; navigate?: (route: string) => void }) { const view = useInvestigationsModule({ user, route, suspended, navigate, openEvidence: vi.fn() }); return <>{view.list}{view.content}</> }
const detail = { id: 'i1', displayTitle: 'CPU 排查', lastActivityAt: '2026-01-01T00:00:00Z', createdAt: '2026-01-01T00:00:00Z', createdBy: 'u', headMessageId: 'm1', activeAttemptId: 'a1', messageCount: 1, attemptCount: 1, sources: [] }
beforeEach(() => { Element.prototype.scrollIntoView ??= vi.fn() })
afterEach(() => { cleanup(); vi.clearAllMocks() })
describe('investigations module', () => {
  it('shows the new-conversation workspace on the default route without creating an investigation', async () => {
    api.list.mockResolvedValue({ items: [] })
    render(<View route="/investigations" />)
    expect(await screen.findByRole('textbox', { name: '消息内容' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '创建并发送' })).toBeDisabled()
    expect(screen.queryByRole('combobox', { name: '业务系统' })).not.toBeInTheDocument()
    expect(screen.queryByText('从左侧创建或打开一次调查。')).not.toBeInTheDocument()
    expect(api.create).not.toHaveBeenCalled()
  })
  it('creates the first message atomically with supplied occurrence sources', async () => {
    api.list.mockResolvedValue({ items: [] }); api.create.mockResolvedValue({ ...detail, id: 'i2' })
    render(<View route="/investigations/new?occurrence=o1&initialAnalysis=a1" />)
    fireEvent.change(screen.getByRole('textbox', { name: '消息内容' }), { target: { value: '排查 CPU' } })
    fireEvent.click(screen.getByRole('button', { name: '创建并发送' }))
    await waitFor(() => expect(api.create).toHaveBeenCalledWith('排查 CPU', [{ type: 'occurrence', sourceId: 'o1' }, { type: 'initial_analysis', sourceId: 'a1' }], []))
  })
  it('creates only after a default-route workspace send', async () => {
    api.list.mockResolvedValue({ items: [] }); api.create.mockResolvedValue({ ...detail, id: 'i2' })
    render(<View route="/investigations" />)
    fireEvent.change(await screen.findByRole('textbox', { name: '消息内容' }), { target: { value: '排查 CPU' } })
    fireEvent.click(screen.getByRole('button', { name: '创建并发送' }))
    await waitFor(() => expect(api.create).toHaveBeenCalledWith('排查 CPU', [], []))
  })
  it('sends the appended alert sources with the next message (ADR-0012「+ 引入告警」)', async () => {
    api.get.mockResolvedValue(detail); api.listMessages.mockResolvedValue({ items: [] }); api.listAttempts.mockResolvedValue({ items: [] })
    api.sendMessage.mockResolvedValue({ id: 'm2', seq: 2, role: 'user', status: 'active', content: '结合这条告警看', attachments: [], evidenceIds: null, createdAt: '2026-01-01T00:00:00Z' })
    const { streamInvestigationMessage } = await import('@/features/investigation/stream')
    vi.mocked(streamInvestigationMessage).mockReturnValue((async function* () {})())
    const { fetchAlerts } = await import('@/features/alerts/api')
    vi.stubGlobal('fetch', vi.fn(async () => new Response(JSON.stringify({ snapshotSeq: 1, items: [
      { id: 'alert-9', source: 'alertmanager', state: 'Firing', rowVersion: 1, severity: 'critical', title: 'CheckoutLatencyHigh', firstSeenAt: '2026-01-01T00:00:00Z', lastStateChangeAt: '2026-01-01T00:00:00Z', labels: {}, correlations: [] },
    ] }), { status: 200, headers: { 'Content-Type': 'application/json' } })))
    void fetchAlerts
    render(<View route="/investigations/i1" />)
    fireEvent.click(await screen.findByRole('button', { name: '引入告警' }))
    fireEvent.click(await screen.findByRole('button', { name: /CheckoutLatencyHigh/ }))
    expect(await screen.findByText('告警 CheckoutLatencyHigh')).toBeInTheDocument()
    fireEvent.change(screen.getByRole('textbox', { name: '消息内容' }), { target: { value: '结合这条告警看' } })
    fireEvent.click(screen.getByRole('button', { name: '发送' }))
    await waitFor(() => expect(api.sendMessage).toHaveBeenCalledWith('i1', '结合这条告警看', 'm1', [], [{ type: 'occurrence', sourceId: 'alert-9' }]))
  })
  it('disables the first-send action until content or an attachment is supplied', async () => {
    api.list.mockResolvedValue({ items: [] }); render(<View route="/investigations/new" />)
    const send = screen.getByRole('button', { name: '创建并发送' })
    expect(send).toBeDisabled()
    expect(api.create).not.toHaveBeenCalled()
  })
  it('cancels the active attempt using its authoritative row version', async () => {
    api.list.mockResolvedValue({ items: [] }); api.get.mockResolvedValue(detail); api.listMessages.mockResolvedValue({ items: [] }); api.listAttempts.mockResolvedValue({ items: [{ id: 'a1', type: 'chat', state: 'Running', rowVersion: 7, createdAt: '2026-01-01T00:00:00Z' }] }); api.cancelAttempt.mockResolvedValue({})
    render(<View route="/investigations/i1" />)
    fireEvent.click(await screen.findByRole('button', { name: '停止' }))
    await waitFor(() => expect(api.cancelAttempt).toHaveBeenCalledWith('i1', 'a1', 7))
  })
  it('disables send during upload and surfaces an upload failure', async () => {
    api.list.mockResolvedValue({ items: [] })
    let rejectUpload: (reason: Error) => void = () => undefined
    uploadAttachment.mockReturnValue(new Promise((_, reject) => { rejectUpload = reject }))
    render(<View route="/investigations/new" />)
    const file = new File(['logs'], 'incident.log', { type: 'text/plain' })
    fireEvent.change(screen.getByLabelText('添加附件'), { target: { files: [file] } })
    expect(await screen.findByRole('status')).toHaveTextContent('正在上传附件')
    expect(screen.getByRole('button', { name: '创建并发送' })).toBeDisabled()
    rejectUpload(new Error('附件校验失败'))
    expect(await screen.findByRole('alert')).toHaveTextContent('附件校验失败')
    vi.useRealTimers()
  })
  it('keeps Stop enabled for a local send stream and cancels the delayed authoritative attempt', async () => {
    let releaseStream: () => void = () => undefined
    const streamOpen = new Promise<void>((resolve) => { releaseStream = resolve })
    api.list.mockResolvedValue({ items: [] }); api.get.mockResolvedValue({ ...detail, activeAttemptId: undefined }); api.listMessages.mockResolvedValue({ items: [] })
    api.listAttempts.mockResolvedValueOnce({ items: [] }).mockResolvedValue({ items: [{ id: 'a2', type: 'chat', state: 'Running', rowVersion: 8, createdAt: '2026-01-01T00:00:00Z' }] })
    api.sendMessage.mockResolvedValue({ id: 'm2', seq: 2, role: 'user', status: 'active', content: '继续排查', attachments: [], evidenceIds: [], createdAt: '2026-01-01T00:00:00Z' })
    api.cancelAttempt.mockResolvedValue({})
    vi.mocked(streamInvestigationMessage).mockImplementation(async function* () { await streamOpen; yield { content: [] } as never })
    render(<View route="/investigations/i1" />)
    const composer = await screen.findByRole('textbox', { name: '消息内容' })
    fireEvent.change(composer, { target: { value: '继续排查' } })
    fireEvent.click(screen.getByRole('button', { name: '发送' }))
    const stop = await screen.findByRole('button', { name: '停止' })
    expect(stop).toBeEnabled()
    fireEvent.click(stop)
    await waitFor(() => expect(api.cancelAttempt).toHaveBeenCalledWith('i1', 'a2', 8))
    releaseStream()
  })
  it('opens the knowledge base from an assistant reply', async () => {
    api.list.mockResolvedValue({ items: [] }); api.get.mockResolvedValue(detail)
    api.listMessages.mockResolvedValue({ items: [{ id: 'm1', seq: 1, role: 'assistant', status: 'active', content: '调查结论', attachments: [], evidenceIds: [], createdAt: '2026-01-01T00:00:00Z' }] })
    api.listAttempts.mockResolvedValue({ items: [] })
    const navigate = vi.fn()
    render(<View route="/investigations/i1" navigate={navigate} />)
    fireEvent.click(await screen.findByRole('button', { name: '查看知识库' }))
    expect(navigate).toHaveBeenCalledWith('/knowledge')
  })
  it('shows a failed list request instead of an empty healthy state', async () => {
    api.list.mockRejectedValue(new Error('调查服务不可用'))
    render(<View route="/investigations" />)
    expect(await screen.findByText('调查服务不可用')).toBeInTheDocument()
  })
})

describe('turn-bound tool cards', () => {
  const baseMessage = { attachments: [], evidenceIds: [], createdAt: '2026-01-01T00:00:00Z' }
  const messageOf = (partial: Record<string, unknown>) => ({ ...baseMessage, ...partial })
  const toolCall: ToolCallItem = { id: 't1', attemptId: 'a1', modelCallId: 'c1', callSeq: 1, toolIndex: 0, providerToolCallId: 'p1', toolName: 'thanos_query', toolVersion: '1', arguments: { query: 'up' }, executionMode: 'read_only', failureMode: 'best_effort', status: 'succeeded', rowVersion: 2, result: { resultType: 'vector', result: [] }, createdAt: '2026-01-01T00:00:00Z' }

  function renderTurn(messages: unknown[], attempts: unknown[]) {
    api.list.mockResolvedValue({ items: [] }); api.get.mockResolvedValue(detail)
    api.listMessages.mockResolvedValue({ items: messages })
    api.listAttempts.mockResolvedValue({ items: attempts })
    return render(<View route="/investigations/i1" />)
  }

  async function mockToolCalls() {
    const tools = await import('@/features/investigation/tools/api')
    return vi.mocked(tools.listToolCalls)
  }

  it('binds tool calls to their own turn with real status, summary, and expandable structured detail', async () => {
    const listToolCalls = await mockToolCalls()
    listToolCalls.mockResolvedValue([toolCall])
    renderTurn(
      [messageOf({ id: 'm1', seq: 1, role: 'user', status: 'active', content: '查一下连通性', attemptId: 'a1' }), messageOf({ id: 'm2', seq: 2, role: 'assistant', status: 'active', content: '连通正常', attemptId: 'a1' })],
      [{ id: 'a1', type: 'chat', state: 'Succeeded', rowVersion: 1, createdAt: '2026-01-01T00:00:00Z' }],
    )
    expect(await screen.findByText('Thanos 查询')).toBeInTheDocument()
    expect(screen.getByText('已完成')).toBeInTheDocument()
    // 摘要只来自真实结果形态，不是编造语义。
    expect(screen.getByText('返回对象 · 2 个字段。')).toBeInTheDocument()
    // 展开后是结构化字段，而不是一整块 JSON。
    fireEvent.click(screen.getByRole('button', { name: /Thanos 查询/ }))
    expect(await screen.findByText('query')).toBeInTheDocument()
    expect(screen.getByText('resultType')).toBeInTheDocument()
    // 完整原文折叠保留，展开后可核查。
    fireEvent.click(screen.getAllByRole('button', { name: '原始 JSON' })[0])
    await waitFor(() => expect(document.body.textContent).toContain('"query": "up"'))
    // 旧的线程底部堆叠入口不再存在。
    expect(screen.queryByRole('button', { name: '工具调用' })).not.toBeInTheDocument()
  })

  it('keeps tool cards bound to their own attempt without mixing turns', async () => {
    const listToolCalls = await mockToolCalls()
    listToolCalls.mockImplementation(async (_investigationId: string, attemptId: string) => attemptId === 'a1' ? [{ ...toolCall, id: 't1', toolName: 'thanos_query' }] : [{ ...toolCall, id: 't2', toolName: 'read' }])
    renderTurn(
      [
        messageOf({ id: 'm1', seq: 1, role: 'user', status: 'active', content: '第一问', attemptId: 'a1' }),
        messageOf({ id: 'm2', seq: 2, role: 'assistant', status: 'active', content: '答一', attemptId: 'a1' }),
        messageOf({ id: 'm3', seq: 3, role: 'user', status: 'active', content: '第二问', attemptId: 'a2' }),
        messageOf({ id: 'm4', seq: 4, role: 'assistant', status: 'active', content: '答二', attemptId: 'a2' }),
      ],
      [
        { id: 'a1', type: 'chat', state: 'Succeeded', rowVersion: 1, createdAt: '2026-01-01T00:00:00Z' },
        { id: 'a2', type: 'chat', state: 'Succeeded', rowVersion: 2, createdAt: '2026-01-01T00:01:00Z' },
      ],
    )
    expect(await screen.findByText('Thanos 查询')).toBeInTheDocument()
    expect(await screen.findByText('读取文件')).toBeInTheDocument()
    expect(listToolCalls).toHaveBeenCalledWith('i1', 'a1')
    expect(listToolCalls).toHaveBeenCalledWith('i1', 'a2')
    expect(screen.getAllByText('Thanos 查询')).toHaveLength(1)
    expect(screen.getAllByText('读取文件')).toHaveLength(1)
  })

  it('distinguishes failed, cancelled, and empty tool results without fabricating success', async () => {
    const listToolCalls = await mockToolCalls()
    listToolCalls.mockResolvedValue([
      { ...toolCall, id: 't1', toolName: 'thanos_query', status: 'failed', errorDetail: '上游查询超时', result: undefined },
      { ...toolCall, id: 't2', toolName: 'read', status: 'cancelled', result: undefined },
      { ...toolCall, id: 't3', toolName: 'grep', status: 'succeeded', result: undefined },
    ])
    renderTurn(
      [messageOf({ id: 'm1', seq: 1, role: 'user', status: 'active', content: '查一下', attemptId: 'a1' }), messageOf({ id: 'm2', seq: 2, role: 'assistant', status: 'active', content: '答', attemptId: 'a1' })],
      [{ id: 'a1', type: 'chat', state: 'Succeeded', rowVersion: 1, createdAt: '2026-01-01T00:00:00Z' }],
    )
    expect(await screen.findByText('失败')).toBeInTheDocument()
    expect(screen.getByText('失败原因：上游查询超时')).toBeInTheDocument()
    expect(screen.getByText('已取消')).toBeInTheDocument()
    expect(screen.getByText('执行已取消。')).toBeInTheDocument()
    expect(screen.getByText('成功返回，无结果内容。')).toBeInTheDocument()
  })
})
