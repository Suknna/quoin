import { useEffect, useRef, useState } from 'react'
import {
  formatTime,
  getConfigVersion,
  publishBusinessSystemConfig,
  ConfigApiError,
  type ConfigVersionDetail,
} from './api'
import { DiscoveryTable, PlanTable } from './BusinessSystemDetail'

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
  const publishedNotified = useRef(false)

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
    return () => {
      cancelled = true
    }
  }, [systemKey, versionId])

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
