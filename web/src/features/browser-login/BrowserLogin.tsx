import RFB from '@novnc/novnc'
import { useCallback, useEffect, useRef, useState } from 'react'
import {
  type BrowserIdentity,
  type BrowserOperation,
  cancelBrowserOperation,
  formatTime,
  getBrowserIdentity,
  getBrowserOperation,
  publishBrowserProfile,
  startBrowserManualLogin,
} from '../admin/business-systems/api'

export function BrowserLogin({ systemKey, onPublished }: { systemKey: string; onPublished?: () => void }) {
  const viewport = useRef<HTMLDivElement>(null)
  const exitRemoteKeyboard = useRef<HTMLButtonElement>(null)
  const remoteKeyboardActive = useRef(false)
  const rfb = useRef<RFB | null>(null)
  const [identity, setIdentity] = useState<BrowserIdentity | null>(null)
  const [operation, setOperation] = useState<BrowserOperation | null>(null)
  const [message, setMessage] = useState('正在读取浏览器身份…')
  const [error, setError] = useState('')
  const [pending, setPending] = useState(false)
  const [attachmentAttempt, setAttachmentAttempt] = useState(0)

  const refreshIdentity = useCallback(async () => {
    const value = await getBrowserIdentity(systemKey)
    setIdentity(value)
    setOperation((current) => value.currentOperation ?? current)
    return value
  }, [systemKey])

  const applyOperation = useCallback((value: BrowserOperation) => {
    setOperation(value)
    if (value.state === 'Succeeded') {
      setMessage('浏览器身份已发布并完成认证。')
      void refreshIdentity()
      onPublished?.()
    } else if (['Failed', 'Cancelled', 'Interrupted'].includes(value.state)) {
      setMessage(`浏览器登录已结束：${value.terminalReason ?? value.state}。`)
      void refreshIdentity()
    }
  }, [onPublished, refreshIdentity])

  const refreshOperation = useCallback(async (id: string) => {
    const value = await getBrowserOperation(systemKey, id)
    applyOperation(value)
    return value
  }, [applyOperation, systemKey])

  useEffect(() => {
    let active = true
    void refreshIdentity()
      .then((value) => {
        if (!active) return
        setMessage(value.currentOperation ? '正在恢复当前浏览器登录状态…' : '需要时可重新登录此浏览器身份。')
      })
      .catch((reason: unknown) => active && setError(reason instanceof Error ? reason.message : '暂时无法读取浏览器身份。'))
    return () => { active = false }
  }, [refreshIdentity])

  useEffect(() => {
    if (!operation || ['Succeeded', 'Failed', 'Cancelled', 'Interrupted'].includes(operation.state)) return
    const timer = window.setTimeout(() => { void refreshOperation(operation.id).catch(() => undefined) }, 1000)
    return () => window.clearTimeout(timer)
  }, [operation, refreshOperation])

  useEffect(() => {
    if (!operation?.canAttach || !viewport.current || rfb.current) return
    const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:'
    const endpoint = `${protocol}//${window.location.host}/api/v1/browser-login/${encodeURIComponent(systemKey)}/operations/${encodeURIComponent(operation.id)}/ws`
    const client = new RFB(viewport.current, endpoint, { shared: false })
    let disposed = false
    const leaveRemoteKeyboard = (event: KeyboardEvent) => {
      if (!remoteKeyboardActive.current || event.key !== 'Escape' || !event.shiftKey) return
      // Capture this local escape before noVNC forwards it to the remote page.
      // The operator can always return to the surrounding action controls.
      event.preventDefault()
      event.stopImmediatePropagation()
      remoteKeyboardActive.current = false
      viewport.current?.querySelector('canvas')?.blur()
      exitRemoteKeyboard.current?.focus()
      setMessage('已退出远程键盘；可继续发布、取消或再次进入远程浏览器。')
    }
    window.addEventListener('keydown', leaveRemoteKeyboard, true)
    client.scaleViewport = true
    client.resizeSession = false
    // This is an operator-controlled manual-login surface: always explicitly
    // enable noVNC input instead of relying on the library default.
    client.viewOnly = false
    client.addEventListener('connect', () => {
      // Connecting never steals focus from Workbench controls. The operator
      // explicitly enters the remote keyboard surface when ready to type.
      setMessage('安全浏览器已连接。选择“进入远程浏览器”后开始输入。')
    })
    client.addEventListener('disconnect', () => {
      if (rfb.current === client) rfb.current = null
      if (disposed) return
      setMessage('安全浏览器连接中断；正在保留登录状态以便重连。')
      // `canAttach` stays true across Running → AwaitingReconnect. A monotonic
      // attempt is therefore the explicit re-attach trigger, rather than
      // relying on a state-field change that may not occur.
      setAttachmentAttempt((attempt) => attempt + 1)
      void refreshOperation(operation.id).catch(() => undefined)
    })
    rfb.current = client
    return () => {
      disposed = true
      remoteKeyboardActive.current = false
      window.removeEventListener('keydown', leaveRemoteKeyboard, true)
      if (rfb.current === client) rfb.current = null
      client.disconnect()
    }
  }, [attachmentAttempt, operation?.canAttach, operation?.id, refreshOperation, systemKey])

  async function start() {
    if (!identity) return
    setPending(true)
    setError('')
    setMessage('浏览器登录已受理，正在准备安全窗口…')
    try {
      setOperation(await startBrowserManualLogin(systemKey, identity.rowVersion))
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : '暂时无法启动浏览器登录。')
    } finally { setPending(false) }
  }

  async function act(action: 'publish' | 'cancel') {
    if (!operation) return
    setPending(true)
    setError('')
    setMessage(action === 'publish' ? '正在验证并发布浏览器身份…' : '正在取消浏览器登录…')
    try {
      const result = action === 'publish' ? await publishBrowserProfile(systemKey, operation) : await cancelBrowserOperation(systemKey, operation)
      applyOperation(result)
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : '暂时无法完成浏览器操作。')
    } finally { setPending(false) }
  }

  if (error && !identity) return <section className="browser-login" aria-labelledby="browser-login-title"><h3 id="browser-login-title">浏览器登录</h3><div className="error-summary" role="alert">{error}<button className="secondary-button compact" onClick={() => void refreshIdentity()}>重试</button></div></section>
  if (!identity) return <section className="browser-login" aria-labelledby="browser-login-title"><h3 id="browser-login-title">浏览器登录</h3><p className="inline-status">正在读取浏览器身份…</p></section>

  const active = operation && !['Succeeded', 'Failed', 'Cancelled', 'Interrupted'].includes(operation.state)
  return (
    <section className="browser-login" aria-labelledby="browser-login-title">
      <div className="section-heading"><h3 id="browser-login-title">浏览器登录</h3><span className={`status-pill ${identity.state === 'Ready' ? 'ok' : 'waiting'}`}>{identity.state === 'Ready' ? '已认证' : '需要登录'}</span></div>
      <p className="inline-status">{identity.currentRevision.name} · 此窗口只连接目标系统；不会记录登录输入、页面内容或屏幕画面。</p>
      {identity.lastProbe && <p className="browser-login-fact">最近认证：{identity.lastProbe.result} · {formatTime(identity.lastProbe.observedAt)}</p>}
      {operation?.reconnectDeadline && <p className="browser-login-fact">连接中断后可在 {formatTime(operation.reconnectDeadline)} 前重新附着。</p>}
      <p className="inline-status" role="status">{message}</p>
      {error && <p className="error-summary" role="alert">{error}</p>}
      {!active && <button className="primary-button" onClick={() => void start()} disabled={pending}>{pending ? '正在提交…' : '重新登录'}</button>}
      {active && <>
        {operation.canAttach ? <>
          <div className="browser-login-viewport" ref={viewport} aria-label="安全远程浏览器" />
          <p className="browser-login-fact">选择“进入远程浏览器”后才会把键盘发送到远程页面。按 Shift+Esc 或选择“退出远程键盘”可回到本页操作。</p>
        </> : <p className="inline-status">运行时正在准备浏览器；此页会自动连接。</p>}
        <div className="browser-login-actions">
          {operation.canAttach && <button className="secondary-button" onClick={() => { remoteKeyboardActive.current = true; rfb.current?.focus(); setMessage('正在向远程浏览器发送键盘输入。按 Shift+Esc 可退出远程键盘。') }}>进入远程浏览器</button>}
          <button ref={exitRemoteKeyboard} className="secondary-button" onClick={() => { remoteKeyboardActive.current = false; viewport.current?.querySelector('canvas')?.blur(); setMessage('已退出远程键盘；可继续发布、取消或再次进入远程浏览器。') }}>退出远程键盘</button>
          <button className="primary-button" onClick={() => void act('publish')} disabled={pending || !operation.canPublish}>{pending ? '正在提交…' : '完成登录并发布'}</button>
          <button className="secondary-button" onClick={() => void act('cancel')} disabled={pending || !operation.canCancel}>取消登录</button>
        </div>
      </>}
    </section>
  )
}
