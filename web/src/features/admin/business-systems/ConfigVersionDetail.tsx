import { useCallback, useEffect, useRef, useState } from 'react'
import {
  cancelVerification,
  formatTime,
  getConfigVersion,
  listVerificationRuns,
  publishBusinessSystemConfig,
  runVerification,
  verificationStateText,
  ConfigApiError,
  type ConfigVersionDetail,
  type VerificationRunSummary,
} from './api'
import { DiscoveryTable, PlanTable } from './BusinessSystemDetail'
import { VerificationRunChecks } from './verification/VerificationRunChecks'

// The immutable version detail (UI-SYSTEM-005): metadata and typed
// projections, the exportable YAML body, and the Admin publish action with
// an explicit confirm that carries expectedCurrentPublishedVersionId. A 409
// re-reads the current pointer instead of overwriting anything.

interface ConfigVersionPageProps {
  systemKey: string
  versionId: string
  isAdmin: boolean
  onBack: () => void
  onPublished: () => void
}

export function ConfigVersionPage({ systemKey, versionId, isAdmin, onBack, onPublished }: ConfigVersionPageProps) {
  const [detail, setDetail] = useState<ConfigVersionDetail | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [publishing, setPublishing] = useState(false)
  const [confirming, setConfirming] = useState(false)
  const [publishError, setPublishError] = useState<string | null>(null)
  const [showYAML, setShowYAML] = useState(false)
  const [runs, setRuns] = useState<VerificationRunSummary[]>([])
  const [runsError, setRunsError] = useState('')
  const [running, setRunning] = useState(false)
  const [cancellingRunID, setCancellingRunID] = useState<string | null>(null)
  const [runError, setRunError] = useState('')
  const [expandedRunID, setExpandedRunID] = useState<string | null>(null)
  const publishedNotified = useRef(false)
  // A ref closes the interval before React can commit disabled button state,
  // so rapid double-click/Space can never create a second cancel command.
  const cancellingRunRef = useRef<string | null>(null)
  const runsRequestRef = useRef(0)

  const loadRuns = useCallback(() => {
    const request = ++runsRequestRef.current
    void listVerificationRuns(systemKey, versionId)
      .then((items) => {
        if (request !== runsRequestRef.current) return
        setRuns(items)
        setRunsError('')
      })
      .catch((reason: unknown) => {
        if (request !== runsRequestRef.current) return
        setRunsError(reason instanceof Error ? reason.message : '暂时无法读取验证历史。')
      })
  }, [systemKey, versionId])

  useEffect(() => {
    let cancelled = false
    setDetail(null)
    setError(null)
    void getConfigVersion(systemKey, versionId)
      .then((value) => {
        if (!cancelled) setDetail(value)
      })
      .catch((reason: unknown) => {
        if (!cancelled) setError(reason instanceof Error ? reason.message : '暂时无法读取配置版本。')
      })
    loadRuns()
    return () => {
      cancelled = true
      // A version switch/unmount invalidates its asynchronous run-history
      // response so it cannot replace the next version's view.
      runsRequestRef.current += 1
    }
  }, [systemKey, versionId, loadRuns])

  // While a run is active the UI keeps polling its authoritative state so
  // the operator sees the real terminal outcome without a manual refresh.
  useEffect(() => {
    if (!runs.some((run) => run.state === 'Queued' || run.state === 'Running')) return
    const timer = window.setTimeout(loadRuns, 2000)
    return () => window.clearTimeout(timer)
  }, [runs, loadRuns])

  if (error) {
    return (
      <div className="detail-content">
        <div className="error-summary" role="alert">
          <strong>暂时无法读取配置版本</strong>
          <span>{error}</span>
          <button className="secondary-button compact" onClick={onBack}>
            返回系统详情
          </button>
        </div>
      </div>
    )
  }
  if (!detail) {
    return (
      <div className="detail-content">
        <p className="inline-status">正在加载配置版本…</p>
      </div>
    )
  }

  async function startRun() {
    setRunning(true)
    setRunError('')
    try {
      await runVerification(systemKey, versionId)
      loadRuns()
    } catch (reason) {
      setRunError(reason instanceof Error ? reason.message : '创建验证 Run 失败。')
    } finally {
      setRunning(false)
    }
  }

  async function cancelRun(run: VerificationRunSummary) {
    if (cancellingRunRef.current !== null) return
    cancellingRunRef.current = run.id
    setCancellingRunID(run.id)
    setRunError('')
    try {
      await cancelVerification(systemKey, versionId, run.id, run.rowVersion)
    } catch (reason) {
      setRunError(reason instanceof Error ? reason.message : '取消验证 Run 失败。')
    } finally {
      cancellingRunRef.current = null
      setCancellingRunID(null)
      loadRuns()
    }
  }

  async function publish() {
    setPublishing(true)
    setPublishError(null)
    try {
      // The expected fence comes from the version detail's own view of the
      // system; the server re-checks it inside the publish transaction.
      const systemResponse = await fetch(`/api/v1/business-systems/${encodeURIComponent(systemKey)}`, {
        credentials: 'include',
      })
      if (!systemResponse.ok) throw new ConfigApiError('暂时无法读取当前发布状态，请重试。', systemResponse.status)
      const system = (await systemResponse.json()) as { currentConfigVersionId?: string | null }
      await publishBusinessSystemConfig(systemKey, versionId, system.currentConfigVersionId ?? null)
      publishedNotified.current = true
      setConfirming(false)
      onPublished()
    } catch (reason) {
      setPublishError(reason instanceof Error ? reason.message : '发布失败，请重试。')
    } finally {
      setPublishing(false)
    }
  }

  const stateLabel =
    detail.state === 'published' ? '已发布' : detail.state === 'draft' ? '草稿' : '已替代'

  return (
    <div className="detail-content config-version-detail">
      <div className="detail-header">
        <button className="secondary-button compact" onClick={onBack}>
          返回系统详情
        </button>
        <h2>
          配置版本 v{detail.versionSeq}
          <span className={`status-pill ${detail.state === 'published' ? 'ok' : detail.state === 'draft' ? 'waiting' : 'muted'}`}>
            {stateLabel}
          </span>
        </h2>
      </div>
      <dl className="fact-list">
        <div>
          <dt>显示名</dt>
          <dd>{detail.displayName}</dd>
        </div>
        <div>
          <dt>启用状态（发布时生效）</dt>
          <dd>{detail.enabled ? 'Enabled' : 'Disabled'}</dd>
        </div>
        <div>
          <dt>时区</dt>
          <dd>{detail.timezone}</dd>
        </div>
        <div>
          <dt>创建时间</dt>
          <dd>{formatTime(detail.createdAt)}</dd>
        </div>
        {detail.publishedAt && (
          <div>
            <dt>发布时间</dt>
            <dd>{formatTime(detail.publishedAt)}</dd>
          </div>
        )}
        <div>
          <dt>Digest</dt>
          <dd>
            <code title={detail.digest}>{detail.digest.slice(0, 24)}…</code>
          </dd>
        </div>
        <div>
          <dt>解析/Schema 版本</dt>
          <dd>
            {detail.parserVersion} / {detail.schemaVersion}
          </dd>
        </div>
        <div>
          <dt>Label Contract</dt>
          <dd>版本行 #{detail.labelContractVersionId}</dd>
        </div>
        <div>
          <dt>Journey Catalog</dt>
          <dd>
            {detail.journeyCatalogVersion} · <code>{detail.journeyCatalogDigest.slice(0, 12)}…</code>
          </dd>
        </div>
      </dl>

      {isAdmin && detail.state === 'draft' && (
        <section aria-labelledby="cv-verify-title" className="verify-block">
          <h3 id="cv-verify-title">Config Verification Run</h3>
          <p className="detail-muted">
            运行测试对这份草稿执行 prepublish 验证；只有 Passed 的 Run 才能作为 Label Contract 联合激活的证据。
          </p>
          <button className="secondary-button" disabled={running || runs.some((run) => run.state === 'Queued' || run.state === 'Running')} onClick={() => void startRun()}>
            {running ? '正在创建…' : '运行测试'}
          </button>
          {runError && (
            <p className="field-error" role="alert">
              {runError}
            </p>
          )}
          {runsError && <p className="field-error" role="alert">{runsError}</p>}
          {runs.length > 0 && (
            <ul className="verify-run-list">
              {runs.map((run) => (
                <li key={run.id}>
                  <div className="verify-run-row">
                    <span className={`status-pill ${run.state === 'Passed' ? 'ok' : run.state === 'Queued' || run.state === 'Running' ? 'waiting' : 'muted'}`}>
                      {verificationStateText[run.state]}
                    </span>
                    <span>Run #{run.id}</span>
                    <time dateTime={run.createdAt}>{formatTime(run.createdAt)}</time>
                    <button
                      className="text-button"
                      aria-expanded={expandedRunID === run.id}
                      onClick={() => setExpandedRunID((current) => (current === run.id ? null : run.id))}
                    >
                      {expandedRunID === run.id ? '收起结果' : '查看结果'}
                    </button>
                    {(run.state === 'Queued' || run.state === 'Running') && (
                      <button className="text-button" disabled={cancellingRunID !== null} onClick={() => void cancelRun(run)}>
                        {cancellingRunID === run.id ? '正在取消…' : '取消'}
                      </button>
                    )}
                  </div>
                  {expandedRunID === run.id && (
                    <VerificationRunChecks key={run.id} systemKey={systemKey} versionId={versionId} runId={run.id} onRunUpdated={loadRuns} />
                  )}
                </li>
              ))}
            </ul>
          )}
        </section>
      )}

      {isAdmin && detail.state === 'draft' && (
        <div className="publish-block">
          {confirming ? (
            <div className="confirm-dialog" role="dialog" aria-label="确认发布">
              <p>发布会把系统当前配置切换到 v{detail.versionSeq}（不可撤销；旧版本变为已替代）。</p>
              <div className="dialog-actions">
                <button className="primary-button" disabled={publishing} onClick={() => void publish()}>
                  {publishing ? '正在发布…' : '确认发布'}
                </button>
                <button className="secondary-button" disabled={publishing} onClick={() => setConfirming(false)}>
                  取消
                </button>
              </div>
            </div>
          ) : (
            <button className="primary-button" onClick={() => setConfirming(true)}>
              发布此版本
            </button>
          )}
          {publishError && (
            <p className="field-error" role="alert">
              {publishError}
            </p>
          )}
        </div>
      )}

      <section aria-labelledby="cv-projections-title">
        <h3 id="cv-projections-title">类型化投影</h3>
        <DiscoveryTable discoveries={detail.discoveries} />
        <PlanTable plans={detail.plans} />
      </section>

      <section aria-labelledby="cv-yaml-title">
        <h3 id="cv-yaml-title">上传原文</h3>
        <button className="secondary-button compact" aria-expanded={showYAML} onClick={() => setShowYAML((value) => !value)}>
          {showYAML ? '收起 YAML' : '展开 YAML'}
        </button>
        {showYAML && (
          <pre className="yaml-body" tabIndex={0}>
            {detail.yamlBody}
          </pre>
        )}
      </section>
    </div>
  )
}
