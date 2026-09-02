import { useCallback, useEffect, useState } from 'react'
import { ModelProviderFields } from '../connections/model-provider/ModelProviderFields'
import { buildModelProviderConnection, type ModelProviderFormValue } from '../connections/model-provider/api'
import { rotateConnection } from './api'
import {
  ConnectionsApiError,
  cancelProbeAttempt,
  createConnection,
  disableConnection,
  enableConnection,
  exitMaintenance,
  fetchMaintenanceState,
  fetchConnection,
  fetchProbeAttempt,
  listConnections,
  listProbeResults,
  probeConnection,
  type ConnectionDetailView,
  type ConnectionSummaryView,
  type MaintenanceStateView,
  type CreateConnectionInput,
  type ProbeAttemptView,
  type ProbeResultView,
} from './api'

// Admin connections panel (T07): typed connections with one-time-secret
// creation, supervisor probe dispatch with live attempt state, immutable
// result history and enable/disable with row-version fences.

const typeLabels: Record<ConnectionSummaryView['type'], string> = {
  thanos: 'Thanos 查询',
  kubernetes: 'Kubernetes 只读',
  model_provider: '模型供应商',
}

const stateLabels: Record<ProbeAttemptView['state'], string> = {
  Queued: '排队中（等待运行组件连接）',
  Assigned: '已派发',
  Running: '探测运行中',
  Cancelling: '正在取消',
  Succeeded: '已完成',
  Failed: '失败',
  Cancelled: '已取消',
  Interrupted: '被中断',
}

const activeStates: ProbeAttemptView['state'][] = ['Queued', 'Assigned', 'Running', 'Cancelling']

export function ConnectionsPanel({ maintenanceMode = false }: { maintenanceMode?: boolean }) {
  const [connections, setConnections] = useState<ConnectionSummaryView[]>([])
  const [selected, setSelected] = useState<string | null>(null)
  const [detail, setDetail] = useState<ConnectionDetailView | null>(null)
  const [results, setResults] = useState<ProbeResultView[]>([])
  const [notice, setNotice] = useState('')
  const [error, setError] = useState('')
  const [creating, setCreating] = useState(false)
  const [createType, setCreateType] = useState<'thanos' | 'kubernetes' | 'model_provider'>('thanos')
  const [form, setForm] = useState({ name: '', baseUrl: '', username: '', password: '', kubeconfig: '', defaultNamespace: '' })
  const [providerForm, setProviderForm] = useState<ModelProviderFormValue>({ baseUrl: '', apiKey: '', chatModelId: '', embeddingModelId: '', contextBudgetTokens: '8192', maxOutputTokens: '1024' })
  const [rotating, setRotating] = useState(false)
  const [maintenance, setMaintenance] = useState<MaintenanceStateView | null>(null)

  const reload = useCallback(async (selection = selected) => {
    try {
      const [items, maintenanceState] = await Promise.all([listConnections(), fetchMaintenanceState()])
      setMaintenance(maintenanceState)
      setConnections(items)
      if (selection && items.some((item) => item.name === selection)) {
        const [nextDetail, nextResults] = await Promise.all([fetchConnection(selection), listProbeResults(selection)])
        setDetail(nextDetail)
        setResults(nextResults)
      } else {
        setDetail(null)
        setResults([])
      }
      setError('')
    } catch (loadError) {
      setError(loadError instanceof ConnectionsApiError ? loadError.message : '暂时无法读取连接。')
    }
  }, [selected])

  useEffect(() => {
    // Initial load + visibility-gated refresh (same shape as the users
    // panel); every setState inside reload() runs after its first await.
    // eslint-disable-next-line react-hooks/set-state-in-effect -- static-analysis false positive on this async loader
    void reload()
    const timer = window.setInterval(() => {
      if (document.visibilityState === 'visible') void reload()
    }, 10_000)
    return () => window.clearInterval(timer)
  }, [reload])

  // Poll the active probe attempt so the operator sees dispatch → result
  // without leaving the panel (state updates happen in the async callback,
  // not synchronously within the effect).
  useEffect(() => {
    const active = detail?.activeProbeAttempt
    if (!active || !selected || !activeStates.includes(active.state)) {
      return
    }
    const timer = window.setTimeout(() => {
      void (async () => {
        try {
          await fetchProbeAttempt(selected, active.id)
        } catch {
          // The attempt read failing still refreshes the authoritative view.
        }
        await reload()
      })()
    }, 1500)
    return () => {
      window.clearTimeout(timer)
    }
  }, [detail, selected, reload])

  async function submitCreation(event: React.FormEvent) {
    event.preventDefault()
    setError('')
    setNotice('')
    try {
      if (createType === 'model_provider') {
        await createConnection(form.name.trim(), buildModelProviderConnection(providerForm) as unknown as CreateConnectionInput)
      } else {
        const input: CreateConnectionInput = { type: createType }
        if (createType === 'thanos') {
          input.baseUrl = form.baseUrl.trim()
          input.username = form.username.trim()
          input.password = form.password
        } else {
          input.kubeconfig = form.kubeconfig
          input.defaultNamespace = form.defaultNamespace.trim()
        }
        await createConnection(form.name.trim(), input)
      }
      setNotice('连接已创建。秘密已加密保存，之后无法再次查看。')
      setForm({ name: '', baseUrl: '', username: '', password: '', kubeconfig: '', defaultNamespace: '' })
      setProviderForm({ baseUrl: '', apiKey: '', chatModelId: '', embeddingModelId: '', contextBudgetTokens: '8192', maxOutputTokens: '1024' })
      setCreating(false)
      await reload(form.name.trim())
      setSelected(form.name.trim())
    } catch (createError) {
      setError(createError instanceof ConnectionsApiError ? createError.message : '创建失败，请重试。')
    }
  }

  async function runRotate(event: React.FormEvent) {
    event.preventDefault()
    if (!detail) {
      return
    }
    setError('')
    setNotice('')
    try {
      let connectionBody: Record<string, unknown>
      if (detail.type === 'model_provider') {
        connectionBody = buildModelProviderConnection(providerForm)
        connectionBody.type = 'model_provider'
      } else if (detail.type === 'thanos') {
        connectionBody = { type: 'thanos', baseUrl: form.baseUrl.trim(), username: form.username.trim(), password: form.password }
      } else {
        connectionBody = { type: 'kubernetes', kubeconfig: form.kubeconfig, defaultNamespace: form.defaultNamespace.trim() }
      }
      const rotated = await rotateConnection(detail.name, detail.rowVersion, connectionBody)
      setNotice('凭据已轮换：旧秘密立即停用；需要重新通过探测才能启用。')
      setRotating(false)
      setForm({ name: detail.name, baseUrl: '', username: '', password: '', kubeconfig: '', defaultNamespace: '' })
      setProviderForm({ baseUrl: String((rotated.config as Record<string, unknown>)?.baseUrl ?? ''), apiKey: '', chatModelId: String((rotated.config as Record<string, unknown>)?.chatModelId ?? ''), embeddingModelId: String((rotated.config as Record<string, unknown>)?.embeddingModelId ?? ''), contextBudgetTokens: '8192', maxOutputTokens: '1024' })
      await reload()
    } catch (rotateError) {
      setError(rotateError instanceof ConnectionsApiError ? rotateError.message : '轮换失败，请刷新后重试。')
      await reload()
    }
  }

  async function runProbe() {
    if (!selected) {
      return
    }
    setError('')
    setNotice('')
    try {
      const attempt = await probeConnection(selected)
      setNotice(`探测已受理（${stateLabels[attempt.state]}）。`)
      await reload()
    } catch (probeError) {
      setError(probeError instanceof ConnectionsApiError ? probeError.message : '探测发起失败。')
    }
  }

  async function cancelActive() {
    const active = detail?.activeProbeAttempt
    if (!selected || !active) {
      return
    }
    setError('')
    try {
      await cancelProbeAttempt(selected, active.id, active.rowVersion)
      setNotice('取消请求已提交。')
      await reload()
    } catch (cancelError) {
      setError(cancelError instanceof ConnectionsApiError ? cancelError.message : '取消失败。')
      await reload()
    }
  }

  async function finishRootKeyRebind() {
    if (!maintenance || maintenance.reason !== 'RootKeyRebind') return
    try {
      await exitMaintenance(maintenance.rowVersion)
      setMaintenance(null)
      setNotice('维护状态已退出。请重启 Quoin 以恢复普通服务。')
    } catch (reason) {
      setError(reason instanceof ConnectionsApiError ? reason.message : '退出维护失败，请刷新后重试。')
      await reload()
    }
  }

  async function confirmDisabled() {
    if (!detail) return
    try {
      const updated = await disableConnection(detail.name, detail.rowVersion)
      setNotice(`已确认 ${updated.name} 保持停用。`)
      await reload(updated.name)
    } catch (reason) {
      setError(reason instanceof ConnectionsApiError ? reason.message : '暂时无法确认停用，请刷新后重试。')
      await reload()
    }
  }

  async function toggleEnabled() {
    if (!detail) {
      return
    }
    setError('')
    setNotice('')
    try {
      if (detail.enabled) {
        await disableConnection(detail.name, detail.rowVersion)
        setNotice('连接已停用。')
      } else if (detail.type === 'model_provider') {
        // Model providers enable only against an explicit passed
        // qualification: use the newest passed result of this connection.
        const passed = [...results].reverse().find((result) => result.outcome === 'passed')
        if (!passed) {
          setError('模型供应商需要先通过探测，才能启用。')
          return
        }
        await enableConnection(detail.name, detail.rowVersion, passed.id)
        setNotice('连接已启用。')
      } else {
        await enableConnection(detail.name, detail.rowVersion)
        setNotice('连接已启用。')
      }
      await reload()
    } catch (toggleError) {
      setError(toggleError instanceof ConnectionsApiError ? toggleError.message : '操作失败，请刷新后重试。')
      await reload()
    }
  }

  return (
    <div className="detail-content admin-connections">
      <header className="admin-connections-head">
        <h2>连接</h2>
        {!maintenanceMode && <button className="text-button compact" onClick={() => setCreating(true)}>新建连接</button>}
      </header>
      {notice && <p className="admin-notice" role="status">{notice}</p>}
      {error && <p className="form-error" role="alert">{error}</p>}
      {maintenance?.active && maintenance.reason === 'RootKeyRebind' && (
        <section className="root-key-rebind-notice" aria-labelledby="root-key-rebind-title">
          <h3 id="root-key-rebind-title">正在更换根密钥</h3>
          <p>旧凭据已不可读取。逐一重新输入凭据，或明确停用不再使用的连接。</p>
          <ul role="list">{maintenance.items.filter((item) => item.kind === 'Connection').map((item) => <li key={item.objectKey}><strong>{item.objectKey}</strong><span className={item.safeState === 'Safe' ? 'root-key-rebind-safe' : 'root-key-rebind-blocking'}>{item.safeState === 'Safe' ? (item.detailCode === 'disabled' ? '已停用' : '已重新输入凭据') : '需要处理'}</span></li>)}</ul>
          {maintenance.items.some((item) => item.safeState === 'Blocking') ? <p>完成全部清单项目后才能退出维护。</p> : <button onClick={finishRootKeyRebind}>退出维护</button>}
        </section>
      )}
      {!maintenanceMode && creating && (
        <form className="admin-create-form admin-reset-form" onSubmit={submitCreation}>
          <h3>新建连接</h3>
          <label>
            类型
            <select value={createType} onChange={(event) => setCreateType(event.target.value as 'thanos' | 'kubernetes' | 'model_provider')}>
              <option value="thanos">Thanos 查询</option>
              <option value="kubernetes">Kubernetes 只读</option>
              <option value="model_provider">模型供应商</option>
            </select>
          </label>
          <label>
            名称
            <input value={form.name} onChange={(event) => setForm({ ...form, name: event.target.value })} required maxLength={200} placeholder="main-thanos" />
          </label>
          {createType === 'thanos' ? (
            <>
              <label>
                查询入口 URL
                <input value={form.baseUrl} onChange={(event) => setForm({ ...form, baseUrl: event.target.value })} required placeholder="https://thanos.example.com" />
              </label>
              <label>
                用户名（可选）
                <input value={form.username} onChange={(event) => setForm({ ...form, username: event.target.value })} placeholder="probe" />
              </label>
              <label>
                密码（一次性提交，保存后不可查看）
                <input type="password" value={form.password} onChange={(event) => setForm({ ...form, password: event.target.value })} autoComplete="new-password" />
              </label>
            </>
          ) : createType === 'kubernetes' ? (
            <>
              <label>
                kubeconfig（一次性提交，保存后不可查看）
                <textarea className="admin-kubeconfig" value={form.kubeconfig} onChange={(event) => setForm({ ...form, kubeconfig: event.target.value })} required rows={8} spellCheck={false} placeholder="apiVersion: v1&#10;kind: Config&#10;..." />
              </label>
              <label>
                默认命名空间（可选）
                <input value={form.defaultNamespace} onChange={(event) => setForm({ ...form, defaultNamespace: event.target.value })} placeholder="default" />
              </label>
            </>
          ) : (
            <ModelProviderFields value={providerForm} onChange={setProviderForm} />
          )}
          <div className="admin-action-row">
            <button type="submit">创建</button>
            <button type="button" className="text-button" onClick={() => setCreating(false)}>取消</button>
          </div>
        </form>
      )}
      <ul className="admin-connection-list" role="list">
        {connections.map((connection) => (
          <li key={connection.name} className="object-row">
            <button
              className={`object-row-main${selected === connection.name ? ' selected' : ''}`}
              onClick={() => {
                setSelected(connection.name)
                void reload(connection.name)
              }}
            >
              <strong>{connection.name}</strong>
              <span className="admin-muted">
                {typeLabels[connection.type]} · {connection.enabled ? '已启用' : '未启用'}
                {connection.revalidationRequired ? ' · 需重新验证' : ''}
              </span>
            </button>
          </li>
        ))}
        {connections.length === 0 && <li className="admin-muted">{maintenanceMode ? '没有需要重新处理的连接。' : '还没有连接。点击“新建连接”创建第一个。'}</li>}
      </ul>
      {detail && (
        <section className="connection-detail-card" aria-labelledby="connection-detail-title">
          <h3 id="connection-detail-title">{detail.name}</h3>
          <div className="runtime-facts">
            <span className="status-pill">{typeLabels[detail.type]}</span>
            <span>{detail.enabled ? '已启用' : '未启用'}</span>
            <span>revision × {detail.revisionCount}</span>
            <span>凭据 × {detail.generationCount}</span>
          </div>
          <div className="admin-action-row">
            {!maintenanceMode && <button onClick={runProbe}>运行探测</button>}
            {maintenanceMode && !detail.enabled && <button className="text-button" onClick={confirmDisabled}>确认保持停用</button>}
            {detail.activeProbeAttempt ? (
              <button className="text-button" onClick={cancelActive}>
                取消探测（{stateLabels[detail.activeProbeAttempt.state]}）
              </button>
            ) : (detail.enabled || !maintenanceMode) ? (
              <button className="text-button" onClick={toggleEnabled}>{detail.enabled ? '停用' : '启用'}</button>
            ) : null}
            <button className="text-button" onClick={() => setRotating(!rotating)}>轮换凭据</button>
          </div>
          {rotating && (
            <form className="admin-create-form admin-reset-form" onSubmit={runRotate}>
              <h3>轮换 {detail.name} 的凭据</h3>
              <p className="admin-muted">提交新秘密后旧秘密立即停用；探测与启用都针对新的凭据。</p>
              {detail.type === 'model_provider' ? (
                <ModelProviderFields value={providerForm} onChange={setProviderForm} />
              ) : detail.type === 'thanos' ? (
                <>
                  <label>
                    查询入口 URL
                    <input value={form.baseUrl} onChange={(event) => setForm({ ...form, baseUrl: event.target.value })} required />
                  </label>
                  <label>
                    用户名（可选）
                    <input value={form.username} onChange={(event) => setForm({ ...form, username: event.target.value })} />
                  </label>
                  <label>
                    新密码（一次性提交，保存后不可查看）
                    <input type="password" value={form.password} onChange={(event) => setForm({ ...form, password: event.target.value })} autoComplete="new-password" />
                  </label>
                </>
              ) : (
                <label>
                  新 kubeconfig（一次性提交，保存后不可查看）
                  <textarea className="admin-kubeconfig" value={form.kubeconfig} onChange={(event) => setForm({ ...form, kubeconfig: event.target.value })} required rows={8} spellCheck={false} />
                </label>
              )}
              <div className="admin-action-row">
                <button type="submit">提交轮换</button>
                <button type="button" className="text-button" onClick={() => setRotating(false)}>取消</button>
              </div>
            </form>
          )}
          {detail.activeProbeAttempt && (
            <p className="admin-muted" role="status">
              当前探测：{stateLabels[detail.activeProbeAttempt.state]}
              {detail.activeProbeAttempt.state === 'Queued' ? '，将在 Plinth 连接后自动派发。' : ''}
            </p>
          )}
          <h4>探测历史</h4>
          <ul className="admin-audit-list" role="list">
            {results.map((result) => (
              <li key={result.id}>
                <span className={result.outcome === 'passed' ? '' : 'admin-danger'}>
                  {result.outcome === 'passed' ? '通过' : result.outcome === 'failed' ? '失败' : result.outcome === 'cancelled' ? '已取消' : '被中断'}
                </span>
                <span className="admin-muted">{new Date(result.finishedAt).toLocaleString()}</span>
                <span className="admin-muted admin-mono">{result.actionSetId} v{result.actionSetVersion}</span>
              </li>
            ))}
            {results.length === 0 && <li className="admin-muted">还没有探测记录。</li>}
          </ul>
        </section>
      )}
    </div>
  )
}
