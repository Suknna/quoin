import '@testing-library/jest-dom/vitest'
import { cleanup, render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { VerificationRunChecks } from './VerificationRunChecks'

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })
}

const runDetail = {
  id: '31',
  purpose: 'prepublish' as const,
  configVersionId: '7',
  labelContractVersionId: '1',
  state: 'Failed' as const,
  rowVersion: 4,
  evidenceAt: '2026-08-27T10:00:00Z',
  createdAt: '2026-08-27T09:59:30Z',
  resultDetail: '存在未通过、部分或失败的配置验证检查',
  checkResults: [
    { planKey: 'browser-plan', checkKey: 'status-page', status: 'ok', evidenceId: '55' },
    { planKey: 'browser-plan', checkKey: 'broken-page', status: 'gap', gapReason: 'journey_failed' },
    { planKey: 'auth-plan', checkKey: 'login-required', status: 'gap', gapReason: 'authentication_required' },
  ],
}

describe('VerificationRunChecks', () => {
  beforeEach(() => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => jsonResponse(runDetail)),
    )
  })
  afterEach(() => {
    cleanup()
    vi.unstubAllGlobals()
  })

  it('renders per-check outcomes with human gap labels and evidence links', async () => {
    render(<VerificationRunChecks systemKey="payments" versionId="7" runId="31" />)
    await waitFor(() => expect(screen.getAllByText('browser-plan').length).toBeGreaterThan(0))
    expect(screen.getByText('status-page').closest('tr')!.textContent).toContain('已留存 Evidence #55')
    expect(screen.getByText('broken-page').closest('tr')!.textContent).toContain('Journey 步骤失败')
    expect(screen.getByText('login-required').closest('tr')!.textContent).toContain('浏览器身份未登录')
    expect(screen.getAllByText('失败').length).toBeGreaterThan(0)
  })

  it('surfaces a read failure without raw JSON', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => jsonResponse({ title: 'boom' }, 500)),
    )
    render(<VerificationRunChecks systemKey="payments" versionId="7" runId="31" />)
    await waitFor(() => expect(screen.getByRole('alert')).toBeInTheDocument())
    expect(screen.queryByText('boom')).not.toBeInTheDocument()
  })
})
