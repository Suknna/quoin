import '@testing-library/jest-dom/vitest'
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { useInvestigationsModule } from './index'
import { streamInvestigationMessage } from '@/features/investigation/stream'

const { api, uploadAttachment } = vi.hoisted(() => ({ api: {
  list: vi.fn(), get: vi.fn(), listMessages: vi.fn(), listAttempts: vi.fn(), businessSystems: vi.fn(), create: vi.fn(), sendMessage: vi.fn(), cancelAttempt: vi.fn(), retryAttempt: vi.fn(), undo: vi.fn(),
}, uploadAttachment: vi.fn() }))
vi.mock('@/features/investigation/api', () => ({ api, sourceLabel: (type: string) => type }))
vi.mock('@/features/investigation/attachments/api', () => ({ attachmentCommandId: () => 'upload-command', uploadAttachment }))
vi.mock('@/features/investigation/tools/api', () => ({ listToolCalls: vi.fn() }))
vi.mock('@/features/investigation/stream', () => ({ streamInvestigationMessage: vi.fn() }))
vi.mock('@/features/feedback/api', () => ({ appendFeedback: vi.fn(), fetchFeedback: vi.fn(), feedbackValueLabels: {} }))
vi.mock('@/features/knowledge/api', () => ({ api: { createMessageCandidate: vi.fn() } }))
const user = { id: 'u', username: 'operator', displayName: 'Operator', role: 'operator' as const, passwordChangeRequired: false, authRevision: 1, enabled: true, lastLoginAt: null, rowVersion: 1 }
function View({ route, suspended = false }: { route: string; suspended?: boolean }) { const view = useInvestigationsModule({ user, route, suspended, navigate: vi.fn(), openEvidence: vi.fn() }); return <>{view.list}{view.content}</> }
const detail = { id: 'i1', displayTitle: 'CPU 排查', lastActivityAt: '2026-01-01T00:00:00Z', createdAt: '2026-01-01T00:00:00Z', createdBy: 'u', headMessageId: 'm1', activeAttemptId: 'a1', messageCount: 1, attemptCount: 1, sources: [] }
beforeEach(() => { api.businessSystems.mockResolvedValue([]); Element.prototype.scrollIntoView ??= vi.fn() })
afterEach(() => { cleanup(); vi.clearAllMocks() })
describe('investigations module', () => {
  it('shows the new-conversation workspace on the default route without creating an investigation', async () => {
    api.list.mockResolvedValue({ items: [] })
    render(<View route="/investigations" />)
    expect(await screen.findByRole('textbox', { name: '消息内容' })).toBeInTheDocument()
    expect(screen.getByRole('combobox', { name: '业务系统' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '创建并发送' })).toBeDisabled()
    expect(screen.queryByText('从左侧创建或打开一次调查。')).not.toBeInTheDocument()
    expect(api.create).not.toHaveBeenCalled()
  })
  it('creates the first message atomically with supplied occurrence sources', async () => {
    api.list.mockResolvedValue({ items: [] }); api.create.mockResolvedValue({ ...detail, id: 'i2' })
    render(<View route="/investigations/new?occurrence=o1&initialAnalysis=a1" />)
    fireEvent.change(screen.getByRole('textbox', { name: '消息内容' }), { target: { value: '排查 CPU' } })
    fireEvent.click(screen.getByRole('button', { name: '创建并发送' }))
    await waitFor(() => expect(api.create).toHaveBeenCalledWith('排查 CPU', [{ type: 'occurrence', sourceId: 'o1' }, { type: 'initial_analysis', sourceId: 'a1' }], [], ''))
  })
  it('creates only after a default-route workspace send', async () => {
    api.list.mockResolvedValue({ items: [] }); api.create.mockResolvedValue({ ...detail, id: 'i2' })
    render(<View route="/investigations" />)
    fireEvent.change(await screen.findByRole('textbox', { name: '消息内容' }), { target: { value: '排查 CPU' } })
    fireEvent.click(screen.getByRole('button', { name: '创建并发送' }))
    await waitFor(() => expect(api.create).toHaveBeenCalledWith('排查 CPU', [], [], ''))
  })
  it('sends the explicit business-system selection with the first message', async () => {
    api.list.mockResolvedValue({ items: [] }); api.businessSystems.mockResolvedValue([{ key: 'mall-live-prometheus', displayName: 'Mall Live Prometheus' }]); api.create.mockResolvedValue({ ...detail, id: 'i2' })
    render(<View route="/investigations/new" />)
    fireEvent.click(await screen.findByRole('combobox', { name: '业务系统' }))
    fireEvent.click(await screen.findByText('Mall Live Prometheus'))
    fireEvent.change(screen.getByRole('textbox', { name: '消息内容' }), { target: { value: '检查商城延迟' } })
    fireEvent.click(screen.getByRole('button', { name: '创建并发送' }))
    await waitFor(() => expect(api.create).toHaveBeenCalledWith('检查商城延迟', [], [], 'mall-live-prometheus'))
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
  it('labels the inactive knowledge action as under development', async () => {
    api.list.mockResolvedValue({ items: [] }); api.get.mockResolvedValue(detail)
    api.listMessages.mockResolvedValue({ items: [{ id: 'm1', seq: 1, role: 'assistant', status: 'active', content: '调查结论', attachments: [], evidenceIds: [], createdAt: '2026-01-01T00:00:00Z' }] })
    api.listAttempts.mockResolvedValue({ items: [] })
    render(<View route="/investigations/i1" />)
    expect(await screen.findByRole('button', { name: '知识库开发中' })).toBeDisabled()
  })
  it('shows a failed list request instead of an empty healthy state', async () => {
    api.list.mockRejectedValue(new Error('调查服务不可用'))
    render(<View route="/investigations" />)
    expect(await screen.findByText('调查服务不可用')).toBeInTheDocument()
  })
})
