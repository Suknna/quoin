import '@testing-library/jest-dom/vitest'
import { render, screen, waitFor } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { App } from './App'

const user = {
  id: '1', username: 'admin', displayName: 'Admin', role: 'admin', enabled: true,
  authRevision: 1, rowVersion: 1, passwordChangeRequired: true, lastLoginAt: new Date().toISOString(),
}

describe('App authentication projection', () => {
  beforeEach(() => {
    vi.stubGlobal('fetch', vi.fn())
  })

  it('shows login after an unauthenticated current-user response', async () => {
    vi.mocked(fetch).mockResolvedValue(new Response(JSON.stringify({ detail: 'unauthorized' }), { status: 401, headers: { 'Content-Type': 'application/json' } }))
    render(<App />)
    expect(screen.getByText('正在确认登录状态…')).toBeInTheDocument()
    expect(await screen.findByRole('heading', { name: '登录工作台' })).toBeInTheDocument()
  })

  it('keeps a temporary-password session out of the workbench', async () => {
    vi.mocked(fetch).mockResolvedValue(new Response(JSON.stringify(user), { status: 200, headers: { 'Content-Type': 'application/json' } }))
    render(<App />)
    expect(await screen.findByRole('heading', { name: '先设置你自己的密码' })).toBeInTheDocument()
    await waitFor(() => expect(screen.queryByLabelText('全局模块')).not.toBeInTheDocument())
  })
})
