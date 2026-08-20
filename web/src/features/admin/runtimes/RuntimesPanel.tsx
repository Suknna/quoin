import { type FormEvent, useCallback, useEffect, useState } from 'react'
import {
  fetchRuntimeStatus,
  formatRuntimeTime,
  retireRuntimeCredential,
  revealRegistrationToken,
  prepareRegistration,
  RuntimesApiError,
  type RegistrationTokenRevealResult,
  type RuntimeSlotView,
  type RuntimeStatusView,
} from './api'

// RuntimesPanel owns the /admin runtime surface (T06): slot readiness cards
// with the prepare → reveal → register command path and the retire flow.
// The one-time registration token is shown in a 60-second reveal box with a
// copy button and never enters a URL or persisted state.

const SLOT_TITLES: Record<'plinth' | 'lintel', string> = {
  plinth: 'Plinth（分析执行组件）',
  lintel: 'Lintel（浏览器执行组件）',
}

const SLOT_HINTS: Record<'plinth' | 'lintel', string> = {
  plinth: '在容器内执行：docker compose exec plinth /plinth register --config /etc/quoin/component.yaml',
  lintel: '在容器内执行：docker compose exec lintel /lintel register --config /etc/quoin/component.yaml',
}

export function RuntimesPanel() {
  const [status, setStatus] = useState<RuntimeStatusView | null>(null)
  const [loadError, setLoadError] = useState(false)
  const [notice, setNotice] = useState('')
  const [error, setError] = useState('')
  const [revealed, setRevealed] = useState<RegistrationTokenRevealResult | null>(null)
  const [revealedSlot, setRevealedSlot] = useState<'plinth' | 'lintel' | null>(null)
  const [countdown, setCountdown] = useState(0)

  const reload = useCallback(async () => {
    try {
      setStatus(await fetchRuntimeStatus())
      setLoadError(false)
    } catch {
      setLoadError(true)
    }
  }, [])

  useEffect(() => {
    // Initial load + visibility-gated refresh (same shape as the users
    // panel); every setState inside reload() runs after its first await.
    // eslint-disable-next-line react-hooks/set-state-in-effect -- static-analysis false positive on this async loader
    void reload()
    const timer = window.setInterval(() => {
      if (document.visibilityState === 'visible') void reload()
    }, 5_000)
    return () => window.clearInterval(timer)
  }, [reload])

  useEffect(() => {
    if (countdown <= 0) return
    const timer = window.setTimeout(() => setCountdown((value) => value - 1), 1_000)
    return () => window.clearTimeout(timer)
  }, [countdown])

  // The countdown reaching zero clears the reveal through the render path:
  // when countdown is 0 no token box renders, and the reveal is dropped on
  // the next prepare. Keeping this as render-time logic avoids the effect.


  async function onPrepare(slot: 'plinth' | 'lintel') {
    setError('')
    setNotice('')
    const current = status?.[slot]
    if (!current) return
    try {
      const preparation = await prepareRegistration(slot, current.rowVersion)
      setNotice(
        preparation.state === 'revoked'
          ? '已进入替换注册：旧凭据已全部退休，请用新令牌注册。'
          : '已进入首次注册：请复制注册令牌并在组件内完成注册。',
      )
      if (preparation.registrationTokenAvailable && preparation.registrationTokenHandle) {
        const reveal = await revealRegistrationToken(preparation.registrationTokenHandle)
        setRevealed(reveal)
        setRevealedSlot(slot)
        setCountdown(60)
      }
      await reload()
    } catch (reason) {
      setError(messageOf(reason))
      await reload()
    }
  }

  async function onRetire(slot: 'plinth' | 'lintel') {
    setError('')
    setNotice('')
    const current = status?.[slot]
    if (!current) return
    try {
      await retireRuntimeCredential(slot, current.rowVersion)
      setNotice('旧凭据已退休；该组件现在只接受当前凭据。')
      await reload()
    } catch (reason) {
      setError(messageOf(reason))
      await reload()
    }
  }

  function copyToken(event: FormEvent) {
    event.preventDefault()
    if (!revealed) return
    void navigator.clipboard?.writeText(JSON.stringify({ slot: revealed.slot, generation: revealed.generation, token: revealed.registrationToken }))
    setNotice('已复制注册 JSON（含令牌），请在 60 秒内使用。')
  }

  return (
    <div className="detail-content">
      <header className="detail-header">
        <div>
          <p className="eyebrow">管理</p>
          <h1>运行组件注册</h1>
        </div>
      </header>
      <p className="detail-muted">
        Plinth 和 Lintel 通过一次性注册令牌接入：准备注册后 60 秒内把令牌交给组件的注册命令；组件会建立长期连接并在握手通过后进入就绪。
      </p>
      {error && <div className="error-summary" role="alert"><strong>操作没有完成</strong><span>{error}</span></div>}
      {notice && <p className="admin-notice" role="status">{notice}</p>}
      {loadError && <p className="detail-muted">暂时无法读取组件状态，正在重试…</p>}
      {status && (['plinth', 'lintel'] as const).map((slot) => (
        <RuntimeCard
          key={slot}
          view={status[slot]}
          onPrepare={() => void onPrepare(slot)}
          onRetire={() => void onRetire(slot)}
          reveal={revealedSlot === slot ? revealed : null}
          countdown={countdown}
          onCopy={copyToken}
        />
      ))}
    </div>
  )
}

interface RuntimeCardProps {
  view: RuntimeSlotView
  onPrepare: () => void
  onRetire: () => void
  reveal: RegistrationTokenRevealResult | null
  countdown: number
  onCopy: (event: FormEvent) => void
}

function RuntimeCard({ view, onPrepare, onRetire, reveal, countdown, onCopy }: RuntimeCardProps) {
  const [confirmReplace, setConfirmReplace] = useState(false)
  const stateText =
    view.state === 'registered'
      ? view.connected
        ? '已连接'
        : '已注册 · 等待组件连接'
      : view.state === 'revoked'
        ? '已吊销 · 等待重新注册'
        : '尚未注册'
  return (
    <section className="detail-section runtime-card" aria-labelledby={`runtime-${view.slot}-title`}>
      <h2 id={`runtime-${view.slot}-title`}>{SLOT_TITLES[view.slot]}</h2>
      <div className="runtime-facts">
        <span className="status-pill">{stateText}</span>
        <span>当前凭据 generation：{view.currentGeneration || '—'}</span>
        {view.retiringGeneration != null && (
          <span className="admin-warn">
            旧凭据 generation {view.retiringGeneration} · {view.retirementState === 'PendingRetirement' ? '已待退休' : '等待新凭据首次使用'}
          </span>
        )}
        {view.connected && <span>boot {view.bootId?.slice(0, 8)} · epoch {view.connectionEpoch}</span>}
        {view.connected && <span>最近心跳 {formatRuntimeTime(view.lastSeenAt)}</span>}
      </div>
      <div className="admin-action-row">
        {view.state !== 'unregistered' && !confirmReplace ? (
          <button className="text-button compact" onClick={() => setConfirmReplace(true)}>替换注册（吊销旧凭据）</button>
        ) : view.state !== 'unregistered' ? (
          <>
            <button className="text-button compact admin-danger" onClick={() => { setConfirmReplace(false); onPrepare() }}>确认替换</button>
            <button className="text-button compact" onClick={() => setConfirmReplace(false)}>取消</button>
          </>
        ) : (
          <button className="primary-button compact" onClick={onPrepare}>准备注册</button>
        )}
        {view.retirementState === 'PendingRetirement' && (
          <button className="text-button compact" onClick={onRetire}>退休旧凭据</button>
        )}
      </div>
      {reveal && (
        <div className="credential-reveal">
          <strong>一次性注册令牌（{countdown} 秒内有效）</strong>
          <code>{reveal.registrationToken}</code>
          <p>generation {reveal.generation} · 令牌只显示这一次；复制后在组件内执行：</p>
          <code>{SLOT_HINTS[reveal.slot]}</code>
          <button className="text-button compact" onClick={onCopy}>复制注册 JSON</button>
        </div>
      )}
    </section>
  )
}

function messageOf(reason: unknown): string {
  if (reason instanceof RuntimesApiError) return reason.message
  return reason instanceof Error ? reason.message : '暂时无法完成操作，请重试。'
}
