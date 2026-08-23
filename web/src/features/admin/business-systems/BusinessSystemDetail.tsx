import { useCallback, useEffect, useState } from 'react'
import {
  formatTime,
  getBusinessSystem,
  getResourceRefreshRun,
  listConfigVersions,
  listObservedResources,
  startResourceRefresh,
  type BusinessSystemDetail,
  type ConfigVersionSummary,
  type ObservedResourceSummary,
  type ResourceRefreshRunDetail,
} from './api'

// The system detail is a continuous page (UI-SYSTEM-003): current state,
// configuration versions, plan/discovery projections of the current
// published version. The version history lists every immutable version
// (UI-SYSTEM-005); publish actions live in the version detail.

interface BusinessSystemDetailPageProps {
  systemKey: string
  isAdmin: boolean
  onBack: () => void
  onOpenVersion: (systemKey: string, versionId: string) => void
}

export function BusinessSystemDetailPage({ systemKey, isAdmin, onBack, onOpenVersion }: BusinessSystemDetailPageProps) {
  const [detail, setDetail] = useState<BusinessSystemDetail | null>(null)
  const [versions, setVersions] = useState<ConfigVersionSummary[]>([])
  const [error, setError] = useState<string | null>(null)
  const [resources, setResources] = useState<ObservedResourceSummary[]>([])
  const [refreshing, setRefreshing] = useState(false)
  const [refreshMessage, setRefreshMessage] = useState('')
  const [refreshRun, setRefreshRun] = useState<ResourceRefreshRunDetail | null>(null)

  const reload = useCallback(() => {
    let cancelled = false
    void getBusinessSystem(systemKey)
      .then((value) => {
        if (!cancelled) setDetail(value)
      })
      .catch((reason: unknown) => {
        if (!cancelled) setError(reason instanceof Error ? reason.message : '暂时无法读取业务系统详情。')
      })
    void listObservedResources(systemKey)
      .then((items) => { if (!cancelled) setResources(items) })
      .catch(() => undefined)
    void listConfigVersions(systemKey)
      .then((items) => {
        if (!cancelled) setVersions(items)
      })
      .catch(() => undefined)
    return () => {
      cancelled = true
    }
  }, [systemKey])

  useEffect(reload, [reload])

  useEffect(() => {
    if (!refreshRun || !['Queued', 'Running'].includes(refreshRun.state)) return
    const timer = window.setTimeout(() => {
      void getResourceRefreshRun(systemKey, refreshRun.id)
        .then(async (run) => {
          setRefreshRun(run)
          setRefreshMessage(run.state === 'Running' || run.state === 'Queued' ? '资源刷新仍在进行；可离开此页，稍后返回查看结果。' : `资源刷新已${run.state}。`)
          if (!['Queued', 'Running'].includes(run.state)) setResources(await listObservedResources(systemKey))
        })
        .catch((reason: unknown) => setRefreshMessage(reason instanceof Error ? reason.message : '暂时无法读取资源刷新状态。'))
    }, 1000)
    return () => window.clearTimeout(timer)
  }, [systemKey, refreshRun])

  async function refreshResources() {
    setRefreshing(true)
    setRefreshMessage('正在提交资源刷新…')
    try {
      const run = await startResourceRefresh(systemKey)
      setRefreshRun(run)
      setRefreshMessage(run.state === 'Running' ? '资源刷新已开始；正在等待结果。' : `资源刷新：${run.state}`)
      const items = await listObservedResources(systemKey)
      setResources(items)
    } catch (reason) {
      setRefreshMessage(reason instanceof Error ? reason.message : '暂时无法开始资源刷新。')
    } finally { setRefreshing(false) }
  }

  if (error) {
    return (
      <div className="detail-content">
        <div className="error-summary" role="alert">
          <strong>暂时无法读取业务系统</strong>
          <span>{error}</span>
          <button className="secondary-button compact" onClick={onBack}>
            返回列表
          </button>
        </div>
      </div>
    )
  }
  if (!detail) {
    return (
      <div className="detail-content">
        <p className="inline-status">正在加载业务系统…</p>
      </div>
    )
  }
  return (
    <div className="detail-content business-system-detail">
      <div className="detail-header">
        <button className="secondary-button compact" onClick={onBack}>
          返回列表
        </button>
        <h2>{detail.displayName}</h2>
        <span className={`status-pill ${detail.enabled ? 'ok' : 'waiting'}`}>
          {detail.enabled ? 'Enabled' : 'Disabled'}
        </span>
      </div>
      <dl className="fact-list">
        <div>
          <dt>稳定 key</dt>
          <dd>{detail.key}</dd>
        </div>
        <div>
          <dt>时区</dt>
          <dd>{detail.timezone ?? '尚未发布'}</dd>
        </div>
        <div>
          <dt>资源刷新周期</dt>
          <dd>{detail.resourceRefreshIntervalSeconds ? `${detail.resourceRefreshIntervalSeconds} 秒` : '尚未发布'}</dd>
        </div>
        <div>
          <dt>配置版本数</dt>
          <dd>{detail.configVersionCount}</dd>
        </div>
      </dl>

      <section aria-labelledby="bs-versions-title">
        <h3 id="bs-versions-title">配置版本</h3>
        {versions.length === 0 ? (
          <p className="inline-status">还没有配置版本。</p>
        ) : (
          <ul className="version-history">
            {versions.map((version) => (
              <li key={version.id}>
                <button className="version-item" onClick={() => onOpenVersion(systemKey, version.id)}>
                  <span className="version-main">
                    <strong>v{version.versionSeq}</strong>
                    <span className={`status-pill ${version.state === 'published' ? 'ok' : version.state === 'draft' ? 'waiting' : 'muted'}`}>
                      {version.state === 'published' ? '已发布' : version.state === 'draft' ? '草稿' : '已替代'}
                    </span>
                    <span>{formatTime(version.createdAt)}</span>
                  </span>
                  <span className="version-digest" title={version.digest}>
                    {version.digest.slice(0, 12)}…
                  </span>
                </button>
              </li>
            ))}
          </ul>
        )}
      </section>

      <section aria-labelledby="bs-current-title">
        <h3 id="bs-current-title">当前配置投影</h3>
        {detail.currentConfigVersionId ? (
          <>
            <DiscoveryTable discoveries={detail.discoveries} />
            <section aria-labelledby="observed-resources-title">
              <div className="section-heading"><h4 id="observed-resources-title">已观测资源</h4>{isAdmin && <button className="secondary-button compact" onClick={() => void refreshResources()} disabled={refreshing}>{refreshing ? '正在提交…' : '立即刷新'}</button>}</div>
              {refreshMessage && <p className="inline-status" role="status">{refreshMessage}</p>}
              <ObservedResourceTable resources={resources} />
            </section>
            <PlanTable plans={detail.plans} />
          </>
        ) : (
          <p className="inline-status">尚未发布配置版本；上传草稿后可发布。</p>
        )}
      </section>
    </div>
  )
}

export function DiscoveryTable({ discoveries }: { discoveries: BusinessSystemDetail['discoveries'] }) {
  if (discoveries.length === 0) return <p className="inline-status">本版本没有资源发现。</p>
  return (
    <div className="table-wrap">
      <table className="fact-table">
        <caption className="visually-hidden">资源发现</caption>
        <thead>
          <tr>
            <th scope="col">发现 key</th>
            <th scope="col">显示名</th>
            <th scope="col">Selector</th>
            <th scope="col">身份 labels</th>
          </tr>
        </thead>
        <tbody>
          {discoveries.map((item) => (
            <tr key={item.discoveryKey}>
              <td>{item.discoveryKey}</td>
              <td>{item.displayName}</td>
              <td>
                <code>{item.selector}</code>
              </td>
              <td>{item.identityLabels.join(', ')}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

export function PlanTable({ plans }: { plans: BusinessSystemDetail['plans'] }) {
  if (plans.length === 0) return <p className="inline-status">本版本没有巡检计划。</p>
  return (
    <div className="plan-list">
      {plans.map((plan) => (
        <article key={plan.planKey} className="plan-card">
          <header>
            <strong>{plan.displayName}</strong>
            <span className="status-pill muted">{plan.cron ? `cron ${plan.cron}` : '仅人工运行'}</span>
          </header>
          {plan.checks.length === 0 ? (
            <p className="inline-status">该计划暂无巡检项。</p>
          ) : (
            <ul className="check-list">
              {plan.checks.map((check) => (
                <li key={check.checkKey}>
                  <span className="check-kind">{check.kind === 'promql' ? `PromQL · ${check.queryMode}` : '浏览器'}</span>
                  <strong>{check.displayName}</strong>
                  <span className="check-question">{check.analysisQuestion}</span>
                  {check.kind === 'promql' ? (
                    <code className="check-expression">{check.expression}</code>
                  ) : check.rangeSeconds !== undefined ? (
                    <span className="check-window">
                      窗口 {check.rangeSeconds}s · 步长 {check.stepSeconds}s
                    </span>
                  ) : null}
                </li>
              ))}
            </ul>
          )}
        </article>
      ))}
    </div>
  )
}

function ObservedResourceTable({ resources }: { resources: ObservedResourceSummary[] }) {
  if (resources.length === 0) return <p className="inline-status">尚未观测到资源。首次刷新完成后会显示在这里。</p>
  return <div className="table-wrap"><table className="fact-table"><caption className="visually-hidden">当前已观测资源</caption><thead><tr><th scope="col">发现项</th><th scope="col">身份</th><th scope="col">最后观测</th><th scope="col">新鲜度</th></tr></thead><tbody>{resources.map((resource) => <tr key={resource.id}><td>{resource.discoveryKey}</td><td>{Object.entries(resource.identityLabels).map(([name, value]) => `${name}=${value}`).join(', ')}</td><td>{resource.observedAt ? formatTime(resource.observedAt) : '—'}</td><td><span className={`status-pill ${resource.stale ? 'waiting' : 'ok'}`}>{resource.stale ? '已过期' : '当前'}</span></td></tr>)}</tbody></table></div>
}
